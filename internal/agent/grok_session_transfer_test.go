package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// withGrokHome plants $GROK_HOME for the test and creates the
// directory. Restores on cleanup.
func withGrokHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	gh := filepath.Join(home, "grok-home")
	if err := os.MkdirAll(gh, 0o755); err != nil {
		t.Fatalf("mkdir grok home: %v", err)
	}
	t.Setenv("GROK_HOME", gh)
	return gh
}

// plantGrokSession writes a session_id pointer in <agentDir>/.grok
// and a session subtree under $GROK_HOME/sessions/<encoded-cwd>/
// <uuid>/ that matches what real grok 0.1.x lays down. Returns the
// session UUID so callers can assert against it.
func plantGrokSession(t *testing.T, agentID, sessionID string) string {
	t.Helper()

	dir := agentDir(agentID)
	if err := os.MkdirAll(filepath.Join(dir, ".grok"), 0o755); err != nil {
		t.Fatalf("mkdir .grok: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".grok", "session_id"), []byte(sessionID), 0o644); err != nil {
		t.Fatalf("write session_id: %v", err)
	}

	sessionRoot := filepath.Join(grokSessionDir(dir), sessionID)
	if err := os.MkdirAll(filepath.Join(sessionRoot, "terminal"), 0o755); err != nil {
		t.Fatalf("mkdir session subtree: %v", err)
	}
	for path, body := range map[string]string{
		"events.jsonl":                   `{"type":"start"}` + "\n",
		"chat_history.jsonl":             `{"role":"user","text":"hi"}` + "\n",
		"summary.json":                   `{"messages":1}`,
		"system_prompt.txt":              "you are a helpful agent",
		"terminal/call-abc-1.log":        "tool output line 1\nline 2\n",
	} {
		full := filepath.Join(sessionRoot, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return sessionID
}

// TestReadGrokSessionFiles_RoundTrip plants a realistic grok
// session, reads it via the public API, and asserts every file
// is captured with the correct relpath + content. Sorts the
// returned files so the test is order-insensitive (filepath.Walk
// orders by lexical directory traversal, but pinning that is
// brittle and not part of the contract).
func TestReadGrokSessionFiles_RoundTrip(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_read_rt"
	const sessionID = "019e588f-419b-7202-9cff-1647e57116d5"
	plantGrokSession(t, agentID, sessionID)

	transfer, skipped, err := ReadGrokSessionFiles(agentID)
	if err != nil {
		t.Fatalf("ReadGrokSessionFiles: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("unexpected skipped files: %v", skipped)
	}
	if transfer == nil {
		t.Fatalf("transfer is nil; want populated")
	}
	if transfer.SessionID != sessionID {
		t.Errorf("session_id mismatch: got %q want %q", transfer.SessionID, sessionID)
	}

	got := make(map[string]string, len(transfer.Files))
	for _, f := range transfer.Files {
		got[f.RelPath] = string(f.Content)
	}
	want := map[string]string{
		"events.jsonl":            `{"type":"start"}` + "\n",
		"chat_history.jsonl":      `{"role":"user","text":"hi"}` + "\n",
		"summary.json":            `{"messages":1}`,
		"system_prompt.txt":       "you are a helpful agent",
		"terminal/call-abc-1.log": "tool output line 1\nline 2\n",
	}
	if len(got) != len(want) {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("file count mismatch: got %d (%v) want %d", len(got), keys, len(want))
	}
	for rel, wantBody := range want {
		gotBody, ok := got[rel]
		if !ok {
			t.Errorf("missing file %q", rel)
			continue
		}
		if gotBody != wantBody {
			t.Errorf("file %q content mismatch: got %q want %q", rel, gotBody, wantBody)
		}
	}
}

// TestReadGrokSessionFiles_NoPointer covers fresh agents and
// non-grok agents: no .grok/session_id → ReadGrokSessionFiles
// returns (nil, nil, nil) and the orchestrator carries on without
// a grok payload.
func TestReadGrokSessionFiles_NoPointer(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_no_pointer"
	if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
		t.Fatalf("mkdir agentDir: %v", err)
	}

	transfer, skipped, err := ReadGrokSessionFiles(agentID)
	if err != nil {
		t.Fatalf("ReadGrokSessionFiles: %v", err)
	}
	if transfer != nil {
		t.Errorf("transfer non-nil for fresh agent: %+v", transfer)
	}
	if len(skipped) != 0 {
		t.Errorf("unexpected skipped: %v", skipped)
	}
}

// TestReadGrokSessionFiles_MalformedPointer covers the security
// path: a poisoned `.grok/session_id` (the agent has write access
// to its own workspace) must be rejected — the value never makes
// it to filepath.Join, and the pointer file is removed by the
// underlying readGrokSessionID so the next turn starts fresh.
func TestReadGrokSessionFiles_MalformedPointer(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_bad_pointer"
	dir := agentDir(agentID)
	if err := os.MkdirAll(filepath.Join(dir, ".grok"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Inject a value that would escape the session dir if joined
	// naively. readGrokSessionID rejects + removes it.
	if err := os.WriteFile(filepath.Join(dir, ".grok", "session_id"), []byte("../../etc"), 0o644); err != nil {
		t.Fatalf("write poisoned pointer: %v", err)
	}

	transfer, _, err := ReadGrokSessionFiles(agentID)
	if err != nil {
		t.Fatalf("ReadGrokSessionFiles surfaced an error for a poisoned pointer: %v", err)
	}
	if transfer != nil {
		t.Errorf("transfer non-nil for poisoned pointer: %+v", transfer)
	}
	// Poisoned pointer should have been removed.
	if _, err := os.Stat(filepath.Join(dir, ".grok", "session_id")); !os.IsNotExist(err) {
		t.Errorf("poisoned session_id file not removed: err=%v", err)
	}
}

// TestReadGrokSessionFiles_PointerMissingDir covers the stale-
// pointer case (manual `grok sessions delete`, GC, peer cleanup
// race): session_id resolves to a directory that no longer exists.
// Return (nil, nil, nil) so the orchestrator ships the rest of the
// payload and the next chat starts a fresh session.
func TestReadGrokSessionFiles_PointerMissingDir(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_missing_dir"
	const sessionID = "019e588f-419b-7202-9cff-1647e57116d5"
	dir := agentDir(agentID)
	if err := os.MkdirAll(filepath.Join(dir, ".grok"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".grok", "session_id"), []byte(sessionID), 0o644); err != nil {
		t.Fatalf("write pointer: %v", err)
	}
	// Intentionally do NOT create the session subtree.

	transfer, _, err := ReadGrokSessionFiles(agentID)
	if err != nil {
		t.Fatalf("ReadGrokSessionFiles: %v", err)
	}
	if transfer != nil {
		t.Errorf("transfer non-nil despite missing session dir: %+v", transfer)
	}
}

// TestStageGrokSession_RoundTrip plants a transfer, stages + commits
// it, and verifies target's agentDir has the resume pointer plus
// every transferred file under target's encoded session path.
func TestStageGrokSession_RoundTrip(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_stage_rt"
	const sessionID = "019e588f-419b-7202-9cff-1647e57116d5"
	// All four core files are required by Stage's wire-side
	// presence check (mirror of Read's source-side check), so
	// every Stage round-trip test must include them.
	transfer := &GrokSessionTransfer{
		SessionID: sessionID,
		Files: []GrokSessionFile{
			{RelPath: "events.jsonl", Content: []byte("ev1\n")},
			{RelPath: "chat_history.jsonl", Content: []byte(`{"role":"user"}` + "\n")},
			{RelPath: "summary.json", Content: []byte(`{"n":1}`)},
			{RelPath: "system_prompt.txt", Content: []byte("you are an agent")},
			{RelPath: "terminal/call-1.log", Content: []byte("out\n")},
		},
	}

	commit, rollback, err := StageGrokSession(agentID, transfer)
	if err != nil {
		t.Fatalf("StageGrokSession: %v", err)
	}
	if commit == nil || rollback == nil {
		t.Fatalf("commit/rollback are nil for non-empty transfer")
	}
	commit()

	// Resume pointer landed.
	pointer, err := os.ReadFile(filepath.Join(agentDir(agentID), ".grok", "session_id"))
	if err != nil {
		t.Fatalf("read pointer: %v", err)
	}
	if string(pointer) != sessionID {
		t.Errorf("pointer mismatch: got %q want %q", pointer, sessionID)
	}

	// Every file landed under the target's encoded session path.
	sessionRoot := filepath.Join(grokSessionDir(agentDir(agentID)), sessionID)
	for _, f := range transfer.Files {
		full := filepath.Join(sessionRoot, filepath.FromSlash(f.RelPath))
		got, err := os.ReadFile(full)
		if err != nil {
			t.Errorf("read %s: %v", f.RelPath, err)
			continue
		}
		if string(got) != string(f.Content) {
			t.Errorf("file %q content mismatch: got %q want %q", f.RelPath, got, f.Content)
		}
	}
}

// TestStageGrokSession_Rollback exercises the rollback path:
// stage + rollback() must leave NO new files behind and restore
// any pre-existing pointer / session-dir state.
func TestStageGrokSession_Rollback(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_stage_rb"
	const oldID = "019e0000-0000-7000-8000-000000000001"
	const newID = "019e1111-1111-7000-8000-000000000002"

	// Pre-populate target with an existing pointer + a file
	// inside the prior session subtree.
	dir := agentDir(agentID)
	if err := os.MkdirAll(filepath.Join(dir, ".grok"), 0o755); err != nil {
		t.Fatalf("mkdir .grok: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".grok", "session_id"), []byte(oldID), 0o644); err != nil {
		t.Fatalf("write old pointer: %v", err)
	}
	oldRoot := filepath.Join(grokSessionDir(dir), oldID)
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		t.Fatalf("mkdir old root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(oldRoot, "events.jsonl"), []byte("old-events\n"), 0o644); err != nil {
		t.Fatalf("write old events: %v", err)
	}

	transfer := &GrokSessionTransfer{
		SessionID: newID,
		Files: []GrokSessionFile{
			{RelPath: "events.jsonl", Content: []byte("new-events\n")},
			{RelPath: "chat_history.jsonl", Content: []byte(`{"role":"user"}` + "\n")},
			{RelPath: "summary.json", Content: []byte(`{"n":1}`)},
			{RelPath: "system_prompt.txt", Content: []byte("you are an agent")},
		},
	}
	commit, rollback, err := StageGrokSession(agentID, transfer)
	if err != nil {
		t.Fatalf("StageGrokSession: %v", err)
	}
	if commit == nil || rollback == nil {
		t.Fatalf("commit/rollback are nil")
	}
	rollback()

	// Pointer restored.
	pointer, err := os.ReadFile(filepath.Join(dir, ".grok", "session_id"))
	if err != nil {
		t.Fatalf("read pointer after rollback: %v", err)
	}
	if string(pointer) != oldID {
		t.Errorf("pointer not restored: got %q want %q", pointer, oldID)
	}

	// New session root must not exist (no pre-existing target
	// for those files; rollback removes them outright).
	newRoot := filepath.Join(grokSessionDir(dir), newID)
	if _, err := os.Stat(filepath.Join(newRoot, "events.jsonl")); !os.IsNotExist(err) {
		t.Errorf("new session file lingered after rollback: err=%v", err)
	}

	// Old subtree still intact.
	oldBody, err := os.ReadFile(filepath.Join(oldRoot, "events.jsonl"))
	if err != nil {
		t.Errorf("old events disturbed by rollback: %v", err)
	}
	if string(oldBody) != "old-events\n" {
		t.Errorf("old events content changed: got %q", oldBody)
	}
}

// TestStageGrokSession_RejectsBadRelPath confirms path-traversal
// attempts are refused BEFORE any file is touched on disk. The
// agent itself controls the contents of a grok session subtree,
// so the wire must enforce containment.
func TestStageGrokSession_RejectsBadRelPath(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_stage_bad"
	const sessionID = "019e588f-419b-7202-9cff-1647e57116d5"

	for _, bad := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"foo/../../bar",
		"",
		".",
		"..",
	} {
		t.Run(bad, func(t *testing.T) {
			transfer := &GrokSessionTransfer{
				SessionID: sessionID,
				Files: []GrokSessionFile{
					{RelPath: bad, Content: []byte("x")},
				},
			}
			_, _, err := StageGrokSession(agentID, transfer)
			if err == nil {
				t.Errorf("Stage accepted bad relpath %q", bad)
			} else if !strings.Contains(err.Error(), "relpath") {
				t.Logf("Stage rejected %q with %v (acceptable — non-relpath error path)", bad, err)
			}
		})
	}
}

// TestStageGrokSession_RejectsBadSessionID confirms a non-UUID
// session_id is refused: it would otherwise land in
// `.grok/session_id` and the next `--resume` would either fail
// loudly or — worse — interpret a crafted value as a CLI flag.
func TestStageGrokSession_RejectsBadSessionID(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_stage_bad_id"
	transfer := &GrokSessionTransfer{
		SessionID: "--cwd=/",
		Files: []GrokSessionFile{
			{RelPath: "events.jsonl", Content: []byte("x")},
		},
	}
	_, _, err := StageGrokSession(agentID, transfer)
	if err == nil {
		t.Fatalf("Stage accepted non-UUID session_id")
	}
}

// TestStageGrokSession_EmptyTransferIsNoOp confirms a nil / empty
// transfer is a no-op (no error, no commit/rollback closures) so
// the orchestrator can call it unconditionally for non-grok
// agents.
func TestStageGrokSession_EmptyTransferIsNoOp(t *testing.T) {
	const agentID = "ag_grok_stage_empty"
	for _, tr := range []*GrokSessionTransfer{
		nil,
		{SessionID: "019e588f-419b-7202-9cff-1647e57116d5"},
	} {
		commit, rollback, err := StageGrokSession(agentID, tr)
		if err != nil {
			t.Errorf("empty transfer surfaced error: %v", err)
		}
		if commit != nil || rollback != nil {
			t.Errorf("empty transfer returned non-nil callbacks: commit=%v rollback=%v", commit != nil, rollback != nil)
		}
	}
}

// TestStageGrokSessionCleanup_PurgesPriorState covers the tombstone
// branch: target inherited a prior `.grok/session_id` + session
// subtree from an earlier time it hosted the agent, and source's
// fresh sync carries NO GrokSession (because source has none). The
// cleanup helper must purge both surfaces on commit and restore
// them on rollback.
func TestStageGrokSessionCleanup_PurgesPriorState(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_tomb"
	const priorID = "019e0000-0000-7000-8000-000000000001"

	// Plant target's prior state.
	plantGrokSession(t, agentID, priorID)
	pointerPath := filepath.Join(agentDir(agentID), ".grok", "session_id")
	priorRoot := filepath.Join(grokSessionDir(agentDir(agentID)), priorID)
	if _, err := os.Stat(pointerPath); err != nil {
		t.Fatalf("pre: pointer missing: %v", err)
	}
	if _, err := os.Stat(priorRoot); err != nil {
		t.Fatalf("pre: prior root missing: %v", err)
	}

	commit, rollback, err := StageGrokSessionCleanup(agentID)
	if err != nil {
		t.Fatalf("StageGrokSessionCleanup: %v", err)
	}
	if commit == nil || rollback == nil {
		t.Fatalf("expected non-nil commit/rollback for populated prior state")
	}
	commit()

	if _, err := os.Stat(pointerPath); !os.IsNotExist(err) {
		t.Errorf("pointer not purged: err=%v", err)
	}
	if _, err := os.Stat(priorRoot); !os.IsNotExist(err) {
		t.Errorf("prior session root not purged: err=%v", err)
	}
}

// TestStageGrokSessionCleanup_RollbackRestores plants prior state,
// stages the cleanup, then ROLLs back. Both surfaces must reappear
// at their original paths with original content.
func TestStageGrokSessionCleanup_RollbackRestores(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_tomb_rb"
	const priorID = "019e0000-0000-7000-8000-000000000001"
	plantGrokSession(t, agentID, priorID)

	commit, rollback, err := StageGrokSessionCleanup(agentID)
	if err != nil {
		t.Fatalf("StageGrokSessionCleanup: %v", err)
	}
	_ = commit
	rollback()

	pointerPath := filepath.Join(agentDir(agentID), ".grok", "session_id")
	if got, err := os.ReadFile(pointerPath); err != nil {
		t.Errorf("pointer not restored: %v", err)
	} else if string(got) != priorID {
		t.Errorf("pointer content drift: got %q want %q", got, priorID)
	}
	priorRoot := filepath.Join(grokSessionDir(agentDir(agentID)), priorID)
	if _, err := os.Stat(filepath.Join(priorRoot, "events.jsonl")); err != nil {
		t.Errorf("prior events.jsonl not restored: %v", err)
	}
}

// TestStageGrokSessionCleanup_NoPriorState confirms the helper is a
// no-op (returns nil-nil-nil) when target has nothing to purge —
// a fresh peer that has never hosted the agent.
func TestStageGrokSessionCleanup_NoPriorState(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_tomb_empty"
	if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	commit, rollback, err := StageGrokSessionCleanup(agentID)
	if err != nil {
		t.Fatalf("StageGrokSessionCleanup: %v", err)
	}
	if commit != nil || rollback != nil {
		t.Errorf("expected nil callbacks for empty prior state; got commit=%v rollback=%v", commit != nil, rollback != nil)
	}
}

// TestStageGrokSession_StaleFilesPurgedOnSwap proves the directory-
// swap strategy actually removes stale files from a prior session.
// Without the swap, a per-file overwrite would leave any file source
// did NOT ship sitting under <sessionRoot> and grok --resume would
// pick it up.
func TestStageGrokSession_StaleFilesPurgedOnSwap(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_swap_stale"
	const sessionID = "019e0000-0000-7000-8000-000000000001"

	// Plant target with a stale session that includes a file
	// source's transfer will NOT include.
	plantGrokSession(t, agentID, sessionID)
	staleFile := filepath.Join(grokSessionDir(agentDir(agentID)), sessionID, "stale-file.json")
	if err := os.WriteFile(staleFile, []byte("STALE"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	transfer := &GrokSessionTransfer{
		SessionID: sessionID,
		Files: []GrokSessionFile{
			{RelPath: "events.jsonl", Content: []byte("fresh\n")},
			{RelPath: "chat_history.jsonl", Content: []byte(`{"role":"user"}` + "\n")},
			{RelPath: "summary.json", Content: []byte(`{"n":1}`)},
			{RelPath: "system_prompt.txt", Content: []byte("you are an agent")},
		},
	}
	commit, _, err := StageGrokSession(agentID, transfer)
	if err != nil {
		t.Fatalf("StageGrokSession: %v", err)
	}
	commit()

	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Errorf("stale file survived directory swap: %v", err)
	}
}

// TestStageGrokSession_DuplicateRelPath rejects a wire payload that
// has the same relpath twice. Allowing it would silently let the
// last-write-wins behaviour decide which content lands on disk;
// the receiver should fail loud instead.
func TestStageGrokSession_DuplicateRelPath(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_dup"
	const sessionID = "019e0000-0000-7000-8000-000000000001"
	transfer := &GrokSessionTransfer{
		SessionID: sessionID,
		Files: []GrokSessionFile{
			{RelPath: "events.jsonl", Content: []byte("a")},
			{RelPath: "events.jsonl", Content: []byte("b")},
		},
	}
	_, _, err := StageGrokSession(agentID, transfer)
	if err == nil {
		t.Fatalf("Stage accepted duplicate relpath")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate; got %v", err)
	}
}

// TestStageGrokSession_TotalSizeCap rejects a payload whose
// per-file ceiling each entry passes but whose SUM exceeds the
// total-payload cap. Without this an attacker could ship thousands
// of just-under-ceiling files to OOM target. We shrink the cap to
// a few KB for the duration of the test (the var is package-
// internal and exists for exactly this purpose) so CI doesn't
// have to allocate the real 256 MiB.
func TestStageGrokSession_TotalSizeCap(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_size"
	const sessionID = "019e0000-0000-7000-8000-000000000001"

	prevCap := grokSessionTransferMaxTotalBytes
	grokSessionTransferMaxTotalBytes = 4 * 1024
	t.Cleanup(func() { grokSessionTransferMaxTotalBytes = prevCap })

	half := make([]byte, grokSessionTransferMaxTotalBytes/2+1)
	transfer := &GrokSessionTransfer{
		SessionID: sessionID,
		Files: []GrokSessionFile{
			{RelPath: "events.jsonl", Content: half},
			{RelPath: "chat_history.jsonl", Content: half},
		},
	}
	_, _, err := StageGrokSession(agentID, transfer)
	if err == nil {
		t.Fatalf("Stage accepted oversized total payload")
	}
	if !strings.Contains(err.Error(), "total payload") {
		t.Errorf("error should mention total payload; got %v", err)
	}
}

// TestReadGrokSessionFiles_TotalSizeCap covers the source-side
// cap that mirrors the stage-side one. Without it source could
// allocate up to grokSessionTransferMaxFiles ×
// grokSessionFileMaxBytes (32 GiB) before the wire cap rejected
// the request. Use the same var-shrink trick to keep CI alloc
// small.
func TestReadGrokSessionFiles_TotalSizeCap(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_read_size"
	const sessionID = "019e0000-0000-7000-8000-000000000001"
	plantGrokSession(t, agentID, sessionID)

	prevCap := grokSessionTransferMaxTotalBytes
	grokSessionTransferMaxTotalBytes = 16 // smaller than the seeded files
	t.Cleanup(func() { grokSessionTransferMaxTotalBytes = prevCap })

	_, _, err := ReadGrokSessionFiles(agentID)
	if err == nil {
		t.Fatalf("Read accepted oversized total payload")
	}
	if !strings.Contains(err.Error(), "session payload exceeds") {
		t.Errorf("error should mention session payload; got %v", err)
	}
}

// TestReadGrokSessionFiles_MissingCoreFile covers the case where
// source's session subtree exists but is missing a required core
// file (events.jsonl etc.) — e.g. a half-deleted session from a
// crashed `grok sessions delete --partial`. The read must fail
// loud rather than ship a torn payload.
func TestReadGrokSessionFiles_MissingCoreFile(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_missing_core"
	const sessionID = "019e0000-0000-7000-8000-000000000001"
	plantGrokSession(t, agentID, sessionID)
	// Yank events.jsonl out of the subtree.
	if err := os.Remove(filepath.Join(grokSessionDir(agentDir(agentID)), sessionID, "events.jsonl")); err != nil {
		t.Fatalf("remove core file: %v", err)
	}

	_, _, err := ReadGrokSessionFiles(agentID)
	if err == nil {
		t.Fatalf("Read accepted session missing a required core file")
	}
	if !strings.Contains(err.Error(), "missing required core file") {
		t.Errorf("error should mention missing required core file; got %v", err)
	}
}

// TestReadGrokSessionFiles_CoreFileOversizedFails confirms the
// "core file is too big → fail the whole read" policy. Shipping a
// session without events.jsonl / chat_history.jsonl / summary.json /
// system_prompt.txt would let target write a session_id pointer
// that resolves to a half-empty directory and resume into garbage.
func TestReadGrokSessionFiles_CoreFileOversizedFails(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_core_big"
	const sessionID = "019e0000-0000-7000-8000-000000000001"
	plantGrokSession(t, agentID, sessionID)

	// Overwrite events.jsonl with a file > grokSessionFileMaxBytes.
	bigPath := filepath.Join(grokSessionDir(agentDir(agentID)), sessionID, "events.jsonl")
	big := make([]byte, grokSessionFileMaxBytes+1)
	if err := os.WriteFile(bigPath, big, 0o644); err != nil {
		t.Fatalf("write big core file: %v", err)
	}

	_, _, err := ReadGrokSessionFiles(agentID)
	if err == nil {
		t.Fatalf("Read accepted oversized core file")
	}
	if !strings.Contains(err.Error(), "core file") {
		t.Errorf("error should mention core file; got %v", err)
	}
}

// TestStageGrokSession_RejectsMissingCoreInWire confirms the
// wire-side core presence check on Stage. Even if source's Read
// enforces the same invariant, target MUST re-check — otherwise
// a corrupted peer or a future protocol bug could let us write
// a `.grok/session_id` pointing at a torn session.
func TestStageGrokSession_RejectsMissingCoreInWire(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_stage_missing_core"
	const sessionID = "019e0000-0000-7000-8000-000000000001"

	transfer := &GrokSessionTransfer{
		SessionID: sessionID,
		Files: []GrokSessionFile{
			{RelPath: "events.jsonl", Content: []byte("a\n")},
			{RelPath: "chat_history.jsonl", Content: []byte("b\n")},
			// summary.json + system_prompt.txt intentionally absent.
		},
	}
	_, _, err := StageGrokSession(agentID, transfer)
	if err == nil {
		t.Fatalf("Stage accepted wire payload missing core files")
	}
	if !strings.Contains(err.Error(), "missing required core file") {
		t.Errorf("error should mention missing required core file; got %v", err)
	}
}

// TestStageGrokSession_RejectsOversizedFile confirms the per-file
// size cap on the wire-side. A corrupt peer could otherwise land
// a 32+ MiB file under <sessionRoot> that target's grok then
// chokes on.
func TestStageGrokSession_RejectsOversizedFile(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const agentID = "ag_grok_stage_big_file"
	const sessionID = "019e0000-0000-7000-8000-000000000001"

	transfer := &GrokSessionTransfer{
		SessionID: sessionID,
		Files: []GrokSessionFile{
			{RelPath: "events.jsonl", Content: make([]byte, grokSessionFileMaxBytes+1)},
		},
	}
	_, _, err := StageGrokSession(agentID, transfer)
	if err == nil {
		t.Fatalf("Stage accepted oversized file")
	}
	if !strings.Contains(err.Error(), "per-file cap") {
		t.Errorf("error should mention per-file cap; got %v", err)
	}
}

// TestReadStageRoundTrip is the end-to-end check: plant a session,
// read it, stage it under a DIFFERENT agent id (simulating the
// cross-peer transfer), and verify target's resume pointer + every
// file resolves identically.
func TestReadStageRoundTrip(t *testing.T) {
	withGrokHome(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	const sourceAgent = "ag_grok_e2e_src"
	const targetAgent = "ag_grok_e2e_dst"
	const sessionID = "019e588f-419b-7202-9cff-1647e57116d5"

	plantGrokSession(t, sourceAgent, sessionID)

	transfer, _, err := ReadGrokSessionFiles(sourceAgent)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if transfer == nil {
		t.Fatalf("Read produced nil transfer for populated source")
	}

	commit, _, err := StageGrokSession(targetAgent, transfer)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	commit()

	pointer, err := os.ReadFile(filepath.Join(agentDir(targetAgent), ".grok", "session_id"))
	if err != nil {
		t.Fatalf("read target pointer: %v", err)
	}
	if string(pointer) != sessionID {
		t.Errorf("target pointer mismatch: got %q want %q", pointer, sessionID)
	}

	targetRoot := filepath.Join(grokSessionDir(agentDir(targetAgent)), sessionID)
	for _, f := range transfer.Files {
		got, err := os.ReadFile(filepath.Join(targetRoot, filepath.FromSlash(f.RelPath)))
		if err != nil {
			t.Errorf("read target %s: %v", f.RelPath, err)
			continue
		}
		if string(got) != string(f.Content) {
			t.Errorf("target %s content mismatch", f.RelPath)
		}
	}
}
