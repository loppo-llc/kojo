package tts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// ffmpegBin caches the resolved ffmpeg executable path. Empty string means
// "not present" (cached negative result).
var (
	ffmpegBin   string
	ffmpegOnce  sync.Once
	ffmpegFound bool
)

// FFmpegAvailable returns whether an ffmpeg binary is on PATH. The lookup
// is performed once and cached for the process lifetime.
func FFmpegAvailable() bool {
	ffmpegOnce.Do(func() {
		if path, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegBin = path
			ffmpegFound = true
		}
	})
	return ffmpegFound
}

// SupportedFormats reports which output container formats kojo can emit
// for a given environment. WAV is always available; opus and mp3 require
// ffmpeg.
func SupportedFormats() []string {
	if FFmpegAvailable() {
		return []string{"opus", "mp3", "wav"}
	}
	return []string{"wav"}
}

// MimeType returns the HTTP Content-Type for a supported format.
func MimeType(format string) string {
	switch format {
	case "opus":
		return "audio/ogg; codecs=opus"
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

// Extension returns the file extension (no leading dot) for a format.
func Extension(format string) string {
	switch format {
	case "opus":
		return "opus"
	case "mp3", "wav":
		return format
	default:
		return "bin"
	}
}

// EncodeFFmpeg pipes raw PCM (16-bit little-endian mono at sampleRate Hz)
// through ffmpeg and returns the encoded bytes in the requested container.
//
// format must be "opus" (Ogg/Opus 24 kbps voip-tuned) or "mp3" (64 kbps
// libmp3lame). For "wav" use pcmToWAV directly — it does not require
// ffmpeg.
//
// A 30 s hard timeout protects against a stuck subprocess; the goroutine
// piping stdin is bounded by ctx as well.
func EncodeFFmpeg(ctx context.Context, format string, pcm []byte, sampleRate uint32) ([]byte, error) {
	if !FFmpegAvailable() {
		return nil, errors.New("ffmpeg not available")
	}
	var args []string
	switch format {
	case "opus":
		args = []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-f", "s16le", "-ar", fmt.Sprint(sampleRate), "-ac", "1", "-i", "pipe:0",
			"-c:a", "libopus", "-b:a", "24k", "-application", "voip",
			"-f", "ogg", "pipe:1",
		}
	case "mp3":
		args = []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-f", "s16le", "-ar", fmt.Sprint(sampleRate), "-ac", "1", "-i", "pipe:0",
			"-c:a", "libmp3lame", "-b:a", "64k",
			"-f", "mp3", "pipe:1",
		}
	default:
		return nil, fmt.Errorf("unsupported ffmpeg format: %q", format)
	}

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, ffmpegBin, args...)
	cmd.Stdin = bytes.NewReader(pcm)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		stderr := errBuf.String()
		if stderr != "" {
			return nil, fmt.Errorf("ffmpeg %s encode failed: %w: %s", format, err, stderr)
		}
		return nil, fmt.Errorf("ffmpeg %s encode failed: %w", format, err)
	}
	return out.Bytes(), nil
}
