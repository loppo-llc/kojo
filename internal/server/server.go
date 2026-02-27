package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/fs"
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
	"github.com/loppo-llc/kojo/internal/filebrowser"
	gitpkg "github.com/loppo-llc/kojo/internal/git"
	"github.com/loppo-llc/kojo/internal/notify"
	"github.com/loppo-llc/kojo/internal/session"
)

type Server struct {
	sessions *session.Manager
	files    *filebrowser.Browser
	git      *gitpkg.Manager
	notify   *notify.Manager
	logger   *slog.Logger
	httpSrv  *http.Server
	devMode  bool
	version  string
}

type Config struct {
	Addr          string
	DevMode       bool
	Logger        *slog.Logger
	StaticFS      fs.FS // embedded web/dist files for production
	Version       string
	NotifyManager *notify.Manager
}

func New(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		sessions: session.NewManager(logger),
		files:    filebrowser.New(logger),
		git:      gitpkg.New(logger),
		notify:   cfg.NotifyManager,
		logger:   logger,
		devMode:  cfg.DevMode,
		version:  cfg.Version,
	}

	// send push notification when a session exits
	if s.notify != nil {
		s.sessions.OnSessionExit = func(sess *session.Session) {
			info := sess.Info()
			payload, _ := json.Marshal(map[string]any{
				"type":      "session_exit",
				"tool":      info.Tool,
				"workDir":   info.WorkDir,
				"exitCode":  info.ExitCode,
				"sessionId": info.ID,
			})
			s.notify.Send(payload)
		}
	}

	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /api/v1/info", s.handleInfo)
	mux.HandleFunc("GET /api/v1/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/v1/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("PATCH /api/v1/sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/restart", s.handleRestartSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}/terminal", s.handleTerminalSession)
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

	// Static files / dev proxy
	if cfg.DevMode {
		// proxy to Vite dev server
		viteURL, _ := url.Parse("http://localhost:5173")
		proxy := httputil.NewSingleHostReverseProxy(viteURL)
		mux.Handle("/", proxy)
	} else if cfg.StaticFS != nil {
		// serve embedded static files with SPA fallback
		fileServer := http.FileServer(http.FS(cfg.StaticFS))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			// try serving the file directly
			path := r.URL.Path
			if path == "/" {
				path = "index.html"
			} else {
				path = strings.TrimPrefix(path, "/")
			}

			if _, err := fs.Stat(cfg.StaticFS, path); err == nil {
				// Cache-Control: hashed assets can be cached forever,
				// everything else must revalidate every time.
				if strings.HasPrefix(r.URL.Path, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
			// SPA fallback: serve index.html for non-file routes.
			// /assets/* never falls back — return 404 for missing hashed assets.
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
			r.URL.Path = "/"
			fileServer.ServeHTTP(w, r)
		})
	}

	s.httpSrv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	return s
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
	cleanupUploads()
	return s.httpSrv.Shutdown(ctx)
}

// --- API Handlers ---

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	homeDir, _ := os.UserHomeDir()
	resp := map[string]any{
		"version": s.version,
		"hostname": hostname,
		"homeDir":  homeDir,
		"tools":    session.ToolAvailability(),
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
	if err := s.sessions.Stop(id); err != nil {
		if strings.Contains(err.Error(), "not found") {
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
	sess, ok := s.sessions.FindChildSession(parentID, "tmux")
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
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		}
		return
	}
	writeJSONResponse(w, http.StatusOK, sess.Info())
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

	// if prefix ends with /, list that directory
	if strings.HasSuffix(prefix, "/") {
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
		if strings.Contains(err.Error(), "unsupported") {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", err.Error())
		} else if strings.Contains(err.Error(), "too large") {
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
	s.files.ServeRaw(w, r, path)
}

// --- Upload Handler ---

const uploadDir = "/tmp/kojo/upload"
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

	safeName := filepath.Base(header.Filename)
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
	result, err := s.git.Log(workDir, limit)
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
