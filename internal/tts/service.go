package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SynthesizeRequest is the input to Service.Synthesize. All fields are
// already sanitized at this layer — callers pass agent-derived
// configuration directly.
type SynthesizeRequest struct {
	Model       string // "" = DefaultModel
	Voice       string // "" = DefaultVoice
	StylePrompt string // "" = DefaultStylePrompt
	Text        string // raw, will be sanitized inside Synthesize
	Format      string // "opus" | "mp3" | "wav"
}

// SynthesizeResult is what Service.Synthesize returns. AudioBytes is the
// fully encoded payload ready to be served as MimeType(Format). Hash is
// the cache key (hex sha256) so handlers can build the audio URL.
type SynthesizeResult struct {
	Hash       string
	Format     string
	AudioBytes []byte
	Cached     bool
}

// Service performs Gemini TTS synthesis and caches results on disk.
//
// The API key is fetched lazily via getAPIKey on every request, so a key
// rotation in the credential store takes effect on the next call without
// needing to rewire the service.
type Service struct {
	getAPIKey func() (string, error)
	http      *http.Client
}

// NewService constructs a Service. apiKeyFn must return the current
// Gemini Developer API key.
func NewService(apiKeyFn func() (string, error)) *Service {
	return &Service{
		getAPIKey: apiKeyFn,
		http: &http.Client{
			// 60 s covers Gemini TTS for the largest accepted input.
			Timeout: 60 * time.Second,
		},
	}
}

// Synthesize is the main entry point. The flow is:
//  1. Sanitize and validate input.
//  2. Hash the request and return the cached file if present.
//  3. Call Gemini :generateContent with safetySettings=OFF.
//  4. Decode the inline-data audio (raw 24 kHz LE16 PCM).
//  5. Encode to the requested container (ffmpeg for opus/mp3, in-process
//     WAV header for wav).
//  6. Persist to cache and return.
func (s *Service) Synthesize(ctx context.Context, req SynthesizeRequest) (*SynthesizeResult, error) {
	if s == nil {
		return nil, errors.New("tts service not initialized")
	}
	model := req.Model
	if model == "" {
		model = DefaultModel
	}
	if !IsValidModel(model) {
		return nil, fmt.Errorf("invalid model: %q", model)
	}
	voice := req.Voice
	if voice == "" {
		voice = DefaultVoice
	}
	if !IsValidVoice(voice) {
		return nil, fmt.Errorf("invalid voice: %q", voice)
	}
	style := strings.TrimSpace(req.StylePrompt)
	if style == "" {
		style = DefaultStylePrompt
	}
	format := req.Format
	if format == "" {
		format = "wav"
	}
	switch format {
	case "opus", "mp3":
		if !FFmpegAvailable() {
			format = "wav" // graceful degrade
		}
	case "wav":
		// always supported
	default:
		return nil, fmt.Errorf("invalid format: %q", format)
	}

	text := Sanitize(req.Text)
	if text == "" {
		return nil, errors.New("empty text after sanitize")
	}

	hash := hashRequest(model, voice, style, text, format, true /* relax/uncensored */)
	if data, ok := cacheGet(hash, format); ok {
		return &SynthesizeResult{Hash: hash, Format: format, AudioBytes: data, Cached: true}, nil
	}

	pcm, err := s.callGemini(ctx, model, voice, style, text)
	if err != nil {
		return nil, err
	}

	// Encoding ladder: try the requested format, fall through to the
	// next-smallest container that the server can produce. Opus → MP3 →
	// WAV. WAV is the in-process header path and never fails.
	var audio []byte
	tryFormats := []string{format}
	if format == "opus" {
		tryFormats = append(tryFormats, "mp3", "wav")
	} else if format == "mp3" {
		tryFormats = append(tryFormats, "wav")
	}
	for _, f := range tryFormats {
		switch f {
		case "wav":
			audio = pcmToWAV(pcm, 24000)
			format = "wav"
		case "opus", "mp3":
			b, encErr := EncodeFFmpeg(ctx, f, pcm, 24000)
			if encErr != nil {
				continue
			}
			audio, format = b, f
		}
		if audio != nil {
			break
		}
	}
	if audio == nil {
		// Should be unreachable — the WAV branch always populates audio.
		return nil, errors.New("encode pipeline produced no audio")
	}
	// Hash is keyed on the *final* format so cache lookups by format
	// land on the right file.
	hash = hashRequest(model, voice, style, text, format, true)

	if err := cachePut(hash, format, audio); err != nil {
		// Cache write failures are non-fatal; we still return the audio.
		_ = err
	}
	return &SynthesizeResult{Hash: hash, Format: format, AudioBytes: audio, Cached: false}, nil
}

// LookupCached returns the bytes for a previously synthesized hash. It is
// used by the GET /audio endpoint to serve the file directly with a long
// browser cache. format must match the on-disk extension.
func (s *Service) LookupCached(hash, format string) ([]byte, bool) {
	return cacheGet(hash, format)
}

// callGemini posts a single :generateContent request with safetySettings
// fully disabled, parses the inline_data audio block, and returns raw PCM.
func (s *Service) callGemini(ctx context.Context, model, voice, style, text string) ([]byte, error) {
	apiKey, err := s.getAPIKey()
	if err != nil {
		return nil, fmt.Errorf("gemini api key: %w", err)
	}

	// All four adjustable categories set to OFF, plus BLOCK_NONE as the
	// schema-compatible fallback in case OFF is rejected by the model.
	const off = "OFF"
	type harm struct {
		Category  string `json:"category"`
		Threshold string `json:"threshold"`
	}
	type voiceCfg struct {
		PrebuiltVoiceConfig struct {
			VoiceName string `json:"voiceName"`
		} `json:"prebuiltVoiceConfig"`
	}
	type speech struct {
		VoiceConfig voiceCfg `json:"voiceConfig"`
	}
	type genCfg struct {
		ResponseModalities []string `json:"responseModalities"`
		SpeechConfig       speech   `json:"speechConfig"`
	}
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Role  string `json:"role,omitempty"`
		Parts []part `json:"parts"`
	}
	type body struct {
		Contents         []content `json:"contents"`
		GenerationConfig genCfg    `json:"generationConfig"`
		SafetySettings   []harm    `json:"safetySettings"`
	}

	// Gemini TTS models reject the `systemInstruction` field
	// ("Developer instruction is not enabled for this model"). Inline
	// the narrator framing and the agent's style prompt into the single
	// user turn instead.
	prompt := SystemInstruction + "\n\n" + style + "\n\n" + text

	mkBody := func(threshold string) body {
		b := body{
			Contents: []content{{
				Role:  "user",
				Parts: []part{{Text: prompt}},
			}},
			GenerationConfig: genCfg{
				ResponseModalities: []string{"AUDIO"},
			},
			SafetySettings: []harm{
				{"HARM_CATEGORY_HARASSMENT", threshold},
				{"HARM_CATEGORY_HATE_SPEECH", threshold},
				{"HARM_CATEGORY_SEXUALLY_EXPLICIT", threshold},
				{"HARM_CATEGORY_DANGEROUS_CONTENT", threshold},
			},
		}
		b.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName = voice
		return b
	}

	pcm, err := s.doGemini(ctx, model, apiKey, mkBody(off))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "invalid") {
		// Some surfaces still reject OFF; retry with BLOCK_NONE which is
		// the next-loosest documented value.
		pcm, err = s.doGemini(ctx, model, apiKey, mkBody("BLOCK_NONE"))
	}
	return pcm, err
}

func (s *Service) doGemini(ctx context.Context, model, apiKey string, payload any) ([]byte, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url := "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Prefer header auth — keeps the key out of access logs.
	req.Header.Set("x-goog-api-key", apiKey)

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // 32 MiB cap
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini http %d: %s", resp.StatusCode, truncate(string(body), 400))
	}

	type respPart struct {
		InlineData *struct {
			MimeType string `json:"mimeType"`
			Data     string `json:"data"`
		} `json:"inlineData,omitempty"`
	}
	type respContent struct {
		Parts []respPart `json:"parts"`
	}
	type candidate struct {
		Content      respContent `json:"content"`
		FinishReason string      `json:"finishReason"`
	}
	type promptFeedback struct {
		BlockReason string `json:"blockReason"`
	}
	var parsed struct {
		Candidates     []candidate    `json:"candidates"`
		PromptFeedback promptFeedback `json:"promptFeedback"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("gemini decode: %w (body=%s)", err, truncate(string(body), 200))
	}
	if parsed.PromptFeedback.BlockReason != "" {
		return nil, fmt.Errorf("gemini blocked: %s", parsed.PromptFeedback.BlockReason)
	}
	if len(parsed.Candidates) == 0 {
		return nil, errors.New("gemini returned no candidates")
	}
	for _, p := range parsed.Candidates[0].Content.Parts {
		if p.InlineData != nil && p.InlineData.Data != "" {
			pcm, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
			if err != nil {
				return nil, fmt.Errorf("base64 decode: %w", err)
			}
			return pcm, nil
		}
	}
	if reason := parsed.Candidates[0].FinishReason; reason != "" && reason != "STOP" {
		return nil, fmt.Errorf("gemini finish=%s without audio", reason)
	}
	return nil, errors.New("gemini returned no audio data")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
