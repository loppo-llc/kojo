package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func newSealTestManager(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	mgr, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr
}

func TestArchiveSealsAndUnarchiveRestores(t *testing.T) {
	mgr := newSealTestManager(t)
	a, err := mgr.Create(AgentConfig{Name: "sealme"})
	if err != nil {
		t.Fatal(err)
	}
	dir := agentDir(a.ID)
	files := map[string]string{
		"work/notes/a.txt": "secret A",
		"work/b.bin":       strings.Repeat("\x00\x01", 50000),
		"memory/x.md":      "# x",
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("notes/a.txt", filepath.Join(dir, "work", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	memBefore, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))

	if err := mgr.Archive(a.ID, ""); !errors.Is(err, ErrArchivePasscodeRequired) {
		t.Fatalf("Archive without passcode err = %v", err)
	}
	if got, _ := mgr.Get(a.ID); got.Archived {
		t.Fatal("rejected archive must not flip the flag")
	}
	if err := mgr.Archive(a.ID, "abc"); !errors.Is(err, ErrArchivePasscodeRequired) {
		t.Fatalf("short passcode err = %v", err)
	}
	if err := mgr.Archive(a.ID, "correct horse"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("plaintext dir still present: %v", err)
	}
	if !AgentSealed(a.ID) {
		t.Fatal("archive file missing")
	}
	raw, _ := os.ReadFile(sealedArchivePath(a.ID))
	if strings.Contains(string(raw), "secret A") {
		t.Fatal("archive is not encrypted")
	}

	// Load-time / watcher syncs must neither tombstone DB rows nor
	// re-materialize plaintext while sealed.
	st := getGlobalStore()
	if err := SyncAgentMemoryFromDisk(context.Background(), st, a.ID, mgr.logger); err != nil {
		t.Fatal(err)
	}
	if err := SyncAgentPersonaFromDisk(context.Background(), st, a.ID, mgr.logger); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileAgentDiskFromDB(context.Background(), st, a.ID, mgr.logger); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("sync re-created plaintext dir: %v", err)
	}
	if _, err := mgr.Fork(a.ID, ForkOptions{Name: "f"}); !errors.Is(err, ErrAgentSealed) {
		t.Fatalf("fork sealed err = %v", err)
	}

	if err := mgr.Unarchive(a.ID, ""); !errors.Is(err, ErrArchivePasscodeRequired) {
		t.Fatalf("unarchive without passcode err = %v", err)
	}
	if err := mgr.Unarchive(a.ID, "wrong horse"); !errors.Is(err, ErrArchivePasscodeInvalid) {
		t.Fatalf("wrong passcode err = %v", err)
	}
	if got, _ := mgr.Get(a.ID); !got.Archived || !AgentSealed(a.ID) {
		t.Fatal("failed unarchive must leave agent archived and sealed")
	}
	// A stray file created while sealed survives; archive entries win.
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("stray"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "work", "junk"), []byte("junk"), 0o644)

	if err := mgr.Unarchive(a.ID, "correct horse"); err != nil {
		t.Fatal(err)
	}
	if got, _ := mgr.Get(a.ID); got.Archived {
		t.Fatal("still archived")
	}
	if AgentSealed(a.ID) {
		t.Fatal("archive not removed")
	}
	for rel, body := range files {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil || string(b) != body {
			t.Fatalf("%s: err=%v match=%v", rel, err, string(b) == body)
		}
		fi, _ := os.Stat(filepath.Join(dir, rel))
		if fi.Mode().Perm() != 0o640 {
			t.Fatalf("%s perm = %v", rel, fi.Mode().Perm())
		}
	}
	if l, err := os.Readlink(filepath.Join(dir, "work", "link")); err != nil || l != "notes/a.txt" {
		t.Fatalf("symlink: %q %v", l, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "emptydir")); err != nil || !fi.IsDir() {
		t.Fatalf("emptydir: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md")); string(b) != string(memBefore) {
		t.Fatal("MEMORY.md changed")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "stray.txt")); string(b) != "stray" {
		t.Fatal("stray file lost")
	}
	if _, err := os.Stat(filepath.Join(dir, "work", "junk")); !os.IsNotExist(err) {
		t.Fatal("archived work/ should replace the stray work/")
	}
}

func TestArchiveSealsAlreadyArchivedAgent(t *testing.T) {
	mgr := newSealTestManager(t)
	a, err := mgr.Create(AgentConfig{Name: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	// Legacy plain archive (pre-sealing): flag only.
	mgr.mu.Lock()
	mgr.agents[a.ID].Archived = true
	mgr.mu.Unlock()
	if err := mgr.Archive(a.ID, ""); err != nil {
		t.Fatalf("idempotent archive: %v", err)
	}
	if AgentSealed(a.ID) {
		t.Fatal("no-passcode re-archive must not seal")
	}
	if err := mgr.Archive(a.ID, "pass1234"); err != nil {
		t.Fatal(err)
	}
	if !AgentSealed(a.ID) {
		t.Fatal("not sealed")
	}
	if err := mgr.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if AgentSealed(a.ID) {
		t.Fatal("delete left the sealed archive behind")
	}
}

func TestUnsealRejectsTraversal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	id := "ag_evil"
	if err := os.MkdirAll(sealedArchivesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, hdrs := range [][]*tar.Header{
		{{Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644}},
		{{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}, {Name: "l/escape.txt", Typeflag: tar.TypeReg, Mode: 0o644}},
	} {
		f, err := os.Create(sealedArchivePath(id))
		if err != nil {
			t.Fatal(err)
		}
		rcp, _ := age.NewScryptRecipient("pass1234")
		rcp.SetWorkFactor(10)
		enc, _ := age.Encrypt(f, rcp)
		gz := gzip.NewWriter(enc)
		tw := tar.NewWriter(gz)
		for _, h := range hdrs {
			_ = tw.WriteHeader(h)
		}
		tw.Close()
		gz.Close()
		enc.Close()
		f.Close()
		if err := unsealAgentDir(id, "pass1234"); err == nil {
			t.Fatalf("unsafe archive %q extracted", hdrs[len(hdrs)-1].Name)
		}
		if _, err := os.Lstat(agentDir(id)); !os.IsNotExist(err) {
			t.Fatal("agent dir installed from unsafe archive")
		}
		if !AgentSealed(id) {
			t.Fatal("archive must survive a failed unseal")
		}
	}
	if _, err := os.Stat("/tmp/escape.txt"); err == nil {
		t.Fatal("escaped to /tmp")
	}
}

func TestArchiveSealRemovesBashCaptures(t *testing.T) {
	mgr := newSealTestManager(t)
	capBase := t.TempDir()
	origBase := bashCaptureBaseDir
	bashCaptureBaseDir = func() string { return capBase }
	t.Cleanup(func() { bashCaptureBaseDir = origBase })

	a, err := mgr.Create(AgentConfig{Name: "captures"})
	if err != nil {
		t.Fatal(err)
	}
	capDir := bashCaptureDir(a.ID)
	if err := os.MkdirAll(capDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capDir, "out-x.log"), []byte("secret output"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Archive(a.ID, "correct horse"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(capDir); !os.IsNotExist(err) {
		t.Fatalf("bash capture dir survived the seal: %v", err)
	}
}
