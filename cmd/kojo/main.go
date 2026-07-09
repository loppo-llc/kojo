package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/eventbus"
	"github.com/loppo-llc/kojo/internal/notify"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/selfupdate"
	"github.com/loppo-llc/kojo/internal/server"
	"github.com/loppo-llc/kojo/internal/session"
	"github.com/loppo-llc/kojo/internal/store"
	"github.com/loppo-llc/kojo/internal/store/secretcrypto"
	"github.com/loppo-llc/kojo/web"
	localTailscale "tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

var version = "0.110.0"

// newCLILogger builds the stderr text logger used by every subcommand and
// the main boot path, at the given level.
func newCLILogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// applyConfigDirFlag applies the --config-dir override when set. A no-op for
// the empty default so configdir keeps its platform-default resolution.
func applyConfigDirFlag(configDir string) {
	if configDir != "" {
		configdir.Set(configDir)
	}
}

func main() {
	// Subcommands are intercepted before flag.Parse. Today every other
	// mode is a flag; positional args were silently ignored, so claiming
	// "update" here is strictly better than letting it fall through.
	if len(os.Args) > 1 && os.Args[1] == "update" {
		os.Exit(runUpdateCommand(os.Args[2:]))
	}

	port := flag.Int("port", 8080, "port number (auto-increments if busy)")
	dev := flag.Bool("dev", false, "enable dev mode (proxy to Vite)")
	local := flag.Bool("local", false, "listen on localhost only (no Tailscale)")
	hostname := flag.String("hostname", "kojo", "tsnet machine name (becomes <name>.<tailnet>.ts.net). Ignored in --local / --dev mode")
	configDir := flag.String("config-dir", "", "override config directory (default: ~/.config/kojo-v1)")
	showVersion := flag.Bool("version", false, "show version")
	noAuth := flag.Bool("no-auth", false, "disable agent-facing auth listener (--local/--dev only)")
	noUpdateCheck := flag.Bool("no-update-check", false, "disable the periodic GitHub release update check (also via KOJO_NO_UPDATE_CHECK=1)")
	noPeerAutoUpdate := flag.Bool("no-peer-autoupdate", false, "with --peer: do not auto-update this peer's binary from a newer Hub (also via KOJO_NO_PEER_AUTOUPDATE=1)")

	// Upgrade-migration flags: import the legacy kojo/ config dir
	// into the new kojo-v1 config dir.
	doMigrate := flag.Bool("migrate", false, "import the legacy kojo/ config dir into the kojo-v1 config dir, then exit")
	migrateRestart := flag.Bool("migrate-restart", false, "discard a partially-imported kojo-v1 config dir and re-import from the legacy kojo/ config dir (mutually exclusive with --migrate)")
	freshInstall := flag.Bool("fresh", false, "skip the legacy kojo/ config dir and start the kojo-v1 config dir from scratch (refuses if kojo-v1 is already populated)")
	migrateExternalCLI := flag.Bool("migrate-external-cli", true, "with --migrate: link external CLI transcripts (claude symlinks) so prior context is reachable")
	migrateForceRecentMtime := flag.Bool("migrate-force-recent-mtime", false, "with --migrate: bypass the legacy kojo/ dir mtime safety window (5min). Use when the legacy install is confirmed dead but a recent `ls`/backup-extract bumped a file's timestamp; mid-import data races silently corrupt if the legacy install is actually live")
	rollbackExternal := flag.Bool("rollback-external-cli", false, "revert --migrate-external-cli changes; required before booting a pre-0.101 binary again")

	peerMode := flag.Bool("peer", false, "run as a daemon peer: bind a Tailscale-interface listener (the OS tailscaled is consulted via LocalAPI WhoIs for identity) on :<port> plus a 127.0.0.1 agent-auth listener on <port>+1. Registers the full peer API (sessions, agents, files, git, /api/v1/peers/*) so a device-switch can land an agent CLI here. Skipped on a peer: peer-registry mutation (POST/DELETE/rotate-key on /api/v1/peers), the Web UI / dev proxy. Owner Bearer is unavailable; inter-peer access is gated by Tailnet identity (peer_registry NodeKey match → RolePeer; --unsafe collapses the check and stamps Owner). Mutually exclusive with --dev / --local / --no-auth.")
	peerList := flag.Bool("peer-list", false, "list peer_registry rows and exit (read-only; coexists with running kojo)")
	peerAdd := flag.String("peer-add", "", "register a remote peer in peer_registry; spec = <device_id>|<name>|<url>. Metadata-only; NodeKey binding is captured by the auto-pairing /join-request Approve flow.")
	peerRemove := flag.String("peer-remove", "", "delete a peer_registry row by device_id; refuses to remove self")
	// Auto-onboarding flags (docs/peer-onboarding-plan.md).
	hubURL := flag.String("hub", "", "with --peer: override Hub auto-discovery. Accepts host:port or scheme://host:port. Falls back to $KOJO_HUB_URL then MagicDNS default `https://kojo.<tailnet>.ts.net:<KOJO_HUB_PORT or 8080>`.")
	tailnetOnly := flag.Bool("tailnet-only", false, "with --peer: bind the listener to the Tailscale interface IP only. Refuses to start if Tailscale is not running. Without this flag, peer mode falls back to 0.0.0.0 when Tailscale is unavailable.")
	unsafePeer := flag.Bool("unsafe", false, "disable Tailscale identity verification. Every caller is admitted as RoleOwner (the listener boundary becomes the sole trust gate). Intended for LAN / docker / CI deployments; do NOT use on a host with an Internet-routable bind.")
	doSnapshot := flag.Bool("snapshot", false, "take a point-in-time snapshot of kojo.db + blobs/global into <configdir>/snapshots/<ts>/ and exit")
	doRestore := flag.String("restore", "", "restore a snapshot (verified sha256) into <configdir>. The configdir must not be in use by a running kojo. KEK and per-peer credentials are NOT restored — supply them out-of-band before booting.")
	restoreForce := flag.Bool("restore-force", false, "with --restore: overwrite an existing kojo.db in the target. Required when the target is a previously-used Hub being re-seeded.")
	doClean := flag.String("clean", "", "housekeeping: 'snapshots' | 'legacy' | 'blobs' | 'agents' | 'events' | 'v0' (soft-delete v0 dir post-migration; explicit only) | 'v0-trash' (purge kojo.deleted-<ts>/ dirs; explicit only) | 'all' (snapshots+legacy). dry-run by default")
	cleanApply := flag.Bool("clean-apply", false, "with --clean: actually delete the listed entries (default is dry-run)")
	cleanMaxAgeDays := flag.Int("clean-max-age-days", 7, "with --clean snapshots/blobs/agents/events: anything older than N days is eligible (snapshots still honor --clean-keep-latest)")
	cleanKeepLatest := flag.Int("clean-keep-latest", 3, "with --clean snapshots: always keep at least N most-recent successful snapshots regardless of age")
	cleanForce := flag.Bool("clean-force", false, "with --clean v0: skip the v0-manifest divergence guard (operator has confirmed v0 was edited post-migration)")
	cleanMinAgeDays := flag.Int("clean-min-age-days", 7, "with --clean v0-trash: only purge trash dirs whose timestamp is older than N days (default 7; pass 0 to purge every age, which defeats the soft-delete recovery window)")

	flag.Parse()

	if *showVersion {
		fmt.Println("kojo", version)
		return
	}

	// Phase G peer subcommands. Run early (before configdir lock /
	// log-level wiring / startup gate) so they coexist with a running
	// daemon and don't waste cycles on init when the user just wants
	// to query the registry. Each opens its own short-lived
	// *store.Store and exits.
	if *peerList || *peerAdd != "" || *peerRemove != "" {
		applyConfigDirFlag(*configDir)
		logger := newCLILogger(slog.LevelInfo)
		switch {
		case *peerList:
			os.Exit(runPeerListCommand(logger, configdir.Path()))
		case *peerAdd != "":
			os.Exit(runPeerAddCommand(logger, configdir.Path(), *peerAdd))
		case *peerRemove != "":
			os.Exit(runPeerRemoveCommand(logger, configdir.Path(), *peerRemove))
		}
	}

	if *doSnapshot {
		// Resolve the config directory before snapshot — same logic as
		// the main path. Snapshot takes its own short-lived store
		// connection; do NOT acquire the long-running configdir lock,
		// the snapshot path co-exists with a running kojo.
		applyConfigDirFlag(*configDir)
		logger := newCLILogger(slog.LevelInfo)
		os.Exit(runSnapshotCommand(logger, configdir.Path()))
	}

	if *doRestore != "" {
		// Restore is destructive — runs an exclusive configdir lock
		// so a concurrent kojo boot can't interleave with the copy.
		// Operator must stop the live daemon first (the lock check
		// surfaces a friendly error otherwise). KEK / per-peer
		// credentials are NOT restored; the operator supplies those
		// out-of-band per docs/snapshot-restore.md.
		applyConfigDirFlag(*configDir)
		logger := newCLILogger(slog.LevelInfo)
		os.Exit(runRestoreCommand(logger, *doRestore, configdir.Path(), *restoreForce))
	}

	if *doClean != "" {
		applyConfigDirFlag(*configDir)
		logger := newCLILogger(slog.LevelInfo)
		os.Exit(runCleanCommand(cleanFlags{
			target:        *doClean,
			apply:         *cleanApply,
			maxAgeDays:    *cleanMaxAgeDays,
			keepLatest:    *cleanKeepLatest,
			force:         *cleanForce,
			minAgeDays:    *cleanMinAgeDays,
			logger:        logger,
			configDirPath: configdir.Path(),
		}))
	}

	logLevel := slog.LevelInfo
	if *dev {
		logLevel = slog.LevelDebug
	}
	if lvl := os.Getenv("KOJO_LOG_LEVEL"); lvl != "" {
		switch strings.ToLower(lvl) {
		case "debug":
			logLevel = slog.LevelDebug
		case "info":
			logLevel = slog.LevelInfo
		case "warn", "warning":
			logLevel = slog.LevelWarn
		case "error":
			logLevel = slog.LevelError
		default:
			// Defer logging the invalid value until the logger exists, so
			// operators notice the misconfiguration instead of silently
			// getting the default level.
			fmt.Fprintf(os.Stderr, "kojo: ignoring invalid KOJO_LOG_LEVEL=%q (valid: debug|info|warn|warning|error)\n", lvl)
		}
	}
	logger := newCLILogger(logLevel)

	// --peer mode mutual exclusion. The Hub-side network shape
	// (tsnet listener, Owner-trusted UI proxy) and the peer-side
	// network shape (plain HTTP, peer surface only) are wired
	// differently; combining them would produce a half-Hub /
	// half-peer that listens for Owner traffic with no UI to
	// answer, or claims --peer while booting tsnet. Refuse early
	// so the operator picks one.
	if *peerMode {
		switch {
		case *dev:
			fmt.Fprintln(os.Stderr, "kojo: --peer is mutually exclusive with --dev (peer has no web UI to dev-proxy)")
			os.Exit(1)
		case *local:
			fmt.Fprintln(os.Stderr, "kojo: --peer is mutually exclusive with --local (peer binds 0.0.0.0 plain HTTP itself)")
			os.Exit(1)
		case *noAuth:
			fmt.Fprintln(os.Stderr, "kojo: --peer is mutually exclusive with --no-auth (peer has no Owner listener to disable)")
			os.Exit(1)
		case *doMigrate || *migrateRestart || *freshInstall || *rollbackExternal:
			// Migration / rollback is a Hub-side concern. The peer
			// binary's only job is to host /api/v1/peers/* over
			// Tailscale, not to ingest v0 data.
			fmt.Fprintln(os.Stderr, "kojo: --peer is mutually exclusive with --migrate / --migrate-restart / --fresh / --rollback-external-cli (run those on the Hub)")
			os.Exit(1)
		}
		if strings.TrimSpace(*hostname) != "kojo" {
			logger.Warn("--hostname is ignored in --peer mode; the tailnet identity is borrowed from the OS daemon")
		}
	} else {
		// --hub / --tailnet-only are peer-mode-only flags. Refuse
		// up front so an operator who forgot --peer doesn't end up
		// with a Hub-mode boot that silently ignored their intent.
		if strings.TrimSpace(*hubURL) != "" {
			fmt.Fprintln(os.Stderr, "kojo: --hub requires --peer")
			os.Exit(1)
		}
		if *tailnetOnly {
			fmt.Fprintln(os.Stderr, "kojo: --tailnet-only requires --peer")
			os.Exit(1)
		}
	}

	// Resolve the config directory before any subsystem reads it.
	if *configDir != "" {
		configdir.Set(*configDir)
	}
	resolvedDir := configdir.Path()
	logger.Info("config directory", "path", resolvedDir)

	// 5.3 startup gate: refuse to silently start fresh when v0 data is
	// present. Honor --migrate / --migrate-restart / --fresh /
	// --rollback-external-cli before any subsystem touches resolvedDir.
	//
	// Peer mode (`--peer`) bypasses this gate entirely. A daemon-only
	// peer never ingests v0 data — its configdir holds only the local
	// peer identity (peer_registry self-row + KEK-sealed private key);
	// the v0 dir, if any, belongs to a Hub the operator has not yet
	// migrated. Refusing to start "because v0 data is present" on the
	// peer host would be useless friction: there's nothing for the
	// peer binary to do with v0 data, and forcing --migrate / --fresh
	// on a peer would either ingest data the peer does not want or
	// clobber a v1 dir the peer already has (e.g. previously generated
	// `auth/kek.bin` from `kojo --peer-self`). We do still need the
	// v1 dir to exist for the lock + KEK; MkdirAll handles that.
	if *peerMode {
		if err := os.MkdirAll(resolvedDir, 0o700); err != nil {
			logger.Error("peer: could not create config directory", "path", resolvedDir, "err", err)
			os.Exit(1)
		}
	} else {
		gateCtx, gateCancel := context.WithCancel(context.Background())
		proceed := applyStartupGate(gateCtx, migrationFlags{
			migrate:                 *doMigrate,
			migrateRestart:          *migrateRestart,
			fresh:                   *freshInstall,
			migrateExternalCLI:      *migrateExternalCLI,
			migrateForceRecentMtime: *migrateForceRecentMtime,
			rollbackExternal:        *rollbackExternal,
		}, logger, version)
		gateCancel()
		if !proceed {
			return
		}
	}

	// Re-probe AFTER the gate decided to proceed so we know which
	// posture the runtime should take w.r.t. v0 data on this boot:
	//   - v1Complete && v0Exists: we are post-migration with v0 still
	//     on disk → the session store may consult v0 sessions.json to
	//     reattach live tmux panes the v0 binary left behind.
	//   - everything else (--fresh, fresh install, v0 cleaned away):
	//     refuse to consult v0; the operator either opted out of v0
	//     data or there is no v0 install to fall back to.
	// This narrow opt-in keeps `--fresh` honest: that flag's whole
	// point is to ignore v0, which the live tmux reattach path must
	// also honor.
	sessionV0LegacyDir := ""
	if probe := probeDirs(); probe.v1Complete && probe.v0Exists {
		sessionV0LegacyDir = probe.v0Path
	}

	// Acquire an exclusive advisory lock on the config dir so a second kojo
	// instance cannot attach to the same directory and clobber shared state
	// (agents.json, credentials.db, vapid.json).
	lock, err := configdir.Acquire(resolvedDir)
	if err != nil {
		logger.Error("could not lock config directory — another kojo instance may be running", "dir", resolvedDir, "err", err)
		fmt.Fprintf(os.Stderr, "\nAnother kojo instance is already using %s.\n", resolvedDir)
		fmt.Fprintf(os.Stderr, "Use --config-dir to point this instance at a different directory.\n\n")
		os.Exit(1)
	}
	defer lock.Release()

	// tmux is required for user tool sessions on Unix
	if runtime.GOOS != "windows" {
		if _, err := exec.LookPath("tmux"); err != nil {
			logger.Warn("tmux not found in PATH; user tool sessions (claude, codex, grok) will not work")
		}
	}

	// embed static files (sub to strip "dist/" prefix). --peer
	// leaves staticFS nil — peer mode has no web UI and skips
	// registering the SPA fallback handler entirely (server.go's
	// PeerOnly branch), so the embedded `dist/` bytes are dead
	// code for that boot. The binary still ships them so a single
	// build artifact can run as either Hub or peer; only the
	// runtime serve is suppressed.
	var staticFS fs.FS
	if !*dev && !*peerMode {
		var err error
		staticFS, err = fs.Sub(web.StaticFiles, "dist")
		if err != nil {
			logger.Error("failed to load embedded static files", "err", err)
			os.Exit(1)
		}
	}

	agentMgr, err := agent.NewManager(logger)
	if err != nil {
		logger.Error("failed to initialize agent manager", "err", err)
		os.Exit(1)
	}
	defer agentMgr.Close()

	// notify.Manager prefers a kv-backed VAPID store (Phase 5 §3.5)
	// over the legacy vapid.json. We can only build it after the agent
	// manager has materialized the store. KEK lives at <auth>/kek.bin
	// — same dir as owner.token. If KEK setup or store-binding fails
	// we fall back to file-only mode rather than refusing to boot;
	// notifications still work, the private key just isn't encrypted.
	var notifyMgr *notify.Manager
	if vapidStore := buildVAPIDStore(agentMgr, resolvedDir, logger); vapidStore != nil {
		nm, err := notify.NewManagerWithVAPIDStore(logger, vapidStore)
		if err != nil {
			logger.Warn("web push notifications disabled (kv path)", "err", err)
		} else {
			notifyMgr = nm
		}
	} else {
		nm, err := notify.NewManager(logger)
		if err != nil {
			logger.Warn("web push notifications disabled", "err", err)
		} else {
			notifyMgr = nm
		}
	}
	groupDMMgr := agent.NewGroupDMManager(agentMgr, logger)
	agentMgr.SetGroupDMManager(groupDMMgr)

	// --no-auth is dev-only — refuse to bypass auth on the public Tailscale
	// listener where another user could reach the API directly.
	if *noAuth && !*local && !*dev {
		fmt.Fprintln(os.Stderr, "kojo: --no-auth requires --local or --dev")
		os.Exit(1)
	}

	// Token store. Owner / per-agent hashes live in kv (namespace=
	// "auth", scope=global) per Phase 2c-2 slice 17; the
	// <configdir>/auth/ directory is retained as a legacy fallback
	// for v1 installs that booted under pre-cutover binaries (the
	// constructor mirrors any surviving disk files into kv on first
	// boot). KOJO_OWNER_TOKEN still bypasses both stores when set.
	authBase := filepath.Join(resolvedDir, "auth")
	var authKV *store.Store
	if agentMgr != nil {
		authKV = agentMgr.Store()
	}
	tokens, err := auth.NewTokenStore(authBase, authKV, os.Getenv("KOJO_OWNER_TOKEN"))
	if err != nil {
		logger.Error("failed to initialize auth token store", "err", err)
		os.Exit(1)
	}
	agentMgr.SetTokenStore(tokens)
	agent.SetAgentTokenLookup(func(id string) (string, bool) {
		t, err := tokens.AgentToken(id)
		if errors.Is(err, auth.ErrTokenRawUnavailable) {
			// Post-restart (and post v0→v1 migration) the kv row
			// retains only the hash; the raw token kojo must inject
			// into the agent PTY's $KOJO_AGENT_TOKEN is gone. The
			// design intends operator re-issue, but kojo-spawned
			// agents have no operator workflow — silently shipping
			// an empty token here leaves every self-API curl from
			// the PTY hitting the auth listener as Guest and 403-ing.
			// ReissueAgentToken atomically drops the stale verifier
			// and issues a fresh one under one write lock so racing
			// PTY spawns for the same agent don't invalidate each
			// other's freshly-issued raw value.
			t, err = tokens.ReissueAgentToken(id)
		}
		if err != nil {
			logger.Warn("agent token lookup failed; PTY will start without $KOJO_AGENT_TOKEN", "agent", id, "err", err)
			return "", false
		}
		return t, true
	})
	resolver := auth.NewResolver(tokens, agentMgr.IsPrivileged)

	// Phase G: peer identity. Load (or generate on first run) this
	// binary's stable {device_id, Ed25519 keypair, name} from kv. The
	// device_id replaces the os.Hostname() placeholder previously
	// used for blob_refs.home_peer, and feeds the peer_registry
	// self-row that Registrar.Start writes below.
	//
	// On any error we fall through with peerIdentity = nil — the
	// downstream blob.Store / Registrar wiring degrades gracefully
	// (blob.home_peer falls back to hostname, Registrar isn't
	// started). A noisy log line surfaces the failure for the
	// operator.
	var peerIdentity *peer.Identity
	// pendingSyncKEK is the same envelope key used for peer
	// identity / VAPID. Captured here so server.Config can seal
	// pendingAgentSyncs entries into kv without re-loading the
	// keyfile. Nil when KEK setup fails — the server falls back
	// to in-memory-only pending storage (a daemon restart between
	// agent-sync and finalize forces the orchestrator to re-run
	// the whole switch).
	var pendingSyncKEK []byte
	if agentMgr != nil {
		if st := agentMgr.Store(); st != nil {
			authDir := filepath.Join(resolvedDir, "auth")
			kek, err := secretcrypto.LoadOrCreateKEK(authDir)
			if err != nil {
				logger.Warn("peer identity: KEK setup failed; running without peer registry",
					"err", err)
			} else {
				pendingSyncKEK = kek
				idCtx, idCancel := context.WithTimeout(context.Background(), 10*time.Second)
				peerIdentity, err = peer.LoadOrCreate(idCtx, st)
				idCancel()
				if err != nil {
					logger.Warn("peer identity: load failed; running without peer registry",
						"err", err)
				}
			}
		}
	}

	// Native blob store (Phase 3 §2.4 / §4.2). Wired only when the agent
	// manager has a backing *store.Store handy (the migration / fresh-
	// install code paths both initialize one before reaching here). The
	// home_peer is the peer identity's device_id (Phase G) when
	// available; otherwise the hostname fallback. Identity-driven
	// home_peer makes blob_refs rows portable across hostname changes
	// (laptop renamed, restored from backup, etc.).
	var blobStore *blob.Store
	if agentMgr != nil {
		if st := agentMgr.Store(); st != nil {
			var homePeer string
			if peerIdentity != nil {
				homePeer = peerIdentity.DeviceID
			} else {
				homePeer, _ = os.Hostname()
				if homePeer == "" {
					homePeer = "kojo-local"
				}
			}
			blobStore = blob.New(resolvedDir,
				blob.WithRefs(blob.NewStoreRefs(st, homePeer)),
				blob.WithHomePeer(homePeer),
			)
			// Hand the blob store to the agent manager so avatar
			// reads/writes (Phase 2c-2 slice 13) can publish to
			// kojo://global/agents/<id>/avatar.<ext>. Wired here
			// because blobStore depends on agentMgr.Store(); the
			// reverse wiring closes the loop before any HTTP /
			// chat surface comes up.
			agentMgr.SetBlobStore(blobStore)

			// Phase D: materialize blob_refs into agentDir(<id>) so
			// the v1 CLI process (cmd.Dir = agentDir) can read
			// books/, outputs/, temp/, and the catchall scratch
			// the importer just published. Idempotent — existing
			// leaves with matching sha256 are skipped, so subsequent
			// boots are O(stat) per ref. Best-effort across agents.
			agentMgr.HydrateAgentBlobsAtLoad()
		}
	}

	// Blob integrity scrubber (§3.15-bis). Re-hashes every blob_refs
	// body on a 24h cadence and quarantines mismatches. Started here
	// (after blobStore + agentMgr are both wired) so the first scrub
	// runs against the full materialized tree, including anything
	// the migration / hydrate path just laid down.
	var blobScrubber *blob.Scrubber
	if blobStore != nil && agentMgr != nil && agentMgr.Store() != nil {
		blobScrubber = blob.NewScrubber(agentMgr.Store(), blobStore, logger, blob.ScrubberOpts{})
		// Use a background context — Scrubber.Stop() will close its
		// stop channel on shutdown. The signal context (ctx, set up
		// below) gets cancelled at the same moment, so the loop's
		// ctx.Done branch fires too; the redundant guard is
		// harmless.
		blobScrubber.Start(context.Background())
	}

	// Phase G: peer registrar. Writes the self-row in peer_registry
	// (UpsertPeer with device_id + name + base64 public_key + status=
	// online) and starts the heartbeat goroutine that refreshes
	// last_seen every 30 s. Skipped silently if peerIdentity load
	// failed earlier — the binary still serves traffic, it just
	// won't appear in cross-peer listings.
	var peerRegistrar *peer.Registrar
	var peerSweeper *peer.OfflineSweeper
	var agentLockGuard *peer.AgentLockGuard
	// Cross-peer status push bus (§3.10). Always wired alongside
	// the registrar so subscribers to /api/v1/peers/events receive
	// register / heartbeat / expire / shutdown events. Nil
	// peerIdentity falls through (no bus, no route registered).
	var peerEvents *peer.EventBus
	// In-memory active-connection set for /api/v1/peers/events. The
	// handler Add/Removes a deviceID per WS; the OfflineSweeper
	// refreshes last_seen on every active peer per sweep tick so a
	// flaky mobile uplink that drops the occasional touch keeps the
	// peer pinned online as long as a WS holds.
	var peerPresence *peer.Presence
	var peerSubscriber *peer.Subscriber
	var peerSubscriberTargetsCtx context.Context
	var peerSubscriberTargetsCancel context.CancelFunc
	// --peer の PruneToOwnedAgentsForPeer 結果。失敗 (false) 時は下の
	// StartSchedulers を fail-closed で skip して cron 二重発火を防ぐ。
	// 既定 true: 非 --peer や peerIdentity nil の経路では prune 自体を
	// 走らせないが、StartSchedulers は走らせたいため。
	peerPruneOK := true
	// Fail-closed for a --peer node that couldn't establish its
	// identity: without peerIdentity the ownership prune below cannot
	// run, so m.agents may still hold agents owned by OTHER peers.
	// Running schedulers OR the file watcher over them would double-fire
	// cron and flush those peers' stale leftover files into the DB,
	// rolling back the real holder. Hub / single-node (non --peer) keeps
	// the default true — it legitimately owns every loaded agent even
	// when peerIdentity is nil.
	if *peerMode && peerIdentity == nil {
		peerPruneOK = false
		logger.Warn("--peer without peer identity; skipping schedulers + file watcher to avoid cross-peer double-fire / stale flush. fix identity and restart.")
	}
	if peerIdentity != nil && agentMgr != nil {
		if st := agentMgr.Store(); st != nil {
			peerEvents = peer.NewEventBus()
			peerPresence = peer.NewPresence()
			peerRegistrar = peer.NewRegistrar(st, peerIdentity, logger)
			peerRegistrar.SetEventBus(peerEvents)
			startCtx, startCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := peerRegistrar.Start(startCtx); err != nil {
				logger.Warn("peer registrar: start failed; cross-peer listings will not see this binary",
					"err", err)
				peerRegistrar = nil
			}
			startCancel()
			// Peer-count lookup feeds SyncDeviceSwitchSkill (in
			// agent.Manager.prepareChat) so the kojo-switch-device
			// SKILL.md is installed iff at least one non-self
			// peer_registry row exists.
			//
			// Gated on registrar success: if the registrar didn't
			// start, the self row may be missing and a remote row
			// we'd be counting could be an orphan from a prior
			// boot of an unrelated host. Suppressing the skill
			// there avoids teasing the agent on a misconfigured
			// boot.
			//
			// No status filter: the gate only checks row presence
			// (any status, any last_seen). Online/freshness
			// enforcement lives in switch_device_handler.go, which
			// 409s a switch dispatched at a peer that isn't
			// currently online. Surfacing "target offline" from
			// the skill body is better UX than the skill missing
			// entirely. The previous online-only gate caused the
			// skill to flicker on/off at chat time as peers
			// cycled in and out of the online status, leaving
			// agents whose last chat happened during a brief
			// offline window without the skill until the next
			// chat.
			if peerRegistrar != nil {
				selfID := peerIdentity.DeviceID
				agent.SetPeerCountLookup(func() int {
					if agentMgr == nil {
						return 0
					}
					lookupStore := agentMgr.Store()
					if lookupStore == nil {
						return 0
					}
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					rows, err := lookupStore.ListPeers(ctx, store.ListPeersOptions{})
					if err != nil {
						return 0
					}
					count := 0
					for _, r := range rows {
						if r.DeviceID == selfID {
							continue
						}
						count++
					}
					return count
				})
			}
			// Sweeper is the v1 stand-in for cross-peer WS heartbeat
			// observation: it flips a registry row to 'offline' once
			// its last_seen has aged past 5× HeartbeatInterval. Wired
			// only when the registrar is up so the loop has a non-nil
			// store handle and a stable self-row to exclude.
			if peerRegistrar != nil {
				peerSweeper = peer.NewOfflineSweeper(st, peerIdentity, logger)
				peerSweeper.SetEventBus(peerEvents)
				peerSweeper.SetPresence(peerPresence)
				peerSweeper.Start()
			}
			// Cross-peer status subscriber (§3.10). Connects to
			// every OTHER peer's /api/v1/peers/events feed so a
			// remote peer's disappearance is observed via the WS
			// drop rather than waiting on Hub-side heartbeat aging.
			// Targets are reconciled periodically against the
			// live peer_registry rows. Single-peer clusters have
			// no targets and the subscriber's goroutine pool is
			// empty.
			if peerRegistrar != nil {
				peerSubscriber = peer.NewSubscriber(peerIdentity, st, logger)
				peerSubscriberTargetsCtx, peerSubscriberTargetsCancel = context.WithCancel(context.Background())
				go peerSubscriberTargetsLoop(peerSubscriberTargetsCtx, st, peerIdentity, peerSubscriber, logger)
			}
			// AgentLockGuard owns the `agent_locks` rows for every
			// agent this binary knows about (docs §3.5 / §3.7).
			// Acquired here so a multi-peer slice can rely on the
			// row being present; fencing-token threading through
			// individual agent-runtime writes is the next slice.
			//
			// Gated on peerRegistrar success: without the self-row
			// in peer_registry, this binary is invisible to cluster
			// listings and holding agent_locks under its DeviceID
			// would just confuse the next online peer about who
			// owns the agent. Skip rather than half-wire it.
			//
			// Also gated on !--peer: daemon-only peers do NOT host
			// agent runtimes (StartSchedulers / SlackHub / push are
			// already off), so taking agent_locks under this peer's
			// DeviceID would deceive the Hub's fencing logic into
			// thinking this peer owns those agents. Lock acquisition
			// has to follow runtime ownership; in `--peer` mode the
			// runtime stays on the Hub.
			// §3.7 startup eviction: before we feed the agent list
			// to AgentLockGuard and StartSchedulers, drop any
			// agent that THIS peer previously released as the
			// §3.7 source (signalled by the `handoff/released/<id>`
			// kv marker written by Manager.ReleaseAgentLocally).
			// NewManager loads every persisted agent row
			// unconditionally — without this filter, a switched-
			// away agent would resurrect on source after a daemon
			// restart, racing the new holder for cron / notify
			// writes against rows it no longer owns.
			//
			// Runs OUTSIDE the `if peerRegistrar != nil` block: a
			// failed registrar start leaves the daemon without the
			// guard but still in Hub mode with schedulers about
			// to fire. Eviction needs only peerIdentity + store,
			// neither of which depends on the registrar succeeding,
			// so a registrar failure must not bypass the eviction.
			// --peer も device-switch 着地点になり得るので、
			// released marker のついた行を schedulers 起動前に落とす
			// 必要がある。さもないと StartSchedulers が peer の m.agents
			// 全件 (released 込み) に cron を載せ、他 peer と二重発火する。
			// 既に他 peer に release した行を evict するロジック自体は
			// mode 非依存なので peer でもそのまま流用できる。
			evictCtx, evictCancel := context.WithTimeout(
				context.Background(), 30*time.Second)
			agentMgr.EvictNonLocalAgentsAtStartup(
				evictCtx, peerIdentity.DeviceID)
			evictCancel()
			// --peer 限定の追加 prune: released marker が無い orphan 行
			// (未finalize / 過去 incarnation の残骸) も schedulers 起動前
			// に detach する。owned 判定は arrived markers ∪ agent_locks
			// holder == self − released markers で再構築。
			// Prune が失敗 (kv 読取エラー等) した場合、m.agents の正当
			// 所有を切り分けられないので fail-closed として StartSchedulers
			// 自体を skip する。さもないと orphan 全件に cron が乗って
			// 他 peer と二重発火する。Operator は警告ログを見て kv 修復後
			// 再起動。
			if *peerMode {
				pruneCtx, pruneCancel := context.WithTimeout(
					context.Background(), 30*time.Second)
				peerPruneOK = agentMgr.PruneToOwnedAgentsForPeer(
					pruneCtx, peerIdentity.DeviceID)
				pruneCancel()
				if !peerPruneOK {
					logger.Warn("--peer prune failed; skipping StartSchedulers to avoid cross-peer double-fire. fix kv and restart.")
				}
			}
			if peerRegistrar != nil {
				// Run on Hub AND on --peer. Hub starts with its
				// owned agents pre-claimed; peer starts empty and
				// gains entries via AddAgent when a §3.7 handoff
				// delivers an agent here. The guard's refresh
				// loop keeps both peers' lock leases alive
				// independently — only one peer is the actual
				// holder at any moment (the orchestrator enforces
				// that via TransferAgentLock), the other guards
				// observe ErrLockHeld and stay quiet until the
				// lease expires or a switch hands them ownership.
				agentLockGuard = peer.NewAgentLockGuard(st, peerIdentity, logger)
				ids := make([]string, 0)
				if !*peerMode {
					for _, a := range agentMgr.List() {
						ids = append(ids, a.ID)
					}
				} else {
					// Peer mode hosts no agent runtime, so
					// agentMgr.List() is empty / irrelevant —
					// but a prior handoff TO this peer landed
					// a durable handoff/arrived/<id> kv marker
					// at finalize. AgentLockGuard.Stop deletes
					// the agent_locks rows on graceful shutdown
					// (ReleaseAgentLockByPeer), so the lock
					// table alone cannot bootstrap the next
					// boot's desired set. The arrival marker
					// survives shutdown by design and is the
					// authoritative "this peer hosts <id>"
					// signal across restarts.
					//
					// Union with agent_locks rows still held by
					// this peer covers crash-restart cases where
					// the row didn't get released. Read failures
					// log warn and proceed with the partial seed
					// — Hub-side fencing keeps writes from this
					// peer scoped correctly even when the guard
					// runs degraded.
					seed := make(map[string]struct{})
					listCtx, listCancel := context.WithTimeout(
						context.Background(), 10*time.Second)
					arrived, aerr := agentMgr.ListArrivedAgents(listCtx)
					if aerr != nil {
						logger.Warn("--peer: arrived-marker seed read failed",
							"err", aerr)
					}
					for _, id := range arrived {
						seed[id] = struct{}{}
					}
					locks, lerr := st.ListAgentLocksByHolder(
						listCtx, peerIdentity.DeviceID)
					if lerr != nil {
						logger.Warn("--peer: agent_locks seed read failed",
							"err", lerr)
					}
					for _, l := range locks {
						seed[l.AgentID] = struct{}{}
					}
					// Released-wins: subtract any agent that has
					// an outstanding source-release marker. Covers
					// the case where ClearAgentArrivedHere failed
					// during a prior switch-away — the stale
					// arrival or lingering lock row would otherwise
					// re-seed AgentLockGuard.desired for an agent
					// we no longer own. ListArrivedAgents already
					// filters internally; this filter handles the
					// agent_locks branch.
					released, rerr := agentMgr.ListReleasedAgents(listCtx)
					listCancel()
					if rerr != nil {
						logger.Warn("--peer: released-marker seed read failed",
							"err", rerr)
					}
					for id := range released {
						delete(seed, id)
					}
					for id := range seed {
						ids = append(ids, id)
					}
				}
				// --peer prune が失敗していたら ids は部分集合 (内部の
				// kv 読取も同じ store を使うので同じく degraded のはず)。
				// 部分 seed で Start すると stale ID を AcquireAgentLock し
				// 直してしまい、本来所有していない agent の lock を奪う恐れ
				// がある。fail-closed で Start も skip し、guard 自体を
				// nil に戻す — 後続の finalize / force-reclaim hook が
				// capturedGuard.AddAgent を呼んで refresh loop の無い lock
				// を取得するのを防ぐため。Hub mode は peerPruneOK 常に true
				// なので影響なし。
				if !*peerMode || peerPruneOK {
					lockCtx, lockCancel := context.WithTimeout(context.Background(), 30*time.Second)
					agentLockGuard.Start(lockCtx, ids)
					lockCancel()
				} else {
					logger.Warn("--peer prune failed; skipping AgentLockGuard.Start and disabling guard hooks to avoid stale lock re-acquire")
					agentLockGuard = nil
				}

				// Restore agent_locks.allowed_proxy_peer for every
				// seeded ID. AgentLockGuard.Stop wipes the lock rows
				// on graceful shutdown, so the Start above just
				// re-inserted them via fresh AcquireAgentLock —
				// which defaults allowed_proxy_peer to self. The
				// original orchestrator (stamped by finalize) was
				// stashed in the arrived_proxy/<id> kv row at the
				// time of arrival; replay it here so the next
				// inbound Hub→target chat proxy passes the
				// agentHolderAdmitMiddleware gate.
				//
				// Best-effort: a missing row means the agent
				// arrived before this fix landed, or finalize never
				// completed the proxy stamp. ErrFencingMismatch
				// means another peer stole the lock between
				// Start and here (concurrent operator action);
				// either way log + skip, the next finalize / force-
				// reclaim will re-stamp.
				if agentMgr != nil && len(ids) > 0 {
					restoreCtx, restoreCancel := context.WithTimeout(
						context.Background(), 10*time.Second)
					for _, id := range ids {
						proxy, perr := agentMgr.GetAgentArrivedProxy(restoreCtx, id)
						if perr != nil {
							logger.Warn("restore allowed_proxy_peer: lookup failed",
								"agent", id, "err", perr)
							continue
						}
						if proxy == "" {
							// Legacy detection: agent arrived
							// before this fix landed, so no
							// arrived_proxy/<id> row exists. If
							// the current agent_locks row points
							// allowed_proxy_peer at self, the
							// Hub→target proxy is broken until
							// the operator re-stamps via a fresh
							// device-switch or direct SQL. Log
							// loudly so the symptom (UI says
							// "agent is busy" / 403) is debuggable.
							if lock, lerr := st.GetAgentLock(restoreCtx, id); lerr == nil &&
								lock != nil &&
								lock.HolderPeer == peerIdentity.DeviceID &&
								(lock.AllowedProxyPeer == "" || lock.AllowedProxyPeer == peerIdentity.DeviceID) {
								logger.Warn("legacy arrival without proxy hint; orchestrator proxy may 403 until re-stamped",
									"agent", id, "allowed_proxy_peer", lock.AllowedProxyPeer)
							}
							continue
						}
						if proxy == peerIdentity.DeviceID {
							continue
						}
						if err := st.UpdateAgentLockAllowedProxy(
							restoreCtx, id, peerIdentity.DeviceID, proxy,
						); err != nil {
							logger.Warn("restore allowed_proxy_peer: stamp failed",
								"agent", id, "proxy", proxy, "err", err)
							continue
						}
						logger.Info("restore allowed_proxy_peer: stamped",
							"agent", id, "proxy", proxy)
					}
					restoreCancel()
				}
			}
		}
	}

	// Phase D barrier: start cron AFTER blob hydrate so a tick that
	// fires immediately doesn't spawn the agent's CLI in an empty
	// CWD (books/, outputs/, etc. would not yet be on disk).
	// NewManager used to call StartSchedulers internally; the split
	// lets cmd/kojo interpose the hydrate.
	//
	// `--peer` host も device-switch の着地点になり得る
	// (ActivateAgentRuntime が arrival 時に呼ばれる)。
	// StartSchedulers を呼ばないと schedulersStarted=false のままで
	// ActivateAgentRuntime が早期 return し、転移先 peer で cron が
	// 永久に未登録になる。boot 直後の peer は agent 0 件なので、
	// StartSchedulers は空 iterate するだけで副作用なし。
	// 例外: --peer で PruneToOwnedAgentsForPeer が失敗した場合は、
	// 正当所有を切り分けられないため schedulers 起動自体を skip
	// (peerPruneOK==false)。
	if agentMgr != nil && peerPruneOK {
		agentMgr.StartSchedulers()
	}

	// Reflect agent-CLI disk writes (MEMORY.md, memory/, persona.md,
	// workspace files) into the DB promptly via a filesystem watcher,
	// instead of waiting for the next prepareChat's lazy sync. Stops on
	// agentMgr.Close(). Best-effort: a watcher init failure degrades to
	// the lazy sync, never to data loss.
	//
	// MUST start AFTER the ownership prune above (EvictNonLocalAgents /
	// PruneToOwnedAgentsForPeer) and only when peerPruneOK: the watcher
	// flushes disk→DB for agents holdsLocally reports as held, so if a
	// released/orphan row were still in m.agents the watcher would push
	// that peer's stale leftover files into the DB and roll back the
	// real holder. Gating on peerPruneOK (same as StartSchedulers)
	// guarantees m.agents holds only agents this peer legitimately owns.
	if agentMgr != nil && peerPruneOK {
		agentMgr.StartFileWatcher()
	}

	// kojo-attach hub forwarder. In --peer mode the daemon may
	// host an agent runtime (post device-switch) that generates
	// attachments locally; the bytes must also reach hub so the
	// UI (hub-only) can serve them via /api/v1/blob/.... The
	// forwarder is a closure over the live store + peer identity
	// so a peer_registry edit (operator promotes a different hub
	// via `--peer-trust`) is picked up on the next attachment
	// without a daemon restart.
	if agentMgr != nil && *peerMode && peerIdentity != nil {
		if st := agentMgr.Store(); st != nil {
			wireAttachForwarder(agentMgr, st, peerIdentity, logger)
		}
	}

	// Invalidation broadcaster (§3.5). Process-local; cross-peer fan-out
	// happens via the WebSocket subscriber pump in handleEventsWS. Closed
	// from Server.Shutdown after the HTTP listeners drain.
	bus := eventbus.New(0) // 0 = DefaultBuffer

	// Forward durable store events into the live broadcast bus. The seq
	// passed into bus.Publish is the SAME value the events table holds,
	// so a peer that fell off the WS feed can resync via
	// /api/v1/changes?since=<seq> without ambiguity. The listener fires
	// post-commit on the writer's goroutine — the bus.Publish call is
	// non-blocking so a slow subscriber cannot stall a domain write.
	if agentMgr != nil {
		if st := agentMgr.Store(); st != nil {
			st.SetEventListener(func(e store.EventRecord) {
				bus.Publish(eventbus.Event{
					Table: e.Table,
					ID:    e.ID,
					ETag:  e.ETag,
					Op:    string(e.Op),
					Seq:   e.Seq,
					TS:    e.TS,
				})
			})
		}
	}

	// Self-update checker: always built so GET /api/v1/system/update can
	// answer even when the periodic loop is disabled (dev builds, operator
	// opt-out). CleanupStaleBinaries reaps Windows .old leftovers at boot.
	selfupdate.CleanupStaleBinaries()
	updateClient := selfupdate.NewClient(version)
	updateChecker := selfupdate.NewChecker(updateClient, version, logger)

	srv := server.New(server.Config{
		Addr:           fmt.Sprintf(":%d", *port),
		DevMode:        *dev,
		Logger:         logger,
		StaticFS:       staticFS,
		Version:        version,
		NotifyManager:  notifyMgr,
		AgentManager:   agentMgr,
		GroupDMManager: groupDMMgr,
		BlobStore:      blobStore,
		EventBus:       bus,
		Store:          agentMgr.Store(),
		PeerIdentity:   peerIdentity,
		PeerEvents:     peerEvents,
		PeerPresence:   peerPresence,
		// docs §3.5 transition: when KOJO_REQUIRE_IF_MATCH=1, every
		// optimistic-concurrency-aware write rejects a missing
		// If-Match header with 428 Precondition Required. Off by
		// default — operators flip it once their UI / agent CLI have
		// caught up and stopped sending bare PUT/PATCH/DELETEs.
		RequireIfMatch: os.Getenv("KOJO_REQUIRE_IF_MATCH") == "1",
		// RepoDir enables POST /api/v1/system/rebuild (`make build` +
		// in-place binary swap). Empty disables the endpoint.
		RepoDir:        os.Getenv("KOJO_REPO_DIR"),
		V0LegacyDir:    sessionV0LegacyDir,
		PeerOnly:       *peerMode,
		PendingSyncKEK: pendingSyncKEK,
		// --no-auth is loopback-only and contractually Owner-trusted
		// ("--no-auth (--local/--dev only): the loopback listener is
		// Owner-trusted"). Collapse it onto the same Unsafe path
		// --unsafe uses so TailnetIdentityMiddleware stamps every
		// caller as Owner without consulting the tsnet WhoIs resolver
		// (which is never wired in --local/--dev anyway, so the
		// sentinel ErrNodeKeyResolverNotReady would otherwise demote
		// every caller to Guest and 403 the API).
		Unsafe:        *unsafePeer || *noAuth,
		UpdateChecker: updateChecker,
	})
	if *unsafePeer {
		logger.Warn("kojo: --unsafe set; tailnet identity disabled. Inter-peer endpoints are open to anyone reachable on the listener.")
	}

	// §3.7 device-switch agent-sync hook. When this peer
	// receives a sync push from the source peer (handler
	// internal/server/peer_agent_sync_handler.go), the store
	// rows have already been written; the hook below threads:
	//   - raw $KOJO_AGENT_TOKEN into the local TokenStore so
	//     subsequent PTY spawns inject the right Bearer.
	//   - the freshly-synced agent row into agent.Manager's
	//     in-memory cache so chat/list/fork see it without
	//     a process restart.
	//   - the agent into AgentLockGuard so the lock acquired
	//     during handoff/complete doesn't expire from this peer.
	//
	// Captures `tokens`, `agentMgr`, and `agentLockGuard` from
	// the outer scope. Any sub-step failure surfaces back to
	// the handler so the HTTP caller (orchestrator) can abort
	// the switch rather than continue with a partially-wired
	// target.
	{
		capturedGuard := agentLockGuard
		// Phase 1 (sync): land the rows + refresh in-memory.
		// This always runs even when the orchestrator later
		// aborts the switch — the row state has to be visible
		// so a follow-up sync can overwrite it.
		srv.SetOnAgentSynced(func(_ context.Context, agentID string) error {
			if agentMgr != nil {
				if err := agentMgr.ReloadAgentFromStore(agentID); err != nil {
					return fmt.Errorf("agent reload: %w", err)
				}
			}
			return nil
		})
		// Phase 2 (finalize): activate runtime state. Only
		// runs after the orchestrator's complete succeeds;
		// an aborted switch fires drop instead, leaving the
		// rows present but not actively claimed by this peer.
		srv.SetOnAgentSyncFinalized(func(hookCtx context.Context, agentID, rawToken, sourceDeviceID, opID string) (bool, error) {
			tokenReissued := false
			// Step order is durability-first: write the durable
			// arrived marker BEFORE touching token / lock-guard
			// state, THEN best-effort clear any prior released
			// marker. Source releases regardless of finalizeErr
			// (lock + blobs have already moved by complete), so
			// any later-step failure must leave target with at
			// least a durable seed for AgentLockGuard on the
			// next restart. A stale older released/<id> row that
			// fails to delete here is harmless — latest-wins
			// arbitration in latestHandoffMarkers picks the new
			// arrival because its timestamp is later. Token-
			// adopt failure after the marker is recoverable via
			// ReissueAgentToken / operator re-handoff; an
			// arrived-marker-missing state would leave the agent
			// unreachable from any future boot because
			// ReleaseAgentLockByPeer on graceful shutdown wipes
			// the agent_locks row.
			if agentMgr != nil {
				// sourceDeviceID becomes the allowed_proxy_peer that
				// finalize stamps below via UpdateAgentLockAllowedProxy
				// (peer_agent_sync_finalize_handler.go). Persist it now
				// so a graceful-shutdown wipe of agent_locks doesn't
				// strand the agent on the next boot — the fresh
				// AcquireAgentLock would otherwise default allowed_proxy
				// _peer back to self and the Hub→target chat proxy
				// would 403 in agentHolderAdmitMiddleware.
				if err := agentMgr.MarkAgentArrivedHere(hookCtx, agentID, sourceDeviceID); err != nil {
					return tokenReissued, fmt.Errorf("mark agent arrived: %w", err)
				}
				if err := agentMgr.ClearAgentReleasedHere(hookCtx, agentID); err != nil {
					// Best-effort: latest-wins picks the
					// new arrival anyway, so a transient
					// kv delete failure must not abort
					// the hook.
					logger.Warn("clear source-release marker failed; latest-wins will cover",
						"agent", agentID, "err", err)
				}
			}
			if tokens != nil {
				if rawToken != "" {
					if err := tokens.AdoptAgentTokenFromPeer(agentID, rawToken); err != nil {
						return tokenReissued, fmt.Errorf("token adopt: %w", err)
					}
				} else {
					// Task A auto-repair: source only held the kv hash
					// (post-restart peer) so no raw token rode the sync.
					// Historically this stranded target in a "manual
					// re-issue required" state. AutoRepairAgentToken
					// mints a fresh raw, CAS-swaps the kv hash row, and
					// updates the in-memory verifier so the next PTY
					// spawn injects a working $KOJO_AGENT_TOKEN. A
					// repair failure fails the hook (pending retained;
					// operator retries finalize) — same contract as an
					// adopt failure.
					reissued, terr := tokens.AutoRepairAgentToken(agentID)
					if terr != nil {
						// Fail the hook so the finalize handler keeps the
						// pending entry and the orchestrator retries —
						// mirrors the AdoptAgentTokenFromPeer failure
						// contract above. Swallowing the error would
						// activate the runtime without a usable
						// $KOJO_AGENT_TOKEN and consume the pending entry.
						return tokenReissued, fmt.Errorf("token auto-repair: %w", terr)
					}
					if reissued {
						logger.Info("agent-sync finalize: raw agent token unavailable; auto-re-issued on this peer",
							"agent", agentID)
						tokenReissued = true
					}
				}
			}
			if capturedGuard != nil {
				capturedGuard.AddAgent(hookCtx, agentID)
			}
			// Verify the lock actually transferred to this host
			// before activating runtime side channels. AddAgent
			// internally calls AcquireAgentLock; ErrLockHeld
			// (stale source row still alive) leaves holder ≠
			// self. Activating cron / notify / arrival chat
			// against an agent we don't actually hold would let
			// the runtime mutate state the source still owns,
			// then surfacing 5xx at finalize would leave a
			// half-active target. Return an error so the
			// finalize handler keeps pending and the orchestrator
			// can retry.
			if agentMgr != nil && agentMgr.Store() != nil && peerIdentity != nil {
				lock, lerr := agentMgr.Store().GetAgentLock(hookCtx, agentID)
				if lerr != nil {
					// Wrap ErrNotFound as ErrFencingMismatch so
					// the finalize handler translates it to a
					// 503 lock_not_self response and the
					// source-side retry loop picks it up.
					// Other lookup errors stay as-is (500).
					if errors.Is(lerr, store.ErrNotFound) {
						return tokenReissued, fmt.Errorf("agent_lock row missing for %q: %w",
							agentID, store.ErrFencingMismatch)
					}
					return tokenReissued, fmt.Errorf("post-AddAgent lock lookup: %w", lerr)
				}
				if lock == nil || lock.HolderPeer != peerIdentity.DeviceID {
					holder := ""
					if lock != nil {
						holder = lock.HolderPeer
					}
					// Wrap with store.ErrFencingMismatch so the
					// finalize handler can errors.Is-detect this
					// holder-mismatch and 503 with `lock_not_self`,
					// which the source-side retry loop already
					// keys on (switch_device_handler.go's
					// finalizeErr.Error() substring check).
					return tokenReissued, fmt.Errorf("agent_lock did not transfer (holder=%q, self=%q); orchestrator should retry finalize: %w",
						holder, peerIdentity.DeviceID, store.ErrFencingMismatch)
				}
			}
			// Activate runtime side channels (cron + notify) AFTER the
			// lock guard has adopted us. Phase-1 (onAgentSynced) only
			// refreshes the in-memory cache; firing schedules before
			// finalize would have target acting on an agent the
			// orchestrator might still abort.
			//
			// NotifyDeviceSwitchArrival is deliberately NOT fired
			// here. The finalize HTTP handler fires it AFTER the
			// Plan A TailMessage has been applied to agent_messages
			// (which has to happen post-hook so the lock check
			// passes). Firing here would build the arrival prompt
			// against a transcript missing the agent's own
			// commitment text. See peer_agent_sync_finalize_handler.go
			// for the new ordering: hook → UpdateAgentLockAllowedProxy
			// → applyFinalizeTailMessage → NotifyDeviceSwitchArrival
			// → commitPendingAgentSync.
			if agentMgr != nil {
				agentMgr.ActivateAgentRuntime(agentID)
			}
			return tokenReissued, nil
		})
		// Source-side hook: after the orchestrator's complete
		// + finalize succeed, drop the agent from THIS peer's
		// AgentLockGuard.desired so target's lease expiry
		// doesn't trigger a stale re-Acquire from here. Also
		// stops the source-side SlackBot (if any) so the
		// migrated agent doesn't keep responding from this
		// peer — v1 doesn't auto-start it on target (the
		// operator re-enables via UI on the target host), but
		// stopping here avoids the duplicate-bot bug.
		srv.SetOnAgentReleasedAsSource(func(hookCtx context.Context, agentID string) {
			// ReleaseAgentLocally FIRST so the durable
			// handoff/released/<id> marker lands before any
			// step that can block or panic. capturedGuard.
			// RemoveAgent can block on store release up to
			// agentLockOpTimeout; if the source process dies
			// mid-block (or earlier, between hook entry and
			// the AgentLockGuard call) without the marker
			// written, restart eviction has no signal and the
			// source resurrects schedulers / cron / notify
			// poller for an agent target now owns. Writing the
			// marker first turns those subsequent steps into
			// best-effort cleanup of side channels that the
			// startup eviction path also covers.
			if agentMgr != nil {
				agentMgr.ReleaseAgentLocally(agentID)
			}
			if capturedGuard != nil {
				capturedGuard.RemoveAgent(hookCtx, agentID)
			}
			if hub := srv.SlackHub(); hub != nil {
				hub.StopBot(agentID)
			}
		})
		// Operator-driven force-reclaim path. After
		// handleAgentHandoffForceReclaim rewrites agent_locks
		// back to this host, the chat surface only comes back if
		// the in-memory AgentLockGuard.desired set knows about
		// the agent (so its refresh loop keeps the lease alive)
		// and the runtime side-channels (cron, notify) are
		// re-activated. Both live outside the store, so the hook
		// runs them here.
		// Target-side state-probe self-heal: peer purged the
		// agent's DB state because the orchestrator's retry
		// found stale lock pointing elsewhere. Strip every
		// in-memory side channel so the guard's refresh loop
		// doesn't immediately re-Acquire what we just deleted
		// and cron / notify / slack stop driving the agent
		// until the orchestrator's agent-sync re-adopts it.
		srv.SetOnAgentRuntimePurged(func(hookCtx context.Context, agentID string) {
			if capturedGuard != nil {
				capturedGuard.RemoveAgent(hookCtx, agentID)
			}
			if agentMgr != nil {
				agentMgr.TeardownAgentRuntime(agentID)
			}
			if hub := srv.SlackHub(); hub != nil {
				hub.StopBot(agentID)
			}
		})
		srv.SetOnAgentForceReclaimed(func(hookCtx context.Context, agentID string) {
			// Re-hydrate the in-memory cache FIRST. Without this
			// ActivateAgentRuntime's Manager.Get returns ok=false
			// (the agent was evicted when we released it as
			// source) and the call is a silent no-op — cron /
			// notify never wake up and the UI still treats the
			// agent as remote on the next list. Reload pulls the
			// row from the store, including any updates that
			// landed during the release.
			if agentMgr != nil {
				if err := agentMgr.ReloadAgentFromStore(agentID); err != nil && logger != nil {
					logger.Warn("force-reclaim: reload from store failed",
						"agent", agentID, "err", err)
				}
				// Clear the prior "this peer released the agent
				// as source" marker so the next daemon restart
				// doesn't evict the row we just reclaimed. Best-
				// effort: a stale marker only matters on the
				// next boot.
				if err := agentMgr.ClearAgentReleasedHere(hookCtx, agentID); err != nil && logger != nil {
					logger.Warn("force-reclaim: clear released marker failed",
						"agent", agentID, "err", err)
				}
			}
			if capturedGuard != nil {
				capturedGuard.AddAgent(hookCtx, agentID)
			}
			if agentMgr != nil {
				agentMgr.ActivateAgentRuntime(agentID)
			}
		})
	}

	// graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), session.ShutdownSignals()...)
	defer stop()

	// Self-restart (POST /api/v1/system/restart): after the server's
	// drain, this trigger funnels into the same ordered graceful-
	// shutdown path as SIGINT; the tail of main then re-execs the
	// (possibly rebuilt) binary in place — same PID, same terminal.
	// Not wired on platforms without exec (windows) → endpoint 501s.
	var restartRequested atomic.Bool
	// tsShutdown, when set (tailscale mode), closes the tsnet server.
	// Normally deferred; the restart path must run it explicitly
	// because a successful exec never returns to run defers.
	var tsShutdown func()
	// requestRestart is shared by POST /api/v1/system/restart and
	// peer auto-update so both funnel through the same graceful
	// path (restartRequested + stop). Nil when restart is unsupported.
	var requestRestart func() bool
	if restartSupported {
		requestRestart = func() bool {
			// If a signal already initiated shutdown, do NOT
			// convert the operator's stop into a restart — the
			// drain goroutine can fire this trigger while the
			// ordered shutdown below is already in flight.
			if ctx.Err() != nil {
				return false
			}
			restartRequested.Store(true)
			stop()
			return true
		}
		srv.SetRestartTrigger(requestRestart)
	}

	// Background sweep of expired idempotency_keys rows (3.5). The
	// dedup window is 24 h; sweeping once an hour keeps the table
	// from growing unbounded without competing with hot writes. The
	// sweep goroutine exits when ctx (the shutdown signal context)
	// is cancelled so it lifecycles with the rest of the binary.
	srv.StartIdempotencySweep(ctx)

	// Periodic GitHub Releases check. Skipped for dev/unparseable
	// stamps (never auto-notify against a dirty describe), operator
	// opt-out, or env kill-switch. The checker itself is still on
	// server.Config so the HTTP endpoints can CheckNow/Apply on demand.
	if *noUpdateCheck || os.Getenv("KOJO_NO_UPDATE_CHECK") == "1" {
		logger.Info("selfupdate: periodic check disabled by flag or KOJO_NO_UPDATE_CHECK")
	} else if _, err := selfupdate.ParseVersion(version); err != nil {
		logger.Debug("selfupdate: periodic check skipped (unparseable version)",
			"version", version, "err", err)
	} else {
		updateChecker.StartLoop(ctx, 30*time.Second, 6*time.Hour)
	}

	if *peerMode {
		// Peer mode: plain HTTP listen on the Tailscale interface
		// (preferred — keeps traffic inside the WireGuard tunnel)
		// or 0.0.0.0 as a last-resort fallback. The §3.7 device
		// switch slice promoted --peer from "daemon-only" to "full
		// peer", so this host can now run agent CLIs after a
		// handoff. The primary listener goes through ServeAuth so
		// the same port accepts BOTH peer-signed inter-peer
		// traffic (RolePeer) AND Bearer-bearing agent runtime
		// traffic from the local PTY (RoleAgent). A separate
		// 127.0.0.1 listener is added below so agent CLIs can
		// reach the API without traversing the Tailscale
		// interface — same pattern Hub mode uses.
		bindHost, tsAddr := peerBindAndAdvertise(ctx, logger)
		if *tailnetOnly && tsAddr == "" {
			// --tailnet-only is the explicit opt-in for "do NOT
			// fall back to 0.0.0.0 when Tailscale is unreachable".
			// peerBindAndAdvertise returned "" for tsAddr and
			// "0.0.0.0" for bindHost; refuse to bind so the
			// operator notices the misconfig rather than silently
			// listening on every interface.
			fmt.Fprintln(os.Stderr, "kojo: --tailnet-only set but Tailscale interface is unavailable; refusing to bind to 0.0.0.0")
			os.Exit(1)
		}
		ln, err := listenWithFallback(bindHost, *port, 10, logger)
		if err != nil {
			logger.Error("failed to listen", "err", err)
			os.Exit(1)
		}
		actualAddr := ln.Addr().String()
		// listenWithFallback may have walked past `*port` to a free
		// neighbor; pull the bound port back out so the self-row
		// advertises the address the OS actually accepted, not the
		// flag's default.
		listenPort := *port
		if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
			listenPort = tcp.Port
		}
		fmt.Fprintf(os.Stderr, "\n  kojo v%s (peer mode) running at:\n\n    http://%s\n\n", version, actualAddr)

		// Stamp the peer_registry self-row with an address the Hub /
		// other peers can dial. Preference order:
		//
		//   1. Tailscale IPv4 from `tailscale ip` (the WireGuard
		//      address — guarantees inter-peer traffic stays inside
		//      the tunnel even if the host's hostname resolves on
		//      a LAN interface as well).
		//   2. OS hostname (or the env var $HOSTNAME if set) — best-
		//      effort when tailscaled isn't reachable from the
		//      binary's environment.
		//   3. The synthetic identity name as a last resort.
		//
		// Operators who need a canonical Tailscale MagicDNS FQDN
		// (`<host>.<tailnet>.ts.net`) can override later via
		// `kojo --peer-add` on the Hub or by writing the row
		// directly — RegisterPeerMetadata preserves status /
		// last_seen so the live heartbeat is unaffected.
		if peerRegistrar != nil {
			addr := tsAddr
			if addr == "" {
				addr = os.Getenv("HOSTNAME")
			}
			if addr == "" {
				if h, herr := os.Hostname(); herr == nil {
					addr = h
				}
			}
			if addr == "" {
				addr = peerIdentity.Name // last resort
			}
			// Scheme prefix is mandatory in peer mode so the
			// Hub-side Subscriber knows to dial plain HTTP rather
			// than try a TLS handshake the daemon-only peer
			// listener will refuse. Hub-side self-rows omit the
			// scheme to match the historical Tailscale TLS shape
			// (peerSubscriberTargetsLoop defaults scheme-less
			// names to https://).
			publicURL := fmt.Sprintf("http://%s:%d", addr, listenPort)
			peerRegistrar.SetPublicURL(publicURL)
			refreshCtx, refreshCancel := context.WithTimeout(ctx, 5*time.Second)
			if rerr := peerRegistrar.RefreshPublicName(refreshCtx); rerr != nil {
				logger.Warn("peer self-row refresh failed; relying on heartbeat-loop retry",
					"err", rerr)
			}
			refreshCancel()

			// Print the pairing triple to stderr so the operator can
			// paste it into the Hub's `kojo --peer-add` (replacing
			// the old `kojo --peer-self` subcommand). Format matches
			// the --peer-add spec exactly.
			printPairingSpec(peerIdentity, publicURL, "peer")
		}

		// Wire the OS tailscaled WhoIs into the Server's tsnet
		// identity middleware. Peer mode does not run a tsnet.Server
		// of its own — it borrows the host's tailscaled — so we hit
		// the LocalAPI socket via tailscale.com/client/local. If
		// tailscaled is not reachable (LAN-only deploy), the
		// resolver returns ErrNoNodeKey and the middleware admits
		// the caller as Guest; `--unsafe` is the supported escape
		// hatch in that case.
		if !*unsafePeer {
			srv.SetNodeKeyResolver(func(ctx context.Context, remoteAddr string) (string, error) {
				w, err := localTailscale.WhoIs(ctx, remoteAddr)
				if err != nil {
					return "", err
				}
				if w == nil || w.Node == nil {
					return "", nil
				}
				return w.Node.Key.String(), nil
			})
			go captureSelfNodeKeyFromOSTailscale(ctx, srv, peerRegistrar, logger)
		}

		go func() {
			// peer-mode primary listener: tsnet + auth chain. Tailnet
			// identity stamps paired peers as RolePeer (peer_registry
			// hit via WhoIs) so policy.AllowNonOwner admits the
			// inter-peer surface; agent loopback Bearer / X-Kojo-Token
			// flows continue to work via AuthMiddleware below it.
			if err := srv.ServeAuthTsnet(ln, resolver); err != nil && err != http.ErrServerClosed {
				logger.Error("server error", "err", err)
				stop()
			}
		}()

		// Auto-onboarding (docs/peer-onboarding-plan.md). Spawn a
		// long-running Discovery goroutine that resolves the Hub
		// URL (--hub / KOJO_HUB_URL / MagicDNS), writes the Hub row
		// into our local peer_registry (trusted=true), and POSTs
		// /api/v1/peers/join-request every 60s until Owner Approve.
		// Best-effort: nil identity or nil store skips silently —
		// the existing pairing-spec banner above gives the operator
		// the fallback path.
		if peerIdentity != nil && agentMgr != nil && agentMgr.Store() != nil {
			peerPublicURL := fmt.Sprintf("http://%s:%d", func() string {
				if tsAddr != "" {
					return tsAddr
				}
				if h := os.Getenv("HOSTNAME"); h != "" {
					return h
				}
				if h, herr := os.Hostname(); herr == nil && h != "" {
					return h
				}
				return peerIdentity.Name
			}(), listenPort)
			// DefaultHubPort is the plan-mandated 8080 default for
			// the MagicDNS form. Peer's own --port is unrelated —
			// peer + Hub run independent listeners. KOJO_HUB_PORT
			// env still overrides inside Discovery.
			disco, derr := peer.NewDiscovery(peer.DiscoveryConfig{
				HubURLOverride: *hubURL,
				DefaultHubPort: 8080,
				PeerPublicURL:  peerPublicURL,
				SelfVersion:    version,
				AutoUpdate:     !*noPeerAutoUpdate && os.Getenv("KOJO_NO_PEER_AUTOUPDATE") != "1",
				RequestRestart: requestRestart,
			}, peerIdentity, agentMgr.Store(), logger)
			if derr != nil {
				logger.Warn("peer discovery: init failed; auto-onboarding disabled",
					"err", derr)
			} else {
				go disco.Run(ctx)
			}
		}

		// Second listener on 127.0.0.1:<port+1> so agent CLIs on
		// this host can reach the API without going out and back
		// in through the Tailscale interface. Same pattern Hub
		// mode uses below; agentAPIBase is stamped into the PTY's
		// $KOJO_API_BASE env so the CLI's curl examples target
		// the loopback listener with a Bearer token.
		authLn, err := listenWithFallback("127.0.0.1", *port+1, 10, logger)
		if err != nil {
			logger.Error("failed to listen on agent loopback auth port", "err", err)
			os.Exit(1)
		}
		agentAPIBase := "http://" + authLn.Addr().String()
		agent.SetKojoAPIBase(agentAPIBase)
		groupDMMgr.SetAPIBase(agentAPIBase)
		fmt.Fprintf(os.Stderr, "    agent API: %s  (Bearer required)\n\n", agentAPIBase)
		go func() {
			if err := srv.ServeAuth(authLn, resolver); err != nil && err != http.ErrServerClosed {
				logger.Error("peer agent loopback listener error", "err", err)
				stop()
			}
		}()
	} else if *local || *dev {
		ln, err := listenWithFallback("127.0.0.1", *port, 10, logger)
		if err != nil {
			logger.Error("failed to listen", "err", err)
			os.Exit(1)
		}
		actualAddr := ln.Addr().String()

		if *noAuth {
			// --no-auth (--local/--dev only): the loopback listener is
			// Owner-trusted. Suitable for hacking on the UI itself; not
			// suitable for running real agents because they can read
			// the full API as Owner. Group DM curl examples stay
			// pointed at this listener.
			fmt.Fprintf(os.Stderr, "\n  kojo v%s running at:\n\n    http://%s\n\n", version, actualAddr)
			groupDMMgr.SetAPIBase("http://" + actualAddr)
			logger.Warn("agent auth listener disabled via --no-auth — agents can read full API as Owner")
			go func() {
				if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
					logger.Error("server error", "err", err)
					stop()
				}
			}()
		} else {
			// Single auth-required listener. The UI and agents share
			// the same port; the difference is the Bearer token they
			// present (Owner vs per-agent). On first visit the UI
			// follows the printed URL whose ?token= query param
			// bootstraps the Owner token into localStorage.
			agentAPIBase := "http://" + actualAddr
			agent.SetKojoAPIBase(agentAPIBase)
			groupDMMgr.SetAPIBase(agentAPIBase)
			// KOJO_OWNER_TOKEN may be a custom string; URL-escape so
			// special chars (& # %) don't break the link.
			ownerTok := url.QueryEscape(tokens.OwnerToken())
			fmt.Fprintf(os.Stderr,
				"\n  kojo v%s running at:\n\n    http://%s\n\n  open this URL once to authorize the UI:\n    http://%s/?token=%s\n\n",
				version, actualAddr, actualAddr, ownerTok)
			go func() {
				if err := srv.ServeAuth(ln, resolver); err != nil && err != http.ErrServerClosed {
					logger.Error("server error", "err", err)
					stop()
				}
			}()
		}
	} else {
		// tailscale mode: listen via tsnet with HTTPS. Hostname
		// is configurable via --hostname so multiple kojo binaries
		// can join the same tailnet without colliding on the
		// default "kojo" name (Tailscale would otherwise suffix
		// duplicates as "kojo-1" etc., making the URL printed at
		// startup unstable).
		tsHost := strings.TrimSpace(*hostname)
		if tsHost == "" {
			tsHost = "kojo"
		}
		tsServer := &tsnet.Server{
			Hostname: tsHost,
			Logf:     func(format string, args ...any) { logger.Debug(fmt.Sprintf(format, args...)) },
		}

		ln, err := tsServer.ListenTLS("tcp", fmt.Sprintf(":%d", *port))
		if err != nil {
			logger.Error("failed to listen on tailscale", "err", err)
			os.Exit(1)
		}

		// get tailscale addresses for display. Display only — the agent
		// API base is wired further below to the local auth listener so
		// system prompts / PreCompact curl examples point Bearer-required.
		fmt.Fprintf(os.Stderr, "\n  kojo v%s running at:\n\n", version)
		lc, lcErr := tsServer.LocalClient()
		if lcErr != nil {
			logger.Warn("could not get tailscale local client", "err", lcErr)
		}
		if lc != nil {
			// Status() may not be populated immediately after
			// ListenTLS returns — tsnet finishes its login flow
			// asynchronously. Print synchronously what we have
			// now, then retry in the background to refresh the
			// peer_registry self-row once the FQDN is known.
			if status, err := lc.Status(ctx); err == nil {
				printTailscaleAddrs(status, *port)
			} else {
				logger.Warn("could not get tailscale status", "err", err)
				fmt.Fprintf(os.Stderr, "    https://%s.<tailnet>.ts.net:%d  (getting status...)\n", tsHost, *port)
			}
			// Wire the WhoIs-backed identity resolver into the
			// Server now that LocalClient is ready. The closure
			// stringifies the resolved tailcfg.Node.Key into the
			// `nodekey:...` form peer_registry.node_key stores.
			srv.SetNodeKeyResolver(func(ctx context.Context, remoteAddr string) (string, error) {
				w, err := lc.WhoIs(ctx, remoteAddr)
				if err != nil {
					return "", err
				}
				if w == nil || w.Node == nil {
					return "", nil
				}
				return w.Node.Key.String(), nil
			})
			// Capture self NodeKey in the background.
			//
			// Two distinct NodeKeys are at play in Hub mode:
			//
			//   1. tsnet.Server's NodeKey — the identity bound to the
			//      tsnet listener (kojo.<tailnet>.ts.net). srv uses this
			//      via SelfNodeKeyFunc to demote self-loop requests
			//      that come back in through the tsnet listener.
			//
			//   2. The host's tailscaled NodeKey — the identity OUTBOUND
			//      HTTP requests originate from. Hub's outbound peer
			//      dials use http.DefaultTransport, NOT tsnet.Server.Dial,
			//      so the source IP/NodeKey peers observe (and resolve
			//      via their WhoIs) is the host's tailscaled node, not
			//      the tsnet.Server. This is what we MUST advertise via
			//      hub-info so peer_registry rows on the receiving end
			//      key off the same NodeKey the peer's WhoIs returns —
			//      a tsnet NodeKey there would never match an inbound
			//      Hub call and every §3.7 inter-peer surface (notably
			//      /agent-sync/state) would 403 forbidden.
			go captureSelfNodeKeyFromTailscale(ctx, lc, srv, nil, logger)
			go captureOSSelfNodeKeyForRegistrar(ctx, peerRegistrar, logger)
			// Background refresh: bounded retry until tsnet
			// reports a DNSName, then SetPublicName +
			// RefreshPublicName so other peers' Subscriber can
			// dial us. Without this, self-row stays at
			// id.Name (OS hostname) for the binary's lifetime
			// when Status() was momentarily not ready.
			if peerRegistrar != nil {
				go refreshPublicNameFromTailscale(ctx, lc, peerRegistrar, *port, logger)
				go printHubPairingSpecOnce(ctx, lc, peerIdentity, *port, logger)
			}
		}

		// tsnet.ListenTLS returns a tls.Listener, serve directly
		go func() {
			// ServeTLS with empty cert/key since TLS is already handled by the listener
			srv.SetTLSConfig(&tls.Config{})
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				logger.Error("server error", "err", err)
				stop()
			}
		}()

		// Additional auth-required listener bound to loopback. Agents
		// running in PTY sessions reach this via $KOJO_API_BASE; the
		// Tailscale listener above stays open for the user UI without
		// any token requirement, preserving the original UX.
		authLn, err := listenWithFallback("127.0.0.1", *port+1, 10, logger)
		if err != nil {
			logger.Error("failed to listen on auth port", "err", err)
			os.Exit(1)
		}
		authAddr := authLn.Addr().String()
		agentAPIBase := "http://" + authAddr
		agent.SetKojoAPIBase(agentAPIBase)
		// Group DM / PreCompact / system-prompt curl examples must hit
		// the auth listener so the agent's Bearer is honored. The
		// Tailscale listener is for the user UI only.
		groupDMMgr.SetAPIBase(agentAPIBase)
		fmt.Fprintf(os.Stderr, "    agent API: %s  (Bearer required)\n\n", agentAPIBase)
		go func() {
			if err := srv.ServeAuth(authLn, resolver); err != nil && err != http.ErrServerClosed {
				logger.Error("auth listener error", "err", err)
				stop()
			}
		}()

		tsShutdown = sync.OnceFunc(func() { _ = tsServer.Close() })
		defer tsShutdown()
	}

	// Consume a restart-wake marker if the previous process armed one
	// (POST /api/v1/system/restart {"wake":true}) — fires ONE chat turn
	// for the marked agent so it can verify its own deploy. Placed after
	// every listener branch so agent.SetKojoAPIBase is already wired
	// into the system prompt the woken turn will be built with. The
	// timestamp fences the consumer to pre-boot markers only.
	go agentMgr.ConsumeRestartWake(version, time.Now())

	<-ctx.Done()
	logger.Info("received shutdown signal")

	// Shutdown ordering (each step is bounded internally so a stuck
	// step cannot stall the rest):
	//
	//   1. Stop the OfflineSweeper. Without it a final sweep could
	//      race the registrar's offline-touch below and re-flip
	//      whichever value just landed.
	//   2. Drain HTTP listeners + in-flight handlers (srv.Shutdown).
	//      Until this returns the agent runtime may still be
	//      writing, so dropping the agent_locks any earlier would
	//      hand the row to another peer while we still issued
	//      writes against it.
	//   3. Release every agent_lock this peer holds. Other peers
	//      can immediately steal — by step 2 we no longer write.
	//   4. Stop the registrar (final offline-touch). After this the
	//      registry says we are offline and the sweeper-equivalent
	//      on other peers won't even need to wait for the lease.
	if peerSweeper != nil {
		peerSweeper.Stop()
	}
	if peerSubscriberTargetsCancel != nil {
		peerSubscriberTargetsCancel()
	}
	if peerSubscriber != nil {
		peerSubscriber.Stop()
	}
	if blobScrubber != nil {
		blobScrubber.Stop()
	}

	// Drain HTTP listeners + in-flight handlers BEFORE releasing
	// agent_locks: another peer that steals a freed lock and starts
	// writing must not race a write we're still letting finish here.
	// Manager.Update / message append / etc. all flow through srv,
	// so its Shutdown is the natural fence.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "err", err)
	}

	// Now safe to drop locks. Stops the refresh loop and runs
	// ReleaseAgentLockByPeer so other peers can pick up the locks
	// without waiting for the lease to expire.
	if agentLockGuard != nil {
		agentLockGuard.Stop()
	}
	if peerRegistrar != nil {
		peerRegistrar.Stop()
	}

	if restartRequested.Load() {
		// exec never returns on success, so deferred cleanups would
		// be skipped — run them explicitly before swapping the
		// process image. Both tolerate a second call from the defers
		// if exec fails. A Close error doesn't cancel the exec:
		// process exit loses the same buffered state either way, and
		// coming back up beats staying down.
		if tsShutdown != nil {
			tsShutdown()
		}
		if err := agentMgr.Close(); err != nil {
			logger.Warn("restart: agent manager close", "err", err)
		}
		execRestart(logger)
		// exec failed. Exit non-zero so a supervising wrapper (or the
		// operator's terminal) sees the restart did not happen rather
		// than a clean stop.
		os.Exit(1)
	}
}

// printTailscaleAddrs writes the human-facing "running at"
// lines for whatever Status() reports right now. Split from the
// inline call so the background refresh loop doesn't re-print.
func printTailscaleAddrs(status *ipnstate.Status, port int) {
	if status == nil {
		return
	}
	if status.Self != nil {
		dnsName := strings.TrimSuffix(status.Self.DNSName, ".")
		if dnsName != "" {
			if port == 443 {
				fmt.Fprintf(os.Stderr, "    https://%s\n", dnsName)
			} else {
				fmt.Fprintf(os.Stderr, "    https://%s:%d\n", dnsName, port)
			}
		}
	}
	for _, ip := range status.TailscaleIPs {
		fmt.Fprintf(os.Stderr, "    https://%s\n", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	}
}

// printHubPairingSpecOnce waits for tsnet to report a stable
// DNSName, then prints the Hub's pairing spec (deviceID|name|url|
// publicKey) to stderr exactly once. Mirrors the `--peer` mode's
// stderr banner so the operator can run `kojo --peer-add` on every
// peer host. Pairing is bidirectional — without the Hub's row on a
// peer's registry, the Hub's signed register-push and Hub→peer
// session-proxy requests fail at the peer's PeerAuth middleware
// with 401.
func printHubPairingSpecOnce(ctx context.Context, lc tailscaleLocalClient, id *peer.Identity, port int, logger *slog.Logger) {
	if id == nil {
		return
	}
	const maxAttempts = 60
	backoff := 1 * time.Second
	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		st, err := lc.Status(ctx)
		if err == nil && st != nil && st.Self != nil {
			dnsName := strings.TrimSuffix(st.Self.DNSName, ".")
			if dnsName != "" {
				printPairingSpec(id, fmt.Sprintf("%s:%d", dnsName, port), "hub")
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	if logger != nil {
		logger.Warn("hub pairing spec: tsnet FQDN not available after retries; skipping --peer-add banner",
			"attempts", maxAttempts)
	}
}

// refreshPublicNameFromTailscale polls LocalClient.Status() until
// a DNSName comes back (or the context is cancelled), then calls
// SetPublicName + RefreshPublicName on the registrar. Without
// this, a transient Status() error at startup would leave the
// peer_registry self-row at id.Name (= os.Hostname()) for the
// binary's lifetime and other peers' Subscriber would fail to
// dial us by Tailscale FQDN.
func refreshPublicNameFromTailscale(ctx context.Context, lc tailscaleLocalClient, reg *peer.Registrar, port int, logger *slog.Logger) {
	const maxAttempts = 60
	backoff := 1 * time.Second
	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		st, err := lc.Status(ctx)
		if err == nil && st != nil && st.Self != nil {
			dnsName := strings.TrimSuffix(st.Self.DNSName, ".")
			if dnsName != "" {
				// peer_registry.name must always carry the explicit
				// port. peer-self's dial-address shape gate
				// (isPeerRegistryDialAddress) rejects bare-FQDN names
				// because a remote Subscriber would otherwise have to
				// guess the default port — and our default is 8080,
				// not 443. Don't elide the port even when it matches
				// HTTPS default; the row is consumed by code, not
				// rendered to users.
				addr := fmt.Sprintf("%s:%d", dnsName, port)
				reg.SetPublicURL(addr)
				rCtx, rCancel := context.WithTimeout(context.Background(), 10*time.Second)
				if rerr := reg.RefreshPublicName(rCtx); rerr != nil && logger != nil {
					logger.Warn("peer self-row refresh failed", "err", rerr)
				}
				rCancel()
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	if logger != nil {
		logger.Warn("peer self-row: tsnet FQDN not available after retries; peer_registry.name stays at OS hostname",
			"attempts", maxAttempts)
	}
}

// tailscaleLocalClient narrows the LocalClient surface to just
// the Status method so test fixtures can inject a fake.
type tailscaleLocalClient interface {
	Status(ctx context.Context) (*ipnstate.Status, error)
}

// captureSelfNodeKeyFromOSTailscale is the peer-mode twin of
// captureSelfNodeKeyFromTailscale. It calls into the host's
// tailscaled LocalAPI (no tsnet.Server here) and stamps the
// resolved Self NodeKey on the Server AND on the Registrar so
// the peer_registry self-row carries node_key.
func captureSelfNodeKeyFromOSTailscale(ctx context.Context, srv *server.Server, reg *peer.Registrar, logger *slog.Logger) {
	const maxAttempts = 30
	for i := 0; i < maxAttempts; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, err := localTailscale.Status(statusCtx)
		cancel()
		if err == nil && st != nil && st.Self != nil {
			nk := st.Self.PublicKey.String()
			if nk != "" {
				srv.SetSelfNodeKey(nk)
				if reg != nil {
					reg.SetSelfNodeKey(nk)
					refreshCtx, refreshCancel := context.WithTimeout(ctx, 5*time.Second)
					_ = reg.RefreshPublicName(refreshCtx)
					refreshCancel()
				}
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	if logger != nil {
		logger.Debug("could not capture self Tailscale NodeKey from OS tailscaled; same-host Owner promotion disabled")
	}
}

// captureOSSelfNodeKeyForRegistrar polls the host's tailscaled
// LocalAPI until Self.NodeKey is populated, then stamps it ONLY on
// the Registrar (peer_registry self-row + hub-info advertisement).
//
// Hub mode runs a tsnet.Server with its own NodeKey distinct from
// the host's tailscaled NodeKey, but outbound HTTP from this
// process goes through http.DefaultTransport — not tsnet's dialer —
// so the source NodeKey peers observe on Hub-outbound calls is the
// host's tailscaled NodeKey. peer_registry rows on the receiving
// end key off the WhoIs-resolved NodeKey, so the value Hub
// advertises via hub-info MUST be the host's tailscaled NodeKey
// (not tsnet.Server's). Without this the §3.7 inter-peer surface
// (POST /api/v1/peers/agent-sync/state, /agent-sync, /agent-sync/
// finalize|drop, /peers/pull) all 403 forbidden on the target
// because its peer_registry lookup misses.
//
// Boot-time the self-row's node_key column is actively wiped (Step
// 1 below) BEFORE OS LocalAPI polling starts, so hub-info advertises
// an empty NodeKey while the host tailscaled answer is still in
// flight — peer Discovery treats empty as "wait" and never latches
// onto a stale value left by a previous binary. If the OS LocalAPI
// never resolves (no host tailscaled installed or running), the
// column stays empty for the binary's lifetime; Hub outbound also
// can't reach peers in that configuration, so the broken pairing is
// moot.
func captureOSSelfNodeKeyForRegistrar(ctx context.Context, reg *peer.Registrar, logger *slog.Logger) {
	if reg == nil {
		return
	}
	// Step 1: actively wipe the existing self-row NodeKey. A previous
	// binary may have stamped tsnet.Server's NodeKey here; if we
	// merely overwrite when the OS LocalAPI eventually succeeds,
	// hub-info would keep advertising the stale tsnet value in the
	// interim and peer Discovery (which exits once approved + NodeKey
	// non-empty) would either skip the refresh entirely (already
	// approved from a previous boot) or latch onto the stale value.
	// Setting it to NULL up-front means hub-info advertises an
	// empty NodeKey until we've confirmed the host tailscaled one,
	// which peer Discovery treats as "wait" rather than "use stale".
	clearCtx, clearCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := reg.ClearSelfNodeKey(clearCtx); err != nil {
		if logger != nil {
			logger.Warn("could not wipe stale Hub self-row NodeKey before OS LocalAPI capture", "err", err)
		}
	}
	clearCancel()

	// Step 2: poll the OS LocalAPI until Self.NodeKey lands.
	// Cadence: 2s for the first 30 attempts (fast path for the
	// common case where tailscaled is already running at boot),
	// then 60s indefinitely until ctx cancellation. A short-circuit
	// "give up at 30" loses recoverability when tailscaled comes
	// online later (laptop wake, daemon restart, network resume) —
	// hub-info would keep advertising an empty NodeKey and the
	// §3.7 inter-peer surface would stay 403'd until kojo itself
	// is restarted.
	attempts := 0
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, err := localTailscale.Status(statusCtx)
		cancel()
		if err == nil && st != nil && st.Self != nil {
			nk := st.Self.PublicKey.String()
			if nk != "" {
				reg.SetSelfNodeKey(nk)
				// Step 3: persist via RefreshPublicName, retrying on
				// transient DB failure. Discarding the error here
				// would leave reg's in-memory NodeKey populated but
				// the DB row (which hub-info reads) at the stale-
				// blank state — peers would never learn the right
				// NodeKey.
				if !persistSelfNodeKey(ctx, reg, logger) {
					return
				}
				if logger != nil {
					logger.Debug("captured host tailscaled NodeKey for Hub self-row", "nodekey", nk)
				}
				return
			}
		}
		attempts++
		if attempts == 30 && !warned && logger != nil {
			logger.Warn("host tailscaled NodeKey still unavailable after 60s; switching to slow-poll. Hub→peer calls will be rejected as Guest until the OS tailscaled LocalAPI starts answering")
			warned = true
		}
		var delay time.Duration
		if attempts < 30 {
			delay = 2 * time.Second
		} else {
			delay = 60 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// persistSelfNodeKey retries reg.RefreshPublicName until the DB
// write succeeds or ctx is cancelled. The OS NodeKey capture has
// already updated reg's in-memory cache; without successful
// persistence, hub-info (which reads the DB row, not the in-memory
// value) would keep advertising the stale-blank NodeKey and peer
// Discovery would loop forever waiting for a non-empty value.
// Returns true on success, false on ctx cancellation.
func persistSelfNodeKey(ctx context.Context, reg *peer.Registrar, logger *slog.Logger) bool {
	const maxAttempts = 30
	for i := 0; i < maxAttempts; i++ {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		refreshCtx, refreshCancel := context.WithTimeout(ctx, 5*time.Second)
		err := reg.RefreshPublicName(refreshCtx)
		refreshCancel()
		if err == nil {
			return true
		}
		if logger != nil {
			logger.Warn("Hub self-row NodeKey persist failed; retrying", "attempt", i+1, "err", err)
		}
		time.Sleep(2 * time.Second)
	}
	if logger != nil {
		logger.Warn("Hub self-row NodeKey persist gave up; hub-info will advertise empty NodeKey")
	}
	return false
}

// captureSelfNodeKeyFromTailscale polls LocalClient.Status() until
// Self.NodeKey is populated, then stamps it on the Server (and,
// when reg is non-nil, on the Registrar so peer_registry's self-row
// carries node_key — peer mode wires it this way; Hub mode passes
// nil because the value advertised via hub-info has to be the
// host's tailscaled NodeKey, not tsnet's, see
// captureOSSelfNodeKeyForRegistrar).
func captureSelfNodeKeyFromTailscale(ctx context.Context, lc tailscaleLocalClient, srv *server.Server, reg *peer.Registrar, logger *slog.Logger) {
	const maxAttempts = 30
	for i := 0; i < maxAttempts; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, err := lc.Status(statusCtx)
		cancel()
		if err == nil && st != nil && st.Self != nil {
			nk := st.Self.PublicKey.String()
			if nk != "" {
				srv.SetSelfNodeKey(nk)
				if reg != nil {
					reg.SetSelfNodeKey(nk)
					refreshCtx, refreshCancel := context.WithTimeout(ctx, 5*time.Second)
					_ = reg.RefreshPublicName(refreshCtx)
					refreshCancel()
				}
				if logger != nil {
					logger.Debug("captured self Tailscale NodeKey", "nodekey", nk)
				}
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	if logger != nil {
		logger.Warn("could not capture self Tailscale NodeKey after retries; same-host Owner promotion disabled")
	}
}

// peerSubscriberTargetsLoop reconciles the Subscriber's target
// set against the live peer_registry rows every
// peerSubscriberPollInterval. Targets are derived from the peer's
// row in the same kv/sqlite — the URL is the row's "name" field
// resolved as a Tailscale DNS hostname (the convention v1 wires:
// every peer's host name == its Tailscale name).
//
// Single-peer clusters resolve to an empty target list and the
// Subscriber sits idle. Multi-peer clusters get one WS
// reconnect-loop per remote peer.
//
// Errors listing the registry are logged at Warn and the loop
// retries on its next tick — a transient DB lock shouldn't drop
// every subscription.
const peerSubscriberPollInterval = 30 * time.Second

func peerSubscriberTargetsLoop(ctx context.Context, st *store.Store, self *peer.Identity, sub *peer.Subscriber, logger *slog.Logger) {
	if st == nil || self == nil || sub == nil {
		return
	}
	reconcile := func() {
		listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		rows, err := st.ListPeers(listCtx, store.ListPeersOptions{})
		cancel()
		if err != nil {
			if logger != nil {
				logger.Warn("peer.Subscriber: peer_registry list failed",
					"err", err)
			}
			return
		}
		targets := make([]peer.SubscriberTarget, 0, len(rows))
		for _, r := range rows {
			if r.DeviceID == self.DeviceID {
				continue // never self-subscribe
			}
			// Convention: peer_registry.url is the Tailscale
			// FQDN, optionally suffixed with ":port" when the
			// peer listens on a non-443 port (kojo default
			// 8080). Operator stamps this via the pairing spec
			// `<id>|<name>|<url>|<pubkey>`; the pipe separator
			// keeps the colon in `host:port` from colliding
			// with the field delimiter.
			if r.URL == "" {
				continue
			}
			// URL MAY carry an explicit scheme prefix
			// ("http://host:port" / "https://host:port"). A
			// scheme-less URL is treated as https (the historical
			// Hub-side Tailscale TLS shape). Daemon-only peers
			// (`kojo --peer`) publish "http://host:port" so the
			// Subscriber dials plain HTTP over the tailnet — the
			// WireGuard layer is the encryption boundary; no
			// TLS handshake would succeed against the peer's
			// plain HTTP listener anyway.
			//
			// Anything else — `ws://...`, `foo://...`, an unparseable
			// authority — is rejected with a warning and the row
			// is skipped. We do NOT want to silently splice
			// "https://" in front of "ws://..." and produce a
			// nonsense URL the Subscriber will mis-dial.
			addr, err := peer.NormalizeAddress(r.URL)
			if err != nil {
				logger.Warn("peer subscriber: dropping unusable peer_registry.url",
					"url", r.URL, "err", err)
				continue
			}
			targets = append(targets, peer.SubscriberTarget{
				DeviceID: r.DeviceID,
				Address:  addr,
			})
		}
		sub.SetTargets(targets)
	}
	reconcile() // initial sync
	t := time.NewTicker(peerSubscriberPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reconcile()
		}
	}
}

// peerBindAndAdvertise resolves the Tailscale IPv4 the OS tailscaled
// reports for this host. The returned bindHost is the address to pass
// to listenWithFallback (Tailscale IP when known, 0.0.0.0 as the
// fallback so the daemon still boots in CI / docker / unconfigured
// environments). The returned advertise is the same Tailscale IP as
// a string for stamping into the peer_registry self-row; "" means
// the caller should fall back to hostname-based advertising.
//
// We shell out to `tailscale ip -4` rather than dialing the LocalAPI
// socket because the path varies across platforms (macOS sandboxes
// it under the GUI app bundle) and the CLI hides those details for
// us. Failure paths log a warning and fall back to 0.0.0.0 — the
// peer middleware still rejects unsigned traffic, but the operator
// is told that the listen surface is no longer tailnet-only.
func peerBindAndAdvertise(parent context.Context, logger *slog.Logger) (bindHost, advertise string) {
	// Bound the CLI exec so a stuck tailscaled / LocalAPI doesn't
	// pin the peer daemon's startup forever. 3s is generous —
	// `tailscale ip -4` is a single LocalAPI call.
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "ip", "-4").Output()
	if err != nil {
		logger.Warn("peer: could not read Tailscale IPv4 (falling back to 0.0.0.0 bind); install tailscale CLI or set up tailscaled to keep inter-peer traffic inside WireGuard",
			"err", err)
		return "0.0.0.0", ""
	}
	ip := strings.TrimSpace(string(out))
	// `tailscale ip -4` may emit multiple lines on some setups; pick
	// the first non-empty token and parse it strictly so a stray
	// banner / "logged out" line doesn't end up in the listen addr.
	if nl := strings.IndexByte(ip, '\n'); nl >= 0 {
		ip = strings.TrimSpace(ip[:nl])
	}
	if parsed, perr := netip.ParseAddr(ip); perr != nil || !parsed.IsValid() {
		logger.Warn("peer: Tailscale CLI returned unparseable IP (falling back to 0.0.0.0 bind)",
			"raw", ip, "err", perr)
		return "0.0.0.0", ""
	}
	return ip, ip
}

func listenWithFallback(host string, startPort, maxAttempts int, logger *slog.Logger) (net.Listener, error) {
	for i := range maxAttempts {
		port := startPort + i
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			if i > 0 {
				logger.Info("port was busy, using fallback", "requested", startPort, "actual", port)
			}
			return ln, nil
		}
		if !strings.Contains(err.Error(), "address already in use") {
			return nil, err
		}
	}
	return nil, fmt.Errorf("all ports %d-%d are in use", startPort, startPort+maxAttempts-1)
}
