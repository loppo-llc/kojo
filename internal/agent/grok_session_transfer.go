package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// grok session transfer for §3.7 device switch.
//
// Mirrors claude_session_transfer.go but for the Grok Build CLI's
// disk layout. Whereas claude stores one JSONL per conversation
// under ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl, grok writes
// a per-session DIRECTORY containing many small files (events.jsonl,
// chat_history.jsonl, summary.json, system_prompt.txt, …) plus an
// optional `terminal/` subdirectory of tool-call output logs:
//
//	$GROK_HOME/sessions/<encoded(abs-cwd)>/<uuid>/{events.jsonl,
//	  chat_history.jsonl, updates.jsonl, summary.json, ...,
//	  terminal/call-<uuid>-<n>.log, ...}
//
// In addition, the agent's RESUME POINTER lives in agentDir itself
// at `<agentDir>/.grok/session_id` — backend_grok.go reads this
// before every non-OneShot turn and passes `--resume <id>`. Without
// the pointer file target's grok would launch a fresh session even
// if the session directory is in place.
//
// So a complete grok handoff transfers both:
//
//	1. <agentDir>/.grok/session_id  → target's identical path.
//	2. $GROK_HOME/sessions/<encoded(source absAgentDir)>/<uuid>/
//	    → target's $GROK_HOME/sessions/<encoded(target absAgentDir)>/<uuid>/
//
// Item 2's parent directory differs across peers because AgentDir
// is machine-local (just like claude's encoded project dir). The
// session UUID stays the same so backend_grok.go's
// `--resume <uuid>` finds the migrated session under target's own
// encoded path.
//
// Cross-OneShot isolation: we transfer ONLY the primary session
// (the one stored in `.grok/session_id`). Any OneShot session
// directories grok created for Slack threads stay on source —
// they were never the device-switch target.

// GrokSessionFile is one transferable file from a grok session
// directory. RelPath is the path RELATIVE to the session UUID
// directory (e.g. "events.jsonl" or "terminal/call-abc-1.log").
// Caller base64-encodes Content for the wire if needed.
type GrokSessionFile struct {
	RelPath string
	Content []byte
}

// GrokSessionTransfer is the complete handoff payload for a grok
// agent. SessionID is the UUID that goes into `.grok/session_id`
// on target; Files are the contents of source's
// `$GROK_HOME/sessions/<encoded(absAgentDir)>/<sessionID>/`
// subtree. The wire layer is responsible for base64 of the file
// bodies if the transport is text-only.
type GrokSessionTransfer struct {
	SessionID string
	Files     []GrokSessionFile
}

// grokSessionFileMaxBytes caps an individual file the transfer
// accepts. grok session files are normally small (a few KB) but
// terminal/ logs can balloon for tool-heavy turns; 32 MiB per file
// matches the claude ceiling and keeps the agent-sync wire payload
// bounded.
const grokSessionFileMaxBytes = 32 << 20

// grokSessionTransferMaxFiles caps the file count to bound the
// per-message overhead of the JSON envelope (each file is a
// {rel_path, content_b64} pair). A healthy primary grok session
// has on the order of 10–50 files including terminal/ logs; an
// envelope ten times larger than that is a sign of corruption or
// an attacker trying to OOM target.
const grokSessionTransferMaxFiles = 1024

// grokSessionCoreFiles are the files that MUST land on target for
// `grok --resume <uuid>` to produce a sensible continuation. If
// any of these is oversized (or missing entirely) we abort the
// transfer rather than ship a torn session that resumes into a
// broken state on target. The list comes from inspecting an
// active grok 0.1.x session subtree: events.jsonl is the canonical
// turn log, chat_history.jsonl is what the UI replays, summary.json
// is the entry-point metadata, system_prompt.txt is the recorded
// system prompt grok seeds the next turn with.
//
// terminal/*.log files are tool-call output — large but
// individually skippable; missing log files only lose past tool
// output replays.
var grokSessionCoreFiles = map[string]struct{}{
	"events.jsonl":       {},
	"chat_history.jsonl": {},
	"summary.json":       {},
	"system_prompt.txt":  {},
}

// ReadGrokSessionFiles collects the agent's PRIMARY grok session
// for handoff. Returns (nil, nil, nil) when there is nothing to
// migrate — no `.grok/session_id` file, an unreadable / malformed
// session id, or an empty session directory. Those are NOT errors:
// a fresh agent or a non-grok agent simply has no grok state to
// move, and we want the orchestrator to ship a claude payload (if
// any) without failing the switch.
//
// File-content size has a per-file ceiling. Oversized
// non-core files are skipped with their RelPath captured in the
// returned skipped slice (caller logs a warning). Oversized CORE
// files (events.jsonl, chat_history.jsonl, summary.json,
// system_prompt.txt) abort the read with an error — shipping a
// session without those would let target resume into a broken
// state worse than starting fresh.
//
// Security: the session_id is validated against isGrokSessionID
// BEFORE it ever reaches filepath.Join, so a poisoned
// `.grok/session_id` (the agent can write its own workspace)
// cannot escape the session subtree.
func ReadGrokSessionFiles(agentID string) (*GrokSessionTransfer, []SkippedSessionFile, error) {
	if agentID == "" {
		return nil, nil, nil
	}
	unlock := lockGrokSessionTransfer(agentID)
	defer unlock()

	absAgentDir, err := filepath.Abs(AgentDir(agentID))
	if err != nil {
		return nil, nil, fmt.Errorf("agent.ReadGrokSessionFiles: abs path: %w", err)
	}

	sessionID := readGrokSessionID(absAgentDir)
	if sessionID == "" {
		return nil, nil, nil
	}

	sessionsDir := grokSessionDir(absAgentDir)
	if sessionsDir == "" {
		// $HOME / GROK_HOME not resolvable — degrade silently
		// so the switch can still ship claude state.
		return nil, nil, nil
	}
	sessionRoot := filepath.Join(sessionsDir, sessionID)
	absSessionRoot, err := filepath.Abs(sessionRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("agent.ReadGrokSessionFiles: abs session root: %w", err)
	}

	info, err := os.Stat(absSessionRoot)
	if err != nil {
		if os.IsNotExist(err) {
			// Pointer file is set but the directory is gone
			// (manual `grok sessions delete`, GC, peer-cleanup
			// race). Treat as "nothing to migrate" — the next
			// chat on target will start a fresh session.
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("agent.ReadGrokSessionFiles: stat session root: %w", err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("agent.ReadGrokSessionFiles: session root is not a directory: %s", absSessionRoot)
	}

	files := make([]GrokSessionFile, 0)
	skipped := make([]SkippedSessionFile, 0)
	var totalBytes int64

	walkErr := filepath.Walk(absSessionRoot, func(path string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if fi.IsDir() {
			return nil
		}
		if !fi.Mode().IsRegular() {
			// Skip symlinks, sockets, devices — anything we
			// can't faithfully replay on target.
			return nil
		}
		rel, err := filepath.Rel(absSessionRoot, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		// Normalise separator for the wire so a Windows source
		// sending "terminal\\foo.log" doesn't trip target's
		// containment check.
		rel = filepath.ToSlash(rel)

		// Defence in depth: must stay inside the session root.
		// filepath.Rel + walking from absSessionRoot guarantees
		// this in practice, but a future refactor that switches
		// to manual recursion shouldn't lose the guarantee.
		if rel == "." || strings.HasPrefix(rel, "../") || rel == ".." {
			return nil
		}

		if fi.Size() > grokSessionFileMaxBytes {
			// Aborting on oversized core files prevents target
			// from receiving a session that resumes into a
			// broken state. terminal/* and other non-core files
			// remain skippable.
			if _, isCore := grokSessionCoreFiles[rel]; isCore {
				return fmt.Errorf("core file %q exceeds size cap (%d bytes)", rel, fi.Size())
			}
			skipped = append(skipped, SkippedSessionFile{
				Path: rel, Reason: "oversized", SizeBytes: fi.Size(),
			})
			return nil
		}

		// Cumulative size guard. Catch a payload that would
		// blow past the wire cap BEFORE we copy every file
		// into RAM — otherwise a buggy / hostile source could
		// allocate up to grokSessionTransferMaxFiles ×
		// grokSessionFileMaxBytes (32 GiB) before the wire cap
		// rejects the request.
		totalBytes += fi.Size()
		if totalBytes > grokSessionTransferMaxTotalBytes {
			return fmt.Errorf("session payload exceeds %d bytes (read up to %s)",
				grokSessionTransferMaxTotalBytes, rel)
		}

		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", rel, readErr)
		}
		files = append(files, GrokSessionFile{RelPath: rel, Content: body})

		if len(files) > grokSessionTransferMaxFiles {
			return fmt.Errorf("session has more than %d files; refusing to build oversized handoff payload",
				grokSessionTransferMaxFiles)
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, fmt.Errorf("agent.ReadGrokSessionFiles: walk %s: %w", absSessionRoot, walkErr)
	}

	if len(files) == 0 {
		// Empty session dir — nothing useful to migrate. Treat
		// as "no grok state" so target doesn't end up with an
		// empty subtree that confuses the next --resume.
		return nil, skipped, nil
	}

	// Required-core completeness check: a transfer that's missing
	// events.jsonl / chat_history.jsonl / summary.json /
	// system_prompt.txt would land on target as a half-formed
	// session and `grok --resume` would dive into garbage. We
	// already abort on OVERSIZED core in the walk; this guard
	// covers the case where source's session was partially deleted
	// (e.g. a crashed `grok sessions delete --partial` left some
	// files behind but trimmed the core ones).
	present := make(map[string]struct{}, len(files))
	for _, f := range files {
		present[f.RelPath] = struct{}{}
	}
	for core := range grokSessionCoreFiles {
		if _, ok := present[core]; !ok {
			return nil, skipped, fmt.Errorf("session missing required core file %q at %s", core, absSessionRoot)
		}
	}

	return &GrokSessionTransfer{SessionID: sessionID, Files: files}, skipped, nil
}

// StageGrokSessionCleanup purges any pre-existing primary grok
// session state from target's agentDir. Used by the agent-sync
// handler when the inbound payload says the agent IS a grok agent
// but carries NO GrokSession block — i.e. source either has no
// session yet OR cleared it via ResetSession. Without this purge,
// target would keep the stale `.grok/session_id` file it inherited
// from a previous switch (or wrote during a local turn while it
// hosted the agent earlier) and the next chat would `--resume`
// that local UUID instead of starting fresh, presenting the user
// with a conversation that bears no relation to source's current
// state.
//
// Same two-phase contract as StageGrokSession: stages the deletion
// by RENAMING the pointer + session root to backups, returns
// commit / rollback callbacks. commit() drops the backups (the
// purge becomes canonical). rollback() restores them (state stays
// as it was).
//
// commit / rollback are nil-safe and idempotent. Returns
// (nil, nil, nil) when there is nothing to purge.
func StageGrokSessionCleanup(agentID string) (commit func(), rollback func(), err error) {
	if agentID == "" {
		return nil, nil, fmt.Errorf("agent.StageGrokSessionCleanup: agent_id required")
	}
	// Hold the per-agent lock across stage AND commit/rollback —
	// see StageGrokSession's docstring for the race rationale.
	releaseOnce := sync.OnceFunc(lockGrokSessionTransfer(agentID))
	lockReleasedToCallbacks := false
	defer func() {
		if !lockReleasedToCallbacks {
			releaseOnce()
		}
	}()

	absAgentDir, err := filepath.Abs(AgentDir(agentID))
	if err != nil {
		return nil, nil, fmt.Errorf("agent.StageGrokSessionCleanup: abs agent dir: %w", err)
	}

	// Locate both candidate paths. Either may be absent — we
	// stage what's there and skip what isn't.
	pointerPath := filepath.Join(absAgentDir, ".grok", "session_id")
	priorID := readGrokSessionID(absAgentDir)
	sessionsDir := grokSessionDir(absAgentDir)
	var sessionRoot string
	if sessionsDir != "" && priorID != "" {
		sessionRoot = filepath.Join(sessionsDir, priorID)
	}

	type backup struct {
		final  string
		backup string
	}
	var backups []backup
	rollbackAll := func() {
		for _, b := range backups {
			if rerr := os.Rename(b.backup, b.final); rerr != nil {
				slog.Default().Warn("StageGrokSessionCleanup: rollback failed to restore",
					"agent", agentID, "backup", b.backup, "final", b.final, "err", rerr)
			}
		}
	}

	// Stage the pointer first. If neither this nor the session
	// root exists, we have nothing to do.
	if _, statErr := os.Stat(pointerPath); statErr == nil {
		bf, bErr := os.CreateTemp(filepath.Dir(pointerPath), ".session_id-purge-*.tmp")
		if bErr != nil {
			return nil, nil, fmt.Errorf("agent.StageGrokSessionCleanup: backup temp pointer: %w", bErr)
		}
		bp := bf.Name()
		_ = bf.Close()
		_ = os.Remove(bp)
		if rerr := os.Rename(pointerPath, bp); rerr != nil {
			return nil, nil, fmt.Errorf("agent.StageGrokSessionCleanup: backup pointer: %w", rerr)
		}
		backups = append(backups, backup{final: pointerPath, backup: bp})
	}

	if sessionRoot != "" {
		if info, statErr := os.Stat(sessionRoot); statErr == nil && info.IsDir() {
			bp := sessionRoot + ".purge-" + priorID
			// CreateTemp on a directory isn't a thing; build a
			// unique sibling by appending the session id then
			// retrying if that exact name happens to exist.
			for i := 0; i < 8; i++ {
				if _, err := os.Stat(bp); os.IsNotExist(err) {
					break
				}
				bp = fmt.Sprintf("%s.%d", bp, i)
			}
			if rerr := os.Rename(sessionRoot, bp); rerr != nil {
				rollbackAll()
				return nil, nil, fmt.Errorf("agent.StageGrokSessionCleanup: backup session root: %w", rerr)
			}
			backups = append(backups, backup{final: sessionRoot, backup: bp})
		}
	}

	if len(backups) == 0 {
		return nil, nil, nil
	}

	lockReleasedToCallbacks = true

	var done bool
	commit = func() {
		if done {
			return
		}
		done = true
		defer releaseOnce()
		for _, b := range backups {
			if rerr := os.RemoveAll(b.backup); rerr != nil {
				slog.Default().Warn("StageGrokSessionCleanup: commit failed to drop backup",
					"agent", agentID, "path", b.backup, "err", rerr)
			}
		}
	}
	rollback = func() {
		if done {
			return
		}
		done = true
		defer releaseOnce()
		rollbackAll()
	}
	return commit, rollback, nil
}

// grokSessionTransferMaxTotalBytes caps the SUM of every file's
// content in a single transfer. A malicious / buggy source could
// otherwise stay under the per-file ceiling while shipping enough
// bytes to OOM target. 256 MiB is generous headroom (grok session
// dirs in practice are <10 MiB even with terminal/ logs) without
// being a free-for-all. var (not const) so tests can shrink the
// cap to drive the size-cap guard without allocating real-world
// quantities of RAM in CI.
var grokSessionTransferMaxTotalBytes int64 = 256 << 20

// grokSessionTransferMu serialises Stage / Cleanup per agentID so
// a finalize retry that races a concurrent prepareChat (or two
// orchestrators racing on a hub-back-to-this-peer switch) cannot
// leave half-renamed staging / backup roots lying around. Same
// pattern as device_switch_skill.go's per-agent mutex: map keyed
// by agentID, entries are never deleted so identity stays stable
// across the agent's lifetime, and the map itself is guarded by
// grokSessionTransferMapMu.
var grokSessionTransferLocks keyedMutex

func lockGrokSessionTransfer(agentID string) func() {
	return grokSessionTransferLocks.Lock(agentID)
}

// StageGrokSession materialises the source-captured grok session
// directory under target's own AgentDir-derived encoded path, plus
// the `<agentDir>/.grok/session_id` resume pointer.
//
// Layout produced on target:
//
//	$GROK_HOME/sessions/<encoded(target absAgentDir)>/<sessionID>/<files...>
//	<target agentDir>/.grok/session_id  ← contains sessionID
//
// The encoded path differs from source's because AgentDir is
// machine-local; the session UUID stays the same so backend_grok.go
// on target finds the migrated state via `--resume <sessionID>`.
//
// Implementation strategy is DIRECTORY SWAP, not per-file
// overwrite: every transferred file is staged into a fresh sibling
// `<sessionRoot>.staging-*` directory; on commit we rename the
// pre-existing `<sessionRoot>` (if any) to `<sessionRoot>.backup-*`,
// then rename `<sessionRoot>.staging-*` to `<sessionRoot>`. The
// resume pointer is staged separately. commit drops both
// backups; rollback restores them.
//
// Why directory swap instead of per-file overwrite: target may be
// holding stale files from a previous handoff (e.g. an aborted
// turn that wrote `events.jsonl.partial`). A naive per-file
// overwrite would leave those orphans on disk, and grok's
// `--resume` would happily pick them up. Swap guarantees target's
// post-commit `<sessionRoot>` contains EXACTLY the files source
// shipped.
//
// commit / rollback are nil-safe and idempotent. Both return
// nil-nil-nil when transfer is nil / empty — the caller treats
// that as "no grok state to migrate" (and may follow up with
// StageGrokSessionCleanup if it also wants target's stale state
// purged).
func StageGrokSession(agentID string, transfer *GrokSessionTransfer) (commit func(), rollback func(), err error) {
	if agentID == "" {
		return nil, nil, fmt.Errorf("agent.StageGrokSession: agent_id required")
	}
	if transfer == nil || len(transfer.Files) == 0 {
		return nil, nil, nil
	}
	// Hold the per-agent lock across the entire stage AND across
	// commit / rollback. A concurrent Stage / Cleanup that took
	// the lock between our rename-into-place and rollback's
	// rename-back would otherwise race for the sessionRoot path
	// and one of the two would end up renaming a directory it
	// didn't own. releaseOnce makes the unlock safe to call from
	// the defer (error paths) AND from commit/rollback (success
	// path) — first caller wins, the rest are no-ops.
	releaseOnce := sync.OnceFunc(lockGrokSessionTransfer(agentID))
	lockReleasedToCallbacks := false
	defer func() {
		if !lockReleasedToCallbacks {
			releaseOnce()
		}
	}()

	if !isGrokSessionID(transfer.SessionID) {
		// Refuse a malformed session id rather than letting it
		// land in `.grok/session_id` where the next turn's
		// `--resume` would crash or — worse — interpret a
		// crafted value as a CLI flag.
		return nil, nil, fmt.Errorf("agent.StageGrokSession: invalid session_id %q", transfer.SessionID)
	}
	if len(transfer.Files) > grokSessionTransferMaxFiles {
		return nil, nil, fmt.Errorf("agent.StageGrokSession: too many files (%d > %d)",
			len(transfer.Files), grokSessionTransferMaxFiles)
	}

	absAgentDir, err := filepath.Abs(AgentDir(agentID))
	if err != nil {
		return nil, nil, fmt.Errorf("agent.StageGrokSession: abs agent dir: %w", err)
	}
	if err := os.MkdirAll(absAgentDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("agent.StageGrokSession: mkdir agent dir: %w", err)
	}

	sessionsDir := grokSessionDir(absAgentDir)
	if sessionsDir == "" {
		return nil, nil, fmt.Errorf("agent.StageGrokSession: cannot resolve grok home")
	}
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("agent.StageGrokSession: mkdir sessions parent: %w", err)
	}
	sessionRoot := filepath.Join(sessionsDir, transfer.SessionID)

	pointerDir := filepath.Join(absAgentDir, ".grok")
	if err := os.MkdirAll(pointerDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("agent.StageGrokSession: mkdir pointer dir: %w", err)
	}
	pointerPath := filepath.Join(pointerDir, "session_id")

	// Stage every file under a fresh sibling directory so we can
	// atomically swap it into place. MkdirTemp picks a unique name.
	stagingRoot, err := os.MkdirTemp(sessionsDir, ".staging-"+transfer.SessionID+"-*")
	if err != nil {
		return nil, nil, fmt.Errorf("agent.StageGrokSession: mkdir staging root: %w", err)
	}
	stagingCleanup := func() { _ = os.RemoveAll(stagingRoot) }

	// Track seen relpaths to reject duplicates — a duplicate
	// entry would mean the second write silently overrides the
	// first under the same final path, which is non-deterministic
	// behaviour we don't want to inherit from the wire.
	//
	// `present` records the set of clean RELPATHS we accept, used
	// AFTER the loop to verify every required core file actually
	// landed in the staged payload. ReadGrokSessionFiles enforces
	// the same invariant on source, but Stage MUST re-check the
	// wire — a corrupted peer (or a future protocol version that
	// silently drops files in transit) could otherwise sneak an
	// incomplete session past target.
	seen := make(map[string]struct{}, len(transfer.Files))
	present := make(map[string]struct{}, len(transfer.Files))
	var totalBytes int64

	for _, f := range transfer.Files {
		// Reject any relpath that escapes the session root.
		// filepath.Clean + leading "../" / abs check covers
		// "../etc", "/etc/passwd", and Windows-style "C:\\…".
		cleanRel := filepath.Clean(filepath.FromSlash(f.RelPath))
		if cleanRel == "." || cleanRel == "" ||
			strings.HasPrefix(cleanRel, "..") ||
			filepath.IsAbs(cleanRel) ||
			strings.ContainsAny(cleanRel, "\x00") {
			stagingCleanup()
			return nil, nil, fmt.Errorf("agent.StageGrokSession: refusing relpath %q", f.RelPath)
		}
		// Normalise so case-only / separator-only collisions don't
		// slip past the seen map on case-insensitive filesystems
		// (NTFS, default APFS). On Linux the ToLower is over-
		// strict but a duplicate within a single session payload
		// is always source-side corruption — never legitimate.
		key := strings.ToLower(filepath.ToSlash(cleanRel))
		if _, dup := seen[key]; dup {
			stagingCleanup()
			return nil, nil, fmt.Errorf("agent.StageGrokSession: duplicate relpath %q", f.RelPath)
		}
		seen[key] = struct{}{}

		// Containment re-check on the joined STAGING path.
		final := filepath.Join(stagingRoot, cleanRel)
		rel, err := filepath.Rel(stagingRoot, final)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			stagingCleanup()
			return nil, nil, fmt.Errorf("agent.StageGrokSession: relpath %q escapes session root", f.RelPath)
		}

		if int64(len(f.Content)) > grokSessionFileMaxBytes {
			stagingCleanup()
			return nil, nil, fmt.Errorf("agent.StageGrokSession: file %q exceeds per-file cap (%d > %d)",
				f.RelPath, len(f.Content), grokSessionFileMaxBytes)
		}
		totalBytes += int64(len(f.Content))
		if totalBytes > grokSessionTransferMaxTotalBytes {
			stagingCleanup()
			return nil, nil, fmt.Errorf("agent.StageGrokSession: total payload exceeds %d bytes",
				grokSessionTransferMaxTotalBytes)
		}

		// Record the clean relpath so we can verify core
		// presence after the loop. Use the slash form (matches
		// grokSessionCoreFiles' keys, which are literal POSIX
		// paths read out of the spec).
		present[filepath.ToSlash(cleanRel)] = struct{}{}

		if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
			stagingCleanup()
			return nil, nil, fmt.Errorf("agent.StageGrokSession: mkdir %s: %w", filepath.Dir(final), err)
		}
		if err := os.WriteFile(final, f.Content, 0o644); err != nil {
			stagingCleanup()
			return nil, nil, fmt.Errorf("agent.StageGrokSession: write %s: %w", cleanRel, err)
		}
	}

	// Required-core completeness check on the wire payload. Source
	// already enforces this in ReadGrokSessionFiles, but Stage
	// MUST re-check — a corrupt peer or a wire-format bug could
	// otherwise let us write a `.grok/session_id` pointer that
	// resolves to a directory missing events.jsonl /
	// chat_history.jsonl / summary.json / system_prompt.txt and
	// leave target's `grok --resume` reading torn state.
	for core := range grokSessionCoreFiles {
		if _, ok := present[core]; !ok {
			stagingCleanup()
			return nil, nil, fmt.Errorf("agent.StageGrokSession: wire payload missing required core file %q", core)
		}
	}

	// Stage the resume-pointer file as a tmp sibling next to its
	// final path (NOT inside stagingRoot, since the pointer lives
	// in agentDir/.grok, not in $GROK_HOME).
	pointerTmp, terr := os.CreateTemp(pointerDir, ".session_id-*.tmp")
	if terr != nil {
		stagingCleanup()
		return nil, nil, fmt.Errorf("agent.StageGrokSession: create pointer temp: %w", terr)
	}
	pointerTmpPath := pointerTmp.Name()
	if _, werr := pointerTmp.WriteString(transfer.SessionID); werr != nil {
		_ = pointerTmp.Close()
		_ = os.Remove(pointerTmpPath)
		stagingCleanup()
		return nil, nil, fmt.Errorf("agent.StageGrokSession: write pointer: %w", werr)
	}
	if cerr := pointerTmp.Close(); cerr != nil {
		_ = os.Remove(pointerTmpPath)
		stagingCleanup()
		return nil, nil, fmt.Errorf("agent.StageGrokSession: close pointer: %w", cerr)
	}

	// Phase 2: atomically swap staging into place.
	//
	// Order matters for rollback safety:
	//   1. Move pre-existing session root (if any) aside.
	//   2. Rename staging root to session root.
	//   3. Move pre-existing pointer (if any) aside.
	//   4. Rename pointer tmp to pointer final.
	// On any error, undo what we already did and remove the
	// staging artefacts so target ends up exactly as it was.

	sessionBackup := ""
	if _, statErr := os.Stat(sessionRoot); statErr == nil {
		// Unique backup name; the trailing "-bk-" + uuid makes
		// concurrent switches' backups collision-free even if a
		// pathological collider attacker controls the uuid.
		sessionBackup = sessionRoot + ".bk-" + transfer.SessionID
		for i := 0; i < 8; i++ {
			if _, err := os.Stat(sessionBackup); os.IsNotExist(err) {
				break
			}
			sessionBackup = fmt.Sprintf("%s.%d", sessionBackup, i)
		}
		if rerr := os.Rename(sessionRoot, sessionBackup); rerr != nil {
			_ = os.Remove(pointerTmpPath)
			stagingCleanup()
			return nil, nil, fmt.Errorf("agent.StageGrokSession: backup session root: %w", rerr)
		}
	}
	if rerr := os.Rename(stagingRoot, sessionRoot); rerr != nil {
		// Restore session backup before bailing.
		if sessionBackup != "" {
			_ = os.Rename(sessionBackup, sessionRoot)
		}
		_ = os.Remove(pointerTmpPath)
		stagingCleanup()
		return nil, nil, fmt.Errorf("agent.StageGrokSession: rename staging to session root: %w", rerr)
	}

	pointerBackup := ""
	if _, statErr := os.Stat(pointerPath); statErr == nil {
		bf, bErr := os.CreateTemp(pointerDir, ".session_id-bk-*.tmp")
		if bErr != nil {
			// Undo session root swap.
			_ = os.RemoveAll(sessionRoot)
			if sessionBackup != "" {
				_ = os.Rename(sessionBackup, sessionRoot)
			}
			_ = os.Remove(pointerTmpPath)
			return nil, nil, fmt.Errorf("agent.StageGrokSession: backup pointer temp: %w", bErr)
		}
		pointerBackup = bf.Name()
		_ = bf.Close()
		if rerr := renameOverwrite(pointerPath, pointerBackup); rerr != nil {
			_ = os.RemoveAll(sessionRoot)
			if sessionBackup != "" {
				_ = os.Rename(sessionBackup, sessionRoot)
			}
			_ = os.Remove(pointerTmpPath)
			return nil, nil, fmt.Errorf("agent.StageGrokSession: backup pointer: %w", rerr)
		}
	}
	if rerr := os.Rename(pointerTmpPath, pointerPath); rerr != nil {
		// Restore pointer backup and session root backup.
		if pointerBackup != "" {
			_ = os.Rename(pointerBackup, pointerPath)
		}
		_ = os.RemoveAll(sessionRoot)
		if sessionBackup != "" {
			_ = os.Rename(sessionBackup, sessionRoot)
		}
		_ = os.Remove(pointerTmpPath)
		return nil, nil, fmt.Errorf("agent.StageGrokSession: rename pointer into place: %w", rerr)
	}

	// Hand the lock to commit / rollback — whichever fires first
	// releases it. The deferred releaseOnce becomes a no-op.
	lockReleasedToCallbacks = true

	var done bool
	commit = func() {
		if done {
			return
		}
		done = true
		defer releaseOnce()
		// commit failures are non-fatal — leaving a backup behind
		// just wastes disk on target. Surface them via the
		// default logger so an operator who's grepping for
		// "rollback failed" can also catch stuck backups.
		if sessionBackup != "" {
			if err := os.RemoveAll(sessionBackup); err != nil {
				slog.Default().Warn("StageGrokSession: commit failed to drop session backup",
					"agent", agentID, "path", sessionBackup, "err", err)
			}
		}
		if pointerBackup != "" {
			if err := os.Remove(pointerBackup); err != nil {
				slog.Default().Warn("StageGrokSession: commit failed to drop pointer backup",
					"agent", agentID, "path", pointerBackup, "err", err)
			}
		}
	}
	rollback = func() {
		if done {
			return
		}
		done = true
		defer releaseOnce()
		// Restore pointer first (cheap), then session root. We
		// log every failure but keep going — a partial restore
		// is still better than aborting and leaving target in a
		// fully-swapped-but-uncommitted state. On Windows a
		// file-in-use error can pin sessionRoot until the holder
		// releases it; operators see the warning and can clean
		// up manually.
		if rerr := os.Remove(pointerPath); rerr != nil && !os.IsNotExist(rerr) {
			slog.Default().Warn("StageGrokSession: rollback failed to remove new pointer",
				"agent", agentID, "path", pointerPath, "err", rerr)
		}
		if pointerBackup != "" {
			if rerr := os.Rename(pointerBackup, pointerPath); rerr != nil {
				slog.Default().Warn("StageGrokSession: rollback failed to restore pointer backup",
					"agent", agentID, "backup", pointerBackup, "final", pointerPath, "err", rerr)
			}
		}
		if rerr := os.RemoveAll(sessionRoot); rerr != nil {
			slog.Default().Warn("StageGrokSession: rollback failed to remove new session root",
				"agent", agentID, "path", sessionRoot, "err", rerr)
		}
		if sessionBackup != "" {
			if rerr := os.Rename(sessionBackup, sessionRoot); rerr != nil {
				slog.Default().Warn("StageGrokSession: rollback failed to restore session backup",
					"agent", agentID, "backup", sessionBackup, "final", sessionRoot, "err", rerr)
			}
		}
	}
	return commit, rollback, nil
}
