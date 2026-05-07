package tts

import (
	"encoding/binary"
)

// pcmToWAV wraps raw PCM 16-bit little-endian mono samples into a minimal
// 44-byte canonical WAV (RIFF/WAVE) container so browsers can play the
// result via <audio> directly.
//
// sampleRate is the PCM sample rate (Gemini TTS returns 24000).
func pcmToWAV(pcm []byte, sampleRate uint32) []byte {
	const (
		numChannels   = uint16(1)
		bitsPerSample = uint16(16)
	)
	byteRate := sampleRate * uint32(numChannels) * uint32(bitsPerSample) / 8
	blockAlign := numChannels * bitsPerSample / 8

	out := make([]byte, 44+len(pcm))
	// RIFF header
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+len(pcm)))
	copy(out[8:12], "WAVE")
	// fmt sub-chunk
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16) // sub-chunk size
	binary.LittleEndian.PutUint16(out[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(out[22:24], numChannels)
	binary.LittleEndian.PutUint32(out[24:28], sampleRate)
	binary.LittleEndian.PutUint32(out[28:32], byteRate)
	binary.LittleEndian.PutUint16(out[32:34], blockAlign)
	binary.LittleEndian.PutUint16(out[34:36], bitsPerSample)
	// data sub-chunk
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(len(pcm)))
	copy(out[44:], pcm)
	return out
}
