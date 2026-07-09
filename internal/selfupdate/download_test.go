package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"runtime"
	"strings"
	"testing"
)

func TestDownloadAndExtract_tarGzHappy(t *testing.T) {
	t.Parallel()
	payload := []byte("fake-kojo-binary-v1")
	archive := buildTarGz(t, "kojo", payload)
	name := AssetName("linux", "amd64")
	sum := sha256Hex(archive)

	srv, rel := releaseServer(t, map[string][]byte{
		name:               archive,
		ChecksumsAssetName: []byte(sum + "  " + name + "\n"),
	})
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	c := testClient(srv)
	bin, err := c.DownloadAndExtract(context.Background(), rel, "linux", "amd64", dir)
	if err != nil {
		t.Fatalf("DownloadAndExtract: %v", err)
	}
	defer os.Remove(bin)

	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(bin)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Fatalf("mode = %v, want executable bits", fi.Mode())
		}
	}
}

func TestDownloadAndExtract_zipHappy(t *testing.T) {
	t.Parallel()
	payload := []byte("fake-kojo.exe")
	archive := buildZip(t, "kojo.exe", payload)
	name := AssetName("windows", "amd64")
	sum := sha256Hex(archive)

	srv, rel := releaseServer(t, map[string][]byte{
		name:               archive,
		ChecksumsAssetName: []byte(sum + "  " + name + "\n"),
	})
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	c := testClient(srv)
	bin, err := c.DownloadAndExtract(context.Background(), rel, "windows", "amd64", dir)
	if err != nil {
		t.Fatalf("DownloadAndExtract: %v", err)
	}
	defer os.Remove(bin)

	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestDownloadAndExtract_leadingDirMember(t *testing.T) {
	t.Parallel()
	payload := []byte("nested-binary")
	// Member stored as dist/kojo — path.Base must still match.
	archive := buildTarGz(t, "dist/kojo", payload)
	name := AssetName("darwin", "arm64")
	sum := sha256Hex(archive)

	srv, rel := releaseServer(t, map[string][]byte{
		name:               archive,
		ChecksumsAssetName: []byte(sum + "  " + name + "\n"),
	})
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	c := testClient(srv)
	bin, err := c.DownloadAndExtract(context.Background(), rel, "darwin", "arm64", dir)
	if err != nil {
		t.Fatalf("DownloadAndExtract: %v", err)
	}
	defer os.Remove(bin)

	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestDownloadAndExtract_checksumMismatch(t *testing.T) {
	t.Parallel()
	archive := buildTarGz(t, "kojo", []byte("body"))
	name := AssetName("linux", "amd64")
	// Deliberately wrong digest.
	bad := strings.Repeat("0", 64)

	srv, rel := releaseServer(t, map[string][]byte{
		name:               archive,
		ChecksumsAssetName: []byte(bad + "  " + name + "\n"),
	})
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	c := testClient(srv)
	_, err := c.DownloadAndExtract(context.Background(), rel, "linux", "amd64", dir)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
}

func TestDownloadAndExtract_missingAsset(t *testing.T) {
	t.Parallel()
	// Release has checksums but no platform archive — the brief
	// post-publish window before CI uploads assets.
	srv, rel := releaseServer(t, map[string][]byte{
		ChecksumsAssetName: []byte(strings.Repeat("a", 64) + "  other.tar.gz\n"),
	})
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	c := testClient(srv)
	_, err := c.DownloadAndExtract(context.Background(), rel, "linux", "amd64", dir)
	if !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("err = %v, want ErrAssetNotFound", err)
	}
}

func TestDownloadAndExtract_missingChecksumsAsset(t *testing.T) {
	t.Parallel()
	archive := buildTarGz(t, "kojo", []byte("x"))
	name := AssetName("linux", "amd64")
	srv, rel := releaseServer(t, map[string][]byte{
		name: archive,
	})
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	c := testClient(srv)
	_, err := c.DownloadAndExtract(context.Background(), rel, "linux", "amd64", dir)
	if !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("err = %v, want ErrAssetNotFound", err)
	}
}

// --- helpers ---

func testClient(srv *httptest.Server) *Client {
	c := NewClient("v0.0.0-test")
	c.HTTPClient = srv.Client()
	// Downloads hit absolute URLs on srv; BaseURL unused here.
	return c
}

// releaseServer serves the given assets at /files/<name> and returns
// a Release whose BrowserDownloadURL points at those paths.
func releaseServer(t *testing.T, files map[string][]byte) (*httptest.Server, *Release) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(r.URL.Path)
		body, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)

	rel := &Release{
		TagName: "v9.9.9",
		HTMLURL: "https://example.com/releases/v9.9.9",
	}
	for name, body := range files {
		rel.Assets = append(rel.Assets, Asset{
			Name:               name,
			Size:               int64(len(body)),
			BrowserDownloadURL: srv.URL + "/files/" + name,
		})
	}
	return srv, rel
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func buildTarGz(t *testing.T, member string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: member,
		Mode: 0o755,
		Size: int64(len(payload)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildZip(t *testing.T, member string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(member)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
