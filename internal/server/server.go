package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"sync"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/filebrowser"
	gitpkg "github.com/loppo-llc/kojo/internal/git"
	"github.com/loppo-llc/kojo/internal/notify"
	gmailpkg "github.com/loppo-llc/kojo/internal/notifysource/gmail"
	"github.com/loppo-llc/kojo/internal/session"
)

type Server struct {
	sessions *session.Manager
	agents   *agent.Manager
	groupdms *agent.GroupDMManager
	files    *filebrowser.Browser
	git      *gitpkg.Manager
	notify   *notify.Manager
	logger   *slog.Logger
	httpSrv  *http.Server
	devMode   bool
	version   string
	oauth2Mgr  *gmailpkg.OAuth2Manager
	oauth2Once sync.Once
}

type Config struct {
	Addr            string
	DevMode         bool
	Logger          *slog.Logger
	StaticFS        fs.FS // embedded web/dist files for production
	Version         string
	NotifyManager   *notify.Manager
	AgentManager    *agent.Manager
	GroupDMManager  *agent.GroupDMManager
}

func New(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		sessions: session.NewManager(logger),
		agents:   cfg.AgentManager,
		groupdms: cfg.GroupDMManager,
		files:    filebrowser.New(logger),
		git:      gitpkg.New(logger),
		notify:   cfg.NotifyManager,
		logger:   logger,
		devMode:  cfg.DevMode,
		version:  cfg.Version,
	}

	// send push notification when an agent finishes its response
	if s.notify != nil && s.agents != nil {
		s.agents.OnChatDone = func(ag *agent.Agent, msg *agent.Message) {
			preview := msg.Content
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			payload, _ := json.Marshal(map[string]any{
				"type":    "agent_chat_done",
				"agentId": ag.ID,
				"name":    ag.Name,
				"preview": preview,
			})
			s.notify.Send(payload)
		}
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux, cfg)

	s.httpSrv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	return s
}

func (s *Server) registerRoutes(mux *http.ServeMux, cfg Config) {
	// Session routes
	mux.HandleFunc("GET /api/v1/info", s.handleInfo)
	mux.HandleFunc("GET /api/v1/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/v1/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("PATCH /api/v1/sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/restart", s.handleRestartSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}/terminal", s.handleTerminalSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/tmux", s.handleTmuxAction)
	mux.HandleFunc("GET /api/v1/sessions/{id}/attachments", s.handleListAttachments)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}/attachments", s.handleDeleteAttachment)
	mux.HandleFunc("GET /api/v1/ws", s.handleWebSocket)

	// Directory suggestions
	mux.HandleFunc("GET /api/v1/dirs", s.handleDirSuggest)

	// File browser
	mux.HandleFunc("GET /api/v1/files", s.handleListFiles)
	mux.HandleFunc("GET /api/v1/files/view", s.handleViewFile)
	mux.HandleFunc("GET /api/v1/files/raw", s.handleRawFile)

	// File upload
	mux.HandleFunc("POST /api/v1/upload", s.handleUpload)

	// Git
	mux.HandleFunc("GET /api/v1/git/status", s.handleGitStatus)
	mux.HandleFunc("GET /api/v1/git/log", s.handleGitLog)
	mux.HandleFunc("GET /api/v1/git/diff", s.handleGitDiff)
	mux.HandleFunc("POST /api/v1/git/exec", s.handleGitExec)

	// Web Push notifications
	mux.HandleFunc("GET /api/v1/push/vapid", s.handleVAPIDKey)
	mux.HandleFunc("POST /api/v1/push/subscribe", s.handlePushSubscribe)
	mux.HandleFunc("POST /api/v1/push/unsubscribe", s.handlePushUnsubscribe)

	// Agent routes
	if s.agents != nil {
		s.registerAgentRoutes(mux)
	}

	// Static files / dev proxy
	if cfg.DevMode {
		viteURL, _ := url.Parse("http://localhost:5173")
		proxy := httputil.NewSingleHostReverseProxy(viteURL)
		mux.Handle("/", proxy)
	} else if cfg.StaticFS != nil {
		s.registerStaticFiles(mux, cfg.StaticFS)
	}
}

func (s *Server) registerAgentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agents/cron-paused", s.handleGetCronPaused)
	mux.HandleFunc("PUT /api/v1/agents/cron-paused", s.handleSetCronPaused)
	mux.HandleFunc("GET /api/v1/agents/directory", s.handleAgentDirectory)
	mux.HandleFunc("GET /api/v1/agents", s.handleListAgents)
	mux.HandleFunc("POST /api/v1/agents", s.handleCreateAgent)
	mux.HandleFunc("GET /api/v1/agents/{id}", s.handleGetAgent)
	mux.HandleFunc("PATCH /api/v1/agents/{id}", s.handleUpdateAgent)
	mux.HandleFunc("POST /api/v1/agents/{id}/reset", s.handleResetAgentData)
	mux.HandleFunc("DELETE /api/v1/agents/{id}", s.handleDeleteAgent)
	mux.HandleFunc("GET /api/v1/agents/{id}/avatar", s.handleGetAvatar)
	mux.HandleFunc("POST /api/v1/agents/{id}/avatar", s.handleUploadAvatar)
	mux.HandleFunc("GET /api/v1/agents/{id}/messages", s.handleGetMessages)
	mux.HandleFunc("POST /api/v1/agents/{id}/avatar/generated", s.handleUploadGeneratedAvatar)
	mux.HandleFunc("POST /api/v1/agents/generate-persona", s.handleGeneratePersona)
	mux.HandleFunc("POST /api/v1/agents/generate-name", s.handleGenerateName)
	mux.HandleFunc("POST /api/v1/agents/generate-avatar", s.handleGenerateAvatar)
	mux.HandleFunc("GET /api/v1/agents/preview-avatar", s.handlePreviewAvatar)
	mux.HandleFunc("GET /api/v1/agents/{id}/credentials", s.handleListCredentials)
	mux.HandleFunc("POST /api/v1/agents/{id}/credentials", s.handleAddCredential)
	mux.HandleFunc("PATCH /api/v1/agents/{id}/credentials/{credId}", s.handleUpdateCredential)
	mux.HandleFunc("DELETE /api/v1/agents/{id}/credentials/{credId}", s.handleDeleteCredential)
	mux.HandleFunc("GET /api/v1/agents/{id}/credentials/{credId}/password", s.handleRevealCredentialPassword)
	mux.HandleFunc("GET /api/v1/agents/{id}/credentials/{credId}/totp", s.handleGetTOTPCode)
	mux.HandleFunc("POST /api/v1/agents/{id}/credentials/parse-qr", s.handleParseQR)
	mux.HandleFunc("POST /api/v1/agents/{id}/credentials/parse-uri", s.handleParseOTPURI)
	mux.HandleFunc("GET /api/v1/agents/{id}/ws", s.handleAgentWebSocket)

	// Tasks
	mux.HandleFunc("GET /api/v1/agents/{id}/tasks", s.handleListTasks)
	mux.HandleFunc("POST /api/v1/agents/{id}/tasks", s.handleCreateTask)
	mux.HandleFunc("PATCH /api/v1/agents/{id}/tasks/{taskId}", s.handleUpdateTask)
	mux.HandleFunc("DELETE /api/v1/agents/{id}/tasks/{taskId}", s.handleDeleteTask)

	// Pre-compaction summary (called by Claude Code's PreCompact hook)
	mux.HandleFunc("POST /api/v1/agents/{id}/pre-compact", s.handlePreCompact)

	// Session reset (CLI session only, keeps conversation history)
	mux.HandleFunc("POST /api/v1/agents/{id}/reset-session", s.handleResetSession)

	// Notify sources
	mux.HandleFunc("GET /api/v1/agents/{id}/notify-sources", s.handleListNotifySources)
	mux.HandleFunc("POST /api/v1/agents/{id}/notify-sources", s.handleCreateNotifySource)
	mux.HandleFunc("PATCH /api/v1/agents/{id}/notify-sources/{sourceId}", s.handleUpdateNotifySource)
	mux.HandleFunc("DELETE /api/v1/agents/{id}/notify-sources/{sourceId}", s.handleDeleteNotifySource)
	mux.HandleFunc("GET /api/v1/agents/{id}/notify-sources/{sourceId}/auth", s.handleNotifySourceAuth)
	mux.HandleFunc("GET /oauth2/callback", s.handleOAuth2Callback)

	// OAuth client configuration
	mux.HandleFunc("GET /api/v1/oauth-clients", s.handleListOAuthClients)
	mux.HandleFunc("POST /api/v1/oauth-clients/{provider}", s.handleSetOAuthClient)
	mux.HandleFunc("DELETE /api/v1/oauth-clients/{provider}", s.handleDeleteOAuthClient)

	// API keys
	mux.HandleFunc("GET /api/v1/api-keys/{provider}", s.handleGetAPIKey)
	mux.HandleFunc("PUT /api/v1/api-keys/{provider}", s.handleSetAPIKey)
	mux.HandleFunc("DELETE /api/v1/api-keys/{provider}", s.handleDeleteAPIKey)

	// Notify source types
	mux.HandleFunc("GET /api/v1/notify-source-types", s.handleListNotifySourceTypes)

	if s.groupdms != nil {
		mux.HandleFunc("GET /api/v1/groupdms", s.handleListGroupDMs)
		mux.HandleFunc("POST /api/v1/groupdms", s.handleCreateGroupDM)
		mux.HandleFunc("GET /api/v1/groupdms/{id}", s.handleGetGroupDM)
		mux.HandleFunc("PATCH /api/v1/groupdms/{id}", s.handleRenameGroupDM)
		mux.HandleFunc("DELETE /api/v1/groupdms/{id}", s.handleDeleteGroupDM)
		mux.HandleFunc("POST /api/v1/groupdms/{id}/members", s.handleAddGroupMember)
		mux.HandleFunc("DELETE /api/v1/groupdms/{id}/members/{agentId}", s.handleLeaveGroup)
		mux.HandleFunc("GET /api/v1/groupdms/{id}/messages", s.handleGetGroupMessages)
		mux.HandleFunc("POST /api/v1/groupdms/{id}/messages", s.handlePostGroupMessage)
		mux.HandleFunc("GET /api/v1/agents/{id}/groups", s.handleListAgentGroups)
	}
}

func (s *Server) registerStaticFiles(mux *http.ServeMux, staticFS fs.FS) {
	fileServer := http.FileServer(http.FS(staticFS))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "index.html"
		} else {
			path = strings.TrimPrefix(path, "/")
		}

		if _, err := fs.Stat(staticFS, path); err == nil {
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}

func (s *Server) Serve(ln net.Listener) error {
	s.logger.Info("server started", "addr", ln.Addr().String())
	return s.httpSrv.Serve(ln)
}

func (s *Server) ServeTLS(ln net.Listener, certFile, keyFile string) error {
	s.logger.Info("server started (TLS)", "addr", ln.Addr().String())
	return s.httpSrv.ServeTLS(ln, certFile, keyFile)
}

func (s *Server) Handler() http.Handler {
	return s.httpSrv.Handler
}

func (s *Server) SetTLSConfig(tlsCfg *tls.Config) {
	s.httpSrv.TLSConfig = tlsCfg
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down...")
	s.sessions.StopAll()
	s.sessions.SaveAll()
	if s.agents != nil {
		s.agents.Shutdown()
	}
	cleanupUploads()
	return s.httpSrv.Shutdown(ctx)
}

// --- API Handlers ---

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	homeDir, _ := os.UserHomeDir()
	resp := map[string]any{
		"version":   s.version,
		"hostname":  hostname,
		"homeDir":   homeDir,
		"tools":     session.ToolAvailability(),
		"shellTool": session.ShellToolName(),
	}
	writeJSONResponse(w, http.StatusOK, resp)
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
		Tool     string   `json:"tool"`
		WorkDir  string   `json:"workDir"`
		Args     []string `json:"args"`
		YoloMode bool     `json:"yoloMode"`
		ParentID string   `json:"parentId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if req.Tool == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "tool is required")
		return
	}
	if req.WorkDir == "" {
		home, _ := os.UserHomeDir()
		req.WorkDir = home
	}

	sess, err := s.sessions.Create(req.Tool, req.WorkDir, req.Args, req.YoloMode, req.ParentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, sess.Info())
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

// --- File Browser Handlers ---

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	hidden := r.URL.Query().Get("hidden") == "true"

	result, err := s.files.List(dir, hidden)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

func (s *Server) handleViewFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	result, err := s.files.View(path)
	if err != nil {
		if errors.Is(err, filebrowser.ErrUnsupportedFile) {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", err.Error())
		} else if errors.Is(err, filebrowser.ErrFileTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

func (s *Server) handleRawFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(path)}))
	}
	s.files.ServeRaw(w, r, path)
}

// --- Upload Handler ---

var uploadDir = filepath.Join(os.TempDir(), "kojo", "upload")
const maxUploadSize = 20 << 20 // 20MB

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "file too large (max 20MB)")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "missing file field")
		return
	}
	defer file.Close()

	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create upload directory")
		return
	}

	safeName := sanitizeFilename(filepath.Base(header.Filename))
	filename := fmt.Sprintf("%d_%s", time.Now().UnixNano(), safeName)
	destPath := filepath.Join(uploadDir, filename)

	dst, err := os.Create(destPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create file")
		return
	}
	defer dst.Close()

	written, err := dst.ReadFrom(file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to write file")
		return
	}

	mime := header.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{
		"path": destPath,
		"name": header.Filename,
		"size": written,
		"mime": mime,
	})
}

func cleanupUploads() {
	os.RemoveAll(uploadDir)
}

// --- Git Handlers ---

func (s *Server) handleGitStatus(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("workDir")
	result, err := s.git.Status(workDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

func (s *Server) handleGitLog(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("workDir")
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := fmt.Sscanf(l, "%d", &limit); n != 1 || err != nil {
			limit = 20
		}
	}
	if limit < 1 {
		limit = 1
	}
	skip := 0
	if sk := r.URL.Query().Get("skip"); sk != "" {
		if n, err := fmt.Sscanf(sk, "%d", &skip); n != 1 || err != nil || skip < 0 {
			skip = 0
		}
	}
	result, err := s.git.Log(workDir, limit, skip)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

func (s *Server) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	workDir := r.URL.Query().Get("workDir")
	ref := r.URL.Query().Get("ref")
	result, err := s.git.Diff(workDir, ref)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

func (s *Server) handleGitExec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkDir string   `json:"workDir"`
		Args    []string `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	result, err := s.git.Exec(req.WorkDir, req.Args)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	writeJSONResponse(w, http.StatusOK, result)
}

// --- Web Push Handlers ---

func (s *Server) handleVAPIDKey(w http.ResponseWriter, r *http.Request) {
	if s.notify == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "push notifications not configured")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]string{
		"publicKey": s.notify.VAPIDPublicKey(),
	})
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.notify == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "push notifications not configured")
		return
	}
	var sub webpush.Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid subscription")
		return
	}
	s.notify.Subscribe(&sub)
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.notify == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "push notifications not configured")
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request")
		return
	}
	s.notify.Unsubscribe(req.Endpoint)
	writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- Helpers ---

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSONResponse(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
