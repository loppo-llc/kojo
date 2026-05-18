package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/session"
)

// --- Session Handlers ---

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	// Non-Owner principals (agents, guests) get only the version. The
	// hostname / home dir / tool availability would otherwise leak the
	// host's deploy details to a curl-happy agent.
	//
	// Trusted RolePeer is the exception: the Hub's NewSession needs
	// to enumerate the target peer's tool availability + homeDir
	// before the operator picks a tool, and trusted peers already
	// have shell create access via the session create proxy. The
	// untrusted RolePeer (and every other non-Owner principal) still
	// falls through to the version-only reduced view.
	p := auth.FromContext(r.Context())
	if !p.IsOwner() && !(p.IsPeer() && p.PeerTrusted) {
		writeJSONResponse(w, http.StatusOK, map[string]any{"version": s.version})
		return
	}
	hostname, _ := os.Hostname()
	homeDir, _ := os.UserHomeDir()
	resp := map[string]any{
		"version":   s.version,
		"hostname":  hostname,
		"homeDir":   homeDir,
		"tools":     session.ToolAvailability(),
		"shellTool": session.ShellToolName(),
	}
	if s.agents != nil {
		resp["agentBackends"] = s.agents.BackendAvailability()
	}
	writeJSONResponse(w, http.StatusOK, resp)
}

// handleCustomModels queries a custom Anthropic Messages API endpoint for available models.
func (s *Server) handleCustomModels(w http.ResponseWriter, r *http.Request) {
	baseURL := r.URL.Query().Get("baseURL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid baseURL")
		return
	}
	host := parsed.Hostname()
	if !isLoopback(host) {
		writeError(w, http.StatusForbidden, "forbidden", "only loopback addresses are allowed")
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(baseURL + "/v1/models")
	if err != nil {
		writeError(w, http.StatusBadGateway, "connection_error", fmt.Sprintf("cannot reach %s: %v", baseURL, err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, "upstream_error", fmt.Sprintf("endpoint returned %d", resp.StatusCode))
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		writeError(w, http.StatusBadGateway, "read_error", err.Error())
		return
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		writeError(w, http.StatusBadGateway, "parse_error", "invalid JSON from models endpoint")
		return
	}
	if len(result.Data) == 0 {
		writeJSONResponse(w, http.StatusOK, map[string]any{"models": []string{}})
		return
	}

	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"models": models})
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	list := s.sessions.List()
	infos := make([]session.SessionInfo, len(list))
	for i, sess := range list {
		infos[i] = sess.Info()
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"sessions": infos})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool               string   `json:"tool"`
		WorkDir            string   `json:"workDir"`
		Args               []string `json:"args"`
		YoloMode           bool     `json:"yoloMode"`
		SimpleSystemPrompt bool     `json:"simpleSystemPrompt"`
		ParentID           string   `json:"parentId"`
		// PeerID lets the Hub UI target a session on a remote peer
		// (NewSession's peer selector). Empty / self → create
		// locally as before. The Hub signs the proxy request as
		// RolePeer; the peer's policy admits /api/v1/sessions for
		// peer principals so the create lands on its local manager.
		// Loop prevention: if the caller is already RolePeer (i.e.
		// we ARE the target peer), ignore the field and create
		// locally — without this guard a misconfigured peerId could
		// cycle the proxy.
		PeerID string `json:"peerId,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "tool is required")
		return
	}
	// Peer-targeted create: forward to the peer's local handler.
	// Loop prevention: a RolePeer-signed request must NOT re-proxy.
	if req.PeerID != "" && s.peerID != nil && req.PeerID != s.peerID.DeviceID {
		if p := auth.FromContext(r.Context()); !p.IsPeer() {
			s.proxyCreateSessionToPeer(w, r, req.PeerID, req)
			return
		}
	}
	if req.WorkDir == "" {
		home, _ := os.UserHomeDir()
		req.WorkDir = home
	}

	if req.SimpleSystemPrompt && (req.Tool == "claude" || req.Tool == "custom") {
		hasSystemPrompt := false
		for _, a := range req.Args {
			if a == "--system-prompt" ||
				strings.HasPrefix(a, "--system-prompt=") ||
				strings.HasPrefix(a, "--system-prompt ") {
				hasSystemPrompt = true
				break
			}
		}
		if !hasSystemPrompt {
			prompt := "Current working directory: " + strconv.Quote(req.WorkDir)
			req.Args = append(req.Args, "--system-prompt", prompt)
		}
	}

	sess, err := s.sessions.Create(req.Tool, req.WorkDir, req.Args, req.YoloMode, req.ParentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Always echo the peer field so the UI knows which host the
	// session lives on (used to stamp the `?peer=` query on later
	// WS / info requests so the Hub proxy routes to the right
	// peer). Empty for local creates — UI treats empty as "self".
	info := sess.Info()
	out := map[string]any{
		"id":        info.ID,
		"tool":      info.Tool,
		"workDir":   info.WorkDir,
		"args":      info.Args,
		"status":    info.Status,
		"createdAt": info.CreatedAt,
		"yoloMode":  info.YoloMode,
		"peer":      "",
	}
	// Carry the rest of the SessionInfo via a quick re-marshal so we
	// don't have to maintain a manual mirror of every field. Then
	// stamp `peer` last.
	full, _ := json.Marshal(info)
	_ = json.Unmarshal(full, &out)
	if s.peerID != nil {
		out["peer"] = s.peerID.DeviceID
	}
	writeJSONResponse(w, http.StatusOK, out)
}

// proxySessionCreateTimeout bounds the inbound peer-targeted
// create proxy. Session spawn is fast (PTY fork + spawn the CLI),
// so 30s is generous; a slow peer either times out or surfaces a
// 503 inside that window.
const proxySessionCreateTimeout = 30 * time.Second

// proxyCreateSessionToPeer forwards the inbound POST /api/v1/sessions
// body to the peer identified by peerID, signing the request as
// RolePeer. On success the response body (the peer's session info)
// is streamed back to the original caller with a `peer` field
// stamped in so the UI can route subsequent WS / info / delete
// requests through the same hub.
func (s *Server) proxyCreateSessionToPeer(w http.ResponseWriter, r *http.Request, peerID string, original any) {
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer routing not available on this host")
		return
	}
	rec, err := s.agents.Store().GetPeer(r.Context(), peerID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"target peer not in registry: "+err.Error())
		return
	}
	addr, err := peer.NormalizeAddress(rec.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"target peer has no usable dial address: "+err.Error())
		return
	}
	body, err := json.Marshal(original)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"re-marshal request: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), proxySessionCreateTimeout)
	defer cancel()
	proxyReq, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/api/v1/sessions", bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	proxyReq.Header.Set("Content-Type", "application/json")
	// Forward Idempotency-Key + If-Match so a session-create retry
	// from the UI dedups at the peer's idempotency middleware
	// (otherwise the Hub→peer round trip would produce a fresh
	// session on every retry). Same headers the agent proxy
	// preserves.
	for _, h := range []string{"Idempotency-Key", "If-Match"} {
		if v := r.Header.Get(h); v != "" {
			proxyReq.Header.Set(h, v)
		}
	}

	if err := peer.AuthorizeOutbound(proxyReq.Context(), s.agents.Store(), proxyReq, peerID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "authorize: "+err.Error())
		return
	}
	client := peer.NoKeepAliveHTTPClient(proxySessionCreateTimeout)
	resp, err := client.Do(proxyReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"peer unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"read peer response: "+err.Error())
		return
	}
	// Re-stamp `peer` to the target deviceID so the UI knows which
	// host owns this session. The peer's local handler already
	// stamps its own ID — overwriting with peerID is idempotent.
	var stamped map[string]any
	if err := json.Unmarshal(respBody, &stamped); err == nil {
		stamped["peer"] = peerID
		out, _ := json.Marshal(stamped)
		respBody = out
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found: "+id)
		return
	}
	writeJSONResponse(w, http.StatusOK, sess.Info())
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found: "+id)
		return
	}
	var err error
	if sess.Info().Status == session.StatusRunning {
		err = s.sessions.Stop(id)
	} else {
		err = s.sessions.Remove(id)
	}
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusConflict, "conflict", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found: "+id)
		return
	}

	var req struct {
		YoloMode *bool `json:"yoloMode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}

	if req.YoloMode != nil {
		sess.SetYoloMode(*req.YoloMode)
	}

	writeJSONResponse(w, http.StatusOK, sess.Info())
}

func (s *Server) handleTerminalSession(w http.ResponseWriter, r *http.Request) {
	parentID := r.PathValue("id")
	sess, ok := s.sessions.FindChildSession(parentID, session.ShellToolName())
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no terminal session for parent: "+parentID)
		return
	}
	writeJSONResponse(w, http.StatusOK, sess.Info())
}

func (s *Server) handleRestartSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.sessions.Restart(id)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, sess.Info())
}

func (s *Server) handleTmuxAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.Action == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "action is required")
		return
	}
	if err := s.sessions.TmuxAction(id, req.Action); err != nil {
		switch {
		case errors.Is(err, session.ErrSessionNotFound):
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		case errors.Is(err, session.ErrSessionNotRunning), errors.Is(err, session.ErrNotTerminal):
			writeError(w, http.StatusConflict, "conflict", err.Error())
		default:
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Attachment Handlers ---

func (s *Server) handleListAttachments(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found: "+id)
		return
	}
	attachments := sess.Attachments()
	if attachments == nil {
		attachments = []*session.Attachment{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"attachments": attachments})
}

func (s *Server) handleDeleteAttachment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found: "+id)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing path parameter")
		return
	}
	// Only allow deleting files that are tracked as attachments
	absPath, _ := filepath.Abs(path)
	if !sess.HasAttachment(absPath) {
		writeError(w, http.StatusBadRequest, "bad_request", "path is not a tracked attachment")
		return
	}
	if err := s.files.ValidatePath(path); err != nil {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	if err := os.Remove(absPath); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	sess.RemoveAttachment(absPath)
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Directory Suggestion Handler ---

func (s *Server) handleDirSuggest(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" {
		writeJSONResponse(w, http.StatusOK, map[string]any{"dirs": []string{}})
		return
	}

	// expand ~ to home directory
	if strings.HasPrefix(prefix, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			prefix = home + prefix[1:]
		}
	}

	// determine parent dir and partial name
	dir := filepath.Dir(prefix)
	partial := filepath.Base(prefix)

	// if prefix ends with a path separator, list that directory
	if strings.HasSuffix(prefix, "/") || strings.HasSuffix(prefix, string(filepath.Separator)) {
		dir = prefix
		partial = ""
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSONResponse(w, http.StatusOK, map[string]any{"dirs": []string{}})
		return
	}

	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if partial != "" && !strings.HasPrefix(strings.ToLower(name), strings.ToLower(partial)) {
			continue
		}
		full := filepath.Join(dir, name)
		dirs = append(dirs, full)
		if len(dirs) >= 10 {
			break
		}
	}

	if dirs == nil {
		dirs = []string{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"dirs": dirs})
}
