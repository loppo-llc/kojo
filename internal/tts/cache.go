package tts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/configdir"
)

// cacheTTL is how long a cached audio file remains valid. Sweep removes
// older files. 24h matches the "短期間" requirement.
const cacheTTL = 24 * time.Hour

// hashRequest computes a stable cache key for a synthesis request.
// Any field that affects the audio output must be part of the key.
func hashRequest(model, voice, stylePrompt, text, format string, relax bool) string {
	h := sha256.New()
	// length-prefix each field to avoid collisions across boundaries
	parts := []string{model, voice, stylePrompt, text, format, fmt.Sprintf("%t", relax)}
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s\x00", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cacheDir returns <configdir>/tts-cache. It is created on demand.
func cacheDir() (string, error) {
	dir := filepath.Join(configdir.Path(), "tts-cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// cachePath returns the on-disk path for a hash + format. The format
// double-checks against directory traversal even though the hex hash is
// already constrained to [0-9a-f].
func cachePath(hash, format string) (string, error) {
	if !isHexHash(hash) {
		return "", fmt.Errorf("invalid hash")
	}
	if Extension(format) == "bin" {
		return "", fmt.Errorf("invalid format")
	}
	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hash+"."+Extension(format)), nil
}

func isHexHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// cacheGet returns cached audio bytes if present and within TTL. Expired
// entries are removed.
func cacheGet(hash, format string) ([]byte, bool) {
	p, err := cachePath(hash, format)
	if err != nil {
		return nil, false
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, false
	}
	if time.Since(info.ModTime()) > cacheTTL {
		_ = os.Remove(p)
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	// Refresh modtime on hit so frequently-played items survive longer.
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return data, true
}

// cachePut writes audio bytes atomically. The temp file is opened with
// O_CREATE|O_EXCL via os.CreateTemp so concurrent writers for the same
// hash cannot truncate each other before rename — each gets its own
// temp file in the same directory and the last rename wins.
func cachePut(hash, format string, data []byte) error {
	p, err := cachePath(hash, format)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, hash+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// startCacheSweep removes expired entries every hour. The first sweep
// runs immediately so a long-stale cache is trimmed at startup.
var sweepOnce sync.Once

func StartCacheSweep() {
	sweepOnce.Do(func() {
		go func() {
			sweepCache()
			t := time.NewTicker(1 * time.Hour)
			defer t.Stop()
			for range t.C {
				sweepCache()
			}
		}()
	})
}

func sweepCache() {
	dir, err := cacheDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-cacheTTL)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Only touch files we own (hex hash + known extension or .tmp).
		name := e.Name()
		if !isOurCacheFile(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

func isOurCacheFile(name string) bool {
	// Stale temp files from os.CreateTemp use the pattern
	// "<hash>-<random>.tmp"; trim the suffix so we only check the
	// hash portion.
	if strings.HasSuffix(name, ".tmp") {
		stem := strings.TrimSuffix(name, ".tmp")
		if dash := strings.IndexByte(stem, '-'); dash > 0 {
			stem = stem[:dash]
		}
		return isHexHash(stem)
	}
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return false
	}
	hash, ext := name[:dot], name[dot+1:]
	if !isHexHash(hash) {
		return false
	}
	switch ext {
	case "opus", "mp3", "wav":
		return true
	}
	return false
}
