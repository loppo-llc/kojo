package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/store"
)

// memorySyncTestEnv stands up an isolated $HOME-rooted configdir, opens a
// fresh v1 store, registers it as the global handle, and seeds an agent
// row so syncAgentMemoryToDB / syncMemoryEntriesToDB find a parent to
// attach to. Returns the store + agent ID for the test.
//
// Tests within this package run sequentially by default; configdir.Set
// is one-shot (sync.Once), so we set HOME instead and let configdir.Path
// resolve through that. This mirrors what other agent_test fixtures do.
func memorySyncTestEnv(t *testing.T, agentID string) *store.Store {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))

	st, err := store.Open(context.Background(), store.Options{ConfigDir: configdir.Path()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		setGlobalStore(nil)
	})
	setGlobalStore(st)

	if _, err := st.InsertAgent(context.Background(), &store.AgentRecord{
		ID:       agentID,
		Name:     "alice",
		Settings: map[string]any{"tool": "claude", "model": "sonnet"},
	}, store.AgentInsertOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Pre-create the agent dir so the file-system writes below have a
	// home. ensureAgentDir would do this for a real agent path; the
	// tests want fine-grained control over what files exist.
	if err := os.MkdirAll(filepath.Join(agentDir(agentID), "memory"), 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	return st
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestSyncAgentMemoryToDB_InsertAndUpdate covers the happy path:
// fresh file → DB row created; edited file → DB row updated; unchanged
// file → no-op (sha256 short-circuit).
func TestSyncAgentMemoryToDB_InsertAndUpdate(t *testing.T) {
	st := memorySyncTestEnv(t, "ag_mem")
	ctx := context.Background()
	logger := quietLogger()

	memPath := filepath.Join(agentDir("ag_mem"), "MEMORY.md")
	if err := os.WriteFile(memPath, []byte("# v1\n"), 0o644); err != nil {
		t.Fatalf("write MEMORY.md v1: %v", err)
	}

	if err := syncAgentMemoryToDB(ctx, st, "ag_mem", logger); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	rec, err := st.GetAgentMemory(ctx, "ag_mem")
	if err != nil {
		t.Fatalf("GetAgentMemory after insert: %v", err)
	}
	if rec.Body != "# v1\n" {
		t.Errorf("body after insert = %q, want %q", rec.Body, "# v1\n")
	}
	firstETag := rec.ETag

	// Re-sync without changing the file: no-op (etag stable).
	if err := syncAgentMemoryToDB(ctx, st, "ag_mem", logger); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	rec2, err := st.GetAgentMemory(ctx, "ag_mem")
	if err != nil {
		t.Fatalf("GetAgentMemory after no-op: %v", err)
	}
	if rec2.ETag != firstETag {
		t.Errorf("etag drifted on no-op sync: %q -> %q", firstETag, rec2.ETag)
	}

	// Edit the file: sync should update the DB row.
	if err := os.WriteFile(memPath, []byte("# v2\n"), 0o644); err != nil {
		t.Fatalf("write MEMORY.md v2: %v", err)
	}
	if err := syncAgentMemoryToDB(ctx, st, "ag_mem", logger); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	rec3, err := st.GetAgentMemory(ctx, "ag_mem")
	if err != nil {
		t.Fatalf("GetAgentMemory after update: %v", err)
	}
	if rec3.Body != "# v2\n" {
		t.Errorf("body after update = %q, want %q", rec3.Body, "# v2\n")
	}
	if rec3.ETag == firstETag {
		t.Errorf("etag did not change on body update")
	}
}

// TestSyncAgentMemoryToDB_MissingFileIsNoop ensures a brand-new agent
// without a MEMORY.md file doesn't error out — the next sync (after
// ensureAgentDir runs) will populate it.
func TestSyncAgentMemoryToDB_MissingFileIsNoop(t *testing.T) {
	st := memorySyncTestEnv(t, "ag_nomem")
	ctx := context.Background()

	if err := syncAgentMemoryToDB(ctx, st, "ag_nomem", quietLogger()); err != nil {
		t.Fatalf("sync of missing file: %v", err)
	}
	if _, err := st.GetAgentMemory(ctx, "ag_nomem"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DB should still be empty: %v", err)
	}
}

// TestSyncAgentMemoryToDB_FileMissingHydratesFromDB covers the
// missing-disk + live-DB branch. Post-cutover the DB is canonical for
// MEMORY.md and disk is a hydrated mirror, so a CLI delete (or a fresh
// boot after v0→v1 migration where the importer populated DB but never
// wrote disk) MUST re-hydrate the file from the DB row, not tombstone
// the row. Cross-device readers continue to see the live body.
//
// Tombstoning of MEMORY.md now happens only via explicit Web UI / API
// DELETE — handleDeleteAgentMemory exercises that path separately.
func TestSyncAgentMemoryToDB_FileMissingHydratesFromDB(t *testing.T) {
	st := memorySyncTestEnv(t, "ag_memdel")
	ctx := context.Background()
	logger := quietLogger()

	memPath := filepath.Join(agentDir("ag_memdel"), "MEMORY.md")
	if err := os.WriteFile(memPath, []byte("# live\n"), 0o644); err != nil {
		t.Fatalf("write MEMORY.md: %v", err)
	}
	if err := syncAgentMemoryToDB(ctx, st, "ag_memdel", logger); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if _, err := st.GetAgentMemory(ctx, "ag_memdel"); err != nil {
		t.Fatalf("row should exist after first sync: %v", err)
	}

	// Delete the file and re-sync — DB row stays live, disk re-
	// hydrates from DB body.
	if err := os.Remove(memPath); err != nil {
		t.Fatalf("rm MEMORY.md: %v", err)
	}
	if err := syncAgentMemoryToDB(ctx, st, "ag_memdel", logger); err != nil {
		t.Fatalf("post-delete sync: %v", err)
	}
	rec, err := st.GetAgentMemory(ctx, "ag_memdel")
	if err != nil {
		t.Fatalf("DB row should remain live after disk delete: %v", err)
	}
	if rec.Body != "# live\n" {
		t.Errorf("DB body changed unexpectedly: got %q", rec.Body)
	}
	if rec.DeletedAt != nil {
		t.Errorf("DB row should not be tombstoned, deleted_at=%v", *rec.DeletedAt)
	}
	hydrated, rerr := os.ReadFile(memPath)
	if rerr != nil {
		t.Fatalf("expected disk re-hydrated, got read err: %v", rerr)
	}
	if string(hydrated) != "# live\n" {
		t.Errorf("hydrated disk body = %q, want %q", string(hydrated), "# live\n")
	}

	// Re-syncing on the now-hydrated state is a no-op (sha matches).
	if err := syncAgentMemoryToDB(ctx, st, "ag_memdel", logger); err != nil {
		t.Errorf("idempotent re-sync: %v", err)
	}
}

// TestSyncAgentMemoryToDB_MissingDiskMissingDBNoop guards against
// over-eager file creation on the no-row + no-file path: a fresh
// agent with neither row nor file must not get an empty MEMORY.md
// minted by the sync.
func TestSyncAgentMemoryToDB_MissingDiskMissingDBNoop(t *testing.T) {
	st := memorySyncTestEnv(t, "ag_memnoop")
	ctx := context.Background()
	logger := quietLogger()

	if err := syncAgentMemoryToDB(ctx, st, "ag_memnoop", logger); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := st.GetAgentMemory(ctx, "ag_memnoop"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir("ag_memnoop"), "MEMORY.md")); !os.IsNotExist(err) {
		t.Errorf("expected no MEMORY.md created, got: %v", err)
	}
}

// TestSyncMemoryEntriesToDB_InsertUpdateAndTombstone exercises the
// memory/ tree walk:
//   - top-level YYYY-MM-DD.md → kind=daily
//   - top-level random.md     → kind=topic
//   - projects/foo.md         → kind=project
//   - people/bob.md           → kind=people
//   - unknown/dir/x.md        → kind=topic, name=unknown/dir/x
// And verifies the tombstone phase: removing a file from disk soft-
// deletes the corresponding DB row.
func TestSyncMemoryEntriesToDB_InsertUpdateAndTombstone(t *testing.T) {
	st := memorySyncTestEnv(t, "ag_entries")
	ctx := context.Background()
	logger := quietLogger()
	root := filepath.Join(agentDir("ag_entries"), "memory")

	mustWrite := func(rel, body string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	mustWrite("2026-05-03.md", "today")
	mustWrite("random.md", "topic top")
	mustWrite("projects/foo.md", "project foo")
	mustWrite("people/bob.md", "bob notes")
	mustWrite("oddball/nested/x.md", "weird")

	if err := syncMemoryEntriesToDB(ctx, st, "ag_entries", logger); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// Verify each expected row exists with the right kind/name.
	type want struct{ kind, name, body string }
	wants := []want{
		{"daily", "2026-05-03", "today"},
		{"topic", "random", "topic top"},
		{"project", "foo", "project foo"},
		{"people", "bob", "bob notes"},
		{"topic", "oddball/nested/x", "weird"},
	}
	for _, w := range wants {
		rec, err := st.FindMemoryEntryByName(ctx, "ag_entries", w.kind, w.name)
		if err != nil {
			t.Errorf("missing entry kind=%s name=%s: %v", w.kind, w.name, err)
			continue
		}
		if rec.Body != w.body {
			t.Errorf("body for %s/%s = %q, want %q", w.kind, w.name, rec.Body, w.body)
		}
	}

	// Edit one file and re-sync — body updates without duplicating.
	mustWrite("projects/foo.md", "project foo v2")
	if err := syncMemoryEntriesToDB(ctx, st, "ag_entries", logger); err != nil {
		t.Fatalf("update sync: %v", err)
	}
	rec, _ := st.FindMemoryEntryByName(ctx, "ag_entries", "project", "foo")
	if rec.Body != "project foo v2" {
		t.Errorf("body after edit = %q, want %q", rec.Body, "project foo v2")
	}

	// Delete one file and re-sync — DB row is tombstoned.
	if err := os.Remove(filepath.Join(root, "people", "bob.md")); err != nil {
		t.Fatalf("remove bob.md: %v", err)
	}
	if err := syncMemoryEntriesToDB(ctx, st, "ag_entries", logger); err != nil {
		t.Fatalf("tombstone sync: %v", err)
	}
	if _, err := st.FindMemoryEntryByName(ctx, "ag_entries", "people", "bob"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected bob tombstoned, got: %v", err)
	}
	// Sibling rows untouched.
	if _, err := st.FindMemoryEntryByName(ctx, "ag_entries", "project", "foo"); err != nil {
		t.Errorf("project/foo should still be live: %v", err)
	}
}

// TestSyncMemoryEntriesToDB_MissingDirIsNoop matches the MEMORY.md
// missing-file behavior — a brand-new agent with no memory/ subtree
// must not error.
func TestSyncMemoryEntriesToDB_MissingDirIsNoop(t *testing.T) {
	st := memorySyncTestEnv(t, "ag_noentries")
	// Remove the memory/ dir created by the env helper so the scan
	// hits the missing-dir branch.
	if err := os.RemoveAll(filepath.Join(agentDir("ag_noentries"), "memory")); err != nil {
		t.Fatalf("rm memory/: %v", err)
	}
	if err := syncMemoryEntriesToDB(context.Background(), st, "ag_noentries", quietLogger()); err != nil {
		t.Fatalf("sync missing dir: %v", err)
	}
}

// TestSyncMemoryEntriesToDB_HydratesWhenDiskUninitialized covers the
// post-v0→v1-migration first-boot scenario: DB has live entries, disk
// memory/ tree is missing entirely. The sync MUST hydrate disk from
// the DB rows rather than tombstoning. Cross-device readers continue
// to see live entries.
func TestSyncMemoryEntriesToDB_HydratesWhenDiskUninitialized(t *testing.T) {
	st := memorySyncTestEnv(t, "ag_hydrate")
	ctx := context.Background()
	logger := quietLogger()
	root := filepath.Join(agentDir("ag_hydrate"), "memory")

	// Seed the DB to mimic a freshly-migrated state: rows exist via
	// the importer's path, but the v1 disk has nothing.
	mustSeed := func(kind, name, body string) {
		t.Helper()
		// Write the file, run the sync to land it as a row, then
		// remove the file. We end up with a live DB row and no disk.
		full := filepath.Join(root, name+".md")
		// Use canonical importer→syncer paths so the sync re-finds
		// the same (kind, name) on hydrate.
		switch kind {
		case "daily":
			full = filepath.Join(root, name+".md")
		case "project":
			full = filepath.Join(root, "projects", name+".md")
		case "people":
			full = filepath.Join(root, "people", name+".md")
		case "topic":
			full = filepath.Join(root, "topics", name+".md")
		case "archive":
			full = filepath.Join(root, "archive", name+".md")
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	mustSeed("daily", "2026-05-03", "today's note")
	mustSeed("project", "alpha", "alpha plan")
	mustSeed("people", "bob", "about bob")
	mustSeed("topic", "research", "research notes")
	if err := syncMemoryEntriesToDB(ctx, st, "ag_hydrate", logger); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	// Remove the entire memory/ tree to mimic the importer-only case.
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("rm memory/: %v", err)
	}

	// Hydrate sync.
	if err := syncMemoryEntriesToDB(ctx, st, "ag_hydrate", logger); err != nil {
		t.Fatalf("hydrate sync: %v", err)
	}

	// All four DB rows must remain LIVE.
	cases := []struct{ kind, name, body, relPath string }{
		{"daily", "2026-05-03", "today's note", "2026-05-03.md"},
		{"project", "alpha", "alpha plan", "projects/alpha.md"},
		{"people", "bob", "about bob", "people/bob.md"},
		{"topic", "research", "research notes", "topics/research.md"},
	}
	for _, c := range cases {
		rec, err := st.FindMemoryEntryByName(ctx, "ag_hydrate", c.kind, c.name)
		if err != nil {
			t.Errorf("DB row should still be live for %s/%s: %v", c.kind, c.name, err)
			continue
		}
		if rec.DeletedAt != nil {
			t.Errorf("%s/%s tombstoned unexpectedly", c.kind, c.name)
		}
		if rec.Body != c.body {
			t.Errorf("%s/%s body = %q, want %q", c.kind, c.name, rec.Body, c.body)
		}
		// And the file must have been hydrated to disk at the
		// canonical path so the next sync sees a populated tree.
		hydrated, rerr := os.ReadFile(filepath.Join(root, c.relPath))
		if rerr != nil {
			t.Errorf("expected hydrate file at %s: %v", c.relPath, rerr)
			continue
		}
		if string(hydrated) != c.body {
			t.Errorf("hydrated %s body = %q, want %q", c.relPath, string(hydrated), c.body)
		}
	}

	// A second sync now sees a populated tree (sha matches) — no-op.
	if err := syncMemoryEntriesToDB(ctx, st, "ag_hydrate", logger); err != nil {
		t.Errorf("idempotent re-sync: %v", err)
	}
	// Rows still live.
	for _, c := range cases {
		if _, err := st.FindMemoryEntryByName(ctx, "ag_hydrate", c.kind, c.name); err != nil {
			t.Errorf("%s/%s lost after idempotent sync: %v", c.kind, c.name, err)
		}
	}
}

// TestNewMemoryEntryID_Format pins the id shape so a future change to
// the helper doesn't silently break the FK debugging story.
func TestNewMemoryEntryID_Format(t *testing.T) {
	id := newMemoryEntryID()
	if !strings.HasPrefix(id, "me_") {
		t.Errorf("id missing me_ prefix: %q", id)
	}
	if len(id) != len("me_")+32 { // 16 random bytes → 32 hex chars
		t.Errorf("id length = %d, want %d", len(id), len("me_")+32)
	}
}
