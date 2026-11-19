package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/filebrowser"
	"github.com/loppo-llc/kojo/internal/peer"
)

func TestHandleUploadStreamsPrivateFileAtomically(t *testing.T) {
	originalUploadDir := uploadDir
	uploadDir = t.TempDir()
	t.Cleanup(func() { uploadDir = originalUploadDir })

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "secret.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{peerID: &peer.Identity{DeviceID: "peer-a"}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	s.handleUpload(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Path   string `json:"path"`
		PeerID string `json:"peerId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.PeerID != "peer-a" {
		t.Fatalf("peerId=%q", out.PeerID)
	}
	info, err := os.Stat(out.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" || bytes.HasPrefix([]byte(entry.Name()), []byte(".upload-")) {
			t.Fatalf("temporary upload remained: %s", entry.Name())
		}
	}
}

func TestValidateAttachmentsRejectsDifferentHolder(t *testing.T) {
	originalUploadDir := uploadDir
	uploadDir = t.TempDir()
	t.Cleanup(func() { uploadDir = originalUploadDir })
	path := filepath.Join(uploadDir, "attachment.txt")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	att := agent.MessageAttachment{Path: path, PeerID: "old-holder"}
	if got := validateAttachmentsForPeer([]agent.MessageAttachment{att}, "new-holder"); len(got) != 0 {
		t.Fatalf("different-holder attachment accepted: %#v", got)
	}
	if got := validateAttachmentsForPeer([]agent.MessageAttachment{att}, "old-holder"); len(got) != 1 {
		t.Fatalf("matching-holder attachment rejected: %#v", got)
	}
}

func TestHandleWriteFileIfMatch(t *testing.T) {
	s := &Server{files: filebrowser.New(nil)}
	p := filepath.Join(t.TempDir(), "memo.txt")
	if err := os.WriteFile(p, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	put := func(ifMatch, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/files/content?path="+url.QueryEscape(p), bytes.NewBufferString(body))
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		rr := httptest.NewRecorder()
		s.handleWriteFile(rr, req)
		return rr
	}
	if rr := put("", `{"content":"x"}`); rr.Code != http.StatusPreconditionRequired {
		t.Fatalf("no If-Match: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := put(`*`, `{"content":"x"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("wildcard: status=%d", rr.Code)
	}
	if rr := put(`"deadbeef"`, `{"content":"x"}`); rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale: status=%d body=%s", rr.Code, rr.Body.String())
	}
	etag := filebrowser.ContentETag([]byte("before"))
	rr := put(`"`+etag+`"`, `{"content":"after"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("save: status=%d body=%s", rr.Code, rr.Body.String())
	}
	want := filebrowser.ContentETag([]byte("after"))
	if rr.Header().Get("ETag") != `"`+want+`"` {
		t.Fatalf("ETag header=%q", rr.Header().Get("ETag"))
	}
	var out struct {
		Content string `json:"content"`
		ETag    string `json:"etag"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Content != "after" || out.ETag != want {
		t.Fatalf("out=%+v", out)
	}
	if got, _ := os.ReadFile(p); string(got) != "after" {
		t.Fatalf("disk=%q", got)
	}
}

func TestPeerProxyIncludesFileSave(t *testing.T) {
	if !isPeerProxyPath("/api/v1/files/content") {
		t.Fatal("file save route is not proxied to the session's peer")
	}
}
