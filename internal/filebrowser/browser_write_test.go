package filebrowser

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteTextRequiresMatchingETag(t *testing.T) {
	b := New(nil)
	dir := t.TempDir()
	p := filepath.Join(dir, "note.md")
	if err := os.WriteFile(p, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	v, err := b.View(p)
	if err != nil {
		t.Fatal(err)
	}
	if v.ETag == "" || v.ETag != ContentETag([]byte("old")) {
		t.Fatalf("view etag=%q", v.ETag)
	}

	if _, err := b.WriteText(p, "x", ""); err == nil {
		t.Fatal("empty etag accepted")
	}
	if _, err := b.WriteText(p, "x", ContentETag([]byte("other"))); !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("stale etag err=%v", err)
	}

	out, err := b.WriteText(p, "new body\n", v.ETag)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "new body\n" {
		t.Fatalf("content=%q", got)
	}
	if out.ETag != ContentETag(got) || out.Size != int64(len(got)) || out.Language != "markdown" {
		t.Fatalf("out=%+v", out)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("mode=%o, want 640", info.Mode().Perm())
		}
	}
	// The old etag is now stale.
	if _, err := b.WriteText(p, "again", v.ETag); !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("reused etag err=%v", err)
	}
}

func TestWriteTextRejectsNonEditable(t *testing.T) {
	b := New(nil)
	dir := t.TempDir()

	bin := filepath.Join(dir, "a.bin")
	os.WriteFile(bin, []byte{'a', 0, 'b'}, 0o644)
	if _, err := b.WriteText(bin, "x", ContentETag([]byte{'a', 0, 'b'})); !errors.Is(err, ErrUnsupportedFile) {
		t.Fatalf("binary err=%v", err)
	}

	img := filepath.Join(dir, "a.png")
	os.WriteFile(img, []byte("not really"), 0o644)
	if _, err := b.WriteText(img, "x", ContentETag([]byte("not really"))); !errors.Is(err, ErrUnsupportedFile) {
		t.Fatalf("image err=%v", err)
	}

	txt := filepath.Join(dir, "a.txt")
	os.WriteFile(txt, []byte("t"), 0o644)
	if _, err := b.WriteText(txt, "a\x00b", ContentETag([]byte("t"))); !errors.Is(err, ErrUnsupportedFile) {
		t.Fatalf("NUL content err=%v", err)
	}
	if _, err := b.WriteText(txt, strings.Repeat("a", maxFileSize+1), ContentETag([]byte("t"))); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("too large err=%v", err)
	}
	if _, err := b.WriteText(filepath.Join(dir, "missing.txt"), "x", "abc"); err == nil {
		t.Fatal("missing file was created")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.txt")); !os.IsNotExist(err) {
		t.Fatalf("missing file exists: %v", err)
	}
	if _, err := b.WriteText(dir, "x", "abc"); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := b.WriteText("/etc/hosts", "x", "abc"); err == nil {
		t.Fatal("path outside allowed roots accepted")
	}
}

func TestWriteTextKeepsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	b := New(nil)
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	link := filepath.Join(dir, "link.txt")
	os.WriteFile(target, []byte("v1"), 0o644)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := b.WriteText(link, "v2", ContentETag([]byte("v1"))); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v %v", fi, err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "v2" {
		t.Fatalf("target=%q", got)
	}
}
