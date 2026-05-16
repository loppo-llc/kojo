package agent

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/loppo-llc/kojo/internal/atomicfile"
)

// kojo-switch-device skill (§3.7 device-switch).
//
// The skill lives at
//
//	<agentDir>/.claude/skills/kojo-switch-device/SKILL.md
//
// alongside the existing settings.local.json written by
// PrepareClaudeSettings. Per-agent placement keeps the skill bound to
// the agent's cwd (which the claude CLI walks up from when it
// resolves project-scoped skills), so we don't need to rely on the
// repository-root walk-up termination — which the official docs
// describe but do not specify the exact stop condition for. Shared
// placement under <configdir>/.claude/skills/ would be ambiguous on
// non-git installs.
//
// Lifecycle:
//
//   - Manager.prepareChat calls SyncDeviceSwitchSkill right after
//     PrepareClaudeSettings. The function (re)writes the SKILL.md
//     when (agent.IsDeviceSwitchEnabled() && peer count > 0), and
//     removes any stale file otherwise.
//
//   - The toggle defaults to true (agent.IsDeviceSwitchEnabled
//     returns true for a nil pointer). Operator disables via PATCH
//     /api/v1/agents/{id} { "deviceSwitchEnabled": false }.
//
//   - Single-node installs (no other peers registered) suppress the
//     skill regardless of the toggle — there's nothing to switch
//     to, and exposing the skill would have claude offer a tool
//     that always 404s on dispatch.

const deviceSwitchSkillDirName = "kojo-switch-device"

// deviceSwitchSkillBody is the SKILL.md content. Frontmatter follows
// the format documented at https://code.claude.com/docs/en/skills.md.
//
// Key fields:
//
//   - description: claude reads this to decide auto-invocation. Put
//     the use case first; the combined description + when_to_use is
//     truncated at 1,536 chars by the CLI.
//
//   - allowed-tools: pre-approves the curl calls so the skill body
//     can drive the begin → pull → complete chain without prompting
//     for permission on every step.
//
//   - argument-hint: shows up in the /-menu; "<peer-name>" is the
//     human-readable label expected from the operator.
//
// The body uses `!``…``` to inline shell output. claude executes the
// command and substitutes the result; the LLM only sees the
// rendered text.
const deviceSwitchSkillBody = `---
name: kojo-switch-device
description: Migrate this agent to another peer host. Use when the user asks to move, switch, transfer, or hand off the conversation to a different machine (laptop, desktop, Windows box, another Mac, etc). The conversation resumes on the target peer via claude --continue with the full transcript, memory, persona, and credentials carried over.
argument-hint: <peer-name-or-device-id>
allowed-tools: Bash(curl:*)
---

User asked to migrate this agent to: $ARGUMENTS

## 1. List peers and resolve the target device_id

The kojo server has a peer registry indexed by device_id. Pick
the entry whose ` + "`name`" + ` matches "$ARGUMENTS" (case-insensitive
substring is fine), whose ` + "`isSelf`" + ` is false, AND whose ` + "`status`" + `
is ` + "`online`" + `. If "$ARGUMENTS" already looks like a device_id (long
opaque string) use it directly, but still verify the row is online.

!` + "`curl -skS -H \"X-Kojo-Token: ${KOJO_AGENT_TOKEN}\" \"${KOJO_API_BASE}/api/v1/peers\"`" + `

If no online peer matches, stop and tell the user the target was
not found or is offline, listing the available online peers.

## 2. Dispatch the switch

Once the target device_id is selected, POST it to the handoff
endpoint. The ` + "`-w`" + ` flag appends a trailing ` + "`HTTP_STATUS:<code>`" + `
line so the response body and the HTTP status code arrive in a
single ` + "`curl`" + ` invocation — that keeps the call inside the
single-binary ` + "`Bash(curl:*)`" + ` pre-approval matcher (mixing in
` + "`mktemp`" + ` / ` + "`rm`" + ` would require their own pre-approvals). Replace
` + "`<DEVICE_ID>`" + ` with the resolved value:

` + "```bash" + `
curl -skS -X POST \
  -H "X-Kojo-Token: ${KOJO_AGENT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"target_peer_id":"<DEVICE_ID>"}' \
  -w '\nHTTP_STATUS:%{http_code}\n' \
  "${KOJO_API_BASE}/api/v1/agents/${KOJO_AGENT_ID}/handoff/switch"
` + "```" + `

The last line of stdout is ` + "`HTTP_STATUS:<code>`" + `; everything
before it is the JSON response body.

## 3. Interpret the response

If the status code is not 2xx, dump the body verbatim to the
user — the body's ` + "`error.message`" + ` field usually explains the
failure (bad target name, target unreachable, agent busy with
another mutation, etc). Do NOT retry automatically.

On 2xx, the response JSON's ` + "`outcome`" + ` field decides the next
action. When the outcome is anything other than ` + "`completed`" + `,
relay the explanation to the user verbatim (do NOT paraphrase or
guess). Pick the field by outcome:

- ` + "`completed_finalize_failed`" + ` → ` + "`finalize_error`" + ` (target's
  runtime activation hook returned this exact string).
- Everything else → ` + "`reason`" + ` (per-step failure detail set by
  the orchestrator). When ` + "`reason`" + ` is missing, dump the entire
  response body verbatim instead — silently swallowing the
  failure is worse than a noisy dump.

Outcome catalog:

- ` + "`completed`" + ` — switch succeeded; this PTY will terminate
  shortly and the agent resumes on the target peer. Tell the
  user the migration is done.
- ` + "`completed_finalize_failed`" + ` — lock + blobs moved but
  target's runtime activation hook failed. Surface
  ` + "`finalize_error`" + ` to the user; the agent IS on target but the
  operator may need to restart it manually.
- ` + "`completed_with_lock_failure`" + ` — blob_refs migrated to
  target but no agent_lock row existed to move. Surface ` + "`reason`" + `
  to the user; operator inspects ` + "`agent_locks`" + ` on target and
  may issue a manual Acquire if the agent should be locked.
- ` + "`aborted` / `abort_failed` / `complete_failed` / `source_drain_failed` / `complete_errored_lock_at_target`" + ` —
  switch did not happen (or completed only partially). Tell the
  user it failed, surface ` + "`reason`" + ` verbatim, and include the
  response body so they can decide whether to retry.
`

// deviceSwitchSkillBodyWindows is the Windows-compatible variant of
// the SKILL.md. Differences from the POSIX body:
//
//   - Uses `curl` (not `curl.exe`); on Windows 10 1803+ `curl`
//     resolves to the system curl.exe in cmd.exe / git-bash. In
//     PowerShell `curl` is an alias to Invoke-WebRequest, but
//     Claude Code's Bash tool pre-approval `Bash(curl:*)` targets
//     the bare `curl` invocation. If PowerShell is the shell,
//     Claude should use `curl.exe` explicitly — the body notes
//     this in Step 1.
//   - Avoids inline execution (!`...`) which depends on a POSIX-
//     compatible shell; uses a regular code block and instructs
//     Claude to run it via Bash tool.
//   - Uses double-quote escaping that works in cmd.exe and
//     git-bash (the two shells Claude Code may choose on Windows).
//   - Environment variable references use %VAR% form with a note
//     to Claude to adapt if it detects a different shell.
const deviceSwitchSkillBodyWindows = `---
name: kojo-switch-device
description: Migrate this agent to another peer host. Use when the user asks to move, switch, transfer, or hand off the conversation to a different machine (laptop, desktop, Windows box, another Mac, etc). The conversation resumes on the target peer via claude --continue with the full transcript, memory, persona, and credentials carried over.
argument-hint: <peer-name-or-device-id>
allowed-tools: Bash(curl:*)
---

User asked to migrate this agent to: $ARGUMENTS

## 1. List peers and resolve the target device_id

The kojo server has a peer registry indexed by device_id. Pick
the entry whose ` + "`name`" + ` matches "$ARGUMENTS" (case-insensitive
substring is fine), whose ` + "`isSelf`" + ` is false, AND whose ` + "`status`" + `
is ` + "`online`" + `. If "$ARGUMENTS" already looks like a device_id (long
opaque string) use it directly, but still verify the row is online.

Run this to get the peer list (adapt env-var syntax if your shell
is not cmd.exe — use $KOJO_AGENT_TOKEN / $KOJO_API_BASE for bash,
$env:KOJO_AGENT_TOKEN / $env:KOJO_API_BASE for PowerShell):

` + "```" + `
curl -skS -H "X-Kojo-Token: %KOJO_AGENT_TOKEN%" "%KOJO_API_BASE%/api/v1/peers"
` + "```" + `

If no online peer matches, stop and tell the user the target was
not found or is offline, listing the available online peers.

## 2. Dispatch the switch

Once the target device_id is selected, POST it to the handoff
endpoint. Replace ` + "`<DEVICE_ID>`" + ` with the resolved value:

` + "```" + `
curl -skS -X POST -H "X-Kojo-Token: %KOJO_AGENT_TOKEN%" -H "Content-Type: application/json" -d "{\"target_peer_id\":\"<DEVICE_ID>\"}" -w "\nHTTP_STATUS:%{http_code}\n" "%KOJO_API_BASE%/api/v1/agents/%KOJO_AGENT_ID%/handoff/switch"
` + "```" + `

The last line of stdout is ` + "`HTTP_STATUS:<code>`" + `; everything
before it is the JSON response body.

## 3. Interpret the response

If the status code is not 2xx, dump the body verbatim to the
user — the body's ` + "`error.message`" + ` field usually explains the
failure (bad target name, target unreachable, agent busy with
another mutation, etc). Do NOT retry automatically.

On 2xx, the response JSON's ` + "`outcome`" + ` field decides the next
action. When the outcome is anything other than ` + "`completed`" + `,
relay the explanation to the user verbatim (do NOT paraphrase or
guess). Pick the field by outcome:

- ` + "`completed_finalize_failed`" + ` → ` + "`finalize_error`" + ` (target's
  runtime activation hook returned this exact string).
- Everything else → ` + "`reason`" + ` (per-step failure detail set by
  the orchestrator). When ` + "`reason`" + ` is missing, dump the entire
  response body verbatim instead — silently swallowing the
  failure is worse than a noisy dump.

Outcome catalog:

- ` + "`completed`" + ` — switch succeeded; this PTY will terminate
  shortly and the agent resumes on the target peer. Tell the
  user the migration is done.
- ` + "`completed_finalize_failed`" + ` — lock + blobs moved but
  target's runtime activation hook failed. Surface
  ` + "`finalize_error`" + ` to the user; the agent IS on target but the
  operator may need to restart it manually.
- ` + "`completed_with_lock_failure`" + ` — blob_refs migrated to
  target but no agent_lock row existed to move. Surface ` + "`reason`" + `
  to the user; operator inspects ` + "`agent_locks`" + ` on target and
  may issue a manual Acquire if the agent should be locked.
- ` + "`aborted` / `abort_failed` / `complete_failed` / `source_drain_failed` / `complete_errored_lock_at_target`" + ` —
  switch did not happen (or completed only partially). Tell the
  user it failed, surface ` + "`reason`" + ` verbatim, and include the
  response body so they can decide whether to retry.
`

// deviceSwitchSkillMu serializes SyncDeviceSwitchSkill per-agent so
// concurrent prepareChat / Manager.Update callers never have one
// goroutine RemoveAll the skill subdir while another is mid-write
// (or one truncate-write a stale body while another reads it from
// claude). map keyed by agent_id; entries are NEVER deleted so the
// mutex identity is stable across the agent's lifetime. The map
// itself is guarded by deviceSwitchSkillMapMu.
var (
	deviceSwitchSkillMapMu sync.Mutex
	deviceSwitchSkillMu    = map[string]*sync.Mutex{}
)

func lockDeviceSwitchSkill(agentID string) func() {
	deviceSwitchSkillMapMu.Lock()
	mu, ok := deviceSwitchSkillMu[agentID]
	if !ok {
		mu = &sync.Mutex{}
		deviceSwitchSkillMu[agentID] = mu
	}
	deviceSwitchSkillMapMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// SyncDeviceSwitchSkill writes or removes the kojo-switch-device
// SKILL.md based on (enabled && peerCountLookup() > 0). Idempotent:
// safe to call on every prepareChat. Failures are logged at warn
// level; the agent can still run without the skill, so an I/O
// error here must not block the chat.
//
// The caller passes the toggle explicitly (typically via
// agent.IsDeviceSwitchEnabled()); peer count is read from the
// package-level callback wired in cmd/kojo/main.go.
//
// On Windows, a cmd.exe / PowerShell-compatible body is installed
// (deviceSwitchSkillBodyWindows) using curl.exe — shipped in
// C:\Windows\System32 since Windows 10 1803. POSIX body stays
// for macOS / Linux.
func SyncDeviceSwitchSkill(agentID string, enabled bool, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	unlock := lockDeviceSwitchSkill(agentID)
	defer unlock()
	dir := agentDir(agentID)
	skillDir := filepath.Join(dir, ".claude", "skills", deviceSwitchSkillDirName)
	skillPath := filepath.Join(skillDir, "SKILL.md")

	shouldInstall := enabled && LookupPeerCount() > 0
	if !shouldInstall {
		// Remove any prior install so a toggle-off (or the last
		// peer being removed) cleans up promptly. RemoveAll on
		// the skill subdir leaves the surrounding .claude/ tree
		// intact — settings.local.json and other skills are
		// unaffected.
		if err := os.RemoveAll(skillDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("failed to remove device-switch skill", "agent", agentID, "err", err)
		}
		return
	}
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		logger.Warn("failed to create device-switch skill dir", "agent", agentID, "err", err)
		return
	}
	// Select the OS-appropriate body: Windows gets a cmd.exe /
	// PowerShell-compatible variant; everything else gets the
	// POSIX body.
	body := deviceSwitchSkillBody
	if runtime.GOOS == "windows" {
		body = deviceSwitchSkillBodyWindows
	}
	// atomicfile.WriteBytes is tmp-rename so a concurrent claude
	// read can never see a half-written body. Combined with the
	// per-agent mutex above, sync is fully serialized.
	if err := atomicfile.WriteBytes(skillPath, []byte(body), 0o644); err != nil {
		logger.Warn("failed to write device-switch SKILL.md", "agent", agentID, "err", err)
		return
	}
	logger.Debug("device-switch skill installed", "agent", agentID, "path", skillPath)
}
