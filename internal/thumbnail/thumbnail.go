// Package thumbnail generates and caches low-resolution JPEG thumbnails for
// images on disk. Used by the file-browser endpoints to keep image grids
// snappy — the original raw bytes are too large to render dozens at once on
// a phone over a Tailscale link.
//
// Cache layout: <TempDir>/kojo/thumb/<sha256(absPath|modtimeNano|filesize|size)>.jpg
//
// The cache key includes modtime AND file size so an in-place edit
// invalidates the thumbnail automatically even when the editor preserves
// modtime (rare, but cheap insurance). Cache entries older than
// cacheMaxAge are swept by PurgeOld — invoke that periodically from the
// owning process; otherwise the OS reclaims tmpdir on reboot.
package thumbnail

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	// Image format decoders. We rely on these registrations so that
	// image.DecodeConfig can sniff the header before we dispatch to a
	// format-specific decoder. Calling png.Decode etc. directly (rather
	// than image.Decode) keeps the decoder set explicit — adding a new
	// format is an opt-in change here, not a side-effect of an import.
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
	"golang.org/x/sync/singleflight"
)

// ErrUnsupportedFormat is returned when the source can't be decoded as an
// image. Callers should map this to HTTP 415.
var (
	ErrUnsupportedFormat = errors.New("unsupported image format")
	// ErrSourceTooLarge guards against decode-bomb DoS. Callers should map
	// to HTTP 413.
	ErrSourceTooLarge = errors.New("source image exceeds size budget")
)

const (
	// MinSize / MaxSize bound the requested thumbnail edge (the longer
	// side after preserve-aspect scaling).
	MinSize = 16
	MaxSize = 1024
	// DefaultSize is what handlers use when no `size` query arg is given.
	// Chosen for a 2x-DPR 150-px grid tile.
	DefaultSize = 256

	jpegQuality = 82

	// Resource budgets. Decoding is the expensive step — a 100-MP PNG
	// allocates ~400 MB of RGBA. Reject anything wildly larger than what
	// a phone or screenshot tool emits.
	maxFileBytes  = 50 * 1024 * 1024 // 50 MB on-disk
	maxSourcePix  = 64 * 1024 * 1024 // 64 MP (e.g. 8192×8192)
	cacheMaxAge   = 7 * 24 * time.Hour
	purgeInterval = 6 * time.Hour
)

// singleflight coalesces concurrent generation of the same thumbnail —
// the keyspace is bounded by (file × size) and entries are removed when
// the in-flight call completes, so memory stays flat under load.
var gen singleflight.Group

// ClampSize returns size clamped to [MinSize, MaxSize], or DefaultSize when
// size <= 0. Handlers should call this before passing user input through.
func ClampSize(size int) int {
	if size <= 0 {
		return DefaultSize
	}
	if size < MinSize {
		return MinSize
	}
	if size > MaxSize {
		return MaxSize
	}
	return size
}

// CacheDir returns the directory where thumbnails are stored. Created on
// demand by Generate.
func CacheDir() string {
	return filepath.Join(os.TempDir(), "kojo", "thumb")
}

// Generate returns the absolute path to a cached JPEG thumbnail for
// srcPath at the requested size (longer edge). srcPath must already be a
// validated, absolute path — this package does no access control.
//
// Returns ErrUnsupportedFormat when the source can't be decoded, or
// ErrSourceTooLarge when the source exceeds the byte/pixel budget.
func Generate(srcPath string, size int) (string, error) {
	size = ClampSize(size)

	info, err := os.Stat(srcPath)
	if err != nil {
		return "", err
	}
	// Only regular files. Symlinks have already been resolved by the
	// access-control layer; FIFOs / devices in a temp dir would block
	// the decoder indefinitely.
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file")
	}
	if info.Size() > maxFileBytes {
		return "", ErrSourceTooLarge
	}

	key := cacheKey(srcPath, info.ModTime().UnixNano(), info.Size(), size)
	dir := CacheDir()
	cachePath := filepath.Join(dir, key+".jpg")

	// Fast path: cache hit. Bump mtime on hit so PurgeOld treats hot
	// entries as LRU rather than evicting them by absolute age.
	if st, err := os.Stat(cachePath); err == nil && st.Size() > 0 {
		now := time.Now()
		_ = os.Chtimes(cachePath, now, now)
		return cachePath, nil
	}

	// singleflight: dedupe concurrent generation for the same key. The
	// internal map drops the entry as soon as Do returns.
	v, err, _ := gen.Do(key, func() (interface{}, error) {
		// Re-check after coalescing — another goroutine may have already
		// written the file before we got the slot.
		if st, err := os.Stat(cachePath); err == nil && st.Size() > 0 {
			return cachePath, nil
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("thumb cache mkdir: %w", err)
		}
		if err := generateOnce(srcPath, cachePath, dir, size); err != nil {
			return "", err
		}
		return cachePath, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func generateOnce(srcPath, cachePath, dir string, size int) error {
	src, err := decodeImage(srcPath)
	if err != nil {
		return err
	}
	dst := resize(src, size)

	// Write to a tmp file in the same dir then rename, so concurrent
	// readers never see a partially-written JPEG.
	tmp, err := os.CreateTemp(dir, "thumb-*.jpg.tmp")
	if err != nil {
		return fmt.Errorf("thumb tmpfile: %w", err)
	}
	tmpName := tmp.Name()
	if err := jpeg.Encode(tmp, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("thumb encode: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("thumb close: %w", err)
	}
	if err := os.Rename(tmpName, cachePath); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("thumb rename: %w", err)
	}
	return nil
}

// decodeImage opens srcPath, checks the declared dimensions against the
// pixel budget, then dispatches to the format-specific decoder. We avoid
// image.Decode's blank-import auto-registration so callers can't smuggle a
// future format past the gate.
func decodeImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Sniff the header for DecodeConfig — this reads only enough bytes
	// for dimensions, so a hostile file can't force a full allocation
	// here.
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > int64(maxSourcePix) {
		return nil, ErrSourceTooLarge
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek: %w", err)
	}
	var (
		img    image.Image
		derr   error
	)
	switch format {
	case "png":
		img, derr = png.Decode(f)
	case "jpeg":
		img, derr = jpeg.Decode(f)
	case "gif":
		// Decode returns only the first frame — sufficient for a static
		// thumbnail and avoids the memory cost of a full multi-frame
		// decode on animated GIFs.
		img, derr = gif.Decode(f)
	case "webp":
		img, derr = webp.Decode(f)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFormat, format)
	}
	if derr != nil {
		// A header that parses but a body that doesn't is, from the
		// caller's perspective, the same as an unsupported format: the
		// thumbnail can't be made. Map to 415 rather than 500 so the UI
		// can fall back to the raw image (or to a generic icon).
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedFormat, derr)
	}
	return img, nil
}

// resize scales src so its longer edge equals maxEdge, preserving aspect.
// Images already smaller than maxEdge are re-encoded at original size —
// callers still benefit from the JPEG re-encode (PNG screenshots compress
// 5–10x as JPEGs).
func resize(src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= 0 || srcH <= 0 {
		return src
	}
	dstW, dstH := srcW, srcH
	longer := srcW
	if srcH > longer {
		longer = srcH
	}
	if longer > maxEdge {
		// Floor division is fine — we don't need sub-pixel accuracy at
		// thumbnail scale.
		dstW = srcW * maxEdge / longer
		dstH = srcH * maxEdge / longer
		if dstW < 1 {
			dstW = 1
		}
		if dstH < 1 {
			dstH = 1
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	// CatmullRom: slower than ApproxBiLinear but visibly cleaner on
	// photos and text-bearing screenshots. At <=1024-px output the cost
	// is acceptable.
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

func cacheKey(path string, modtimeNano, fileSize int64, size int) string {
	h := sha256.New()
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(modtimeNano, 10)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(fileSize, 10)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(size)))
	return hex.EncodeToString(h.Sum(nil))
}

// IsSupportedExt reports whether ext (lower-case, leading dot) is a format
// we know how to thumbnail. Handlers can short-circuit obvious non-images
// before opening the file. Match the formats handled in decodeImage.
func IsSupportedExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// PurgeOld removes cache entries whose mtime is older than cacheMaxAge.
// Safe to call concurrently with Generate — the worst case is that a
// just-written entry races a sweep and gets re-generated on the next
// request. Returns the number of files removed.
func PurgeOld() (int, error) {
	dir := CacheDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-cacheMaxAge)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// StartPurger runs PurgeOld every purgeInterval until ctx-style done
// channel closes. Callers should invoke this once at server start. The
// function returns immediately; the loop runs in its own goroutine.
func StartPurger(done <-chan struct{}) {
	// Sweep once at startup so a long-running cache doesn't grow
	// unbounded across restarts that happen faster than the interval.
	_, _ = PurgeOld()
	go func() {
		t := time.NewTicker(purgeInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_, _ = PurgeOld()
			}
		}
	}()
}

