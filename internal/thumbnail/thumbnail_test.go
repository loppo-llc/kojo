package thumbnail

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePNG creates an opaque w×h test image at path. Color is a
// per-pixel gradient so the JPEG encoder doesn't collapse it to a
// single block (which would hide resize bugs).
func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

func TestClampSize(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, DefaultSize},
		{-5, DefaultSize},
		{1, MinSize},
		{MinSize, MinSize},
		{500, 500},
		{MaxSize, MaxSize},
		{MaxSize + 1, MaxSize},
		{100000, MaxSize},
	}
	for _, c := range cases {
		if got := ClampSize(c.in); got != c.want {
			t.Errorf("ClampSize(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestGenerate_PNG_ResizesAndCaches(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.png")
	writePNG(t, src, 800, 600)

	out, err := Generate(src, 200)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("output missing: %v", err)
	}

	// Inspect the encoded JPEG to verify the resize math.
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	// 800x600 → longer=800, scaled to 200 ⇒ 200x150.
	if cfg.Width != 200 || cfg.Height != 150 {
		t.Errorf("dimensions = %dx%d, want 200x150", cfg.Width, cfg.Height)
	}

	// Cache hit: same call returns the same path. mtime is bumped to
	// support PurgeOld's LRU behavior so we compare file size instead of
	// modtime to confirm the file wasn't actually rewritten.
	st1, _ := os.Stat(out)
	out2, err := Generate(src, 200)
	if err != nil {
		t.Fatalf("Generate hit: %v", err)
	}
	if out2 != out {
		t.Errorf("cache key changed: %s vs %s", out, out2)
	}
	st2, _ := os.Stat(out)
	if st1.Size() != st2.Size() {
		t.Errorf("cache hit re-wrote file (size changed: %d → %d)", st1.Size(), st2.Size())
	}
}

// writeHugePNGHeader emits a PNG whose IHDR claims width × height pixels.
// The IDAT chunk is intentionally empty/garbage — we only need
// DecodeConfig to read the header. This lets us exercise the pixel-budget
// guard without allocating the actual pixel buffer (which would be
// gigabytes for the dimensions we're testing).
func writeHugePNGHeader(t *testing.T, path string, width, height uint32) {
	t.Helper()
	// PNG signature + IHDR chunk only. CRC is required for DecodeConfig.
	sig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	ihdr := make([]byte, 13)
	// width, height, big-endian
	for i, v := range []uint32{width, height} {
		ihdr[i*4+0] = byte(v >> 24)
		ihdr[i*4+1] = byte(v >> 16)
		ihdr[i*4+2] = byte(v >> 8)
		ihdr[i*4+3] = byte(v)
	}
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // color type (RGB)
	// remaining filter/interlace bytes stay zero
	chunk := func(typ string, data []byte) []byte {
		buf := make([]byte, 0, 12+len(data))
		l := uint32(len(data))
		buf = append(buf, byte(l>>24), byte(l>>16), byte(l>>8), byte(l))
		buf = append(buf, []byte(typ)...)
		buf = append(buf, data...)
		crc := crc32IEEE(append([]byte(typ), data...))
		buf = append(buf, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
		return buf
	}
	out := append([]byte{}, sig...)
	out = append(out, chunk("IHDR", ihdr)...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func crc32IEEE(data []byte) uint32 {
	// PNG uses the IEEE polynomial. hash/crc32 is the obvious dep but
	// inlining this keeps the test self-contained.
	const poly uint32 = 0xedb88320
	var crc uint32 = 0xffffffff
	for _, b := range data {
		crc ^= uint32(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ poly
			} else {
				crc >>= 1
			}
		}
	}
	return crc ^ 0xffffffff
}

func TestGenerate_RejectsHugePixelCount(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "huge.png")
	// Just claim 16k×16k = 256 MP — well past the 64 MP budget.
	writeHugePNGHeader(t, src, 16384, 16384)
	_, err := Generate(src, 256)
	if err == nil {
		t.Fatal("expected ErrSourceTooLarge")
	}
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Errorf("err = %v, want ErrSourceTooLarge", err)
	}
}

func TestGenerate_RejectsNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "dir-as-file")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := Generate(src, 100)
	if err == nil {
		t.Fatal("expected error for non-regular file")
	}
}

func TestPurgeOld_RemovesStaleEntries(t *testing.T) {
	// Generate a real entry, then back-date it past cacheMaxAge.
	dir := t.TempDir()
	src := filepath.Join(dir, "src.png")
	writePNG(t, src, 100, 100)
	out, err := Generate(src, 64)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	past := time.Now().Add(-cacheMaxAge - time.Hour)
	if err := os.Chtimes(out, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	removed, err := PurgeOld()
	if err != nil {
		t.Fatalf("PurgeOld: %v", err)
	}
	if removed < 1 {
		t.Errorf("removed = %d, want >= 1", removed)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("stale entry survived purge: %v", err)
	}
}

func TestGenerate_ModtimeInvalidatesCache(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.png")
	writePNG(t, src, 100, 100)

	out1, err := Generate(src, 64)
	if err != nil {
		t.Fatalf("first gen: %v", err)
	}

	// Bump modtime so the cache key changes.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(src, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	out2, err := Generate(src, 64)
	if err != nil {
		t.Fatalf("second gen: %v", err)
	}
	if out1 == out2 {
		t.Errorf("cache key did not change on modtime bump")
	}
}

func TestGenerate_UnsupportedFormat(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "garbage.png")
	if err := os.WriteFile(src, []byte("not actually an image"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Generate(src, 100)
	if err == nil {
		t.Fatal("expected error on garbage input")
	}
}

func TestIsSupportedExt(t *testing.T) {
	for _, ok := range []string{".png", ".jpg", ".JPEG", ".gif", ".webp"} {
		if !IsSupportedExt(ok) {
			t.Errorf("IsSupportedExt(%q) = false, want true", ok)
		}
	}
	for _, no := range []string{".heic", ".pdf", ".txt", ""} {
		if IsSupportedExt(no) {
			t.Errorf("IsSupportedExt(%q) = true, want false", no)
		}
	}
}
