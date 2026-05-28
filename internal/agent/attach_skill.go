package agent

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/loppo-llc/kojo/internal/atomicfile"
)

// kojo-attach skill.
//
// Mechanism: the agent writes the file it wants to attach into a
// dedicated subdirectory of its working dir
//
//	<agentDir>/.kojo/attach/
//
// During the turn the LLM may write any number of files there. When
// the turn completes, manager.processChatEvents calls
// ScanAndIngestAttachments (attach_scan.go) which moves every file
// out of that directory into the blob store and attaches a
// MessageAttachment to the assistant Message. On non-hub peers the
// bytes are also forwarded to hub via the peer-signed ingest
// endpoint so the UI (which always lives on hub) can preview /
// download them through the standard /api/v1/blob/{scope}/{path}
// route.
//
// We deliberately use a magic directory instead of a parse marker
// inside the agent's chat text: it keeps the user-visible reply
// clean, works identically for claude / codex / grok / llama.cpp
// (none of them have to mutate the streamed text), and lets the
// agent stage many files atomically by writing then renaming into
// place.
//
// Lifecycle (mirrors device_switch_skill.go):
//
//   - Manager.prepareChat calls SyncAttachSkill right after
//     PrepareClaudeSettings / SyncDeviceSwitchSkill.
//   - The skill is installed only when the blob store is wired and
//     the operator has not opted out via PATCH /api/v1/agents/{id}
//     {"attachmentsEnabled": false}. There is no peer-count gate:
//     unlike device-switch, attachments work in single-node mode
//     too (the bytes land in the local blob store).
//   - Idempotent — safe to call on every prepareChat. A toggle-off
//     removes the SKILL.md and the surrounding kojo-attach/ subdir
//     in one RemoveAll so claude no longer sees the skill.

const attachSkillDirName = "kojo-attach"

// attachStagingSubpath is the path (relative to agentDir) where the
// agent writes files it wants to attach. The scan code uses the
// same constant so the SKILL.md and the daemon agree on a single
// well-known directory.
const attachStagingSubpath = ".kojo/attach"

// attachSkillBody is the SKILL.md content for the kojo-attach skill.
// Frontmatter follows the format documented at
// https://code.claude.com/docs/en/skills.md.
//
// The skill is intentionally short and concrete: tell the agent
// where to write files, what kojo does after the turn, and a few
// gotchas (subdirectories not preserved, only files written under
// .kojo/attach are picked up).
const attachSkillBody = `---
name: kojo-attach
description: Send a file (image, audio, video, PDF, archive, anything) to the user as a downloadable attachment on your next reply. Use whenever you need to surface bytes the user must see in the chat UI — generated images, rendered charts, fetched documents, build artifacts. The file is forwarded to the user automatically; you do NOT have to paste a link or describe a path in your reply.
---

## How to attach a file

1. Make sure the directory ` + "`.kojo/attach/`" + ` exists under your
   current working directory:

   ` + "```bash" + `
   mkdir -p .kojo/attach
   ` + "```" + `

2. Write (or move) the file you want to send into
   ` + "`.kojo/attach/<your-filename>`" + `. Use a plain filename
   (no subdirectories — they are ignored). Examples:

   ` + "```bash" + `
   cp /tmp/chart.png .kojo/attach/chart.png
   mv ./out/report.pdf .kojo/attach/report.pdf
   ` + "```" + `

3. Finish your reply normally. As soon as your turn ends kojo
   moves every file out of ` + "`.kojo/attach/`" + `, stores it in the
   user-facing blob store, attaches it to the message you just
   sent, and (when you are running on a non-hub peer) forwards a
   copy to the hub so the UI can download it.

## Rules

- Only regular files directly inside ` + "`.kojo/attach/`" + ` are
  picked up. Symlinks, sockets, FIFOs, hidden dotfiles, and any
  nested subdirectories are ignored.
- Per-file size is capped at 10 GiB (the same ceiling the user
  upload path enforces). Anything larger is skipped with a
  warning in the daemon log.
- The directory is emptied after every turn. Do not stash files
  there hoping to reference them in a later turn — copy them
  somewhere else if you need them.
- You do not need to mention the attachment in your reply.
  The UI renders previewable thumbnails (images / video) or a
  download chip (everything else) on the assistant bubble. A
  short caption (« here is the chart you asked for ») is still
  fine if it helps the user, but writing the raw path or a curl
  command in the reply is redundant.
- Filenames are sanitised: the daemon keeps a basename only,
  rejects ` + "`..`" + ` segments, and resolves clashes by appending
  a numeric suffix. Pick a descriptive base name and let kojo
  worry about uniqueness.
`

// attachSkillMu serializes SyncAttachSkill per-agent for the same
// reason device_switch_skill.go does: concurrent prepareChat /
// Manager.Update callers must not race a RemoveAll against a
// mid-write. Map keyed by agent_id; entries are never deleted so
// the mutex identity stays stable across the agent's lifetime.
var (
	attachSkillMapMu sync.Mutex
	attachSkillMu    = map[string]*sync.Mutex{}
)

func lockAttachSkill(agentID string) func() {
	attachSkillMapMu.Lock()
	mu, ok := attachSkillMu[agentID]
	if !ok {
		mu = &sync.Mutex{}
		attachSkillMu[agentID] = mu
	}
	attachSkillMapMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// SyncAttachSkill writes or removes the kojo-attach SKILL.md based
// on `enabled`. Idempotent: safe to call on every prepareChat.
// Failures are logged at warn level; the agent can still run
// without the skill so an I/O error here must not block the chat.
//
// Skill placement mirrors SyncDeviceSwitchSkill: per-agent under
// <agentDir>/.claude/skills/kojo-attach/SKILL.md. The claude CLI
// walks up from its cwd looking for skills/, and we set cmd.Dir to
// agentDir when spawning, so the skill is in scope.
func SyncAttachSkill(agentID string, enabled bool, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	unlock := lockAttachSkill(agentID)
	defer unlock()
	dir := agentDir(agentID)
	skillDir := filepath.Join(dir, ".claude", "skills", attachSkillDirName)
	skillPath := filepath.Join(skillDir, "SKILL.md")

	if !enabled {
		// Remove any prior install so a toggle-off cleans up
		// promptly. RemoveAll on the skill subdir leaves the
		// surrounding .claude/ tree intact — settings.local.json
		// and other skills are unaffected.
		if err := os.RemoveAll(skillDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("failed to remove attach skill", "agent", agentID, "err", err)
		}
		return
	}
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		logger.Warn("failed to create attach skill dir", "agent", agentID, "err", err)
		return
	}
	// atomicfile.WriteBytes is tmp-rename so a concurrent claude
	// read can never see a half-written body. Combined with the
	// per-agent mutex above, sync is fully serialized.
	if err := atomicfile.WriteBytes(skillPath, []byte(attachSkillBody), 0o644); err != nil {
		logger.Warn("failed to write attach SKILL.md", "agent", agentID, "err", err)
		return
	}
	logger.Debug("attach skill installed", "agent", agentID, "path", skillPath)
}
