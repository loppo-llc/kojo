package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// noopImporter is the smallest possible Importer that lets Run() complete.
// Tests register it via withTestImporter so Run() does not refuse on
// ErrNoImporters. We tag every domain it touches so assertions can check
// migration_status was updated.
type noopImporter struct{ name string }

func (n *noopImporter) Domain() string { return n.name }
func (n *noopImporter) Run(ctx context.Context, st *store.Store, _ Options) error {
	return MarkPhase(ctx, st, n.name, "complete", 0, "")
}

// withTestImporter swaps the package-level importer registry for the
// duration of a test. Restored on cleanup so other tests are unaffected.
var registryMu sync.Mutex

func withTestImporter(t *testing.T, imps ...Importer) {
	t.Helper()
	registryMu.Lock()
	saved := registered
	registered = append([]Importer(nil), imps...)
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registered = saved
		registryMu.Unlock()
	})
}

// frozenNow returns a fixed time so mtime checks are predictable.
func frozenNow() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }

// fakeV0Dir builds a small fixture directory and pins all mtimes to `at`
// so the mtime safety window doesn't trigger.
func fakeV0Dir(t *testing.T, at time.Time) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"agents.json":                          `[]`,
		"sessions.json":                        `[]`,
		"agents/ag_1/persona.md":               "I am Hana.\n",
		"agents/ag_1/MEMORY.md":                "# Memory\n",
		"agents/ag_1/messages.jsonl":           ``,
		"agents/groupdms/groups.json":          `[]`,
		"vapid.json":                           `{}`,
	}
	for rel, body := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(abs, at, at); err != nil {
			t.Fatal(err)
		}
	}
	// pin parent dirs as well so WalkDir doesn't see "now" mtimes.
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		_ = os.Chtimes(path, at, at)
		return nil
	})
	return root
}

func TestManifestDeterministic(t *testing.T) {
	old := frozenNow().Add(-time.Hour)
	dir := fakeV0Dir(t, old)
	a, err := ManifestSHA256(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ManifestSHA256(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("manifest not deterministic: %s != %s", a, b)
	}
}

func TestManifestChangesOnEdit(t *testing.T) {
	old := frozenNow().Add(-time.Hour)
	dir := fakeV0Dir(t, old)
	before, _ := ManifestSHA256(dir)
	// touch a file's content
	target := filepath.Join(dir, "agents/ag_1/persona.md")
	if err := os.WriteFile(target, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatal(err)
	}
	after, _ := ManifestSHA256(dir)
	if before == after {
		t.Error("manifest must change after content edit")
	}
}

func TestManifestIgnoresLockFile(t *testing.T) {
	old := frozenNow().Add(-time.Hour)
	dir := fakeV0Dir(t, old)
	before, _ := ManifestSHA256(dir)
	lock := filepath.Join(dir, "kojo.lock")
	if err := os.WriteFile(lock, []byte("pid:123"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	after, _ := ManifestSHA256(dir)
	if before != after {
		t.Errorf("manifest must ignore kojo.lock: %s != %s", before, after)
	}
}

func TestRunHappyPath(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})

	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := filepath.Join(t.TempDir(), "v1")

	res, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v1,
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	// migration_complete.json must exist
	if _, err := os.Stat(filepath.Join(v1, CompleteFileName)); err != nil {
		t.Errorf("complete file missing: %v", err)
	}
	// lock file must NOT exist
	if _, err := os.Stat(filepath.Join(v1, LockFileName)); err == nil {
		t.Error("lock file should be removed on success")
	}
}

func TestRunRefusesEmptyImporters(t *testing.T) {
	// Default registry is empty in tests unless withTestImporter runs.
	withTestImporter(t)
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := filepath.Join(t.TempDir(), "v1")
	_, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v1,
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	})
	if !errors.Is(err, ErrNoImporters) {
		t.Errorf("want ErrNoImporters, got %v", err)
	}
	// complete file must NOT be present (silent loss prevention)
	if _, err := os.Stat(filepath.Join(v1, CompleteFileName)); err == nil {
		t.Error("complete file must not exist when run aborted on empty importer list")
	}
}

func TestRunRefusesV0EqualsV1(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	_, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v0, // same path
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	})
	if !errors.Is(err, ErrV0EqualsV1) {
		t.Errorf("want ErrV0EqualsV1, got %v", err)
	}
}

func TestRunResumeMismatch(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := filepath.Join(t.TempDir(), "v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate a stale in-progress lock with a fake manifest.
	lock := LockFile{
		V0Path:           v0,
		V0SHA256Manifest: "0000000000000000000000000000000000000000000000000000000000000000",
		StartedAt:        old.UnixMilli(),
		MigratorVersion:  "old",
	}
	if err := writeLock(filepath.Join(v1, LockFileName), lock); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v1,
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	})
	if !errors.Is(err, ErrResumeMismatch) {
		t.Errorf("want ErrResumeMismatch, got %v", err)
	}
}

func TestRunRestartWipesIncomplete(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := filepath.Join(t.TempDir(), "v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	// Drop a v1 sentinel + a stale lock.
	if err := os.WriteFile(filepath.Join(v1, store.DBFileName), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLock(filepath.Join(v1, LockFileName), LockFile{V0Path: v0, V0SHA256Manifest: "x"}); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v1,
		Restart:         true,
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	})
	if err != nil {
		t.Fatalf("Run with Restart: %v", err)
	}
}

// TestRunResumeAcceptsCredentialFiles verifies the post-bug-fix
// behaviour: credentials.{db,db-wal,db-shm,key} created by an
// earlier normal v1 startup do NOT block --migrate. canResumeV1
// admits them as known siblings (alongside the kojo.db migration
// sentinel) and migration runs to completion. All four file
// variants survive untouched.
func TestRunResumeAcceptsCredentialFiles(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := filepath.Join(t.TempDir(), "v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate prior normal v1 startup that opened agent.Manager:
	// kojo.db (migration sentinel) + every credential file variant.
	if err := os.WriteFile(filepath.Join(v1, store.DBFileName), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	credNames := []string{"credentials.db", "credentials.db-wal", "credentials.db-shm", "credentials.key"}
	for _, name := range credNames {
		if err := os.WriteFile(filepath.Join(v1, name), []byte("preserved-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v1,
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	}); err != nil {
		t.Fatalf("Run should accept credentials files as resumable, got %v", err)
	}
	// Every credential file variant must survive the migration.
	for _, name := range credNames {
		body, err := os.ReadFile(filepath.Join(v1, name))
		if err != nil {
			t.Errorf("%s missing after Run: %v", name, err)
			continue
		}
		if string(body) != "preserved-"+name {
			t.Errorf("%s clobbered: %q", name, body)
		}
	}
}

// TestRunRefusesCredentialOnlyWithoutSentinel: a v1 dir that holds
// ONLY credentials.{db,key} (no kojo.db, no migration_in_progress
// .lock) is suspicious — it could be an operator-rebuilt secrets
// snapshot, NOT a partial migration. Refuse with ErrPartialV1
// instead of silently importing on top.
func TestRunRefusesCredentialOnlyWithoutSentinel(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := filepath.Join(t.TempDir(), "v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"credentials.db", "credentials.key"} {
		if err := os.WriteFile(filepath.Join(v1, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v1,
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	})
	if !errors.Is(err, ErrPartialV1) {
		t.Errorf("want ErrPartialV1 for credential-only v1 dir, got %v", err)
	}
}

// TestRunRestartOnCredentialOnlyDir is the end-to-end pin for the
// "first-time --migrate-restart" path: the v1 dir holds only
// credentials files (operator started v1 once, typed an API key,
// then decided to redo migration). wasFreshV1Dir must classify this
// as fresh, wipeIncompleteV1 must no-op, and Run must proceed to
// completion without "no v1 sentinel" refusal.
func TestRunRestartOnCredentialOnlyDir(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := filepath.Join(t.TempDir(), "v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"credentials.db", "credentials.key"} {
		if err := os.WriteFile(filepath.Join(v1, name), []byte("preserved-"+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v1,
		Restart:         true,
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	}); err != nil {
		t.Fatalf("Run with Restart on credentials-only dir: %v", err)
	}
	for _, name := range []string{"credentials.db", "credentials.key"} {
		body, err := os.ReadFile(filepath.Join(v1, name))
		if err != nil {
			t.Errorf("%s missing after restart: %v", name, err)
			continue
		}
		if string(body) != "preserved-"+name {
			t.Errorf("%s clobbered during restart: %q", name, body)
		}
	}
}

// TestRunRestartRefusesCredentialsAsDirectory: a directory named
// `credentials.db` is NOT a credentials file, no matter what the
// name suggests. wasFreshV1Dir must reject the v1 dir (because the
// entry isn't a regular file), wipeIncompleteV1's sentinel guard
// must fire, and Run must refuse with the "no v1 sentinel" error
// rather than no-op'ing over operator-confused state.
func TestRunRestartRefusesCredentialsAsDirectory(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := filepath.Join(t.TempDir(), "v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	// credentials.db as a *directory* — same name as the regular
	// file the agent credential store would write, but a directory.
	if err := os.MkdirAll(filepath.Join(v1, "credentials.db"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v1,
		Restart:         true,
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	})
	if err == nil || !contains(err.Error(), "no v1 sentinel") {
		t.Errorf("Run with Restart should refuse credentials.db-as-directory, got %v", err)
	}
}

// TestRunRestartRefusesCredentialsAsSymlink: same defense-in-depth
// as TestRunRestartRefusesCredentialsAsDirectory, but the offending
// entry is a symlink rather than a directory. Filesystems that
// return DT_UNKNOWN for readdir would otherwise let
// `DirEntry.Type().IsRegular()` mis-classify the symlink as a
// regular file; wasFreshV1Dir uses e.Info().Mode() to guard against
// that, so this test pins the behavior.
func TestRunRestartRefusesCredentialsAsSymlink(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := filepath.Join(t.TempDir(), "v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	// Real target file outside the v1 dir, then a symlink at the
	// "credentials.db" name inside v1.
	target := filepath.Join(t.TempDir(), "real-credentials.db")
	if err := os.WriteFile(target, []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(v1, "credentials.db")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v1,
		Restart:         true,
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	})
	if err == nil || !contains(err.Error(), "no v1 sentinel") {
		t.Errorf("Run with Restart should refuse credentials.db-as-symlink, got %v", err)
	}
}

// TestRunRestartOnMissingDir is the simplest case: v1 dir does not
// exist at all when the operator types --migrate-restart. Run must
// create it, treat the dir as fresh, and complete cleanly.
func TestRunRestartOnMissingDir(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := filepath.Join(t.TempDir(), "never-existed-v1")
	if _, err := os.Stat(v1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("test precondition: v1 should not exist, got %v", err)
	}
	if _, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v1,
		Restart:         true,
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	}); err != nil {
		t.Fatalf("Run with Restart on missing dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(v1, CompleteFileName)); err != nil {
		t.Errorf("migration_complete.json missing after restart: %v", err)
	}
}

// TestRunRestartPreservesCredentialFiles: --migrate-restart must
// preserve credentials.{db,db-wal,db-shm,key} (encrypted user
// secrets and SQLite WAL/SHM siblings) while wiping the migration's
// own sentinels and any partial blob trees.
func TestRunRestartPreservesCredentialFiles(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := filepath.Join(t.TempDir(), "v1")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	// Migration sentinel so wipeIncompleteV1 doesn't refuse.
	if err := os.WriteFile(filepath.Join(v1, store.DBFileName), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	credNames := []string{"credentials.db", "credentials.db-wal", "credentials.db-shm", "credentials.key"}
	for _, name := range credNames {
		if err := os.WriteFile(filepath.Join(v1, name), []byte("preserved-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v1,
		Restart:         true,
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	}); err != nil {
		t.Fatalf("Run with Restart: %v", err)
	}
	for _, name := range credNames {
		body, err := os.ReadFile(filepath.Join(v1, name))
		if err != nil {
			t.Errorf("%s wiped despite preserve list: %v", name, err)
			continue
		}
		if string(body) != "preserved-"+name {
			t.Errorf("%s clobbered: %q", name, body)
		}
	}
}

// TestWipeIncompleteOnFreshDir: --migrate-restart against a v1 dir
// that only holds the runtime kojo.lock (migrate.Run's own
// configdir.Acquire creates this BEFORE the wipe runs) must be a
// no-op. Before this guard, the sentinel check refused on the false
// premise that "kojo.lock alone == v0 layout" and the operator could
// not invoke --migrate-restart cleanly on a clean slate.
func TestWipeIncompleteOnFreshDir(t *testing.T) {
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := t.TempDir()
	// Mimic what configdir.Acquire leaves behind on entry: a single
	// kojo.lock file owned by us, nothing else.
	if err := os.WriteFile(filepath.Join(v1, "kojo.lock"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wipeIncompleteV1(v1, v0, true); err != nil {
		t.Fatalf("wipe on lock-only v1 dir should be no-op, got %v", err)
	}
	// The lock file is preserved end-to-end.
	if _, err := os.Stat(filepath.Join(v1, "kojo.lock")); err != nil {
		t.Errorf("kojo.lock disappeared during no-op wipe: %v", err)
	}
}

// TestWipeIncompleteRefusesPreexistingLock: same shape as
// TestWipeIncompleteOnFreshDir but allowFreshRestart=false. Run
// observed something in the dir BEFORE acquiring its own lock,
// so we can no longer trust the kojo.lock is ours — refuse.
func TestWipeIncompleteRefusesPreexistingLock(t *testing.T) {
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := t.TempDir()
	if err := os.WriteFile(filepath.Join(v1, "kojo.lock"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	err := wipeIncompleteV1(v1, v0, false)
	if err == nil || !contains(err.Error(), "no v1 sentinel") {
		t.Errorf("wipe must refuse a lock-only dir we didn't just create, got %v", err)
	}
}

// TestWipeIncompleteOnLockAndCredentialsOnly: same shape as the
// fresh-dir case above, but with credentials files already present
// (operator typed an API key into the running v1 binary, then crashed
// before the first --migrate). credentials files are preserve-only,
// so they participate in the "effectively empty" judgement.
func TestWipeIncompleteOnLockAndCredentialsOnly(t *testing.T) {
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := t.TempDir()
	if err := os.WriteFile(filepath.Join(v1, "kojo.lock"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1, "credentials.db"), []byte("c1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1, "credentials.key"), []byte("c2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := wipeIncompleteV1(v1, v0, true); err != nil {
		t.Fatalf("wipe on lock+credentials-only v1 dir should be no-op, got %v", err)
	}
	for _, name := range []string{"kojo.lock", "credentials.db", "credentials.key"} {
		if _, err := os.Stat(filepath.Join(v1, name)); err != nil {
			t.Errorf("%s disappeared during no-op wipe: %v", name, err)
		}
	}
}

func TestWipeIncompleteRefusesV0Sentinels(t *testing.T) {
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	// Pretend v1 dir is actually the v0 layout (kojo.lock + agents/).
	if err := wipeIncompleteV1(v0, v0, false); !errors.Is(err, ErrV0EqualsV1) {
		t.Errorf("want ErrV0EqualsV1 when v1==v0, got %v", err)
	}

	// A separate dir that only has v0-style sentinels (no v1 ones) must be refused.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "kojo.lock"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(other, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := wipeIncompleteV1(other, v0, false)
	if err == nil || !contains(err.Error(), "no v1 sentinel") {
		t.Errorf("wipe should refuse without v1 sentinel, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRunRefusesAlreadyComplete(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	v1 := t.TempDir()
	if err := os.WriteFile(filepath.Join(v1, CompleteFileName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{V0Dir: v0, V1Dir: v1, Now: frozenNow})
	if !errors.Is(err, ErrAlreadyComplete) {
		t.Errorf("want ErrAlreadyComplete, got %v", err)
	}
}

func TestRunRejectsRecentMtime(t *testing.T) {
	v0 := fakeV0Dir(t, frozenNow()) // brand new mtime
	v1 := filepath.Join(t.TempDir(), "v1")
	_, err := Run(context.Background(), Options{V0Dir: v0, V1Dir: v1, Now: frozenNow})
	if !errors.Is(err, ErrV0RecentlyChanged) {
		t.Errorf("want ErrV0RecentlyChanged, got %v", err)
	}
}

func TestRunDoesNotWriteV0(t *testing.T) {
	withTestImporter(t, &noopImporter{name: "agents"})
	old := frozenNow().Add(-time.Hour)
	v0 := fakeV0Dir(t, old)
	preManifest, _ := ManifestSHA256(v0)
	v1 := filepath.Join(t.TempDir(), "v1")

	if _, err := Run(context.Background(), Options{
		V0Dir:           v0,
		V1Dir:           v1,
		MigratorVersion: "v1.0.0-test",
		Now:             frozenNow,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	postManifest, _ := ManifestSHA256(v0)
	if preManifest != postManifest {
		t.Errorf("v0 dir was written during migration: pre=%s post=%s", preManifest, postManifest)
	}
}
