package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func sha256Hex(t *testing.T, b []byte) string {
	t.Helper()
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// putString is a thin Put wrapper that turns a string into a stream so
// the table-driven tests below stay readable.
func putString(t *testing.T, s *Store, scope Scope, p, body string, opts PutOptions) *Object {
	t.Helper()
	o, err := s.Put(scope, p, strings.NewReader(body), opts)
	if err != nil {
		t.Fatalf("Put(%q): %v", p, err)
	}
	return o
}

func TestPutHeadGetRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	body := "alice avatar bytes"
	want := sha256Hex(t, []byte(body))

	got := putString(t, s, ScopeGlobal, "agents/ag_1/avatar.png", body, PutOptions{})
	if got.SHA256 != want {
		t.Errorf("Put.SHA256 = %s want %s", got.SHA256, want)
	}
	if got.ETag != "sha256:"+want {
		t.Errorf("Put.ETag = %s want sha256:%s", got.ETag, want)
	}
	if got.Size != int64(len(body)) {
		t.Errorf("Put.Size = %d want %d", got.Size, len(body))
	}

	// Head returns size+modtime; SHA256 left empty until slice 2 wires
	// the cache.
	h, err := s.Head(ScopeGlobal, "agents/ag_1/avatar.png")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if h.Size != int64(len(body)) {
		t.Errorf("Head.Size = %d", h.Size)
	}
	if h.SHA256 != "" || h.ETag != "" {
		t.Errorf("Head should leave digest empty in slice 1: %+v", h)
	}

	rc, ho, err := s.Get(ScopeGlobal, "agents/ag_1/avatar.png")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	read, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(read) != body {
		t.Errorf("Get body = %q want %q", string(read), body)
	}
	if ho.Size != int64(len(body)) {
		t.Errorf("Get.Size = %d", ho.Size)
	}

	// Verify computes sha256 from disk.
	v, err := s.Verify(ScopeGlobal, "agents/ag_1/avatar.png")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if v.SHA256 != want || v.ETag != "sha256:"+want {
		t.Errorf("Verify mismatch: %+v", v)
	}
}

func TestPutAtomicAbortOnSHA256Mismatch(t *testing.T) {
	s := New(t.TempDir())
	body := "first body"
	if _, err := s.Put(ScopeGlobal, "agents/ag_1/avatar.png", strings.NewReader(body), PutOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Bogus ExpectedSHA256 → stream aborts, on-disk file stays at the
	// seeded body.
	_, err := s.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader("would-replace"),
		PutOptions{ExpectedSHA256: "0000000000000000000000000000000000000000000000000000000000000000"})
	if err == nil {
		t.Fatal("expected sha256 mismatch error")
	}
	rc, _, err := s.Get(ScopeGlobal, "agents/ag_1/avatar.png")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != body {
		t.Errorf("aborted Put leaked: body = %q want %q", got, body)
	}
}

func TestPutIfMatch(t *testing.T) {
	s := New(t.TempDir())
	first := putString(t, s, ScopeGlobal, "agents/ag_1/avatar.png", "v1", PutOptions{})

	// Wrong etag → ErrETagMismatch.
	_, err := s.Put(ScopeGlobal, "agents/ag_1/avatar.png", strings.NewReader("v2"),
		PutOptions{IfMatch: "sha256:0000000000000000000000000000000000000000000000000000000000000000"})
	if !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("got %v want ErrETagMismatch", err)
	}

	// Right etag → succeeds.
	o, err := s.Put(ScopeGlobal, "agents/ag_1/avatar.png", strings.NewReader("v2"),
		PutOptions{IfMatch: first.ETag})
	if err != nil {
		t.Fatalf("Put with matching IfMatch: %v", err)
	}
	if o.ETag == first.ETag {
		t.Errorf("etag did not change after rewrite")
	}

	// IfMatch on a missing path is also a mismatch.
	_, err = s.Put(ScopeGlobal, "agents/ag_1/missing.bin", strings.NewReader("x"),
		PutOptions{IfMatch: "sha256:abcd"})
	if !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("missing IfMatch: got %v want ErrETagMismatch", err)
	}
}

func TestDelete(t *testing.T) {
	s := New(t.TempDir())
	o := putString(t, s, ScopeGlobal, "agents/ag_1/avatar.png", "x", PutOptions{})

	// IfMatch wrong → mismatch, file stays.
	if err := s.Delete(ScopeGlobal, "agents/ag_1/avatar.png", DeleteOptions{IfMatch: "sha256:0"}); !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("wrong IfMatch: %v", err)
	}
	if _, err := s.Head(ScopeGlobal, "agents/ag_1/avatar.png"); err != nil {
		t.Fatalf("file removed despite mismatch: %v", err)
	}

	// IfMatch right → gone.
	if err := s.Delete(ScopeGlobal, "agents/ag_1/avatar.png", DeleteOptions{IfMatch: o.ETag}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Head(ScopeGlobal, "agents/ag_1/avatar.png"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Head after delete: %v", err)
	}
	if err := s.Delete(ScopeGlobal, "agents/ag_1/avatar.png", DeleteOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestList(t *testing.T) {
	s := New(t.TempDir())
	putString(t, s, ScopeGlobal, "agents/ag_1/avatar.png", "a", PutOptions{})
	putString(t, s, ScopeGlobal, "agents/ag_1/books/x.md", "x", PutOptions{})
	putString(t, s, ScopeGlobal, "agents/ag_2/avatar.png", "b", PutOptions{})
	putString(t, s, ScopeLocal, "agents/ag_1/temp/scratch.bin", "c", PutOptions{})

	got, err := s.List(ScopeGlobal, "agents/ag_1/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	paths := []string{}
	for _, o := range got {
		paths = append(paths, o.Path)
	}
	sort.Strings(paths)
	want := []string{"agents/ag_1/avatar.png", "agents/ag_1/books/x.md"}
	if len(paths) != len(want) {
		t.Fatalf("List paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("List[%d] = %q want %q", i, paths[i], want[i])
		}
	}

	// Empty scope (no Put yet) → empty list, not error.
	empty, err := s.List(ScopeMachine, "")
	if err != nil {
		t.Fatalf("empty scope List: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty scope returned %d entries", len(empty))
	}
}

func TestListSkipsReservedJunk(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	putString(t, s, ScopeGlobal, "agents/ag_1/avatar.png", "ok", PutOptions{})
	// Drop OS junk straight onto disk to simulate Finder/Office cruft
	// that bypassed the Put validation path.
	junkDir := filepath.Join(root, "global", "agents", "ag_1")
	for _, name := range []string{".DS_Store", "._avatar.png", "~$lock"} {
		if err := os.WriteFile(filepath.Join(junkDir, name), []byte("junk"), 0o644); err != nil {
			t.Fatalf("seed junk %s: %v", name, err)
		}
	}
	got, err := s.List(ScopeGlobal, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, o := range got {
		segs := strings.Split(o.Path, "/")
		leaf := segs[len(segs)-1]
		if leaf == ".DS_Store" || strings.HasPrefix(leaf, "._") || strings.HasPrefix(leaf, "~$") {
			t.Errorf("List leaked junk: %q", o.Path)
		}
	}
}

func TestPathValidation(t *testing.T) {
	s := New(t.TempDir())
	bad := []string{
		"",
		"/abs",
		"a//b",
		"a/./b",
		"../escape",
		"a/../b",
		"trailing/",
		"a/.DS_Store",
		"._dot",
		"~$lock.docx",
		"a\x00b",
		"a\\b",
	}
	for _, p := range bad {
		_, err := s.Put(ScopeGlobal, p, strings.NewReader("x"), PutOptions{})
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Put(%q) err = %v, want ErrInvalidPath", p, err)
		}
	}

	// Invalid scope on an otherwise fine path.
	_, err := s.Put(Scope("garbage"), "ok", strings.NewReader("x"), PutOptions{})
	if !errors.Is(err, ErrInvalidScope) {
		t.Errorf("invalid scope err = %v", err)
	}
}

func TestHeadRefusesDirectory(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	putString(t, s, ScopeGlobal, "agents/ag_1/avatar.png", "x", PutOptions{})
	// "agents/ag_1" is a directory — Head must say not-found rather
	// than exposing dir metadata as a blob.
	if _, err := s.Head(ScopeGlobal, "agents/ag_1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Head on dir: %v", err)
	}
}

func TestPutCreatesParentDirs(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	// Deep path with dirs that don't exist yet.
	o := putString(t, s, ScopeGlobal, "agents/ag_1/books/sub/deep.md", "deep", PutOptions{})
	if o.Size != 4 {
		t.Errorf("size = %d", o.Size)
	}
	full := filepath.Join(root, "global", "agents", "ag_1", "books", "sub", "deep.md")
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, []byte("deep")) {
		t.Errorf("content = %q", got)
	}
}

func TestPutLeavesNoTempOnSuccess(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	putString(t, s, ScopeGlobal, "agents/ag_1/avatar.png", "x", PutOptions{})

	dir := filepath.Join(root, "global", "agents", "ag_1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".blob-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestPathValidationCaseFoldedReserved(t *testing.T) {
	s := New(t.TempDir())
	// Reserved-name table is case-insensitive: a hostile peer can't
	// stash `THUMBS.DB` past the validator that lower-cases its keys.
	for _, p := range []string{
		"agents/ag_1/.DS_STORE",
		"agents/ag_1/Thumbs.DB",
		"agents/ag_1/desktop.INI",
		"agents/ag_1/THUMBS.db",
	} {
		_, err := s.Put(ScopeGlobal, p, strings.NewReader("x"), PutOptions{})
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Put(%q) err = %v, want ErrInvalidPath", p, err)
		}
	}
}

func TestPathValidationWindowsReserved(t *testing.T) {
	s := New(t.TempDir())
	// Windows refuses NT device names regardless of case or extension;
	// blocking them here keeps the cluster homogeneously addressable.
	for _, p := range []string{
		"con", "Con", "CON",
		"nul.txt", "PRN.md", "aux.json",
		"com1", "COM9.txt", "lpt5.bin",
		"agents/ag_1/CON.png",
	} {
		_, err := s.Put(ScopeGlobal, p, strings.NewReader("x"), PutOptions{})
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Put(%q) err = %v, want ErrInvalidPath", p, err)
		}
	}
	// `console.txt` is fine — only the bare device names are reserved.
	if _, err := s.Put(ScopeGlobal, "agents/ag_1/console.txt", strings.NewReader("x"), PutOptions{}); err != nil {
		t.Errorf("Put(console.txt) should succeed: %v", err)
	}
}

func TestPathValidationRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	s := New(root)

	// Lay down a symlink that would let `agents/ag_1/avatar.png` land
	// in `outside` rather than under the scope dir.
	if err := os.MkdirAll(filepath.Join(root, "global", "agents"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(outside, "ag_1")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(root, "global", "agents", "ag_1")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// A symlink in a parent component is an environment defect, not a
	// logical-path validation failure — it surfaces as
	// ErrScopeContainmentBroken so callers (especially the migration
	// importer) can distinguish "client sent a bad path" from "the
	// v1 blob tree has been tampered with" and abort instead of
	// warn-and-skip.
	_, err := s.Put(ScopeGlobal, "agents/ag_1/avatar.png", strings.NewReader("x"), PutOptions{})
	if !errors.Is(err, ErrScopeContainmentBroken) {
		t.Fatalf("Put through symlink parent: got %v want ErrScopeContainmentBroken", err)
	}
	// Get / Head / Verify / Delete all share the same fsPath guard.
	if _, _, err := s.Get(ScopeGlobal, "agents/ag_1/avatar.png"); !errors.Is(err, ErrScopeContainmentBroken) {
		t.Errorf("Get through symlink: %v", err)
	}
	if _, err := s.Head(ScopeGlobal, "agents/ag_1/avatar.png"); !errors.Is(err, ErrScopeContainmentBroken) {
		t.Errorf("Head through symlink: %v", err)
	}
	if _, err := s.Verify(ScopeGlobal, "agents/ag_1/avatar.png"); !errors.Is(err, ErrScopeContainmentBroken) {
		t.Errorf("Verify through symlink: %v", err)
	}
	if err := s.Delete(ScopeGlobal, "agents/ag_1/avatar.png", DeleteOptions{}); !errors.Is(err, ErrScopeContainmentBroken) {
		t.Errorf("Delete through symlink: %v", err)
	}
}

func TestListPrefixAcceptsLegitimateDoubleDot(t *testing.T) {
	s := New(t.TempDir())
	// `foo..bar` is a perfectly valid blob path component (two dots
	// adjacent inside a name) and List should not refuse it as if it
	// were a `..` traversal.
	putString(t, s, ScopeGlobal, "agents/ag..1/avatar.png", "x", PutOptions{})
	got, err := s.List(ScopeGlobal, "agents/ag..1/")
	if err != nil {
		t.Fatalf("List with embedded double-dot: %v", err)
	}
	if len(got) != 1 || got[0].Path != "agents/ag..1/avatar.png" {
		t.Errorf("List = %+v", got)
	}

	// A genuine `..` segment must still be rejected.
	if _, err := s.List(ScopeGlobal, "agents/../escape"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("List with traversal prefix: got %v want ErrInvalidPath", err)
	}
}

func TestPutIfMatchSerializesConcurrentWrites(t *testing.T) {
	s := New(t.TempDir())
	first := putString(t, s, ScopeGlobal, "agents/ag_1/avatar.png", "v0", PutOptions{})

	// Two writers race with the same expected etag. The pathLock must
	// linearize them so exactly one wins; the loser sees ErrETagMismatch
	// instead of silently overwriting the winner.
	const n = 8
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			body := fmt.Sprintf("v%d", i+1)
			_, err := s.Put(ScopeGlobal, "agents/ag_1/avatar.png",
				strings.NewReader(body), PutOptions{IfMatch: first.ETag})
			results <- err
		}()
	}
	wins, losses := 0, 0
	for i := 0; i < n; i++ {
		err := <-results
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrETagMismatch):
			losses++
		default:
			t.Errorf("unexpected err: %v", err)
		}
	}
	if wins != 1 {
		t.Errorf("concurrent IfMatch winners = %d, want exactly 1", wins)
	}
	if losses != n-1 {
		t.Errorf("concurrent IfMatch losers = %d, want %d", losses, n-1)
	}
}

func TestPathValidationRejectsNonNFC(t *testing.T) {
	s := New(t.TempDir())
	// "cafe" + acute can be encoded NFC (single U+00E9) or NFD (`e`
	// + U+0301 combining acute). The native API requires NFC; an
	// NFD caller gets ErrInvalidPath rather than silently round-
	// tripping to a different blob name. Both forms are built with
	// explicit \u escapes so the source-file encoding can't quietly
	// normalize one into the other.
	nfc := "agents/caf\u00e9/avatar.png"
	nfd := "agents/cafe\u0301/avatar.png"
	if _, err := s.Put(ScopeGlobal, nfd, strings.NewReader("x"), PutOptions{}); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("non-NFC Put: got %v want ErrInvalidPath", err)
	}
	if _, err := s.Put(ScopeGlobal, nfc, strings.NewReader("x"), PutOptions{}); err != nil {
		t.Errorf("NFC Put: %v", err)
	}
}

func TestPathValidationRejectsWindowsIllegalChars(t *testing.T) {
	s := New(t.TempDir())
	bad := []string{
		"trailing.",
		"trailing ",
		"with<lt",
		"with>gt",
		"with:colon",
		`with"quote`,
		"with|pipe",
		"with?qm",
		"with*star",
		"agents/ag_1/foo.",
		"agents/ag_1/foo ",
	}
	for _, p := range bad {
		_, err := s.Put(ScopeGlobal, p, strings.NewReader("x"), PutOptions{})
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Put(%q) err = %v, want ErrInvalidPath", p, err)
		}
	}
}

func TestPathValidationRejectsBlobTempPrefix(t *testing.T) {
	s := New(t.TempDir())
	// `.blob-` is reserved for atomicWrite's temp files; allowing a
	// caller to Put one would let them collide with an in-flight
	// publish.
	for _, p := range []string{".blob-foo", "agents/ag_1/.blob-temp", "agents/ag_1/.BLOB-temp"} {
		_, err := s.Put(ScopeGlobal, p, strings.NewReader("x"), PutOptions{})
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Put(%q) err = %v, want ErrInvalidPath", p, err)
		}
	}
}

func TestDeleteRejectsDirectory(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	putString(t, s, ScopeGlobal, "agents/ag_1/avatar.png", "x", PutOptions{})
	// Delete on a directory must say not-found rather than silently
	// removing the empty dir below it.
	if err := s.Delete(ScopeGlobal, "agents/ag_1", DeleteOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete on dir: %v", err)
	}
	// And the file underneath is still there.
	if _, err := s.Head(ScopeGlobal, "agents/ag_1/avatar.png"); err != nil {
		t.Errorf("Head after dir-delete: %v", err)
	}
}

func TestDeleteIfMatchOnMissingReportsMismatch(t *testing.T) {
	s := New(t.TempDir())
	first := putString(t, s, ScopeGlobal, "agents/ag_1/avatar.png", "x", PutOptions{})
	if err := s.Delete(ScopeGlobal, "agents/ag_1/avatar.png", DeleteOptions{}); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	// IfMatch on missing: precondition cannot match anything → mismatch.
	// Same semantic Put gives in TestPutIfMatch.
	err := s.Delete(ScopeGlobal, "agents/ag_1/avatar.png", DeleteOptions{IfMatch: first.ETag})
	if !errors.Is(err, ErrETagMismatch) {
		t.Errorf("Delete missing with IfMatch: got %v want ErrETagMismatch", err)
	}
	// Without IfMatch the same call is plain ErrNotFound — idempotent
	// double-delete is a familiar Web semantic.
	if err := s.Delete(ScopeGlobal, "agents/ag_1/avatar.png", DeleteOptions{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing no-IfMatch: %v", err)
	}
}

func TestPathValidationRejectsControlChars(t *testing.T) {
	s := New(t.TempDir())
	for _, ctrl := range []byte{0x01, 0x09, 0x0a, 0x0d, 0x1f} {
		p := "agents/ag_1/" + string(ctrl) + "x.png"
		_, err := s.Put(ScopeGlobal, p, strings.NewReader("x"), PutOptions{})
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Put with control 0x%02x: got %v want ErrInvalidPath", ctrl, err)
		}
	}
}

func TestPutCleansUpTempOnAbort(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	_, err := s.Put(ScopeGlobal, "agents/ag_1/avatar.png", strings.NewReader("body"),
		PutOptions{ExpectedSHA256: "deadbeef"})
	if err == nil {
		t.Fatal("expected abort")
	}
	dir := filepath.Join(root, "global", "agents", "ag_1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Parent might not exist if mkdir-then-abort pattern leaves
		// nothing — that's also acceptable.
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".blob-") {
			t.Errorf("temp left after abort: %s", e.Name())
		}
	}
}
