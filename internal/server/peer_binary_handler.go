package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strconv"
)

// hubBinaryPath resolves the on-disk binary this Hub advertises in
// hub-info and streams from GET /api/v1/peers/binary. It is a package
// var (default os.Executable) so tests can point it at a fixture file
// without corrupting the test process binary.
var hubBinaryPath = os.Executable

// openHubBinary opens the Hub executable, returns a memoized SHA-256
// hex digest for its current (path, size, mtime), and leaves the file
// positioned at offset 0 so the caller can stream the same bytes that
// were hashed. Callers must Close the file.
//
// The memo retries after a prior failure (hubBinaryOK stays false) and
// invalidates when size or mtime changes — a rebuild or self-update
// can rewrite the binary without restarting this process.
func (s *Server) openHubBinary() (*os.File, string, int64, error) {
	path, err := hubBinaryPath()
	if err != nil {
		return nil, "", 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, "", 0, err
	}
	size := fi.Size()
	mtime := fi.ModTime()

	s.hubBinaryMu.Lock()
	defer s.hubBinaryMu.Unlock()

	if s.hubBinaryOK &&
		s.hubBinaryCachedPath == path &&
		s.hubBinarySize == size &&
		s.hubBinaryMtime.Equal(mtime) {
		return f, s.hubBinaryDigest, size, nil
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		s.hubBinaryOK = false
		return nil, "", 0, err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		s.hubBinaryOK = false
		return nil, "", 0, err
	}
	s.hubBinaryCachedPath = path
	s.hubBinarySize = size
	s.hubBinaryMtime = mtime
	s.hubBinaryDigest = digest
	s.hubBinaryOK = true
	return f, digest, size, nil
}

// hubBinarySHA256 returns the memoized digest of the Hub executable
// without leaving a file open. ok is false when the path cannot be
// opened or hashed; the failure is logged Warn once.
func (s *Server) hubBinarySHA256() (digest string, ok bool) {
	f, dig, _, err := s.openHubBinary()
	if err != nil {
		s.warnHubBinaryOnce(err)
		return "", false
	}
	_ = f.Close()
	return dig, true
}

// warnHubBinaryOnce logs a single Warn when the Hub binary cannot be
// hashed so hub-info omits binarySha256 without spamming every refresh.
func (s *Server) warnHubBinaryOnce(err error) {
	s.hubBinaryMu.Lock()
	defer s.hubBinaryMu.Unlock()
	if s.hubBinaryWarned {
		return
	}
	s.hubBinaryWarned = true
	if s.logger != nil {
		s.logger.Warn("hub binary digest unavailable; peer auto-update will not advertise binarySha256",
			"err", err)
	}
}

// handlePeerBinary streams this Hub's own executable for peer
// auto-update. Auth: RolePeer or RoleOwner (policy allowlist +
// requirePeerOrOwner). Header X-Kojo-Binary-SHA256 matches the body
// bytes: openHubBinary hashes and streams the same open file handle.
func (s *Server) handlePeerBinary(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePeerOrOwner(w, r); !ok {
		return
	}
	f, digest, size, err := s.openHubBinary()
	if err != nil {
		s.warnHubBinaryOnce(err)
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"hub binary unavailable")
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Kojo-Binary-SHA256", digest)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, f); err != nil {
		// Client gone mid-stream; nothing useful left to write.
		if s.logger != nil {
			s.logger.Debug("peer binary: stream interrupted", "err", err)
		}
	}
}
