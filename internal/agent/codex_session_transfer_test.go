package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func setupCodexTransferTest(t *testing.T) (agentID, codexRoot string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	codexRoot = filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", codexRoot)
	agentID = "ag_codex_transfer"
	return agentID, codexRoot
}

func TestReadCodexSessionFiles_ReadsRefAndRollout(t *testing.T) {
	agentID, codexRoot := setupCodexTransferTest(t)
	threadID := "019e7cc9-dd5e-7971-b654-7840c683879e"
	rel := filepath.Join("sessions", "2026", "05", "31",
		"rollout-2026-05-31T00-00-00-"+threadID+".jsonl")
	rolloutPath := filepath.Join(codexRoot, rel)
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o755); err != nil {
		t.Fatalf("mkdir rollout parent: %v", err)
	}
	if err := os.WriteFile(rolloutPath, []byte(`{"type":"session_meta"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	writeCodexThreadRef(agentID, "", codexThreadRef{ThreadID: threadID, RolloutPath: rolloutPath}, logger)

	got, skipped, err := ReadCodexSessionFiles(agentID)
	if err != nil {
		t.Fatalf("ReadCodexSessionFiles: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none", skipped)
	}
	if got == nil || len(got.Threads) != 1 {
		t.Fatalf("threads len = %v, want 1", got)
	}
	th := got.Threads[0]
	if th.ThreadID != threadID || th.RefName != "main.json" {
		t.Fatalf("thread metadata = %#v", th)
	}
	if th.RolloutRelPath != filepath.ToSlash(rel) {
		t.Fatalf("relpath = %q, want %q", th.RolloutRelPath, filepath.ToSlash(rel))
	}
	if string(th.RolloutContent) == "" {
		t.Fatalf("rollout content empty")
	}
}

func TestStageCodexSession_RollbackAndCommit(t *testing.T) {
	agentID, codexRoot := setupCodexTransferTest(t)
	threadID := "019e7cc9-dd5e-7971-b654-7840c683879e"
	rel := filepath.ToSlash(filepath.Join("sessions", "2026", "05", "31",
		"rollout-2026-05-31T00-00-00-"+threadID+".jsonl"))
	transfer := &CodexSessionTransfer{Threads: []CodexThreadTransfer{{
		RefName:        "main.json",
		ThreadID:       threadID,
		RolloutRelPath: rel,
		RolloutContent: []byte(`{"type":"session_meta"}` + "\n"),
	}}}

	commit, rollback, err := StageCodexSession(agentID, transfer)
	if err != nil {
		t.Fatalf("StageCodexSession: %v", err)
	}
	rolloutPath := filepath.Join(codexRoot, filepath.FromSlash(rel))
	refPath := codexThreadRefPath(agentID, "")
	if _, err := os.Stat(rolloutPath); err != nil {
		t.Fatalf("rollout not staged: %v", err)
	}
	if _, err := os.Stat(refPath); err != nil {
		t.Fatalf("ref not staged: %v", err)
	}
	rollback()
	if _, err := os.Stat(rolloutPath); !os.IsNotExist(err) {
		t.Fatalf("rollout survived rollback: %v", err)
	}
	if _, err := os.Stat(refPath); !os.IsNotExist(err) {
		t.Fatalf("ref survived rollback: %v", err)
	}
	_ = commit

	commit, rollback, err = StageCodexSession(agentID, transfer)
	if err != nil {
		t.Fatalf("StageCodexSession second: %v", err)
	}
	commit()
	_ = rollback
	if _, err := os.Stat(rolloutPath); err != nil {
		t.Fatalf("rollout missing after commit: %v", err)
	}
	ref, err := readCodexThreadRef(agentID, "")
	if err != nil {
		t.Fatalf("read ref: %v", err)
	}
	if ref.ThreadID != threadID || ref.RolloutPath != rolloutPath {
		t.Fatalf("ref = %#v, want thread %s path %s", ref, threadID, rolloutPath)
	}

	files, threads, err := clearCodexSessionCounted(agentID)
	if err != nil {
		t.Fatalf("clearCodexSessionCounted: %v", err)
	}
	if files != 1 || threads != 1 {
		t.Fatalf("clear counts = files %d threads %d, want 1/1", files, threads)
	}
	if _, err := os.Stat(rolloutPath); !os.IsNotExist(err) {
		t.Fatalf("rollout survived clear: %v", err)
	}
	if _, err := os.Stat(refPath); !os.IsNotExist(err) {
		t.Fatalf("ref survived clear: %v", err)
	}
}
