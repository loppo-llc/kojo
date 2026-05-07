package tts

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestPCMToWAVHeader(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
	wav := pcmToWAV(pcm, 24000)

	if len(wav) != 44+len(pcm) {
		t.Fatalf("unexpected length: got %d, want %d", len(wav), 44+len(pcm))
	}
	if !bytes.Equal(wav[0:4], []byte("RIFF")) {
		t.Errorf("missing RIFF magic")
	}
	if !bytes.Equal(wav[8:12], []byte("WAVE")) {
		t.Errorf("missing WAVE magic")
	}
	if !bytes.Equal(wav[12:16], []byte("fmt ")) {
		t.Errorf("missing fmt chunk")
	}
	if got := binary.LittleEndian.Uint32(wav[24:28]); got != 24000 {
		t.Errorf("sample rate = %d, want 24000", got)
	}
	if got := binary.LittleEndian.Uint16(wav[22:24]); got != 1 {
		t.Errorf("num channels = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint16(wav[34:36]); got != 16 {
		t.Errorf("bits per sample = %d, want 16", got)
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != uint32(len(pcm)) {
		t.Errorf("data size = %d, want %d", got, len(pcm))
	}
	if !bytes.Equal(wav[44:], pcm) {
		t.Errorf("payload mismatch")
	}
}

func TestHashRequestStable(t *testing.T) {
	a := hashRequest("m", "v", "s", "t", "opus", true)
	b := hashRequest("m", "v", "s", "t", "opus", true)
	if a != b {
		t.Fatalf("hash not stable: %s vs %s", a, b)
	}
	if c := hashRequest("m", "v", "s", "t", "opus", false); c == a {
		t.Errorf("relax flag should change hash")
	}
	if c := hashRequest("m", "v", "s", "t", "wav", true); c == a {
		t.Errorf("format should change hash")
	}
	// Adversarial: ensure length-prefixing avoids boundary collisions.
	x := hashRequest("ab", "cd", "", "", "wav", true)
	y := hashRequest("a", "bcd", "", "", "wav", true)
	if x == y {
		t.Errorf("boundary collision: hashes should differ")
	}
}

func TestIsHexHash(t *testing.T) {
	good := strings.Repeat("a", 64)
	if !isHexHash(good) {
		t.Errorf("64 hex chars rejected")
	}
	if isHexHash(strings.Repeat("a", 63)) {
		t.Errorf("63 chars accepted")
	}
	if isHexHash(strings.Repeat("g", 64)) {
		t.Errorf("non-hex char accepted")
	}
	if isHexHash(strings.Repeat("A", 64)) {
		t.Errorf("uppercase should be rejected (we encode lowercase)")
	}
}

func TestIsValidVoiceModel(t *testing.T) {
	if !IsValidVoice("Kore") {
		t.Errorf("Kore should be valid")
	}
	if IsValidVoice("") {
		t.Errorf("empty voice should be invalid")
	}
	if IsValidVoice("Bogus") {
		t.Errorf("Bogus should be invalid")
	}
	if !IsValidModel(DefaultModel) {
		t.Errorf("DefaultModel should be valid")
	}
	if IsValidModel("gpt-5") {
		t.Errorf("gpt-5 should be invalid")
	}
}

func TestSanitize(t *testing.T) {
	if got := Sanitize("hello"); got != "hello" {
		t.Errorf("plain text mangled: %q", got)
	}
	in := "see ```go\nfmt.Println(1)\n``` for details"
	got := Sanitize(in)
	if strings.Contains(got, "fmt.Println") {
		t.Errorf("code block leaked: %q", got)
	}
	if !strings.Contains(got, "コード省略") {
		t.Errorf("code block placeholder missing: %q", got)
	}
	urlIn := "see https://example.com/x for more"
	urlOut := Sanitize(urlIn)
	if strings.Contains(urlOut, "example.com") {
		t.Errorf("URL leaked: %q", urlOut)
	}
	long := strings.Repeat("あ", MaxChars+50)
	clipped := Sanitize(long)
	if r := []rune(clipped); len(r) > MaxChars+1 {
		t.Errorf("clip failed: got %d runes", len(r))
	}
}

func TestExtensionMime(t *testing.T) {
	for _, f := range []string{"opus", "mp3", "wav"} {
		if Extension(f) == "bin" {
			t.Errorf("%s extension fallback hit", f)
		}
		if MimeType(f) == "application/octet-stream" {
			t.Errorf("%s mime fallback hit", f)
		}
	}
	if Extension("aac") != "bin" {
		t.Errorf("aac extension should be bin, got %q", Extension("aac"))
	}
}
