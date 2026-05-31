package agent

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
//   - Manager.prepareChat calls SyncDeviceSwitchSkillForTool right
//     after PrepareClaudeSettings. That dispatcher routes by
//     agent.Tool: claude/custom write the Claude-Code-flavored
//     body (deviceSwitchSkillBody / deviceSwitchSkillBodyWindows),
//     grok writes the grok-flavored body
//     (deviceSwitchSkillBodyGrokPOSIX / deviceSwitchSkillBodyGrokWindows
//     — no Claude-Code-only `!`exec`` substitution, mentions
//     `grok --resume` instead of `claude --continue`), codex writes
//     a `.codex/skills` body that mentions Codex thread resume, and
//     llama.cpp is no-op. The Update path additionally
//     re-fires the dispatcher whenever Tool changes so a tool
//     switch overwrites the on-disk body with the variant the
//     new backend can actually execute.
//
//   - For every supported tool, the underlying writer
//     (syncDeviceSwitchSkillBody) installs the SKILL.md when
//     (agent.IsDeviceSwitchEnabled() && peer count > 0), and
//     removes any stale file otherwise.
//
//   - The toggle defaults to true (agent.IsDeviceSwitchEnabled
//     returns true for a nil pointer). Operator disables via PATCH
//     /api/v1/agents/{id} { "deviceSwitchEnabled": false }.
//
//   - Single-node installs (no other peers registered) suppress the
//     skill regardless of the toggle — there's nothing to switch
//     to, and exposing the skill would have the backend offer a
//     tool that always 404s on dispatch.

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
// The body uses `!“…``` to inline shell output. claude executes the
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

- ` + "`completed`" + ` / ` + "`completed_finalize_failed`" + ` — switch succeeded
  (lock + blobs moved to target; for ` + "`completed_finalize_failed`" + `
  only target's runtime activation hook lagged, the agent IS on
  target). **Do not emit any further assistant text after this
  tool result.** The source PTY tears down within seconds and the
  sync snapshot to target was taken BEFORE the curl returned, so
  anything you stream here is dropped — neither the source DB nor
  target receives it. The target peer fires its own arrival prompt
  with the full migrated state; that is where the conversation
  continues. Treat the curl response as the end of your turn:
  no acknowledgement, no farewell, no plan, no summary. Just stop.
- ` + "`completed_with_lock_failure`" + ` — blob_refs migrated to
  target but no agent_lock row existed to move. Source still owns
  the chat session, so a normal text response IS persisted:
  surface ` + "`reason`" + ` to the user; operator inspects ` + "`agent_locks`" + `
  on target and may issue a manual Acquire if the agent should be
  locked.
- ` + "`aborted` / `abort_failed` / `complete_failed` / `source_drain_failed` / `complete_errored_lock_at_target`" + ` —
  switch did not happen (or completed only partially). Source is
  still the holder so your reply IS persisted normally: tell the
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

- ` + "`completed`" + ` / ` + "`completed_finalize_failed`" + ` — switch succeeded
  (lock + blobs moved to target; for ` + "`completed_finalize_failed`" + `
  only target's runtime activation hook lagged, the agent IS on
  target). **Do not emit any further assistant text after this
  tool result.** The source PTY tears down within seconds and the
  sync snapshot to target was taken BEFORE the curl returned, so
  anything you stream here is dropped — neither the source DB nor
  target receives it. The target peer fires its own arrival prompt
  with the full migrated state; that is where the conversation
  continues. Treat the curl response as the end of your turn:
  no acknowledgement, no farewell, no plan, no summary. Just stop.
- ` + "`completed_with_lock_failure`" + ` — blob_refs migrated to
  target but no agent_lock row existed to move. Source still owns
  the chat session, so a normal text response IS persisted:
  surface ` + "`reason`" + ` to the user; operator inspects ` + "`agent_locks`" + `
  on target and may issue a manual Acquire if the agent should be
  locked.
- ` + "`aborted` / `abort_failed` / `complete_failed` / `source_drain_failed` / `complete_errored_lock_at_target`" + ` —
  switch did not happen (or completed only partially). Source is
  still the holder so your reply IS persisted normally: tell the
  user it failed, surface ` + "`reason`" + ` verbatim, and include the
  response body so they can decide whether to retry.
`

// deviceSwitchSkillBodyGrokPOSIX is the grok-flavored SKILL.md
// body (POSIX shells: bash / zsh / fish on macOS / Linux).
// Differences from deviceSwitchSkillBody:
//
//   - No Claude-Code-only `!`curl …“ inline shell substitution.
//     grok renders inline-backticked text literally (binary
//     strings: "Inside fenced code blocks and inline backticked
//     text, content is shown literally") so an `!`-style command
//     would arrive at the LLM as a string, NOT as the curl
//     output. Instead the body shows a regular fenced bash block
//     and tells the agent to run it through its Bash tool.
//
//   - Description mentions `grok --resume` instead of
//     `claude --continue` — both backends resume by replaying the
//     migrated session, but the user-visible CLI invocation differs.
//
//   - Permission frontmatter still uses Bash(curl:*) — grok's
//     allowed-tools loader accepts the same Claude Code syntax
//     (confirmed via the grok inspect output for kojo agents).
const deviceSwitchSkillBodyGrokPOSIX = `---
name: kojo-switch-device
description: Migrate this agent to another peer host. Use when the user asks to move, switch, transfer, or hand off the conversation to a different machine (laptop, desktop, Windows box, another Mac, etc). The conversation resumes on the target peer via grok --resume with the full transcript, memory, persona, credentials, and grok session state carried over.
argument-hint: <peer-name-or-device-id>
allowed-tools: Bash(curl:*)
---

User asked to migrate this agent to: $ARGUMENTS

## 1. List peers and resolve the target device_id

The kojo server has a peer registry indexed by device_id. Pick the
entry whose ` + "`name`" + ` matches "$ARGUMENTS" (case-insensitive
substring is fine), whose ` + "`isSelf`" + ` is false, AND whose ` + "`status`" + `
is ` + "`online`" + `. If "$ARGUMENTS" already looks like a device_id (long
opaque string) use it directly, but still verify the row is online.

Run this curl through your Bash tool (do NOT trust an inline-
substituted result — grok shows inline-backticked text literally):

` + "```bash" + `
curl -skS -H "X-Kojo-Token: ${KOJO_AGENT_TOKEN}" "${KOJO_API_BASE}/api/v1/peers"
` + "```" + `

If no online peer matches, stop and tell the user the target was
not found or is offline, listing the available online peers.

## 2. Dispatch the switch

Once the target device_id is selected, POST it to the handoff
endpoint. The ` + "`-w`" + ` flag appends a trailing ` + "`HTTP_STATUS:<code>`" + `
line so the response body and the HTTP status code arrive in a
single ` + "`curl`" + ` invocation. Replace ` + "`<DEVICE_ID>`" + ` with the
resolved value:

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

- ` + "`completed`" + ` / ` + "`completed_finalize_failed`" + ` — switch succeeded
  (lock + blobs moved to target; for ` + "`completed_finalize_failed`" + `
  only target's runtime activation hook lagged, the agent IS on
  target). **Do not emit any further assistant text after this
  tool result.** The source grok process tears down within seconds
  and the sync snapshot to target was taken BEFORE the curl
  returned, so anything you stream here is dropped — neither the
  source DB nor target receives it. The target peer fires its own
  arrival prompt against the migrated grok session state; that is
  where the conversation continues. Treat the curl response as the
  end of your turn: no acknowledgement, no farewell, no plan, no
  summary. Just stop.
- ` + "`completed_with_lock_failure`" + ` — blob_refs migrated to
  target but no agent_lock row existed to move. Source still owns
  the chat session, so a normal text response IS persisted:
  surface ` + "`reason`" + ` to the user; operator inspects ` + "`agent_locks`" + `
  on target and may issue a manual Acquire if the agent should be
  locked.
- ` + "`aborted` / `abort_failed` / `complete_failed` / `source_drain_failed` / `complete_errored_lock_at_target`" + ` —
  switch did not happen (or completed only partially). Source is
  still the holder so your reply IS persisted normally: tell the
  user it failed, surface ` + "`reason`" + ` verbatim, and include the
  response body so they can decide whether to retry.
`

// deviceSwitchSkillBodyGrokWindows mirrors the grok POSIX body for
// cmd.exe / PowerShell on Windows. Same shell quoting rules as
// deviceSwitchSkillBodyWindows: %VAR% references default to cmd.exe;
// a note at the top instructs the agent to swap to $env: form when
// the detected shell is PowerShell.
const deviceSwitchSkillBodyGrokWindows = `---
name: kojo-switch-device
description: Migrate this agent to another peer host. Use when the user asks to move, switch, transfer, or hand off the conversation to a different machine (laptop, desktop, Windows box, another Mac, etc). The conversation resumes on the target peer via grok --resume with the full transcript, memory, persona, credentials, and grok session state carried over.
argument-hint: <peer-name-or-device-id>
allowed-tools: Bash(curl:*)
---

User asked to migrate this agent to: $ARGUMENTS

## 1. List peers and resolve the target device_id

The kojo server has a peer registry indexed by device_id. Pick the
entry whose ` + "`name`" + ` matches "$ARGUMENTS" (case-insensitive
substring is fine), whose ` + "`isSelf`" + ` is false, AND whose ` + "`status`" + `
is ` + "`online`" + `. If "$ARGUMENTS" already looks like a device_id (long
opaque string) use it directly, but still verify the row is online.

Run this curl through your Bash tool (adapt env-var syntax if
your shell is not cmd.exe — use $KOJO_AGENT_TOKEN / $KOJO_API_BASE
for bash, $env:KOJO_AGENT_TOKEN / $env:KOJO_API_BASE for
PowerShell):

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

- ` + "`completed`" + ` / ` + "`completed_finalize_failed`" + ` — switch succeeded
  (lock + blobs moved to target; for ` + "`completed_finalize_failed`" + `
  only target's runtime activation hook lagged, the agent IS on
  target). **Do not emit any further assistant text after this
  tool result.** The source grok process tears down within seconds
  and the sync snapshot to target was taken BEFORE the curl
  returned, so anything you stream here is dropped — neither the
  source DB nor target receives it. The target peer fires its own
  arrival prompt against the migrated grok session state; that is
  where the conversation continues. Treat the curl response as the
  end of your turn: no acknowledgement, no farewell, no plan, no
  summary. Just stop.
- ` + "`completed_with_lock_failure`" + ` — blob_refs migrated to
  target but no agent_lock row existed to move. Source still owns
  the chat session, so a normal text response IS persisted:
  surface ` + "`reason`" + ` to the user; operator inspects ` + "`agent_locks`" + `
  on target and may issue a manual Acquire if the agent should be
  locked.
- ` + "`aborted` / `abort_failed` / `complete_failed` / `source_drain_failed` / `complete_errored_lock_at_target`" + ` —
  switch did not happen (or completed only partially). Source is
  still the holder so your reply IS persisted normally: tell the
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
	// Select the OS-appropriate body: Windows gets a cmd.exe /
	// PowerShell-compatible variant; everything else gets the
	// POSIX body.
	body := deviceSwitchSkillBody
	if runtime.GOOS == "windows" {
		body = deviceSwitchSkillBodyWindows
	}
	syncDeviceSwitchSkillBody(agentID, body, enabled, logger)
}

// SyncGrokDeviceSwitchSkill is the grok-flavored counterpart to
// SyncDeviceSwitchSkill. Uses the same lock + atomic-write
// machinery but writes the deviceSwitchSkillBodyGrok* variant so
// the body no longer relies on Claude-Code-only `!`...“ inline
// shell substitution and mentions `grok --resume` instead of
// `claude --continue`. Same enable + peer-count gate.
func SyncGrokDeviceSwitchSkill(agentID string, enabled bool, logger *slog.Logger) {
	body := deviceSwitchSkillBodyGrokPOSIX
	if runtime.GOOS == "windows" {
		body = deviceSwitchSkillBodyGrokWindows
	}
	syncDeviceSwitchSkillBody(agentID, body, enabled, logger)
}

// SyncCodexDeviceSwitchSkill installs Codex's project-scoped skill
// under `.codex/skills`. The body follows the grok-compatible
// no-inline-exec shape because Codex skills do not support Claude
// Code's `!`...“ substitution syntax.
func SyncCodexDeviceSwitchSkill(agentID string, enabled bool, logger *slog.Logger) {
	body := codexDeviceSwitchSkillBody(deviceSwitchSkillBodyGrokPOSIX)
	if runtime.GOOS == "windows" {
		body = codexDeviceSwitchSkillBody(deviceSwitchSkillBodyGrokWindows)
	}
	syncDeviceSwitchSkillBodyAt(agentID, ".codex", body, enabled, logger)
}

func codexDeviceSwitchSkillBody(body string) string {
	repl := strings.NewReplacer(
		"grok --resume", "Codex app-server thread/resume",
		"grok session state", "Codex thread state",
		"grok agent", "Codex agent",
		"grok process", "codex process",
		"grok session", "Codex thread",
		"grok state", "Codex state",
		"Grok", "Codex",
		"grok", "codex",
	)
	return repl.Replace(body)
}

// syncDeviceSwitchSkillBody is the shared writer for both the
// claude/custom and grok flavors. Acquires the per-agent mutex,
// either RemoveAll's the skill subdir (when the agent should not
// see the skill) or atomicfile-writes the chosen body.
//
// Sharing the writer keeps the lock semantics identical across
// flavors — concurrent claude→grok tool changes that race a Sync
// with a Stage on the SKILL path cannot end up with a
// half-overwritten body, regardless of which flavor wins the
// rename.
func syncDeviceSwitchSkillBody(agentID, body string, enabled bool, logger *slog.Logger) {
	syncDeviceSwitchSkillBodyAt(agentID, ".claude", body, enabled, logger)
}

func syncDeviceSwitchSkillBodyAt(agentID, root, body string, enabled bool, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	unlock := lockDeviceSwitchSkill(agentID)
	defer unlock()
	dir := agentDir(agentID)
	skillDir := filepath.Join(dir, root, "skills", deviceSwitchSkillDirName)
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
	// atomicfile.WriteBytes is tmp-rename so a concurrent claude /
	// grok read can never see a half-written body. Combined with the
	// per-agent mutex above, sync is fully serialized.
	if err := atomicfile.WriteBytes(skillPath, []byte(body), 0o644); err != nil {
		logger.Warn("failed to write device-switch SKILL.md", "agent", agentID, "err", err)
		return
	}
	logger.Debug("device-switch skill installed", "agent", agentID, "path", skillPath)
}

// SyncDeviceSwitchSkillForTool is the backend-aware entry point that
// callers (prepareChat, peer arrival, Update) should use instead of
// the lower-level Sync*DeviceSwitchSkill variants. Dispatches on
// the agent's current Tool value:
//
//   - "claude" / "custom": install the Claude-Code body when
//     enabled, remove otherwise (SyncDeviceSwitchSkill).
//
//   - "grok": install the grok-flavored body when enabled, remove
//     otherwise (SyncGrokDeviceSwitchSkill). The grok body avoids
//     Claude-Code-only `!`exec“ substitution and mentions
//     `grok --resume` instead of `claude --continue`. Writing
//     OVER any pre-existing claude-body SKILL.md is intentional:
//     a tool change must yield a body the new backend can
//     execute.
//
//   - "codex": install the codex-flavored body under `.codex/skills`
//     when enabled, remove otherwise (SyncCodexDeviceSwitchSkill).
//
//   - any other tool (llama.cpp): no-op.
func SyncDeviceSwitchSkillForTool(agentID, tool string, enabled bool, logger *slog.Logger) {
	switch tool {
	case "claude", "custom":
		SyncDeviceSwitchSkill(agentID, enabled, logger)
	case "grok":
		SyncGrokDeviceSwitchSkill(agentID, enabled, logger)
	case "codex":
		SyncCodexDeviceSwitchSkill(agentID, enabled, logger)
	}
}
