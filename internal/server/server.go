package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/eventbus"
	"github.com/loppo-llc/kojo/internal/filebrowser"
	gitpkg "github.com/loppo-llc/kojo/internal/git"
	"github.com/loppo-llc/kojo/internal/notify"
	gmailpkg "github.com/loppo-llc/kojo/internal/notifysource/gmail"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/session"
	"github.com/loppo-llc/kojo/internal/slackbot"
	"github.com/loppo-llc/kojo/internal/store"
	"github.com/loppo-llc/kojo/internal/store/secretcrypto"
	"github.com/loppo-llc/kojo/internal/tts"
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
	sessions        *session.Manager
	agents          *agent.Manager
	groupdms        *agent.GroupDMManager
	slackHub        *slackbot.Hub
	files           *filebrowser.Browser
	git             *gitpkg.Manager
	notify          *notify.Manager
	blob            *blob.Store    // native blob API (Phase 3); nil disables /api/v1/blob/...
	blobMaxPutBytes int64          // per-PUT body cap; 0 = defaultBlobMaxPutBytes
	events          *eventbus.Bus  // invalidation broadcast (Phase 4); nil disables /api/v1/events
	peerID          *peer.Identity // local peer identity (Phase G); nil disables /api/v1/peers
	peerEvents      *peer.EventBus // cross-peer status push bus (§3.10); nil disables /api/v1/peers/events
	peerNonces      *peer.NonceCache
	requireIfMatch  bool           // 428 on missing If-Match (docs §3.5 transition)
	webdavTokens    *auth.WebDAVTokenStore
	// onAgentSynced is fired by handlePeerAgentSync after the
	// store rows commit. ONLY the minimum side effects that are
	// safe even if the orchestrator later aborts the switch
	// belong here — currently: agent.Manager in-memory reload so
	// /api/v1/agents/{id} surfaces the new row immediately.
	// Token adoption + AgentLockGuard.AddAgent are deferred to
	// onAgentSyncFinalized, fired by the orchestrator AFTER
	// complete (see handleAgentHandoffFinalize), so an aborted
	// switch doesn't leave target peer with stale runtime state.
	onAgentSynced func(ctx context.Context, agentID string) error
	// onAgentSyncFinalized runs after a successful complete on
	// the source side, when the orchestrator notifies target via
	// POST /api/v1/peers/agent-sync/finalize. Adopts the raw
	// $KOJO_AGENT_TOKEN into the local TokenStore, registers
	// the agent with AgentLockGuard so the lock acquired during
	// complete doesn't expire from this peer, and fires a
	// system-message chat so the agent can resume immediately.
	// sourceDeviceID identifies the originating peer (for the
	// arrival notification prompt).
	onAgentSyncFinalized func(ctx context.Context, agentID, rawToken, sourceDeviceID string) error
	// onAgentReleasedAsSource fires after the orchestrator's
	// successful complete + finalize. Source peer drops the
	// agent from its local AgentLockGuard so a target lease
	// expiry doesn't trigger a re-Acquire from here. Set by
	// cmd/kojo/main.go via SetOnAgentReleasedAsSource.
	onAgentReleasedAsSource func(ctx context.Context, agentID string)
	// pendingAgentSyncs holds the per-op state delivered by
	// /api/v1/peers/agent-sync until the matching finalize or
	// drop call arrives. Keyed by (agent_id, op_id) so a
	// late-arriving drop from an aborted switch can't erase a
	// fresh retry's pending entry — the orchestrator mints a
	// random op_id at sync-dispatch time and replays the same
	// id on its finalize/drop. Guarded by pendingTokensMu.
	pendingAgentSyncs map[pendingSyncKey]pendingSyncEntry
	pendingTokensMu   sync.Mutex
	// pendingSyncKEK is the 32-byte envelope key used to seal
	// pendingAgentSyncs entries into kv so a daemon restart
	// between agent-sync and finalize doesn't drop the raw
	// token. Nil disables persistence — the map remains
	// in-memory-only and a restart strands the pending op
	// (orchestrator retries the whole switch). Set via
	// Config.PendingSyncKEK; main.go feeds the same KEK that
	// peer.LoadOrCreate uses.
	pendingSyncKEK []byte
	// pendingSyncDB is the kv handle used to persist
	// pendingAgentSyncs entries. New() copies it from cfg.Store
	// (the same handle session.Manager gets) so the persistence
	// path doesn't depend on an agent.Manager — tests can wire a
	// bare *store.Store without spinning up a Manager.
	pendingSyncDB *store.Store
	logger          *slog.Logger
	mux             *http.ServeMux
	httpSrv         *http.Server // public (Owner-trusted) listener
	authSrv         *http.Server // agent-facing auth-required listener (lazy)
	authMu          sync.Mutex
	devMode         bool
	version         string
	oauth2Mgr       *gmailpkg.OAuth2Manager
	oauth2Once      sync.Once
	idempSweepOnce  sync.Once // guards StartIdempotencySweep
	webdavSweepOnce sync.Once // guards StartWebDAVTokenSweep
}

type Config struct {
	Addr           string
	DevMode        bool
	Logger         *slog.Logger
	StaticFS       fs.FS // embedded web/dist files for production
	Version        string
	NotifyManager  *notify.Manager
	AgentManager   *agent.Manager
	GroupDMManager *agent.GroupDMManager
	// BlobStore is optional; when nil, /api/v1/blob/... routes are not
	// registered. Phase 3 wires it in cmd/kojo/main.go.
	BlobStore *blob.Store
	// MaxBlobPutBytes overrides the per-PUT body cap. 0 = use the
	// package default (256MB). Tests pass a small cap so they can
	// exercise the 413 path without allocating hundreds of megabytes.
	MaxBlobPutBytes int64
	// EventBus is the invalidation broadcaster. When non-nil, the server
	// registers `GET /api/v1/events` (WebSocket) and exposes
	// Server.PublishEvent for write handlers. When nil, both the route
	// and PublishEvent become no-ops.
	EventBus *eventbus.Bus
	// Store is the kv-backed handle the session.Manager uses for
	// runtime persistence (Phase 2c-2 slice 28). Pass nil to disable
	// session persistence — tests that exercise non-session routes
	// can leave it nil; the production wiring in cmd/kojo/main.go
	// always passes the AgentManager's *store.Store handle.
	Store *store.Store
	// PeerIdentity is the local peer's stable {device_id, pubkey,
	// privkey} (Phase G). When non-nil the server registers
	// /api/v1/peers and uses DeviceID to flag the "self" row in list
	// responses + reject self-deletion. Nil disables the entire peers
	// API (test wiring + builds where peer identity load failed).
	PeerIdentity *peer.Identity
	// RequireIfMatch enables the docs §3.5 transition: every write
	// path that supports optimistic concurrency refuses requests
	// without an If-Match header (428 Precondition Required) instead
	// of allowing the legacy "best-effort" pass-through. Off by
	// default — operators flip it once their clients have all caught
	// up. cmd/kojo/main.go reads $KOJO_REQUIRE_IF_MATCH=1 to set this.
	RequireIfMatch bool
	// PeerEvents is the in-memory pub/sub for peer_registry
	// status mutations (docs §3.10). When non-nil the server
	// registers /api/v1/peers/events and the registrar /
	// OfflineSweeper publish to it on every status change.
	// Nil disables the cross-peer push surface.
	PeerEvents *peer.EventBus
	// PeerNonces backs the peer-auth replay cache. Shared by the
	// AuthMiddleware that fronts /api/v1/peers/* routes. nil =>
	// NewNonceCache(AuthMaxClockSkew) auto-default.
	PeerNonces *peer.NonceCache

	// PeerOnly switches the server into the daemon-mode shape used
	// by `kojo --peer`: only the two inbound peer endpoints
	// (/api/v1/peers/events, /api/v1/peers/blobs/) are registered,
	// the SPA / static FS / dev proxy is skipped, and every Hub-
	// side route (sessions, agents, files, git, push, WebDAV, kv,
	// oplog flush, peer-registry mutation) returns 404. The
	// auth middleware chain stays the same — RolePeer requests
	// signed with Ed25519 still get stamped via peer.AuthMiddleware
	// and a tailnet-reach Owner promotion is still applied — but
	// with nothing else mounted, the peer binary cannot accidentally
	// expose Hub state even if a client crafts an Owner-shaped
	// request.
	PeerOnly bool

	// V0LegacyDir, when non-empty, enables the session store's v0
	// fallback: on kv miss + no v1-dir sessions.json, Load() reads
	// <V0LegacyDir>/sessions.json and mirrors it into kv so the v0
	// → v1 cutover can reattach live tmux panes. The v0 file is
	// never unlinked from this path (rollback + `kojo --clean v0`
	// territory). Empty disables v0 fallback — supply "" on --fresh
	// / pure new-install paths where v0 data must not leak in.
	// cmd/kojo/main.go fills this from configdir.V0Path() iff the
	// startup gate observed v1Complete=true (migration done).
	V0LegacyDir string
	// PendingSyncKEK is the 32-byte envelope key used to seal
	// per-op state in pendingAgentSyncs into kv so the raw
	// $KOJO_AGENT_TOKEN survives a daemon restart between
	// agent-sync and finalize. main.go threads in the same KEK
	// it loaded via secretcrypto.LoadOrCreateKEK for the peer
	// identity row. Nil disables persistence (in-memory map
	// only); a restart with a pending op forces the
	// orchestrator to re-run the whole switch.
	PendingSyncKEK []byte
	// WebDAVTokenStore wires the short-lived WebDAV token surface
	// (docs §3.4 / §5.6). When non-nil the server registers the
	// /api/v1/auth/webdav-tokens issue/list/revoke handlers, exposes
	// StartWebDAVTokenSweep for the boot loop to call, and lets the
	// WebDAV mount gate accept tokens alongside the Owner principal.
	// Nil disables every WebDAV-token surface — tests / builds with
	// no kv handle leave it unset.
	WebDAVTokenStore *auth.WebDAVTokenStore
}

func New(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// session.Manager runs on Hub AND on --peer. The §3.7 device
	// switch hands agent runtime ownership to the target peer at
	// complete time; the target then needs a session.Manager to
	// spawn the agent CLI via tmux/PTY. The historical
	// "peer is daemon-only" stance (commit 94875f7) is rolled
	// back here — leaving session.Manager nil on peer would
	// mean the agent can never actually run there.
	//
	// Caveat: session.Manager's platformInit calls
	// cleanupOrphanedTmuxSessions which kills every `kojo_*`
	// tmux session not in its own known set. Running --peer on
	// the SAME host as a separate Hub instance would have the
	// peer wipe out the Hub's live PTYs. That configuration is
	// unsupported (run one kojo per host); the regular cross-
	// machine peer setup is unaffected.
	sessMgr := session.NewManager(logger, cfg.Store, session.ManagerOptions{
		V0LegacyDir: cfg.V0LegacyDir,
	})
	if baseURL := os.Getenv("CUSTOM_API_BASE_URL"); baseURL != "" {
		sessMgr.SetCustomBaseURL(baseURL)
	}

	s := &Server{
		sessions:        sessMgr,
		agents:          cfg.AgentManager,
		groupdms:        cfg.GroupDMManager,
		files:           filebrowser.New(logger),
		git:             gitpkg.New(logger),
		notify:          cfg.NotifyManager,
		blob:            cfg.BlobStore,
		blobMaxPutBytes: cfg.MaxBlobPutBytes,
		events:          cfg.EventBus,
		peerID:          cfg.PeerIdentity,
		peerEvents:      cfg.PeerEvents,
		peerNonces:      cfg.PeerNonces,
		requireIfMatch:  cfg.RequireIfMatch,
		webdavTokens:    cfg.WebDAVTokenStore,
		pendingSyncKEK:  cfg.PendingSyncKEK,
		pendingSyncDB:   cfg.Store,
		logger:          logger,
		devMode:         cfg.DevMode,
		version:         cfg.Version,
	}

	// Initialize Slack bot hub — server owns the lifecycle. PeerOnly
	// skips this: the daemon-only peer is an inbound-traffic
	// terminus, not an agent host, and a second SlackHub from the
	// peer would race the Hub's hub for the same Slack tokens.
	if s.agents != nil && !cfg.PeerOnly {
		creds := s.agents.Credentials()
		s.slackHub = slackbot.NewHub(
			s.agents, creds,
			func(id string) string { return agent.AgentDir(id) },
			logger,
		)
		for _, a := range s.agents.List() {
			if a.Archived {
				continue
			}
			if a.SlackBot != nil && a.SlackBot.Enabled {
				s.slackHub.StartBot(a.ID, *a.SlackBot)
			}
		}
	}

	// send push notification when an agent finishes its response.
	// PeerOnly skips this wiring: the peer doesn't host agents, so
	// OnChatDone would never fire — but more importantly, even if
	// the Hub forwarded a chat event through the peer surface
	// somehow, the peer must not push out a duplicate web-push.
	if s.notify != nil && s.agents != nil && !cfg.PeerOnly {
		s.agents.OnChatDone = func(ag *agent.Agent, msg *agent.Message) {
			payload, _ := json.Marshal(map[string]any{
				"type":    "agent_chat_done",
				"agentId": ag.ID,
				// cap name and preview to stay well under the 2048-byte
				// encrypted record budget enforced by the push provider.
				"name":    truncateUTF8(ag.Name, 80),
				"preview": truncateUTF8(msg.Content, 200),
			})
			s.notify.Send(payload)
		}
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux, cfg)
	s.mux = mux

	// Start the TTS cache sweep at process startup so a stale cache
	// from a previous run is trimmed before the first synthesize call.
	tts.StartCacheSweep()

	// The public listener (the kojo user's mobile UI) is unconditionally
	// trusted as Owner — preserves the original UX with no token setup.
	// The agent-facing auth listener is created lazily by ServeAuth.
	//
	// Middleware order: PeerAuth (Ed25519-signed inter-peer requests
	// stamp RolePeer in ctx) → OwnerOnly (Guest → Owner, skips when
	// PeerAuth already stamped a non-Guest principal) → Idempotency
	// (24h dedup of write retries; pass-through for non-API/GET/SSE)
	// → mux. PeerAuth runs first so a peer-signed request keeps its
	// scoped identity rather than getting promoted to Owner by the
	// "Tailscale reach == Owner" rule on the public listener.
	//
	// PeerOnly mode (daemon peer, `kojo --peer`) is an exception: the
	// listener is plain HTTP on 0.0.0.0, and the only routes mounted
	// are the inter-peer endpoints whose handlers accept Owner OR
	// Peer. If OwnerOnlyMiddleware still ran here, every unsigned
	// request reaching the peer over the tailnet would get Owner-
	// promoted and waltz through those handlers without ever having
	// to produce an Ed25519 signature — peer endpoints would be
	// effectively unauth. Drop OwnerOnly in PeerOnly so the only
	// accepted principal is RolePeer (peerAuth-stamped) or Guest
	// (which the handlers refuse).
	publicHandler := s.idempotencyMiddleware(mux)
	publicHandler = s.remoteAgentProxyMiddleware(publicHandler)
	if !cfg.PeerOnly {
		publicHandler = auth.OwnerOnlyMiddleware(publicHandler)
	}
	if s.peerID != nil && s.agents != nil && s.agents.Store() != nil {
		peerAuth := peer.NewAuthMiddleware(s.agents.Store(), s.peerNonces, s.peerID.DeviceID)
		publicHandler = peerAuth.Wrap(publicHandler)
	}
	s.httpSrv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           publicHandler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	return s
}

func (s *Server) registerRoutes(mux *http.ServeMux, cfg Config) {
	// --peer mode (`kojo --peer`) used to register only the
	// inter-peer endpoints (peers/events, peers/blobs/, peers/pull)
	// and short-circuit here. The §3.7 device-switch slice promotes
	// it to a full peer: agent runtime + agent-facing routes are
	// registered too so a switch can land the agent CLI on this
	// host. Only the Hub-management surface stays Hub-only —
	// gated below with `cfg.PeerOnly`:
	//
	//   - peer_registry mutation (POST/DELETE/rotate-key on
	//     /api/v1/peers): pairing is operator workflow, not
	//     something an agent or peer drives.
	//   - static Web UI / dev proxy: peer hosts have no UI.

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

	// Custom API model discovery
	mux.HandleFunc("GET /api/v1/custom-models", s.handleCustomModels)

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
	// kv (config / non-secret blob store). Owner-only. Secret rows
	// are daemon-internal — POST/PUT with secret=true is refused.
	if s.agents != nil && s.agents.Store() != nil {
		mux.HandleFunc("GET /api/v1/kv/{namespace}", s.handleListKV)
		mux.HandleFunc("GET /api/v1/kv/{namespace}/{key...}", s.handleGetKV)
		mux.HandleFunc("PUT /api/v1/kv/{namespace}/{key...}", s.handlePutKV)
		mux.HandleFunc("DELETE /api/v1/kv/{namespace}/{key...}", s.handleDeleteKV)
	}

	// WebDAV aux mount (#21). Owner-only — the share holds ad-hoc
	// files the user wants the agent to see (drag-and-drop
	// attachments). Reserved-filename auto-discard, NFC, and
	// case-collision detection live in internal/webdav.
	if h, err := s.buildWebDAVHandler(); err != nil {
		s.logger.Warn("webdav mount disabled", "err", err)
	} else if h != nil {
		// Register on the entire WebDAV verb surface — net/http's
		// ServeMux routes by exact pattern match per method, but
		// WebDAV uses non-standard verbs (PROPFIND, PROPPATCH,
		// MKCOL, COPY, MOVE, LOCK, UNLOCK) that ServeMux won't
		// route by method. Use an empty-method registration via
		// the catch-all path so all verbs reach the handler.
		mux.Handle("/api/v1/webdav/", s.webdavGate(h))
		mux.Handle("/api/v1/webdav", s.webdavGate(h))
	}

	// Op-log replay endpoint (§3.13.1). Owner-only. Wired only when
	// the agent store is available — the fencing gate + every
	// dispatch path need it. The endpoint is idempotent at the
	// per-entry level (op_id collisions on INSERT are treated as
	// success) so a peer retrying after a network blip never
	// duplicates rows.
	if s.agents != nil && s.agents.Store() != nil {
		mux.HandleFunc("POST /api/v1/oplog/flush", s.handleOplogFlush)
	}

	// WebDAV short-lived token API (§3.4 / §5.6). Owner-only. Wired
	// only when a WebDAVTokenStore is supplied — leaving it nil
	// disables both the gate-side fallback and the management
	// endpoints so a deploy without kv can't half-enable the
	// feature.
	if s.webdavTokens != nil {
		mux.HandleFunc("GET /api/v1/auth/webdav-tokens", s.handleListWebDAVTokens)
		mux.HandleFunc("POST /api/v1/auth/webdav-tokens", s.handleIssueWebDAVToken)
		mux.HandleFunc("DELETE /api/v1/auth/webdav-tokens/{id}", s.handleRevokeWebDAVToken)
	}

	mux.HandleFunc("GET /api/v1/push/vapid", s.handleVAPIDKey)
	mux.HandleFunc("POST /api/v1/push/subscribe", s.handlePushSubscribe)
	mux.HandleFunc("POST /api/v1/push/unsubscribe", s.handlePushUnsubscribe)

	// Agent routes
	if s.agents != nil {
		s.registerAgentRoutes(mux)
	}

	// Native blob API (Phase 3 §4.2). Registered only when a Store is
	// wired so the handler never sees a nil store. Listing requires a
	// trailing slash (`/api/v1/blob/<scope>/`) so it is unambiguous
	// against an object whose logical path is empty (which Put rejects).
	if s.blob != nil {
		mux.HandleFunc("GET /api/v1/blob/{scope}/{path...}", s.handleBlobGet)
		mux.HandleFunc("HEAD /api/v1/blob/{scope}/{path...}", s.handleBlobGet)
		mux.HandleFunc("PUT /api/v1/blob/{scope}/{path...}", s.handleBlobPut)
		mux.HandleFunc("DELETE /api/v1/blob/{scope}/{path...}", s.handleBlobDelete)
	}

	// Invalidation broadcast (§3.5 / §4.1). Registered only when an
	// EventBus is wired so the handler never accepts a connection it
	// cannot serve. Auth: gated by OwnerOnlyMiddleware on the public
	// listener; not exposed on the agent-facing listener.
	if s.events != nil {
		mux.HandleFunc("GET /api/v1/events", s.handleEventsWS)
	}

	// Resync cursor companion to /api/v1/events. Registered whenever
	// agents are configured because the store backing it lives behind
	// the agent manager. Peers MUST be able to recover from dropped
	// invalidations; if a deployment ever needs the bus without the
	// cursor (or vice versa), wire a dedicated Store handle.
	if s.agents != nil {
		mux.HandleFunc("GET /api/v1/changes", s.handleChanges)
	}

	// Peer registry HTTP surface (Phase G slice 2). Reads
	// (list / self) are available on every host so an agent or
	// the UI can discover targets; the MUTATION routes
	// (register / delete / rotate-key) are Hub-only — peer
	// pairing is an operator workflow that runs on the Hub.
	if s.peerID != nil && s.agents != nil && s.agents.Store() != nil {
		mux.HandleFunc("GET /api/v1/peers", s.handleListPeers)
		mux.HandleFunc("GET /api/v1/peers/self", s.handleGetSelfPeer)
		if !cfg.PeerOnly {
			mux.HandleFunc("POST /api/v1/peers", s.handleRegisterPeer)
			mux.HandleFunc("DELETE /api/v1/peers/{id}", s.handleDeletePeer)
			mux.HandleFunc("POST /api/v1/peers/{id}/rotate-key", s.handleRotatePeerKey)
		}
		// Cross-peer status push (§3.10). Auth: RolePeer (Ed25519-
		// signed inter-peer request) OR RoleOwner. Wired only when
		// the bus is supplied — nil leaves the route unregistered
		// so a misconfigured deploy doesn't accept connections that
		// would never receive events.
		if s.peerEvents != nil {
			mux.HandleFunc("GET /api/v1/peers/events", s.handlePeerEventsWS)
		}
		// Cross-peer blob fetch (§3.7 step 4). Auth: RolePeer or
		// RoleOwner. The path tail is the kojo:// URI of the
		// blob; ParseURI decodes it. Wired only when the blob
		// store is up — without it Get/Head would have nothing
		// to serve.
		if s.blob != nil {
			mux.HandleFunc("GET /api/v1/peers/blobs/", s.handlePeerBlobGet)
		}
		// Device-switch orchestration (§3.7). Owner-only. The
		// begin/complete/abort triplet drives the blob_refs +
		// agent_locks state machine; the actual cross-peer blob
		// pull happens between begin and complete via the
		// /api/v1/peers/blobs/ endpoint above.
		mux.HandleFunc("POST /api/v1/agents/{id}/handoff/begin", s.handleAgentHandoffBegin)
		mux.HandleFunc("POST /api/v1/agents/{id}/handoff/complete", s.handleAgentHandoffComplete)
		mux.HandleFunc("POST /api/v1/agents/{id}/handoff/abort", s.handleAgentHandoffAbort)
		// Agent-self orchestrated switch (begin → pull → complete).
		// Owner OR self-agent; the policy layer enforces the
		// caller-matches-{id} invariant for non-owner principals.
		mux.HandleFunc("POST /api/v1/agents/{id}/handoff/switch", s.handleAgentHandoffSwitch)
		// Target-side pull endpoint that the orchestrator dials.
		// Owner OR RolePeer (the orchestrator signs as its own
		// peer identity); the source-side blob serve at
		// /api/v1/peers/blobs/ above is the receiving end of the
		// per-URI GET loop the handler runs.
		if s.blob != nil {
			mux.HandleFunc("POST /api/v1/peers/pull", s.handlePeerPull)
		}
		// Agent metadata sync (§3.7 step 4-bis). Owner or RolePeer.
		// The orchestrator pushes the source's agent record +
		// persona + memory + transcript + memory_entries here
		// BEFORE the blob pull so target has the row state to
		// spawn the CLI by the time blobs arrive.
		mux.HandleFunc("POST /api/v1/peers/agent-sync", s.handlePeerAgentSync)
		// Incremental device-switch (§3.7). Target reports its
		// max(agent_messages.seq) / max(memory_entries.seq) and
		// the agent / persona / memory etags it currently holds;
		// source uses the response to ship only the delta the
		// target doesn't already have. First-time switch (Known
		// = false) falls back to full sync.
		mux.HandleFunc("POST /api/v1/peers/agent-sync/state", s.handlePeerAgentSyncState)
		// Finalize + drop endpoints for the §3.7 two-phase
		// agent-sync. agent-sync lands rows but defers token
		// adoption + lock guard activation; finalize commits
		// them after the orchestrator's complete succeeds, drop
		// rolls them back on abort.
		mux.HandleFunc("POST /api/v1/peers/agent-sync/finalize", s.handlePeerAgentSyncFinalize)
		mux.HandleFunc("POST /api/v1/peers/agent-sync/drop", s.handlePeerAgentSyncDrop)
	}

	// Static files / dev proxy — Hub only. --peer hosts have no
	// Web UI; leaving the SPA fallback handler off there means
	// the peer's plain-HTTP listener returns 404 for non-API
	// paths instead of 200 with an empty index.html.
	if !cfg.PeerOnly {
		if cfg.DevMode {
			viteURL, _ := url.Parse("http://localhost:5173")
			proxy := httputil.NewSingleHostReverseProxy(viteURL)
			mux.Handle("/", proxy)
		} else if cfg.StaticFS != nil {
			s.registerStaticFiles(mux, cfg.StaticFS)
		}
	}
}

// registerPeerOnlyRoutes is retained as an entry point for the
// peer-mode-routes test fixture; production wiring goes through
// registerRoutes which now serves both Hub and --peer mode in a
// single switch-friendly registration. The function's behavior
// is intentionally a strict subset of registerRoutes — the §3.10
// cross-subscribe feed, the §3.7 cross-peer blob fetch, and the
// pull endpoint — so the existing test continues to assert that
// the minimal inter-peer surface lights up under PeerOnly
// configuration.
func (s *Server) registerPeerOnlyRoutes(mux *http.ServeMux) {
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		return
	}
	if s.peerEvents != nil {
		mux.HandleFunc("GET /api/v1/peers/events", s.handlePeerEventsWS)
	}
	if s.blob != nil {
		mux.HandleFunc("GET /api/v1/peers/blobs/", s.handlePeerBlobGet)
		mux.HandleFunc("POST /api/v1/peers/pull", s.handlePeerPull)
	}
}

func (s *Server) registerAgentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agents/cron-paused", s.handleGetCronPaused)
	mux.HandleFunc("PUT /api/v1/agents/cron-paused", s.handleSetCronPaused)
	mux.HandleFunc("GET /api/v1/agents/directory", s.handleAgentDirectory)
	mux.HandleFunc("GET /api/v1/agents", s.handleListAgents)
	mux.HandleFunc("POST /api/v1/agents", s.handleCreateAgent)
	mux.HandleFunc("GET /api/v1/agents/{id}", s.handleGetAgent)
	mux.HandleFunc("GET /api/v1/agents/{id}/files", s.handleListAgentFiles)
	mux.HandleFunc("GET /api/v1/agents/{id}/files/view", s.handleViewAgentFile)
	mux.HandleFunc("GET /api/v1/agents/{id}/files/raw", s.handleRawAgentFile)
	mux.HandleFunc("PATCH /api/v1/agents/{id}", s.handleUpdateAgent)
	mux.HandleFunc("POST /api/v1/agents/{id}/reset", s.handleResetAgentData)
	mux.HandleFunc("POST /api/v1/agents/{id}/memory/truncate", s.handleTruncateAgentMemory)
	mux.HandleFunc("POST /api/v1/agents/{id}/fork", s.handleForkAgent)
	mux.HandleFunc("POST /api/v1/agents/{id}/privilege", s.handlePrivilegeAgent)
	mux.HandleFunc("DELETE /api/v1/agents/{id}", s.handleDeleteAgent)
	mux.HandleFunc("POST /api/v1/agents/{id}/unarchive", s.handleUnarchiveAgent)
	mux.HandleFunc("GET /api/v1/agents/{id}/avatar", s.handleGetAvatar)
	mux.HandleFunc("POST /api/v1/agents/{id}/avatar", s.handleUploadAvatar)
	mux.HandleFunc("GET /api/v1/agents/{id}/messages", s.handleGetMessages)
	mux.HandleFunc("PATCH /api/v1/agents/{id}/messages/{msgId}", s.handleUpdateMessage)
	mux.HandleFunc("DELETE /api/v1/agents/{id}/messages/{msgId}", s.handleDeleteMessage)
	mux.HandleFunc("POST /api/v1/agents/{id}/messages/{msgId}/regenerate", s.handleRegenerateMessage)
	// MEMORY.md (singleton per agent). Disk file remains canonical
	// for the CLI process; PUT/DELETE write the file under the per-
	// agent memory-sync gate and then sync the v1 store row so cross-
	// device readers observe the change immediately. If-Match is
	// optional but the editing UI is expected to send it.
	mux.HandleFunc("GET /api/v1/agents/{id}/memory", s.handleGetAgentMemory)
	mux.HandleFunc("PUT /api/v1/agents/{id}/memory", s.handlePutAgentMemory)
	mux.HandleFunc("DELETE /api/v1/agents/{id}/memory", s.handleDeleteAgentMemory)
	// memory_entries (per-agent rolling notes / journals). File on
	// disk under memory/<kind>/<name>.md remains canonical for the
	// CLI; the DB row is the multi-device read path. Body cap 4 MiB
	// per entry; If-Match is optional but the editing UI sends it.
	mux.HandleFunc("GET /api/v1/agents/{id}/memory-entries", s.handleListAgentMemoryEntries)
	mux.HandleFunc("POST /api/v1/agents/{id}/memory-entries", s.handleCreateAgentMemoryEntry)
	mux.HandleFunc("GET /api/v1/agents/{id}/memory-entries/{entryId}", s.handleGetAgentMemoryEntry)
	mux.HandleFunc("PATCH /api/v1/agents/{id}/memory-entries/{entryId}", s.handleUpdateAgentMemoryEntry)
	mux.HandleFunc("DELETE /api/v1/agents/{id}/memory-entries/{entryId}", s.handleDeleteAgentMemoryEntry)
	// Single-agent export — owner-only zip of persona / MEMORY.md /
	// memory_entries / transcript. Bounded by exportZipMaxBytes.
	mux.HandleFunc("GET /api/v1/agents/{id}/export", s.handleExportAgent)
	// persona.md (singleton per agent). Same disk-canonical / DB-as-
	// Web-read pattern as MEMORY.md. Empty body via PUT clears the
	// persona (writePersonaFile removes the file). PATCH /agents/{id}
	// can also update persona via cfg.Persona — this dedicated
	// endpoint adds proper If-Match flow against the agent_persona
	// row's etag (the agents-row etag wouldn't bump on body-only
	// changes).
	mux.HandleFunc("GET /api/v1/agents/{id}/persona", s.handleGetAgentPersona)
	mux.HandleFunc("PUT /api/v1/agents/{id}/persona", s.handlePutAgentPersona)
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
	mux.HandleFunc("GET /api/v1/agents/{id}/active", s.handleGetAgentActive)
	mux.HandleFunc("GET /api/v1/agents/{id}/ws", s.handleAgentWebSocketRouting)

	// Text-to-speech
	mux.HandleFunc("GET /api/v1/tts/capability", s.handleTTSCapability)
	mux.HandleFunc("POST /api/v1/tts/preview", s.handleTTSPreview)
	mux.HandleFunc("POST /api/v1/agents/{id}/tts/synthesize", s.handleTTSSynthesize)
	mux.HandleFunc("GET /api/v1/tts/audio/{file}", s.handleTTSAudio)
	mux.HandleFunc("HEAD /api/v1/tts/audio/{file}", s.handleTTSAudio)

	// Tasks
	mux.HandleFunc("GET /api/v1/agents/{id}/tasks", s.handleListTasks)
	mux.HandleFunc("POST /api/v1/agents/{id}/tasks", s.handleCreateTask)
	mux.HandleFunc("PATCH /api/v1/agents/{id}/tasks/{taskId}", s.handleUpdateTask)
	mux.HandleFunc("DELETE /api/v1/agents/{id}/tasks/{taskId}", s.handleDeleteTask)

	// Pre-compaction summary (called by Claude Code's PreCompact hook)
	mux.HandleFunc("POST /api/v1/agents/{id}/pre-compact", s.handlePreCompact)

	// Session reset (CLI session only, keeps conversation history)
	mux.HandleFunc("POST /api/v1/agents/{id}/reset-session", s.handleResetSession)

	// Manual check-in (fires the periodic check-in prompt on demand)
	mux.HandleFunc("POST /api/v1/agents/{id}/checkin", s.handleCheckin)

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

	// Embedding model setting
	mux.HandleFunc("PUT /api/v1/embedding-model", s.handleSetEmbeddingModel)
	mux.HandleFunc("GET /api/v1/embedding-models", s.handleListEmbeddingModels)

	// Notify source types
	mux.HandleFunc("GET /api/v1/notify-source-types", s.handleListNotifySourceTypes)

	// MCP tool server (Streamable HTTP transport)
	mcpHandler := newMCPHandler(s.agents, s.logger)
	mux.Handle("/api/v1/agents/{id}/mcp", mcpHandler)

	if s.groupdms != nil {
		mux.HandleFunc("GET /api/v1/groupdms", s.handleListGroupDMs)
		mux.HandleFunc("POST /api/v1/groupdms", s.handleCreateGroupDM)
		mux.HandleFunc("GET /api/v1/groupdms/{id}", s.handleGetGroupDM)
		mux.HandleFunc("PATCH /api/v1/groupdms/{id}", s.handleRenameGroupDM)
		mux.HandleFunc("DELETE /api/v1/groupdms/{id}", s.handleDeleteGroupDM)
		mux.HandleFunc("POST /api/v1/groupdms/{id}/members", s.handleAddGroupMember)
		mux.HandleFunc("PATCH /api/v1/groupdms/{id}/members/{agentId}", s.handleSetGroupMemberSettings)
		mux.HandleFunc("DELETE /api/v1/groupdms/{id}/members/{agentId}", s.handleLeaveGroup)
		mux.HandleFunc("GET /api/v1/groupdms/{id}/messages", s.handleGetGroupMessages)
		mux.HandleFunc("POST /api/v1/groupdms/{id}/messages", s.handlePostGroupMessage)
		mux.HandleFunc("POST /api/v1/groupdms/{id}/user-messages", s.handlePostGroupUserMessage)
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

// ServeAuth serves the agent-facing auth-required listener using the
// supplied resolver. The handler chain is AuthMiddleware (sets
// Principal from Authorization header) → EnforceMiddleware (denies
// non-Owner principals on routes outside the allowlist) → mux.
//
// Intended use: bind to 127.0.0.1 only, expose to local PTY processes
// via $KOJO_API_BASE. The public listener (Serve) bypasses auth and
// keeps the user UX intact.
func (s *Server) ServeAuth(ln net.Listener, resolver *auth.Resolver) error {
	srv := s.ensureAuthServer(resolver)
	s.logger.Info("auth listener started", "addr", ln.Addr().String())
	return srv.Serve(ln)
}

func (s *Server) ensureAuthServer(resolver *auth.Resolver) *http.Server {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.authSrv != nil {
		return s.authSrv
	}
	// Auth listener middleware order: PeerAuth (Ed25519-signed
	// inter-peer requests stamp RolePeer in ctx) → AuthMiddleware
	// (sets Principal from Bearer; skips when peer already stamped)
	// → EnforceMiddleware (route-level allowlist for non-Owner
	// principals) → RemoteAgentProxy (§3.7: forward requests for
	// remote agents to the holder peer; proxied requests bypass
	// local idempotency + fencing since the target runs its own)
	// → Idempotency (dedup write retries — sandwiched AFTER
	// Enforce so a leaked key can't replay another principal's
	// cached 2xx) → AgentFencing (refuse agent-runtime mutations
	// when agent_locks.holder_peer ≠ this peer; §3.7) → mux.
	//
	// Idempotency sits OUTSIDE Fencing so a successful retry
	// doesn't get re-blocked by transient lock state that
	// drifted between calls. Fencing's 409 responses stamp
	// X-Kojo-No-Idempotency-Cache so they aren't saved — a
	// retry after the lock comes back must re-check rather than
	// replay the stale 409.
	handler := http.Handler(s.mux)
	if s.peerID != nil && s.agents != nil && s.agents.Store() != nil {
		handler = auth.AgentFencingMiddleware(
			s.agents.Store(), s.peerID.DeviceID, s.logger)(handler)
	}
	handler = s.idempotencyMiddleware(handler)
	handler = s.remoteAgentProxyMiddleware(handler)
	handler = auth.EnforceMiddleware(handler)
	handler = auth.AuthMiddleware(resolver)(handler)
	if s.peerID != nil && s.agents != nil && s.agents.Store() != nil {
		peerAuth := peer.NewAuthMiddleware(s.agents.Store(), s.peerNonces, s.peerID.DeviceID)
		handler = peerAuth.Wrap(handler)
	}
	s.authSrv = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	return s.authSrv
}

// pendingSyncKey is the (agent_id, op_id) tuple that keys the
// agent-sync stash. Op IDs are caller-minted UUIDs so a late
// drop from an aborted earlier switch can't collide with a
// fresh retry's pending entry.
type pendingSyncKey struct {
	AgentID string
	OpID    string
}

// pendingSyncEntry is the per-op state captured at agent-sync
// time. RawToken is the only field today; structuring as a
// struct leaves room for future per-op state (claude session
// digests, sync timestamps) without another schema change.
type pendingSyncEntry struct {
	RawToken string `json:"raw_token"`
}

// pendingAgentSyncNamespace + pendingAgentSyncKey persist
// pendingAgentSyncs entries into kv so the raw agent token
// survives a daemon restart between agent-sync and finalize.
// Reuses the "handoff" namespace shared with released/<id>
// and arrived/<id> markers so all device-switch artefacts
// live under one prefix.
const pendingAgentSyncNamespace = "handoff"

func pendingAgentSyncKey(agentID, opID string) string {
	return "pending/" + agentID + "/" + opID
}

// pendingAgentSyncAAD binds a sealed entry to its (agentID,
// opID) pair so a row swapped onto a different key fails the
// open. Stable string form; the colon delimiter never appears
// in agent_id (UUID-shaped) or op_id (UUID-shaped).
func pendingAgentSyncAAD(agentID, opID string) []byte {
	return []byte("kojo:pending-agent-sync:" + agentID + ":" + opID)
}

// sealPendingSync envelope-encrypts an entry for kv storage.
// Returns (nil, nil) when no KEK was configured — callers
// treat that as "skip kv persistence; in-memory only."
func (s *Server) sealPendingSync(agentID, opID string, entry pendingSyncEntry) ([]byte, error) {
	if len(s.pendingSyncKEK) == 0 {
		return nil, nil
	}
	plain, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("sealPendingSync: marshal: %w", err)
	}
	sealed, err := secretcrypto.Seal(s.pendingSyncKEK, plain, pendingAgentSyncAAD(agentID, opID))
	if err != nil {
		return nil, fmt.Errorf("sealPendingSync: seal: %w", err)
	}
	return sealed, nil
}

// openPendingSync decrypts a previously sealed entry. AAD
// binding catches a row read under the wrong (agentID, opID)
// pair as a decryption failure.
func (s *Server) openPendingSync(agentID, opID string, sealed []byte) (pendingSyncEntry, error) {
	if len(s.pendingSyncKEK) == 0 {
		return pendingSyncEntry{}, fmt.Errorf("openPendingSync: no KEK configured")
	}
	plain, err := secretcrypto.Open(s.pendingSyncKEK, sealed, pendingAgentSyncAAD(agentID, opID))
	if err != nil {
		return pendingSyncEntry{}, fmt.Errorf("openPendingSync: open: %w", err)
	}
	var entry pendingSyncEntry
	if err := json.Unmarshal(plain, &entry); err != nil {
		return pendingSyncEntry{}, fmt.Errorf("openPendingSync: unmarshal: %w", err)
	}
	return entry, nil
}

// pendingSyncStore returns the kv handle used by the
// persistence path. New() populates pendingSyncDB from
// cfg.Store; nil means "no kv configured", which is the
// in-memory-only fallback.
func (s *Server) pendingSyncStore() *store.Store {
	return s.pendingSyncDB
}

// recordPendingAgentSync stashes the per-op state until the
// matching finalize / drop arrives. Writes the kv row FIRST
// (so a finalize that arrives mid-write either sees the full
// sealed entry or no entry at all), then the in-memory cache.
// Refuses an op_id collision (same agent_id + same op_id) by
// overwriting — a retry of the same sync is idempotent.
//
// Returns an error when kv persistence is configured but the
// write fails; the caller surfaces that as 500 so the
// orchestrator retries. In-memory-only fallback (no KEK or no
// store) never errors here.
func (s *Server) recordPendingAgentSync(ctx context.Context, agentID, opID, rawToken string) error {
	entry := pendingSyncEntry{RawToken: rawToken}
	if st := s.pendingSyncStore(); st != nil && len(s.pendingSyncKEK) > 0 {
		sealed, err := s.sealPendingSync(agentID, opID, entry)
		if err != nil {
			return err
		}
		if _, err := st.PutKV(ctx, &store.KVRecord{
			Namespace:      pendingAgentSyncNamespace,
			Key:            pendingAgentSyncKey(agentID, opID),
			ValueEncrypted: sealed,
			Type:           store.KVTypeBinary,
			Scope:          store.KVScopeMachine,
			Secret:         true,
		}, store.KVPutOptions{}); err != nil {
			return fmt.Errorf("recordPendingAgentSync: put kv: %w", err)
		}
	}
	s.pendingTokensMu.Lock()
	if s.pendingAgentSyncs == nil {
		s.pendingAgentSyncs = make(map[pendingSyncKey]pendingSyncEntry)
	}
	s.pendingAgentSyncs[pendingSyncKey{AgentID: agentID, OpID: opID}] = entry
	s.pendingTokensMu.Unlock()
	return nil
}

// consumePendingAgentSync returns the stashed entry for the
// (agent_id, op_id) pair WITHOUT removing it. The finalize
// handler reads it, applies side effects, and only commits the
// delete after the hook succeeds — that way a transient adopt
// failure surfaces as a retryable error rather than leaking the
// raw permanently.
//
// Lookup order: in-memory cache → kv (decrypts on hit and
// repopulates the cache). A daemon restart between agent-sync
// and finalize empties the cache; kv carries the entry across.
// Returns (entry, true, nil) on hit, (zero, false, nil) on miss,
// (zero, false, err) on a kv read or decrypt failure.
func (s *Server) consumePendingAgentSync(ctx context.Context, agentID, opID string) (pendingSyncEntry, bool, error) {
	s.pendingTokensMu.Lock()
	if entry, ok := s.pendingAgentSyncs[pendingSyncKey{AgentID: agentID, OpID: opID}]; ok {
		s.pendingTokensMu.Unlock()
		return entry, true, nil
	}
	s.pendingTokensMu.Unlock()
	st := s.pendingSyncStore()
	if st == nil || len(s.pendingSyncKEK) == 0 {
		return pendingSyncEntry{}, false, nil
	}
	rec, err := st.GetKV(ctx, pendingAgentSyncNamespace, pendingAgentSyncKey(agentID, opID))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return pendingSyncEntry{}, false, nil
		}
		return pendingSyncEntry{}, false, fmt.Errorf("consumePendingAgentSync: get kv: %w", err)
	}
	if !rec.Secret || len(rec.ValueEncrypted) == 0 {
		return pendingSyncEntry{}, false, fmt.Errorf("consumePendingAgentSync: kv row not encrypted (db hand-edited?)")
	}
	// Defensive shape check — writes always set binary + machine.
	// A row with a different type or scope is corruption or a
	// hand-edit; refuse rather than silently decrypt. AAD already
	// catches a value swapped onto the wrong key, so the only
	// failure mode this guards against is a future bug landing a
	// mismatched record under the same key.
	if rec.Type != store.KVTypeBinary || rec.Scope != store.KVScopeMachine {
		return pendingSyncEntry{}, false, fmt.Errorf(
			"consumePendingAgentSync: kv row shape mismatch (type=%q scope=%q, want binary/machine)",
			rec.Type, rec.Scope)
	}
	entry, err := s.openPendingSync(agentID, opID, rec.ValueEncrypted)
	if err != nil {
		return pendingSyncEntry{}, false, err
	}
	s.pendingTokensMu.Lock()
	if s.pendingAgentSyncs == nil {
		s.pendingAgentSyncs = make(map[pendingSyncKey]pendingSyncEntry)
	}
	s.pendingAgentSyncs[pendingSyncKey{AgentID: agentID, OpID: opID}] = entry
	s.pendingTokensMu.Unlock()
	return entry, true, nil
}

// commitPendingAgentSync removes a stashed entry after the
// finalize hook succeeds. Deletes the kv row FIRST so a
// repeated finalize after a crash between cache-clear and
// kv-delete still surfaces the row, and then drops the
// in-memory cache. A kv delete failure returns an error so
// the caller can retry — leaving a stale kv row would let a
// later boot's consume hand the raw token back out.
func (s *Server) commitPendingAgentSync(ctx context.Context, agentID, opID string) error {
	if st := s.pendingSyncStore(); st != nil && len(s.pendingSyncKEK) > 0 {
		if err := st.DeleteKV(ctx, pendingAgentSyncNamespace, pendingAgentSyncKey(agentID, opID), ""); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("commitPendingAgentSync: delete kv: %w", err)
			}
		}
	}
	s.pendingTokensMu.Lock()
	delete(s.pendingAgentSyncs, pendingSyncKey{AgentID: agentID, OpID: opID})
	s.pendingTokensMu.Unlock()
	return nil
}

// dropPendingAgentSync clears a stashed entry without
// finalizing. Used by the orchestrator's abort path. Same
// kv-first ordering as commit; a kv delete failure returns an
// error so the orchestrator retries instead of leaving a
// stranded entry that consume could later resurrect.
func (s *Server) dropPendingAgentSync(ctx context.Context, agentID, opID string) error {
	return s.commitPendingAgentSync(ctx, agentID, opID)
}

// SlackHub returns the wired-in slackbot.Hub or nil when the
// server was built without one (--peer mode or test fixtures).
// Used by cmd/kojo/main.go's source-release hook to stop a
// migrated agent's Slack bot on the source peer.
func (s *Server) SlackHub() *slackbot.Hub {
	return s.slackHub
}

// SetOnAgentSynced installs the post-agent-sync hook that
// handlePeerAgentSync invokes after a successful row write. The
// hook MUST be set on the target peer before it can receive a
// §3.7 device-switch agent-sync payload — without it, the synced
// rows land in kojo.db but no one tells the agent_lock guard or
// the in-memory agent.Manager that a new agent has arrived.
//
// Threadsafe wrt the handler: handlePeerAgentSync reads the field
// after the Server's mux has been built, by which point New() has
// long returned and any cmd/kojo/main.go wiring is in place. We
// don't bother with a mutex — the field is set exactly once at
// boot and never mutated under load.
func (s *Server) SetOnAgentSynced(fn func(ctx context.Context, agentID string) error) {
	s.onAgentSynced = fn
}

// SetOnAgentSyncFinalized installs the post-complete hook that
// the orchestrator's /api/v1/peers/agent-sync/finalize endpoint
// invokes. See onAgentSyncFinalized for the contract.
func (s *Server) SetOnAgentSyncFinalized(fn func(ctx context.Context, agentID, rawToken, sourceDeviceID string) error) {
	s.onAgentSyncFinalized = fn
}

// onAgentReleasedAsSource is fired by the §3.7 switch_device
// handler after a successful complete + finalize. The hook
// drops the agent from this peer's AgentLockGuard.desired set
// so target's lease expiry doesn't trigger a re-Acquire from
// this peer (which would steal the lock back with stale state).
// Set via SetOnAgentReleasedAsSource.
func (s *Server) SetOnAgentReleasedAsSource(fn func(ctx context.Context, agentID string)) {
	s.onAgentReleasedAsSource = fn
}

func (s *Server) Handler() http.Handler {
	return s.httpSrv.Handler
}

func (s *Server) SetTLSConfig(tlsCfg *tls.Config) {
	s.httpSrv.TLSConfig = tlsCfg
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down...")

	// Drain HTTP listeners first so no new requests can land while the
	// rest of the system tears down. Otherwise an in-flight agent chat
	// could be cancelled mid-stream by agents.Shutdown() and surface a
	// confusing partial response to the user. Errors from each listener
	// are logged independently so a timeout on the auth listener cannot
	// hide a problem on the public listener.
	if s.authSrv != nil {
		if err := s.authSrv.Shutdown(ctx); err != nil {
			s.logger.Warn("auth listener shutdown error", "err", err)
		}
	}
	if err := s.httpSrv.Shutdown(ctx); err != nil {
		s.logger.Warn("public listener shutdown error", "err", err)
	}

	// Now stop background producers / hubs.
	if s.slackHub != nil {
		s.slackHub.Stop()
	}
	if s.sessions != nil {
		s.sessions.StopAll()
		s.sessions.SaveAll()
	}
	if s.agents != nil {
		s.agents.Shutdown()
	}
	// Close the event bus AFTER HTTP listeners have drained so any
	// in-flight WebSocket handler exits cleanly via its subscriber's
	// channel-close signal rather than getting torn down with the
	// connection.
	if s.events != nil {
		s.events.Close()
	}
	cleanupUploads()
	return nil
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

// truncateUTF8 returns s clipped so the resulting string never exceeds
// maxBytes bytes. If truncation is needed an ellipsis ("...") is appended,
// and the cut is rolled back to a UTF-8 rune boundary so no multi-byte
// sequence is split. maxBytes <= 0 returns "".
func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "..."
	if maxBytes <= len(ellipsis) {
		// no room for the ellipsis: return a hard byte-aligned slice
		cut := maxBytes
		for cut > 0 && (s[cut]&0xC0) == 0x80 {
			cut--
		}
		return s[:cut]
	}
	cut := maxBytes - len(ellipsis)
	// walk back over UTF-8 continuation bytes (10xxxxxx) so we land on a
	// rune boundary
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + ellipsis
}
