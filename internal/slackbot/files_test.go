package slackbot

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// newFileTestBot returns a minimal Bot suitable for exercising downloadSlackFiles
// and appendFileInfo. It does NOT start a real Slack socket connection.
func newFileTestBot(t *testing.T, botToken string) *Bot {
	t.Helper()
	return &Bot{
		agentID:  "test-agent",
		logger:   testLogger,
		botToken: botToken,
	}
}

// withTempUploadDir redirects uploadDir to a per-test directory so multiple
// tests don't clobber each other.
func withTempUploadDir(t *testing.T) string {
	t.Helper()
	orig := uploadDir
	dir := filepath.Join(t.TempDir(), "kojo-upload")
	uploadDir = dir
	t.Cleanup(func() { uploadDir = orig })
	return dir
}

func TestAppendFileInfo(t *testing.T) {
	t.Run("no files and no errors returns original text", func(t *testing.T) {
		got := appendFileInfo("hello", nil, nil)
		if got != "hello" {
			t.Fatalf("got %q, want %q", got, "hello")
		}
	})

	t.Run("files section is appended", func(t *testing.T) {
		files := []downloadedFile{
			{Path: "/tmp/kojo/upload/1_a.png", Name: "a.png", Mime: "image/png", Size: 42},
		}
		got := appendFileInfo("see attached", files, nil)
		if !strings.Contains(got, "[Attached files]") {
			t.Errorf("missing attached files header: %q", got)
		}
		if !strings.Contains(got, "a.png") || !strings.Contains(got, "image/png") {
			t.Errorf("file metadata missing: %q", got)
		}
	})

	t.Run("errors section is appended", func(t *testing.T) {
		errs := []fileError{{Name: "big.bin", Err: "file too large"}}
		got := appendFileInfo("", nil, errs)
		if !strings.Contains(got, "[File download errors]") {
			t.Errorf("missing error header: %q", got)
		}
		if !strings.Contains(got, "big.bin") || !strings.Contains(got, "file too large") {
			t.Errorf("error metadata missing: %q", got)
		}
	})
}

func TestPreflightSlackFile(t *testing.T) {
	t.Run("oversize rejected", func(t *testing.T) {
		err := preflightSlackFile(slack.File{Size: maxFileSize + 1, URLPrivateDownload: "https://x"})
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Errorf("got err=%v, want too-large error", err)
		}
	})
	t.Run("missing URL rejected", func(t *testing.T) {
		err := preflightSlackFile(slack.File{Size: 10})
		if err == nil || !strings.Contains(err.Error(), "no download URL") {
			t.Errorf("got err=%v, want missing-url error", err)
		}
	})
	t.Run("ok when size and URL are valid", func(t *testing.T) {
		err := preflightSlackFile(slack.File{Size: 10, URLPrivateDownload: "https://x"})
		if err != nil {
			t.Errorf("unexpected err: %v", err)
		}
	})
}

func TestBuildLocalPath(t *testing.T) {
	now := time.Unix(1_700_000_000, 1234)
	t.Run("sanitizes path traversal", func(t *testing.T) {
		got := buildLocalPath("/tmp", now, "../etc/passwd")
		want := fmt.Sprintf("/tmp/%d_passwd", now.UnixNano())
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("preserves ordinary filename", func(t *testing.T) {
		got := buildLocalPath("/tmp", now, "photo.png")
		want := fmt.Sprintf("/tmp/%d_photo.png", now.UnixNano())
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"simple.txt":              "simple.txt",
		"../etc/passwd":           "passwd",
		"dir/sub/file.png":        "file.png",
		"weird\x00name.jpg":       "weird_name.jpg",
		`back\slash.png`:          "back_slash.png",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDownloadSlackFilesSuccess(t *testing.T) {
	dir := withTempUploadDir(t)
	_ = dir

	payload := []byte("hello, slack file")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxb-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	bot := newFileTestBot(t, "xoxb-test")
	files := []slack.File{{
		ID:                 "F1",
		Name:               "note.txt",
		Mimetype:           "text/plain",
		Size:               len(payload),
		URLPrivateDownload: srv.URL + "/note.txt",
	}}

	got, errs := bot.downloadSlackFiles(context.Background(), files)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 file, got %d", len(got))
	}

	// Content round-trips.
	data, err := os.ReadFile(got[0].Path)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("content mismatch: got %q, want %q", data, payload)
	}

	// Permissions are restrictive (0600) on platforms where it matters.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(got[0].Path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("file permissions = %o, want 0600", perm)
		}
	}
}

func TestDownloadSlackFilesTooLarge(t *testing.T) {
	withTempUploadDir(t)

	// Slack pre-announces size > maxFileSize → fast-path rejection, server
	// must not be hit. We still expose a server URL so URLPrivateDownload
	// is non-empty and we hit the size check rather than the URL check.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be called when Slack-declared size exceeds cap")
	}))
	t.Cleanup(srv.Close)

	bot := newFileTestBot(t, "xoxb-test")
	files := []slack.File{{
		ID:                 "F2",
		Name:               "huge.bin",
		Size:               maxFileSize + 1,
		URLPrivateDownload: srv.URL + "/huge.bin",
	}}

	got, errs := bot.downloadSlackFiles(context.Background(), files)
	if len(got) != 0 {
		t.Errorf("expected no downloads, got %+v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %+v", errs)
	}
	if !strings.Contains(errs[0].Err, "too large") {
		t.Errorf("unexpected error: %q", errs[0].Err)
	}
}

func TestDownloadSlackFilesServerLies(t *testing.T) {
	dir := withTempUploadDir(t)

	// Slack claims the file is small, but the server streams more than the
	// cap. The hard cap should reject the download and delete the partial
	// file rather than silently truncating it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		// Write 2× the cap, one chunk at a time.
		chunk := bytes.Repeat([]byte("A"), 64*1024)
		written := 0
		for written < 2*maxFileSize {
			n, err := w.Write(chunk)
			if err != nil {
				return
			}
			written += n
		}
	}))
	t.Cleanup(srv.Close)

	bot := newFileTestBot(t, "xoxb-test")
	files := []slack.File{{
		ID:                 "F3",
		Name:               "lying.bin",
		Size:               1024, // below cap — bypasses fast-path
		URLPrivateDownload: srv.URL + "/lying.bin",
	}}

	got, errs := bot.downloadSlackFiles(context.Background(), files)
	if len(got) != 0 {
		t.Errorf("expected no successful downloads, got %+v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %+v", errs)
	}
	if !strings.Contains(errs[0].Err, "exceeds max size") {
		t.Errorf("unexpected error: %q", errs[0].Err)
	}

	// Partial file must be cleaned up.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read upload dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "lying.bin") {
			t.Errorf("partial file not removed: %s", e.Name())
		}
	}
}

func TestDownloadSlackFilesNoDownloadURL(t *testing.T) {
	withTempUploadDir(t)

	bot := newFileTestBot(t, "xoxb-test")
	files := []slack.File{{ID: "F4", Name: "no-url.txt", Size: 10}}

	got, errs := bot.downloadSlackFiles(context.Background(), files)
	if len(got) != 0 {
		t.Errorf("expected no downloads, got %+v", got)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Err, "no download URL") {
		t.Fatalf("expected no-download-url error, got %+v", errs)
	}
}

func TestDownloadSlackFilesHTTPError(t *testing.T) {
	withTempUploadDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	bot := newFileTestBot(t, "xoxb-test")
	files := []slack.File{{
		ID:                 "F5",
		Name:               "denied.txt",
		Size:               10,
		URLPrivateDownload: srv.URL + "/denied.txt",
	}}

	got, errs := bot.downloadSlackFiles(context.Background(), files)
	if len(got) != 0 {
		t.Errorf("expected no downloads, got %+v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %+v", errs)
	}
	if !strings.Contains(errs[0].Err, fmt.Sprintf("%d", http.StatusForbidden)) {
		t.Errorf("unexpected error: %q", errs[0].Err)
	}
}
