package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/blob"
)

// TestSanitizeAttachBasename pins the rejection table: dotfiles,
// path traversal, control chars, separators, oversize names. The
// daemon depends on this being strict because the cleaned name is
// concatenated into a blob path and an HTTP URL.
func TestSanitizeAttachBasename(t *testing.T) {
	cases := []struct {
		in       string
		want     string
		wantOK   bool
		wantTrim bool // true when we expect the result to differ from in
	}{
		{"chart.png", "chart.png", true, false},
		{"report.pdf", "report.pdf", true, false},
		{"  spaced.txt  ", "spaced.txt", true, true},
		{"sub/dir/file.png", "file.png", true, true},
		{`sub\dir\file.png`, "file.png", true, true},
		{"", "", false, false},
		{".", "", false, false},
		{"..", "", false, false},
		{".env", "", false, false},  // dotfile rejected
		{".bashrc", "", false, false}, // dotfile rejected
		{"with\x00nul.txt", "", false, false},
		{"with\nnewline.txt", "", false, false},
		{"with\rcr.txt", "", false, false},
		{"with\ttab.txt", "", false, false},
	}
	for _, c := range cases {
		got, ok := sanitizeAttachBasename(c.in)
		if ok != c.wantOK {
			t.Errorf("sanitize(%q) ok=%v; want %v", c.in, ok, c.wantOK)
			continue
		}
		if got != c.want {
			t.Errorf("sanitize(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// TestSanitizeAttachBasename_LengthCap verifies a 200+ char input
// is trimmed but keeps the extension so the UI can still infer
// type from .png / .pdf etc.
func TestSanitizeAttachBasename_LengthCap(t *testing.T) {
	stem := strings.Repeat("a", 400)
	in := stem + ".png"
	got, ok := sanitizeAttachBasename(in)
	if !ok {
		t.Fatalf("sanitize(long.png) rejected")
	}
	if !strings.HasSuffix(got, ".png") {
		t.Errorf("trimmed name lost extension: %q", got)
	}
	if len(got) > 200 {
		t.Errorf("trimmed name still > 200 chars: %d", len(got))
	}
}

// TestReserveUniqueName ensures a collision in the batch resolves
// to `<stem>-1.ext`, `<stem>-2.ext`, ... so the second file with
// the same basename doesn't silently overwrite the first.
func TestReserveUniqueName(t *testing.T) {
	used := map[string]struct{}{}
	first := reserveUniqueName("chart.png", used)
	if first != "chart.png" {
		t.Fatalf("first = %q; want chart.png", first)
	}
	used[first] = struct{}{}

	second := reserveUniqueName("chart.png", used)
	if second != "chart-1.png" {
		t.Fatalf("second = %q; want chart-1.png", second)
	}
	used[second] = struct{}{}

	third := reserveUniqueName("chart.png", used)
	if third != "chart-2.png" {
		t.Fatalf("third = %q; want chart-2.png", third)
	}
}

// TestBuildAttachBlobPath asserts the canonical layout used by
// scanAndIngestAttachments. The path is concatenated directly into
// the blob URI; a layout drift here would land attachments on the
// wrong key and the UI's blob fetch would 404.
func TestBuildAttachBlobPath(t *testing.T) {
	got := buildAttachBlobPath("ag_1", "m_abc", "chart.png")
	want := "agents/ag_1/attach/m_abc/chart.png"
	if got != want {
		t.Errorf("buildAttachBlobPath = %q; want %q", got, want)
	}

	// Empty messageID falls back to a timestamped tier so distinct
	// abort-path persists never overwrite each other on the same
	// agent.
	fallback := buildAttachBlobPath("ag_1", "", "chart.png")
	if !strings.HasPrefix(fallback, "agents/ag_1/attach/ts_") {
		t.Errorf("empty messageID fallback = %q; want ts_ prefix", fallback)
	}
}

// TestGuessMime asserts extension wins over body-sniff so we don't
// surprise the UI with a sniffed type (image/png) for a file the
// agent named `.txt`.
func TestGuessMime(t *testing.T) {
	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

	if got := guessMime("chart.png", pngBytes); !strings.HasPrefix(got, "image/png") {
		t.Errorf("chart.png mime = %q; want image/png*", got)
	}
	if got := guessMime("notes.txt", pngBytes); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("notes.txt mime = %q; want text/plain* (extension overrides body sniff)", got)
	}
	// Extension-less file: fall through to body sniff.
	if got := guessMime("unknown", pngBytes); !strings.HasPrefix(got, "image/png") {
		t.Errorf("unknown mime = %q; want image/png* (body sniff)", got)
	}
	// Extension-less + empty body: octet-stream.
	if got := guessMime("unknown", nil); got != "application/octet-stream" {
		t.Errorf("unknown empty mime = %q; want application/octet-stream", got)
	}
}

// TestScanAndIngestAttachments_HappyPath drives the full integration:
// stage two files, run the scan, expect two MessageAttachment
// entries with blob URIs, expect the staging dir to be empty, and
// expect the bodies to be reachable from the blob store via the
// canonical kojo:// URI.
func TestScanAndIngestAttachments_HappyPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "") // Windows configdir fallback path

	bs := newWiredBlob(t)

	agentID := "ag_attach_happy"
	stageDir := filepath.Join(agentDir(agentID), attachStagingSubpath)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatalf("mkdir stage: %v", err)
	}
	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'b', 'o', 'd', 'y'}
	if err := os.WriteFile(filepath.Join(stageDir, "chart.png"), pngBytes, 0o644); err != nil {
		t.Fatalf("write chart: %v", err)
	}
	pdfBytes := []byte("%PDF-1.4 ... fake body")
	if err := os.WriteFile(filepath.Join(stageDir, "report.pdf"), pdfBytes, 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}

	m := &Manager{blobStore: bs}
	out := m.scanAndIngestAttachments(context.Background(), agentID, "m_abc")

	if len(out) != 2 {
		t.Fatalf("got %d attachments; want 2", len(out))
	}
	// Lexical sort: chart.png < report.pdf
	if out[0].Name != "chart.png" || out[1].Name != "report.pdf" {
		t.Errorf("name order = [%q,%q]; want [chart.png,report.pdf]",
			out[0].Name, out[1].Name)
	}
	for _, a := range out {
		if !strings.HasPrefix(a.Path, "kojo://global/agents/"+agentID+"/attach/m_abc/") {
			t.Errorf("path = %q; want kojo://global/agents/.../attach/m_abc/<name>", a.Path)
		}
		if a.Size <= 0 {
			t.Errorf("size = %d; want > 0", a.Size)
		}
		if a.Mime == "" {
			t.Errorf("mime empty for %q", a.Name)
		}
	}

	// Staging dir must be cleaned out.
	entries, err := os.ReadDir(stageDir)
	if err == nil && len(entries) > 0 {
		t.Errorf("staging dir still has %d entries after scan", len(entries))
	}

	// The body must be reachable from the blob store via the
	// canonical URI. Re-derive (scope, path) the same way the
	// daemon does so a path-format drift here breaks the test.
	got, _, err := bs.Open(blob.ScopeGlobal,
		"agents/"+agentID+"/attach/m_abc/chart.png")
	if err != nil {
		t.Fatalf("open chart blob: %v", err)
	}
	defer got.Close()
	gotBody, err := io.ReadAll(got)
	if err != nil {
		t.Fatalf("read chart blob: %v", err)
	}
	if string(gotBody) != string(pngBytes) {
		t.Errorf("chart body mismatch: got %d bytes, want %d", len(gotBody), len(pngBytes))
	}
}

// TestScanAndIngestAttachments_RejectsBadEntries proves dotfiles,
// path-traversal segments, symlinks, and oversize names are all
// dropped (with the source file still removed so they cannot
// linger into the next turn).
func TestScanAndIngestAttachments_RejectsBadEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "")

	bs := newWiredBlob(t)
	agentID := "ag_attach_bad"
	stageDir := filepath.Join(agentDir(agentID), attachStagingSubpath)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 1 good + 3 bad: dotfile, empty, symlink-to-outside.
	if err := os.WriteFile(filepath.Join(stageDir, "good.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write good: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, ".env"), []byte("SECRET=x"), 0o644); err != nil {
		t.Fatalf("write dotfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "empty.bin"), nil, 0o644); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("steal me"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(stageDir, "smuggle.txt")); err != nil {
		// On Windows test runs without admin, symlink may fail. Skip
		// that one rejection rather than fail the whole test.
		t.Logf("symlink not supported; skipping symlink rejection check: %v", err)
	}

	m := &Manager{blobStore: bs}
	out := m.scanAndIngestAttachments(context.Background(), agentID, "m_bad")

	if len(out) != 1 || out[0].Name != "good.txt" {
		names := make([]string, 0, len(out))
		for _, a := range out {
			names = append(names, a.Name)
		}
		t.Fatalf("got %v; want [good.txt] only", names)
	}

	// Every staged file (good + bad) must be gone — we never want
	// a rejected entry to linger and re-fire on the next scan.
	entries, _ := os.ReadDir(stageDir)
	if len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("staging dir still has entries after scan: %v", names)
	}
}

// TestScanAndIngestAttachments_ForwarderInvoked confirms the
// AttachmentForwarder callback is called once per successful
// ingest with the same digest the local Put recorded — that
// invariant is what the hub-side ingest handler depends on for
// X-Kojo-Expected-SHA256 verification.
func TestScanAndIngestAttachments_ForwarderInvoked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "")

	bs := newWiredBlob(t)
	agentID := "ag_attach_fwd"
	stageDir := filepath.Join(agentDir(agentID), attachStagingSubpath)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "x.bin"),
		[]byte("hello world"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	type fwdCall struct {
		scope    blob.Scope
		path     string
		sha256   string
		readSize int
	}
	var calls []fwdCall
	m := &Manager{blobStore: bs}
	m.SetAttachmentForwarder(func(
		_ context.Context,
		scope blob.Scope,
		path string,
		sha256Hex string,
		body io.Reader,
		size int64,
	) error {
		buf, _ := io.ReadAll(body)
		calls = append(calls, fwdCall{
			scope:    scope,
			path:     path,
			sha256:   sha256Hex,
			readSize: len(buf),
		})
		_ = size
		return nil
	})

	out := m.scanAndIngestAttachments(context.Background(), agentID, "m_fwd")
	if len(out) != 1 {
		t.Fatalf("got %d; want 1", len(out))
	}
	if len(calls) != 1 {
		t.Fatalf("forwarder called %d times; want 1", len(calls))
	}
	if calls[0].scope != blob.ScopeGlobal {
		t.Errorf("scope = %q; want global", calls[0].scope)
	}
	if calls[0].path != "agents/"+agentID+"/attach/m_fwd/x.bin" {
		t.Errorf("path = %q; unexpected", calls[0].path)
	}
	if calls[0].sha256 == "" {
		t.Errorf("forwarder received empty sha256")
	}
	if calls[0].readSize != len("hello world") {
		t.Errorf("forwarder body size = %d; want %d", calls[0].readSize, len("hello world"))
	}
}

// TestScanAndIngestAttachments_RejectsSymlinkStageDir proves a
// `.kojo/attach` symlink (e.g. ag does `ln -s /etc .kojo/attach`)
// does NOT cause kojo to scan or RemoveAll the symlink target.
// We must detect the non-directory and bail without touching the
// pointed-to filesystem subtree.
func TestScanAndIngestAttachments_RejectsSymlinkStageDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "")

	bs := newWiredBlob(t)
	agentID := "ag_attach_symdir"
	stageDir := filepath.Join(agentDir(agentID), attachStagingSubpath)
	if err := os.MkdirAll(filepath.Dir(stageDir), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "secret.txt"),
		[]byte("DO NOT DELETE"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	if err := os.Symlink(victim, stageDir); err != nil {
		t.Skipf("symlink not supported on this fs: %v", err)
	}

	m := &Manager{blobStore: bs}
	out := m.scanAndIngestAttachments(context.Background(), agentID, "m_symdir")
	if len(out) != 0 {
		t.Errorf("scan followed symlink and produced %d attachments", len(out))
	}

	// Victim's file MUST still exist — RemoveAll on a symlink
	// pointing at it would have nuked the contents.
	if _, err := os.Stat(filepath.Join(victim, "secret.txt")); err != nil {
		t.Errorf("victim file was removed via symlink follow: %v", err)
	}
}

// TestScanAndIngestAttachments_ForwarderErrorKeepsAttachment
// verifies a hub-forward failure KEEPS the attachment on the
// returned message. The codex-review-blessed drop-on-failure
// posture lost user data when a transient hub outage interrupted
// the push; the bytes still exist on the holder peer and a
// future device-switch back to here surfaces them. Better to
// render a chip whose URL 404s temporarily than to silently drop
// what the agent generated.
func TestScanAndIngestAttachments_ForwarderErrorKeepsAttachment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "")

	bs := newWiredBlob(t)
	agentID := "ag_attach_fwderr"
	stageDir := filepath.Join(agentDir(agentID), attachStagingSubpath)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "y.bin"),
		[]byte("body"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	m := &Manager{blobStore: bs}
	m.SetAttachmentForwarder(func(
		context.Context, blob.Scope, string, string, io.Reader, int64,
	) error {
		return errors.New("simulated hub down")
	})

	out := m.scanAndIngestAttachments(context.Background(), agentID, "m_fwderr")
	if len(out) != 1 {
		t.Fatalf("got %d; want 1 (forward failure must NOT silently drop the attachment)", len(out))
	}
	if out[0].Name != "y.bin" {
		t.Errorf("name = %q; want y.bin", out[0].Name)
	}

	// Local blob still exists — the holder peer keeps its
	// authoritative copy regardless of hub reachability.
	if _, err := bs.Head(blob.ScopeGlobal,
		"agents/"+agentID+"/attach/m_fwderr/y.bin"); err != nil {
		t.Errorf("local blob lost after forward failure: %v", err)
	}
}
