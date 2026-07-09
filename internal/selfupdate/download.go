package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
)

// maxArchiveBytes caps both the downloaded archive and the extracted
// binary. Release assets are tens of MiB; anything past this is either
// a misconfigured mirror or a zip bomb, and we refuse either way.
const maxArchiveBytes = 500 << 20 // 500 MiB

// maxChecksumsBytes caps checksums.txt. The file is a few lines of
// hex digests; 1 MiB is already generous.
const maxChecksumsBytes = 1 << 20 // 1 MiB

// ErrAssetNotFound means the release has no archive (or no
// checksums.txt) for the requested platform. This is expected in the
// brief window after a tag is published and before CI uploads assets.
var ErrAssetNotFound = errors.New("selfupdate: asset not found for platform")

// ErrChecksumMismatch means the downloaded archive's SHA-256 did not
// match the entry in checksums.txt. Treat as a hard failure: do not
// install the bytes.
var ErrChecksumMismatch = errors.New("selfupdate: checksum mismatch")

// DownloadAndExtract fetches the platform archive for rel, verifies
// its SHA-256 against checksums.txt, and extracts the single kojo
// binary into destDir. The returned path is a temp file in destDir
// (mode 0755); the caller is responsible for swapping it into place
// and cleaning it up. The intermediate archive is always removed.
func (c *Client) DownloadAndExtract(ctx context.Context, rel *Release, goos, goarch, destDir string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("nil selfupdate client")
	}
	if rel == nil {
		return "", fmt.Errorf("nil release")
	}
	name := AssetName(goos, goarch)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrAssetNotFound, name)
	}
	sumsAsset, ok := rel.FindAsset(ChecksumsAssetName)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrAssetNotFound, ChecksumsAssetName)
	}

	wantSum, err := c.downloadChecksum(ctx, sumsAsset.BrowserDownloadURL, name)
	if err != nil {
		return "", err
	}

	archivePath, gotSum, err := c.downloadArchive(ctx, asset.BrowserDownloadURL, destDir)
	if err != nil {
		return "", err
	}
	// Always drop the archive once we are done with it — success
	// leaves only the extracted binary; failure leaves nothing.
	defer os.Remove(archivePath)

	if !strings.EqualFold(gotSum, wantSum) {
		return "", fmt.Errorf("%w: got %s want %s", ErrChecksumMismatch, gotSum, wantSum)
	}

	binPath, err := extractBinary(archivePath, goos, destDir)
	if err != nil {
		return "", err
	}
	return binPath, nil
}

// downloadChecksum GETs checksums.txt and returns the hex digest for
// the named archive. Missing entry is ErrAssetNotFound: the release
// listed the archive but checksums.txt has no line for it.
func (c *Client) downloadChecksum(ctx context.Context, url, archiveName string) (string, error) {
	resp, err := c.get(ctx, url)
	if err != nil {
		return "", fmt.Errorf("download checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("download checksums: status %d", resp.StatusCode)
	}
	sums, err := parseChecksums(io.LimitReader(resp.Body, maxChecksumsBytes))
	if err != nil {
		return "", fmt.Errorf("parse checksums: %w", err)
	}
	sum, ok := sums[archiveName]
	if !ok {
		return "", fmt.Errorf("%w: checksum entry for %s", ErrAssetNotFound, archiveName)
	}
	return sum, nil
}

// downloadArchive streams the archive into a temp file under destDir
// while hashing. LimitReader is set to maxArchiveBytes+1 so a single
// extra byte is enough to detect overflow without buffering the body.
func (c *Client) downloadArchive(ctx context.Context, url, destDir string) (path string, hexSum string, err error) {
	resp, err := c.get(ctx, url)
	if err != nil {
		return "", "", fmt.Errorf("download archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", "", fmt.Errorf("download archive: status %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(destDir, ".kojo-archive-*")
	if err != nil {
		return "", "", fmt.Errorf("create archive temp: %w", err)
	}
	tmpName := tmp.Name()
	// On any failure after create, drop the partial file. Success
	// returns the path and the caller owns cleanup.
	success := false
	defer func() {
		if !success {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	n, err := io.Copy(tmp, io.TeeReader(io.LimitReader(resp.Body, maxArchiveBytes+1), h))
	if err != nil {
		return "", "", fmt.Errorf("download archive body: %w", err)
	}
	if n > maxArchiveBytes {
		return "", "", fmt.Errorf("archive exceeds %d bytes", maxArchiveBytes)
	}
	if err := tmp.Close(); err != nil {
		return "", "", fmt.Errorf("close archive temp: %w", err)
	}
	success = true
	return tmpName, hex.EncodeToString(h.Sum(nil)), nil
}

// parseChecksums reads sha256sum-format lines:
//
//	<64-hex><spaces><filename>
//
// Binary-mode markers ("*") before the filename are stripped. Lines
// that do not look like digests are skipped rather than failing the
// whole file — release notes occasionally get pasted in by mistake.
func parseChecksums(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)
	sc := bufio.NewScanner(r)
	// Default 64 KiB is fine; digests are one short line each.
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum, name := fields[0], fields[1]
		if len(sum) != 64 || !isHex(sum) {
			continue
		}
		name = strings.TrimPrefix(name, "*")
		// Some tools emit "./kojo_..." — match on the base name too
		// would be wrong here: AssetName is the exact key we look up.
		out[name] = sum
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// extractBinary opens the archive and writes the single member named
// "kojo" (tar.gz) or "kojo.exe" (zip) into a temp file under destDir.
// Members stored under a leading directory are accepted via path.Base
// so CI packaging that nests the binary still works.
func extractBinary(archivePath, goos, destDir string) (string, error) {
	want := "kojo"
	if goos == "windows" {
		want = "kojo.exe"
	}
	if goos == "windows" {
		return extractZipMember(archivePath, want, destDir)
	}
	return extractTarGzMember(archivePath, want, destDir)
}

func extractTarGzMember(archivePath, wantBase, destDir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar: %w", err)
		}
		// Skip non-regular members (dirs, links). FileInfo maps both
		// TypeReg and the legacy TypeRegA ('\x00') to a regular mode.
		if !hdr.FileInfo().Mode().IsRegular() {
			continue
		}
		if path.Base(hdr.Name) != wantBase {
			continue
		}
		return writeBinaryTemp(destDir, tr)
	}
	return "", fmt.Errorf("archive missing member %q", wantBase)
}

func extractZipMember(archivePath, wantBase, destDir string) (string, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("zip: %w", err)
	}
	defer r.Close()
	for _, zf := range r.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		if path.Base(zf.Name) != wantBase {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return "", err
		}
		bin, err := writeBinaryTemp(destDir, rc)
		rc.Close()
		return bin, err
	}
	return "", fmt.Errorf("archive missing member %q", wantBase)
}

// writeBinaryTemp streams src into a 0755 temp file under destDir,
// capping at maxArchiveBytes as a zip-bomb guard on the expanded size.
func writeBinaryTemp(destDir string, src io.Reader) (string, error) {
	tmp, err := os.CreateTemp(destDir, ".kojo-bin-*")
	if err != nil {
		return "", fmt.Errorf("create binary temp: %w", err)
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	n, err := io.Copy(tmp, io.LimitReader(src, maxArchiveBytes+1))
	if err != nil {
		return "", fmt.Errorf("extract binary: %w", err)
	}
	if n > maxArchiveBytes {
		return "", fmt.Errorf("extracted binary exceeds %d bytes", maxArchiveBytes)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return "", err
	}
	success = true
	return tmpName, nil
}
