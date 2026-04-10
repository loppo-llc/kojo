package lmsproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"strings"
)

// Proxy translates Anthropic Messages API requests into OpenAI Responses API
// requests for LM Studio, maintaining stateful sessions via previous_response_id.
type Proxy struct {
	lmsBaseURL string
	session    *SessionStore
	client     *http.Client
	logger     *slog.Logger
	server     *http.Server
}

// New creates a new proxy targeting the given LM Studio base URL.
func New(lmsBaseURL string, logger *slog.Logger) *Proxy {
	p := &Proxy{
		lmsBaseURL: strings.TrimRight(lmsBaseURL, "/"),
		session:    NewSessionStore(),
		client:     &http.Client{},
		logger:     logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", p.handleMessages)
	mux.HandleFunc("POST /session/{sessionID}/v1/messages", p.handleMessages)
	mux.HandleFunc("GET /v1/models", p.handleModels)
	mux.HandleFunc("GET /session/{sessionID}/v1/models", p.handleModels)
	// Configure session-level settings (model override, allowed tools).
	mux.HandleFunc("PUT /session/{sessionID}/config", p.handleSessionConfig)
	p.server = &http.Server{Handler: mux}
	return p
}

// Start listens on preferredPort (with fallback) and serves in the background.
// Returns the actual port. The server stops when ctx is cancelled.
func (p *Proxy) Start(ctx context.Context, preferredPort int) (int, error) {
	ln, err := listenWithFallback("127.0.0.1", preferredPort, 10)
	if err != nil {
		return 0, fmt.Errorf("lmsproxy listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		if err := p.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			p.logger.Error("lmsproxy server error", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		p.server.Shutdown(context.Background())
	}()

	return port, nil
}

func (p *Proxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	// Extract session ID from path (CLI sessions) or use "default" (agent chat).
	sessionID := r.PathValue("sessionID")
	if sessionID == "" {
		sessionID = "default"
	}

	var req AnthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAnthropicErrorResponse(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	if len(req.Messages) == 0 {
		writeAnthropicErrorResponse(w, http.StatusBadRequest, "invalid_request_error", "messages array is empty")
		return
	}

	// Session lookup: find previous_response_id and extract delta.
	prevID, newMsgs := p.session.Lookup(sessionID, req.Model, req.Messages)

	// Apply session config (model override, allowed tools).
	var allowedTools map[string]bool
	if cfg, ok := p.session.GetConfig(sessionID); ok {
		if cfg.ModelOverride != "" {
			req.Model = cfg.ModelOverride
		}
		if len(cfg.AllowedTools) > 0 {
			allowedTools = cfg.AllowedTools
		}
	}

	// Build OAI Responses request.
	oaiReq, err := BuildOAIRequest(&req, prevID, newMsgs, allowedTools)
	if err != nil {
		writeAnthropicErrorResponse(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Forward to LM Studio with retry fallback.
	oaiResp, err := p.forwardWithRetry(r.Context(), &req, oaiReq, prevID, allowedTools)
	if err != nil {
		writeAnthropicErrorResponse(w, http.StatusBadGateway, "api_error", "LM Studio unavailable: "+err.Error())
		return
	}
	defer oaiResp.Body.Close()

	if oaiResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(oaiResp.Body, 4096))
		p.logger.Error("lms returned error", "status", oaiResp.StatusCode, "body", string(body))
		writeAnthropicErrorResponse(w, oaiResp.StatusCode, "api_error", string(body))
		return
	}
	if req.Stream {
		// Stream SSE conversion.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		converter := NewStreamConverter(w, req.Model)
		if err := converter.Process(oaiResp.Body); err != nil {
			p.logger.Error("stream conversion error", "err", err)
			return
		}

		// Store session for next request.
		if rid := converter.ResponseID(); rid != "" {
			p.session.Store(sessionID, req.Model, rid)
			p.logger.Debug("session stored", "session", sessionID, "model", req.Model, "responseID", rid)
		}
	} else {
		// Non-streaming: accumulate the OAI SSE into a single Anthropic Message JSON.
		msg, err := AccumulateResponse(oaiResp.Body, req.Model)
		if err != nil {
			writeAnthropicErrorResponse(w, http.StatusBadGateway, "api_error", "stream accumulation failed: "+err.Error())
			return
		}
		if msg.ResponseID != "" {
			p.session.Store(sessionID, req.Model, msg.ResponseID)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(msg.Message)
	}
}

// forwardWithRetry sends the request to LM Studio. If previous_response_id was
// set and the request fails (connection error or non-200), it retries without it
// (full context reload).
func (p *Proxy) forwardWithRetry(ctx context.Context, antReq *AnthropicRequest, oaiReq *OAIRequest, prevID string, allowedTools map[string]bool) (*http.Response, error) {
	resp, err := p.forwardToLMS(ctx, oaiReq)
	if err == nil && resp.StatusCode == http.StatusOK {
		return resp, nil
	}

	// No retry if we didn't have a session.
	if prevID == "" {
		return resp, err
	}

	// Retry without previous_response_id.
	p.logger.Info("retrying without previous_response_id")
	if resp != nil {
		resp.Body.Close()
	}

	oaiReq2, err2 := BuildOAIRequest(antReq, "", antReq.Messages, allowedTools)
	if err2 != nil {
		return nil, fmt.Errorf("build retry request: %w", err2)
	}
	return p.forwardToLMS(ctx, oaiReq2)
}

func (p *Proxy) forwardToLMS(ctx context.Context, oaiReq *OAIRequest) (*http.Response, error) {
	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal OAI request: %w", err)
	}

	p.logger.Debug("forwarding to LMS", "url", p.lmsBaseURL+"/v1/responses", "body", string(body))

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.lmsBaseURL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	return p.client.Do(httpReq)
}

// claudeModelIDs lists Claude model identifiers that Claude Code CLI may use.
// The proxy returns these so the CLI's model validation passes.
var claudeModelIDs = []string{
	"claude-opus-4-6",
	"claude-opus-4-6[1m]",
	"claude-sonnet-4-6",
	"claude-sonnet-4-6[1m]",
	"claude-sonnet-4-5-20241022",
	"claude-haiku-4-5-20251001",
	"claude-3-5-sonnet-20241022",
	"claude-3-5-haiku-20241022",
}

// handleModels returns Claude model IDs so the CLI's model validation passes.
// The actual model mapping to LM Studio happens in handleMessages via session
// config or query parameter.
func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	type modelEntry struct {
		ID string `json:"id"`
	}
	data := make([]modelEntry, len(claudeModelIDs))
	for i, id := range claudeModelIDs {
		data[i] = modelEntry{ID: id}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"data": data,
	})
}

// handleSessionConfig sets per-session configuration (model override, allowed tools).
func (p *Proxy) handleSessionConfig(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	if sessionID == "" {
		writeAnthropicErrorResponse(w, http.StatusBadRequest, "invalid_request_error", "session ID required")
		return
	}
	var req struct {
		Model        string   `json:"model"`
		AllowedTools []string `json:"allowedTools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAnthropicErrorResponse(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	allowed := make(map[string]bool)
	for _, t := range req.AllowedTools {
		allowed[t] = true
	}
	p.session.SetConfig(sessionID, SessionConfig{
		ModelOverride: req.Model,
		AllowedTools:  allowed,
	})
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func writeAnthropicErrorResponse(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// listenWithFallback tries to listen on the preferred port, incrementing up to
// maxAttempts times on failure.
func listenWithFallback(host string, port, maxAttempts int) (net.Listener, error) {
	for i := 0; i < maxAttempts; i++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port+i))
		if err == nil {
			return ln, nil
		}
	}
	return nil, fmt.Errorf("no available port in range %d-%d", port, port+maxAttempts-1)
}

// MinContextLength is the minimum context length required for Claude Code's
// tools + system prompt. Claude Code sends ~20K tokens of tools definitions alone.
const MinContextLength = 32768

// EnsureModelContext checks loaded models via "lms ps" and reloads any model
// whose context length is below MinContextLength. This is necessary because
// Claude Code sends large tool definitions that exceed small context windows.
func EnsureModelContext(logger *slog.Logger) {
	out, err := exec.Command("lms", "ps", "--json").Output()
	if err != nil {
		logger.Debug("lms ps failed", "err", err)
		return
	}

	var models []struct {
		Identifier    string `json:"identifier"`
		ContextLength int    `json:"contextLength"`
		MaxParallel   int    `json:"maxParallel"`
	}
	if json.Unmarshal(out, &models) != nil {
		// lms ps --json may not be supported; try parsing text output.
		ensureModelContextText(logger)
		return
	}

	for _, m := range models {
		needsReload := false
		if m.ContextLength > 0 && m.ContextLength < MinContextLength {
			logger.Info("model context too small",
				"model", m.Identifier,
				"current", m.ContextLength,
				"target", MinContextLength)
			needsReload = true
		}
		if m.MaxParallel > 1 {
			logger.Info("model parallel > 1, reloading with parallel=1 for KV cache",
				"model", m.Identifier,
				"current", m.MaxParallel)
			needsReload = true
		}
		if needsReload {
			reloadModel(m.Identifier, MinContextLength, logger)
		}
	}
}

// ensureModelContextText parses "lms ps" text output as fallback.
func ensureModelContextText(logger *slog.Logger) {
	out, err := exec.Command("lms", "ps").Output()
	if err != nil {
		return
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines[1:] { // skip header
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		identifier := fields[0]
		// Context column is typically the 5th field.
		ctxStr := fields[4]
		ctx := 0
		fmt.Sscanf(ctxStr, "%d", &ctx)
		parallel := 0
		if len(fields) > 5 {
			fmt.Sscanf(fields[5], "%d", &parallel)
		}
		if (ctx > 0 && ctx < MinContextLength) || parallel > 1 {
			logger.Info("reloading model",
				"model", identifier,
				"context", ctx,
				"parallel", parallel)
			reloadModel(identifier, MinContextLength, logger)
		}
	}
}

func reloadModel(identifier string, contextLength int, logger *slog.Logger) {
	if err := exec.Command("lms", "unload", identifier).Run(); err != nil {
		logger.Warn("failed to unload model", "model", identifier, "err", err)
		return
	}
	cmd := exec.Command("lms", "load", identifier,
		"-c", fmt.Sprintf("%d", contextLength),
		"--parallel", "1",
		"--yes")
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("failed to reload model", "model", identifier, "err", err, "output", string(out))
	} else {
		logger.Info("model reloaded", "model", identifier, "context", contextLength, "parallel", 1)
	}
}

// DetectLMSBaseURL runs "lms status" to discover the LM Studio server port.
// Returns "http://localhost:1234" as fallback.
func DetectLMSBaseURL() string {
	out, err := exec.Command("lms", "status").Output()
	if err != nil {
		return "http://localhost:1234"
	}
	text := string(out)
	if idx := strings.Index(text, "port:"); idx >= 0 {
		rest := strings.TrimSpace(text[idx+5:])
		if end := strings.IndexByte(rest, ')'); end > 0 {
			return "http://localhost:" + strings.TrimSpace(rest[:end])
		}
	}
	return "http://localhost:1234"
}
