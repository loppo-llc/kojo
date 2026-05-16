package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/tts"
)

// ttsService is created lazily on first synthesis request so we don't
// pay the cost (and so tests don't need a credential store) until
// somebody actually presses the play button.
var (
	ttsServiceOnce sync.Once
	ttsService     *tts.Service
	// ttsConcurrency caps the number of in-flight synthesize requests so
	// a connected mobile UI replaying many messages doesn't fork-bomb the
	// ffmpeg subprocess pool. 4 is a deliberate small value.
	ttsConcurrency = make(chan struct{}, 4)
)

func (s *Server) ensureTTSService() *tts.Service {
	ttsServiceOnce.Do(func() {
		creds := s.agents.Credentials()
		ttsService = tts.NewService(func() (string, error) {
			return agent.LoadGeminiAPIKey(creds)
		})
	})
	return ttsService
}

// ttsSynthesizeRequest is the POST body for /api/v1/agents/{id}/tts/synthesize.
type ttsSynthesizeRequest struct {
	Text   string `json:"text"`
	Format string `json:"format,omitempty"` // "opus" | "mp3" | "wav"
}

// ttsSynthesizeResponse describes the server-cached audio. Clients fetch
// the audio bytes via GET /api/v1/tts/audio/<hash>.<ext> so the body of
// this POST stays small (and so re-fetches go straight through the
// browser HTTP cache).
type ttsSynthesizeResponse struct {
	Hash   string `json:"hash"`
	Format string `json:"format"`
	URL    string `json:"url"`
	Bytes  int    `json:"bytes"`
	Cached bool   `json:"cached"`
}

func (s *Server) handleTTSSynthesize(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.agents == nil {
		writeError(w, http.StatusNotFound, "not_found", "agents not enabled")
		return
	}
	p := auth.FromContext(r.Context())
	if !p.CanReadFull(id) {
		writeError(w, http.StatusForbidden, "forbidden", "not allowed")
		return
	}
	a, ok := s.agents.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}
	if a.TTS == nil || !a.TTS.Enabled {
		writeError(w, http.StatusForbidden, "tts_disabled", "tts is disabled for this agent")
		return
	}

	var body ttsSynthesizeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "empty text")
		return
	}
	format := body.Format
	if format == "" {
		format = "opus"
	}
	switch format {
	case "opus", "mp3", "wav":
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "invalid format")
		return
	}

	// Concurrency gate. Use ctx so a navigation away cancels the wait.
	select {
	case ttsConcurrency <- struct{}{}:
		defer func() { <-ttsConcurrency }()
	case <-r.Context().Done():
		writeError(w, http.StatusRequestTimeout, "timeout", "synthesis queue timed out")
		return
	}

	svc := s.ensureTTSService()
	res, err := svc.Synthesize(r.Context(), tts.SynthesizeRequest{
		Model:       a.TTS.Model,
		Voice:       a.TTS.Voice,
		StylePrompt: a.TTS.StylePrompt,
		Text:        body.Text,
		Format:      format,
	})
	if err != nil {
		s.logger.Warn("tts synthesize failed", "agent", id, "err", err)
		writeError(w, http.StatusBadGateway, "tts_failed", err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, ttsSynthesizeResponse{
		Hash:   res.Hash,
		Format: res.Format,
		URL:    "/api/v1/tts/audio/" + res.Hash + "." + tts.Extension(res.Format),
		Bytes:  len(res.AudioBytes),
		Cached: res.Cached,
	})
}

// ttsPreviewRequest is the POST body for /api/v1/tts/preview. Unlike
// /agents/{id}/tts/synthesize this endpoint runs without an agent
// context so users can audition voices on the settings screen before
// committing.
type ttsPreviewRequest struct {
	Voice       string `json:"voice"`
	Model       string `json:"model,omitempty"`
	StylePrompt string `json:"stylePrompt,omitempty"`
	Format      string `json:"format,omitempty"`
}

// previewSampleText is read by handleTTSPreview when no custom style
// prompt is supplied. Kept short to bound preview cost — at 25 audio
// tokens/sec a one-line sample lands well under $0.01.
const previewSampleText = "テスト。これは音声サンプルです。設定で選んだ声でしゃべってる。"

func (s *Server) handleTTSPreview(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeError(w, http.StatusNotFound, "not_found", "agents not enabled")
		return
	}
	// Voice preview is owner-trusted Settings UX; we still want some
	// auth, so anyone with full read on any agent can use it. The
	// simplest available check is CanForkOrCreate (owner-only).
	p := auth.FromContext(r.Context())
	if !p.CanForkOrCreate() {
		writeError(w, http.StatusForbidden, "forbidden", "not allowed")
		return
	}

	var body ttsPreviewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if !tts.IsValidVoice(body.Voice) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid voice")
		return
	}
	format := body.Format
	if format == "" {
		format = "opus"
	}
	switch format {
	case "opus", "mp3", "wav":
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "invalid format")
		return
	}

	select {
	case ttsConcurrency <- struct{}{}:
		defer func() { <-ttsConcurrency }()
	case <-r.Context().Done():
		writeError(w, http.StatusRequestTimeout, "timeout", "synthesis queue timed out")
		return
	}

	svc := s.ensureTTSService()
	res, err := svc.Synthesize(r.Context(), tts.SynthesizeRequest{
		Model:       body.Model,
		Voice:       body.Voice,
		StylePrompt: body.StylePrompt,
		Text:        previewSampleText,
		Format:      format,
	})
	if err != nil {
		s.logger.Warn("tts preview failed", "voice", body.Voice, "err", err)
		writeError(w, http.StatusBadGateway, "tts_failed", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, ttsSynthesizeResponse{
		Hash:   res.Hash,
		Format: res.Format,
		URL:    "/api/v1/tts/audio/" + res.Hash + "." + tts.Extension(res.Format),
		Bytes:  len(res.AudioBytes),
		Cached: res.Cached,
	})
}

// handleTTSAudio serves the cached audio bytes for a previously
// synthesized request. The hash + extension form a path segment captured
// by ServeMux ("{name}.{ext}" cannot be expressed, so the path is
// "/api/v1/tts/audio/{file}" and we split inside).
func (s *Server) handleTTSAudio(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeError(w, http.StatusNotFound, "not_found", "agents not enabled")
		return
	}
	file := r.PathValue("file")
	dot := strings.LastIndexByte(file, '.')
	if dot <= 0 || dot == len(file)-1 {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid file")
		return
	}
	hash, ext := file[:dot], file[dot+1:]
	switch ext {
	case "opus", "mp3", "wav":
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "invalid format")
		return
	}
	svc := s.ensureTTSService()
	data, ok := svc.LookupCached(hash, ext)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "audio expired or unknown hash")
		return
	}

	// Always emit ETag/Cache-Control — including on 304 — so the
	// browser keeps revalidating with the same key on the next refresh.
	etag := `"` + hash + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", tts.MimeType(ext))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := w.Write(data); err != nil {
		s.logger.Debug("tts audio write failed", "err", err)
	}
}

// handleTTSCapability reports the server's audio-encoding capabilities so
// the React client can pick a format the server can actually produce.
// `voiceCatalog` carries the descriptive trait label alongside each
// voice id; `voices` is preserved as a flat list for legacy callers.
func (s *Server) handleTTSCapability(w http.ResponseWriter, r *http.Request) {
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"ffmpeg":       tts.FFmpegAvailable(),
		"formats":      tts.SupportedFormats(),
		"voices":       tts.Voices,
		"voiceCatalog": tts.VoiceCatalog,
		"models":       tts.Models,
		"defaults": map[string]string{
			"model":       tts.DefaultModel,
			"voice":       tts.DefaultVoice,
			"stylePrompt": tts.DefaultStylePrompt,
		},
	})
}
