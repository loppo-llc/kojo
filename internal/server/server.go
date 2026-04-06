package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"sync"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/filebrowser"
	gitpkg "github.com/loppo-llc/kojo/internal/git"
	"github.com/loppo-llc/kojo/internal/notify"
	gmailpkg "github.com/loppo-llc/kojo/internal/notifysource/gmail"
	"github.com/loppo-llc/kojo/internal/session"
)

func init() {
	// Register video MIME types as fallback for systems where the OS MIME
	// database is missing or incomplete (e.g. minimal Linux containers).
	// mime.AddExtensionType is a no-op if the type is already registered.
	for ext, ct := range map[string]string{
		".mp4":  "video/mp4",
		".webm": "video/webm",
		".mov":  "video/quicktime",
		".avi":  "video/x-msvideo",
		".mkv":  "video/x-matroska",
		".ogv":  "video/ogg",
		".flv":  "video/x-flv",
		".wmv":  "video/x-ms-wmv",
		".m4v":  "video/x-m4v",
	} {
		mime.AddExtensionType(ext, ct)
	}
}

// wsOriginPatterns lists allowed WebSocket origins for both session and agent endpoints.
var wsOriginPatterns = []string{"100.*.*.*", "*.ts.net", "localhost:*", "127.0.0.1:*"}

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

	sessMgr := session.NewManager(logger)
	if cfg.AgentManager != nil {
		if port := cfg.AgentManager.LMSProxyPort(); port > 0 {
			models := cfg.AgentManager.LMStudioModels()
			defaultModel := ""
			if len(models) > 0 {
				defaultModel = models[0]
			}
			sessMgr.SetLMSProxy(port, defaultModel)
		}
	}

	s := &Server{
		sessions: sessMgr,
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

	// Slack bot
	mux.HandleFunc("GET /api/v1/agents/{id}/slackbot", s.handleGetSlackBot)
	mux.HandleFunc("PUT /api/v1/agents/{id}/slackbot", s.handleSetSlackBot)
	mux.HandleFunc("DELETE /api/v1/agents/{id}/slackbot", s.handleDeleteSlackBot)
	mux.HandleFunc("POST /api/v1/agents/{id}/slackbot/test", s.handleTestSlackBot)

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
