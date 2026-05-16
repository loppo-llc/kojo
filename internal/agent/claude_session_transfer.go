package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

// claude session JSONL transfer for §3.7 device switch.
//
// claude stores its conversation state at
// ~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl. The cwd
// kojo passes to claude is AgentDir(agentID) (see
// backend_claude.go: `cmd.Dir = agentDir(agent.ID)`), NOT the
// agent's Settings.workDir — workDir is for the user's "where my
// project files live" surface and is unrelated to claude's
// session JSONL placement.
//
// When an agent moves between peers, the JSONL files have to
// ride along — without them, `claude --continue` on the new peer
// launches a fresh conversation with no memory of the previous
// turns.
//
// Cross-platform: AgentDir is machine-local (a
// /Users/alice/.config/kojo-v1/agents/<id> path on macOS vs
// C:\Users\alice\AppData\Roaming\kojo-v1\agents\<id> on
// Windows). claudeEncodePath maps both shapes to a hyphenated
// project dir, so the source-side encoded dir won't match
// target's. We capture the JSONLs by content (read all files
// from source's AgentDir-derived project dir) and replay them
// under target's own AgentDir-derived project dir. The session_id
// (filename) stays the same; only the parent dir differs across
// peers.

// ClaudeSessionFile is one transferable JSONL entry: the
// session UUID (filename without .jsonl) plus the raw file body.
// Caller base64-encodes for the wire if the transport demands
// plain JSON.
type ClaudeSessionFile struct {
	SessionID string
	Content   []byte
}

// ReadClaudeSessionFiles pulls every session JSONL claude has
// recorded for the given agent's source AgentDir. Returns an
// empty slice (no error) if the project dir doesn't exist —
// agents that have never started a claude conversation have no
// state to migrate.
//
// File-content size has a per-file ceiling (claudeSessionMaxBytes)
// so a runaway log file can't blow up the agent-sync payload.
// Files larger than the ceiling are skipped with a warning marker
// (filename appears in the returned skipped slice).
func ReadClaudeSessionFiles(agentID string) ([]ClaudeSessionFile, []string, error) {
	if agentID == "" {
		return nil, nil, nil
	}
	absDir, err := filepath.Abs(AgentDir(agentID))
	if err != nil {
		return nil, nil, fmt.Errorf("agent.ReadClaudeSessionFiles: abs path: %w", err)
	}
	projectDir := claudeProjectDir(absDir)
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("agent.ReadClaudeSessionFiles: readdir: %w", err)
	}
	out := make([]ClaudeSessionFile, 0, len(entries))
	skipped := make([]string, 0)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		full := filepath.Join(projectDir, e.Name())
		st, statErr := os.Stat(full)
		if statErr != nil {
			continue
		}
		if st.Size() > claudeSessionMaxBytes {
			skipped = append(skipped, e.Name())
			continue
		}
		body, readErr := os.ReadFile(full)
		if readErr != nil {
			return nil, nil, fmt.Errorf("agent.ReadClaudeSessionFiles: read %s: %w", e.Name(), readErr)
		}
		sessionID := e.Name()[:len(e.Name())-len(".jsonl")]
		out = append(out, ClaudeSessionFile{SessionID: sessionID, Content: body})
	}
	return out, skipped, nil
}

// WriteClaudeSessionFiles materialises the source-captured JSONLs
// into target's claude project dir, computed from target's own
// AgentDir. The encoded path differs from source's (AgentDir is
// machine-local) but the per-file session_id is preserved so
// `claude --continue` finds the conversation it was running on
// the source peer.
//
// Existing files are overwritten — the assumption is that a
// device switch is the authoritative state transfer. Files from
// previous (now-stale) switches sitting in the same dir DO get
// clobbered if they share a session_id; otherwise they remain
// untouched.
//
// agentID is used to resolve target's AgentDir; the per-host
// $HOME and platform-specific AppData layout are handled by the
// kojo runtime so the same call works on macOS, Linux, and
// Windows. The target AgentDir is created (with parents) before
// the JSONL files are written so a fresh post-handoff agent on
// target doesn't fail with ENOENT.
func WriteClaudeSessionFiles(agentID string, files []ClaudeSessionFile) error {
	commit, rollback, err := StageClaudeSessionFiles(agentID, files)
	if err != nil {
		return err
	}
	if commit == nil {
		// No-op (empty file set). rollback is also nil.
		return nil
	}
	commit()
	_ = rollback
	return nil
}

// StageClaudeSessionFiles is the two-phase variant of
// WriteClaudeSessionFiles. It writes the JSONLs to their final
// paths (with backups of any pre-existing files held aside) and
// returns commit/rollback callbacks. commit() drops the backups
// — the new content is the canonical state. rollback() restores
// the backups — the agent's pre-sync session files are intact.
//
// The §3.7 agent-sync handler uses this so a DB sync failure
// AFTER the JSONL stage doesn't strand target with overwritten
// session files; the rollback restores the previous state.
//
// commit / rollback are nil-safe and idempotent (calling either
// twice is a no-op). Both are nil when files is empty.
func StageClaudeSessionFiles(agentID string, files []ClaudeSessionFile) (commit func(), rollback func(), err error) {
	if agentID == "" {
		return nil, nil, fmt.Errorf("agent.StageClaudeSessionFiles: agent_id required")
	}
	if len(files) == 0 {
		return nil, nil, nil
	}
	targetAgentDir, aerr := filepath.Abs(AgentDir(agentID))
	if aerr != nil {
		return nil, nil, fmt.Errorf("agent.StageClaudeSessionFiles: abs path: %w", aerr)
	}
	if merr := os.MkdirAll(targetAgentDir, 0o755); merr != nil {
		return nil, nil, fmt.Errorf("agent.StageClaudeSessionFiles: mkdir agent dir %s: %w", targetAgentDir, merr)
	}
	projectDir := claudeProjectDir(targetAgentDir)
	if merr := os.MkdirAll(projectDir, 0o755); merr != nil {
		return nil, nil, fmt.Errorf("agent.StageClaudeSessionFiles: mkdir %s: %w", projectDir, merr)
	}

	// Two-phase write to make the BATCH atomic-ish:
	//   1. Stage every file as a .tmp sibling next to its
	//      final path. Failure in this phase rolls back ALL
	//      tmps (no partial commit).
	//   2. Rename all tmps into place. Rename is per-file
	//      atomic on POSIX; the batch isn't, but staging makes
	//      a stage-time failure (validation, disk full, etc.)
	//      cheap to recover from. If a mid-batch rename fails
	//      we still try to roll back the renames we already
	//      committed — best-effort, but it bounds the partial-
	//      state surface to "one or two files renamed, the
	//      rest didn't".
	type staged struct {
		final string
		tmp   string
	}
	stagedFiles := make([]staged, 0, len(files))
	cleanupTmps := func() {
		for _, s := range stagedFiles {
			_ = os.Remove(s.tmp)
		}
	}

	for _, f := range files {
		if f.SessionID == "" {
			continue
		}
		// Cheap sanity check: refuse a session_id with path
		// separators — a hostile or buggy source could
		// otherwise escape projectDir by sending "../../etc".
		if filepath.Base(f.SessionID) != f.SessionID {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageClaudeSessionFiles: refusing session_id with path separators: %q", f.SessionID)
		}
		final := filepath.Join(projectDir, f.SessionID+".jsonl")
		tmp, terr := os.CreateTemp(projectDir, ".session-*.jsonl.tmp")
		if terr != nil {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageClaudeSessionFiles: create temp: %w", terr)
		}
		tmpPath := tmp.Name()
		stagedFiles = append(stagedFiles, staged{final: final, tmp: tmpPath})
		if _, werr := tmp.Write(f.Content); werr != nil {
			_ = tmp.Close()
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageClaudeSessionFiles: write %s: %w", f.SessionID, werr)
		}
		if cerr := tmp.Close(); cerr != nil {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageClaudeSessionFiles: close temp: %w", cerr)
		}
	}

	// Phase 2: backup existing files, then rename tmps into
	// place. On mid-batch failure we restore the backups so
	// target's existing --continue state survives the failed
	// sync (without the backup step a rename failure could
	// leave an existing JSONL replaced AND the rollback
	// "delete the new file" leaves the dst missing entirely —
	// worse than the no-op outcome).
	type backedUp struct {
		final  string
		backup string // empty when no pre-existing file
	}
	backups := make([]backedUp, 0, len(stagedFiles))
	rollbackBackups := func() {
		for _, b := range backups {
			if b.backup != "" {
				// best-effort restore: rename backup back to
				// its final path. If a fresh write already
				// landed there, remove that first.
				_ = os.Remove(b.final)
				_ = os.Rename(b.backup, b.final)
			} else {
				// No pre-existing file → just remove the
				// freshly-renamed one.
				_ = os.Remove(b.final)
			}
		}
	}
	for _, s := range stagedFiles {
		// Snapshot the existing final (if any) under a
		// timestamped backup name BEFORE renaming the tmp in.
		// CreateTemp picks a unique suffix so concurrent
		// switches can't collide.
		backupPath := ""
		if _, statErr := os.Stat(s.final); statErr == nil {
			bf, bErr := os.CreateTemp(projectDir, ".sync-backup-*.jsonl")
			if bErr != nil {
				rollbackBackups()
				cleanupTmps()
				return nil, nil, fmt.Errorf("agent.StageClaudeSessionFiles: backup temp: %w", bErr)
			}
			backupPath = bf.Name()
			_ = bf.Close()
			_ = os.Remove(backupPath) // Rename needs the target to not exist on Windows
			if rerr := os.Rename(s.final, backupPath); rerr != nil {
				rollbackBackups()
				cleanupTmps()
				return nil, nil, fmt.Errorf("agent.StageClaudeSessionFiles: backup %s: %w", s.final, rerr)
			}
		}
		if rerr := os.Rename(s.tmp, s.final); rerr != nil {
			// Restore the backup we just took for THIS file.
			// remove-then-rename so Windows + race scenarios
			// where s.final somehow exists despite our failed
			// rename can't block the restore Rename.
			if backupPath != "" {
				_ = os.Remove(s.final)
				_ = os.Rename(backupPath, s.final)
			}
			rollbackBackups()
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageClaudeSessionFiles: rename %s: %w", s.final, rerr)
		}
		backups = append(backups, backedUp{final: s.final, backup: backupPath})
	}
	// Renames committed; backups still on disk. commit/rollback
	// decide their fate.
	var done bool
	commit = func() {
		if done {
			return
		}
		done = true
		// Drop backups: the new content is canonical.
		for _, b := range backups {
			if b.backup != "" {
				_ = os.Remove(b.backup)
			}
		}
	}
	rollback = func() {
		if done {
			return
		}
		done = true
		rollbackBackups()
	}
	return commit, rollback, nil
}

// DefaultAgentWorkDir returns the portable per-peer agent work
// directory used when an §3.7 sync arrives without a workDir
// suitable for the local platform. Format:
// `<userhome>/.kojo/agent-workspaces/<agent_id>`. Resolves the
// home dir via os.UserHomeDir so $HOME (macOS / Linux) or
// %USERPROFILE% (Windows) is honored. Falls back to the kojo
// AgentDir as a last resort if home is unavailable.
func DefaultAgentWorkDir(agentID string) (string, error) {
	if agentID == "" {
		return "", fmt.Errorf("agent.DefaultAgentWorkDir: agent_id required")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Fall back to kojo's per-agent state dir; not as nice
		// for "claude reads my project files" but at least the
		// path exists and writable.
		return AgentDir(agentID), nil
	}
	return filepath.Join(home, ".kojo", "agent-workspaces", agentID), nil
}

// claudeSessionMaxBytes caps an individual JSONL file the
// transfer accepts. claude session files routinely reach a few
// MB; 32 MiB is comfortable headroom without inviting a hostile
// source to balloon the agent-sync payload.
const claudeSessionMaxBytes = 32 << 20
