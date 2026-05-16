package secretcrypto

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// KEKFileName is the canonical name of the on-disk KEK file under the
// kojo auth directory. The file holds raw 32 bytes; ownership and
// permissions are 0600.
const KEKFileName = "kek.bin"

// LoadOrCreateKEK reads the KEK at <dir>/KEKFileName, or creates a new
// random one with mode 0600 if absent. Returns the raw 32 bytes.
//
// Permission paranoia: if an existing file is world- or group-readable
// we refuse to load it — a leaked KEK is a fleet-wide compromise.
// The fix is documented in the returned error so an operator can
// chmod 0600 and retry.
func LoadOrCreateKEK(dir string) ([]byte, error) {
	if dir == "" {
		return nil, errors.New("secretcrypto.LoadOrCreateKEK: dir required")
	}
	path := filepath.Join(dir, KEKFileName)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) != KEKSize {
			return nil, fmt.Errorf("secretcrypto.LoadOrCreateKEK: %s is %d bytes, want %d (file corrupted?)", path, len(data), KEKSize)
		}
		if err := assertSecretFilePerms(path); err != nil {
			return nil, err
		}
		return data, nil
	case errors.Is(err, fs.ErrNotExist):
		return generateKEKAt(dir, path)
	default:
		return nil, fmt.Errorf("secretcrypto.LoadOrCreateKEK: read %s: %w", path, err)
	}
}

func generateKEKAt(dir, path string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secretcrypto.LoadOrCreateKEK: mkdir: %w", err)
	}
	kek := make([]byte, KEKSize)
	if _, err := io.ReadFull(rand.Reader, kek); err != nil {
		return nil, fmt.Errorf("secretcrypto.LoadOrCreateKEK: random: %w", err)
	}
	// Atomic write: temp + chmod + rename. fsync the parent dir so a
	// crash between rename and a later read doesn't leave a torn file.
	tmp, err := os.CreateTemp(dir, KEKFileName+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("secretcrypto.LoadOrCreateKEK: tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(kek); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("secretcrypto.LoadOrCreateKEK: write: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("secretcrypto.LoadOrCreateKEK: chmod: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("secretcrypto.LoadOrCreateKEK: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("secretcrypto.LoadOrCreateKEK: close: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return nil, fmt.Errorf("secretcrypto.LoadOrCreateKEK: rename: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return kek, nil
}

// assertSecretFilePerms refuses to load a KEK file that is readable by
// anyone other than the owner. The mode bits Go surfaces on Windows
// are derived from NTFS ACL heuristics (read-only attribute mapped to
// 0444 vs 0666 etc.), not from POSIX semantics — a tightly ACLed file
// can still report 0666, and chmod is a no-op for the parts an
// operator would care about. Skip the check on Windows entirely; on
// Unix demand owner-only as designed.
func assertSecretFilePerms(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("secretcrypto: stat: %w", err)
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return fmt.Errorf("secretcrypto: %s has mode %#o; expected 0600. fix with `chmod 600 %s`", path, mode, path)
	}
	return nil
}
