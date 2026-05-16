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
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/configdir"
	"github.com/loppo-llc/kojo/internal/eventbus"
	"github.com/loppo-llc/kojo/internal/notify"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/server"
	"github.com/loppo-llc/kojo/internal/session"
	"github.com/loppo-llc/kojo/internal/store"
	"github.com/loppo-llc/kojo/internal/store/secretcrypto"
	"github.com/loppo-llc/kojo/web"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

var version = "0.19.0"

func main() {
	port := flag.Int("port", 8080, "port number (auto-increments if busy)")
	dev := flag.Bool("dev", false, "enable dev mode (proxy to Vite)")
	local := flag.Bool("local", false, "listen on localhost only (no Tailscale)")
	hostname := flag.String("hostname", "kojo", "tsnet machine name (becomes <name>.<tailnet>.ts.net). Ignored in --local / --dev mode")
	configDir := flag.String("config-dir", "", "override config directory (default: ~/.config/kojo-v1)")
	showVersion := flag.Bool("version", false, "show version")
	noAuth := flag.Bool("no-auth", false, "disable agent-facing auth listener (--local/--dev only)")

	// v1 migration flags (docs/multi-device-storage.md §5).
	doMigrate := flag.Bool("migrate", false, "import v0 data into v1 dir, then exit")
	migrateRestart := flag.Bool("migrate-restart", false, "discard a partially-imported v1 dir and re-import from v0 (mutually exclusive with --migrate)")
	freshInstall := flag.Bool("fresh", false, "skip v0 import and start v1 from scratch (refuses if v1 dir already populated)")
	migrateExternalCLI := flag.Bool("migrate-external-cli", true, "with --migrate: link external CLI transcripts (claude/codex symlink, gemini projects.json) so prior context is reachable")
	migrateBackup := flag.String("migrate-backup", "", "with --migrate: write a read-only zip of v0 to PATH before importing")
	migrateForceRecentMtime := flag.Bool("migrate-force-recent-mtime", false, "with --migrate: bypass the v0 mtime safety window (5min). Use when v0 is confirmed dead but a recent `ls`/backup-extract bumped a file's timestamp; mid-import data races silently corrupt if v0 is actually live")
	rollbackExternal := flag.Bool("rollback-external-cli", false, "revert --migrate-external-cli changes; required before booting a v0 binary again")

	peerMode := flag.Bool("peer", false, "run as a daemon-only peer: bind plain HTTP to 0.0.0.0:<port>, expose ONLY /api/v1/peers/events + /api/v1/peers/blobs/* (Ed25519-signed inter-peer requests), skip the web UI / dev proxy / Hub-side routes / tsnet. The tailnet identity is borrowed from the OS tailscaled (no --hostname). Mutually exclusive with --dev / --local / --no-auth.")
	peerList := flag.Bool("peer-list", false, "list peer_registry rows and exit (read-only; coexists with running kojo)")
	peerAdd := flag.String("peer-add", "", "register a remote peer in peer_registry; spec = <device_id>|<name>|<base64-public-key>; run `kojo --peer-self` on the other host to obtain its triple. The pipe `|` separator lets <name> hold `host:port` for Tailscale FQDNs.")
	peerRemove := flag.String("peer-remove", "", "delete a peer_registry row by device_id; refuses to remove self")
	peerSelf := flag.Bool("peer-self", false, "print this binary's identity triple (for paste into another host's --peer-add)")
	doSnapshot := flag.Bool("snapshot", false, "take a point-in-time snapshot of kojo.db + blobs/global into <configdir>/snapshots/<ts>/ and exit")
	doRestore := flag.String("restore", "", "restore a snapshot (verified sha256) into <configdir>. The configdir must not be in use by a running kojo. KEK and per-peer credentials are NOT restored — supply them out-of-band before booting.")
	restoreForce := flag.Bool("restore-force", false, "with --restore: overwrite an existing kojo.db in the target. Required when the target is a previously-used Hub being re-seeded.")
	doClean := flag.String("clean", "", "housekeeping: 'snapshots' | 'legacy' | 'v0' (soft-delete v0 dir post-migration; not included in 'all') | 'v0-trash' (purge kojo.deleted-<ts>/ dirs; not included in 'all') | 'all' (v2 adds 'blobs', 'agents'). dry-run by default")
	cleanApply := flag.Bool("clean-apply", false, "with --clean: actually delete the listed entries (default is dry-run)")
	cleanMaxAgeDays := flag.Int("clean-max-age-days", 7, "with --clean snapshots: anything older than N days (and not in --clean-keep-latest) is dropped")
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
	if *peerList || *peerAdd != "" || *peerRemove != "" || *peerSelf {
		if *configDir != "" {
			configdir.Set(*configDir)
		}
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
		switch {
		case *peerList:
			os.Exit(runPeerListCommand(logger, configdir.Path()))
		case *peerSelf:
			os.Exit(runPeerSelfCommand(logger, configdir.Path()))
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
		if *configDir != "" {
			configdir.Set(*configDir)
		}
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
		os.Exit(runSnapshotCommand(logger, configdir.Path()))
	}

	if *doRestore != "" {
		// Restore is destructive — runs an exclusive configdir lock
		// so a concurrent kojo boot can't interleave with the copy.
		// Operator must stop the live daemon first (the lock check
		// surfaces a friendly error otherwise). KEK / per-peer
		// credentials are NOT restored; the operator supplies those
		// out-of-band per docs/snapshot-restore.md.
		if *configDir != "" {
			configdir.Set(*configDir)
		}
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
		os.Exit(runRestoreCommand(logger, *doRestore, configdir.Path(), *restoreForce))
	}

	if *doClean != "" {
		if *configDir != "" {
			configdir.Set(*configDir)
		}
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
	}))

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
			migrateBackup:           *migrateBackup,
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
			logger.Warn("tmux not found in PATH; user tool sessions (claude, codex, gemini) will not work")
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

	// Short-lived WebDAV token store (docs §3.4 / §5.6). Wired only
	// when kv is available — pre-cutover installs that haven't
	// migrated the auth row layout yet can't back the kv rows that
	// the verifier reads. The resolver gains the store via
	// SetWebDAVStore so it can recognise tokens presented on the
	// auth listener; the server gains it via Config so the WebDAV
	// mount gate + the management endpoints come online together.
	var webdavTokens *auth.WebDAVTokenStore
	if authKV != nil {
		webdavCtx, webdavCancel := context.WithTimeout(context.Background(), 10*time.Second)
		webdavTokens, err = auth.NewWebDAVTokenStore(webdavCtx, authKV)
		webdavCancel()
		if err != nil {
			logger.Warn("webdav token store: init failed; short-lived tokens disabled",
				"err", err)
			webdavTokens = nil
		} else {
			resolver.SetWebDAVStore(webdavTokens)
		}
	}

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
				peerIdentity, err = peer.LoadOrCreate(idCtx, st, kek)
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
	var peerNonces *peer.NonceCache
	var peerSubscriber *peer.Subscriber
	var peerSubscriberTargetsCtx context.Context
	var peerSubscriberTargetsCancel context.CancelFunc
	if peerIdentity != nil && agentMgr != nil {
		if st := agentMgr.Store(); st != nil {
			peerEvents = peer.NewEventBus()
			peerNonces = peer.NewNonceCache(peer.AuthMaxClockSkew)
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
			// SKILL.md is only installed when there's at least one
			// OTHER ONLINE peer registered.
			//
			// Gated on registrar success: if the registrar didn't
			// start, the self row may be missing or stale and the
			// remote rows we'd be counting could be orphans from a
			// prior boot. Suppressing the skill there avoids
			// teasing the agent into a switch that always 4xxs.
			//
			// Status filter is "online" only. degraded rows are
			// excluded — they're tracked by the offline sweeper
			// for visibility, but a degraded target's handoff
			// pull leg is likely to time out and surface a
			// confusing partial-state outcome to the user.
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
					rows, err := lookupStore.ListPeers(ctx, store.ListPeersOptions{Status: store.PeerStatusOnline})
					if err != nil {
						return 0
					}
					// Boot-stale guard: a row that was online at the
					// last shutdown sits with status=online + a stale
					// last_seen until the OfflineSweeper's first tick
					// (SweepInterval after Start). Until that tick the
					// status filter alone would count peers that
					// haven't been seen since the previous boot.
					// Re-validate freshness inline against
					// OfflineThreshold so the skill gate is robust
					// across daemon restarts.
					cutoffMillis := time.Now().Add(-peer.OfflineThreshold).UnixMilli()
					count := 0
					for _, r := range rows {
						if r.DeviceID == selfID {
							continue
						}
						if r.LastSeen <= 0 || r.LastSeen < cutoffMillis {
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
				peerSubscriber = peer.NewSubscriber(peerIdentity, peerEvents, logger)
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
			// Hub-mode only: --peer hosts no agent runtime at all
			// and never feeds the list to schedulers.
			if !*peerMode {
				evictCtx, evictCancel := context.WithTimeout(
					context.Background(), 30*time.Second)
				agentMgr.EvictNonLocalAgentsAtStartup(
					evictCtx, peerIdentity.DeviceID)
				evictCancel()
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
				lockCtx, lockCancel := context.WithTimeout(context.Background(), 30*time.Second)
				agentLockGuard.Start(lockCtx, ids)
				lockCancel()
			}
		}
	}

	// Phase D barrier: start cron + notify poller AFTER blob hydrate
	// so a tick that fires immediately doesn't spawn the agent's CLI
	// in an empty CWD (books/, outputs/, etc. would not yet be on
	// disk). NewManager used to call StartSchedulers internally; the
	// split lets cmd/kojo interpose the hydrate.
	//
	// Peer mode (`--peer`) skips agent schedulers entirely: a daemon-
	// only peer hosts no agent runtime, runs no Slack / notify
	// producers, and has no Owner-facing API. Letting StartSchedulers
	// fire on the peer would tick cron jobs / notify pollers with no
	// listener to dispatch them to and risk duplicate work on the
	// Hub that owns the agent lock.
	if agentMgr != nil && !*peerMode {
		agentMgr.StartSchedulers()
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
		PeerNonces:     peerNonces,
		// docs §3.5 transition: when KOJO_REQUIRE_IF_MATCH=1, every
		// optimistic-concurrency-aware write rejects a missing
		// If-Match header with 428 Precondition Required. Off by
		// default — operators flip it once their UI / agent CLI have
		// caught up and stopped sending bare PUT/PATCH/DELETEs.
		RequireIfMatch:   os.Getenv("KOJO_REQUIRE_IF_MATCH") == "1",
		WebDAVTokenStore: webdavTokens,
		V0LegacyDir:      sessionV0LegacyDir,
		PeerOnly:         *peerMode,
		PendingSyncKEK:   pendingSyncKEK,
	})

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
		srv.SetOnAgentSyncFinalized(func(hookCtx context.Context, agentID, rawToken, sourceDeviceID, opID string) error {
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
				if err := agentMgr.MarkAgentArrivedHere(hookCtx, agentID); err != nil {
					return fmt.Errorf("mark agent arrived: %w", err)
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
			if rawToken != "" && tokens != nil {
				if err := tokens.AdoptAgentTokenFromPeer(agentID, rawToken); err != nil {
					return fmt.Errorf("token adopt: %w", err)
				}
			}
			if capturedGuard != nil {
				capturedGuard.AddAgent(hookCtx, agentID)
			}
			// Activate runtime side channels (cron + notify) AFTER the
			// lock guard has adopted us. Phase-1 (onAgentSynced) only
			// refreshes the in-memory cache; firing schedules before
			// finalize would have target acting on an agent the
			// orchestrator might still abort.
			if agentMgr != nil {
				agentMgr.ActivateAgentRuntime(agentID)

				// Resolve source peer's human-readable name for the
				// arrival prompt. Best-effort: fall back to the raw
				// device_id on lookup failure.
				sourceName := sourceDeviceID
				if sourceDeviceID != "" && agentMgr.Store() != nil {
					if rec, err := agentMgr.Store().GetPeer(hookCtx, sourceDeviceID); err == nil && rec.Name != "" {
						sourceName = rec.Name
					}
				}
				agentMgr.NotifyDeviceSwitchArrival(agentID, sourceName, opID)
			}
			return nil
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
	}

	// graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), session.ShutdownSignals()...)
	defer stop()

	// Background sweep of expired idempotency_keys rows (3.5). The
	// dedup window is 24 h; sweeping once an hour keeps the table
	// from growing unbounded without competing with hot writes. The
	// sweep goroutine exits when ctx (the shutdown signal context)
	// is cancelled so it lifecycles with the rest of the binary.
	srv.StartIdempotencySweep(ctx)

	// Sweep expired short-lived WebDAV tokens (§3.4 / §5.6). No-op
	// when the store is nil (kv unavailable). Goroutine lifecycle is
	// tied to ctx so it exits with the rest of the binary.
	srv.StartWebDAVTokenSweep(ctx)

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
			publicName := fmt.Sprintf("http://%s:%d", addr, listenPort)
			peerRegistrar.SetPublicName(publicName)
			refreshCtx, refreshCancel := context.WithTimeout(ctx, 5*time.Second)
			if rerr := peerRegistrar.RefreshPublicName(refreshCtx); rerr != nil {
				logger.Warn("peer self-row refresh failed; relying on heartbeat-loop retry",
					"err", rerr)
			}
			refreshCancel()
		}

		go func() {
			if err := srv.ServeAuth(ln, resolver); err != nil && err != http.ErrServerClosed {
				logger.Error("server error", "err", err)
				stop()
			}
		}()

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
			// Background refresh: bounded retry until tsnet
			// reports a DNSName, then SetPublicName +
			// RefreshPublicName so other peers' Subscriber can
			// dial us. Without this, self-row stays at
			// id.Name (OS hostname) for the binary's lifetime
			// when Status() was momentarily not ready.
			if peerRegistrar != nil {
				go refreshPublicNameFromTailscale(ctx, lc, peerRegistrar, *port, logger)
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

		defer tsServer.Close()
	}

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
				reg.SetPublicName(addr)
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
			// Convention: peer_registry.name is the Tailscale
			// FQDN, optionally suffixed with ":port" when the
			// peer listens on a non-443 port (kojo default
			// 8080). Operator stamps this via `--peer-add
			// <id>|<host:port>|<pubkey>`; the pipe separator
			// keeps the colon in `host:port` from colliding
			// with the field delimiter.
			if r.Name == "" {
				continue
			}
			// Name MAY carry an explicit scheme prefix
			// ("http://host:port" / "https://host:port"). A
			// scheme-less name is treated as https (the historical
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
			addr, err := peer.NormalizeAddress(r.Name)
			if err != nil {
				logger.Warn("peer subscriber: dropping unusable peer_registry.name",
					"name", r.Name, "err", err)
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

// tsAddrForURL formats a Tailscale IP + port as a URL host string,
// wrapping IPv6 addresses in brackets.
//
// (Note: peer_registry.name → dial URL normalization moved to
// peer.NormalizeAddress so the switch-device orchestrator + the
// blob-pull client can share the same logic.)

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

func tsAddrForURL(ip netip.Addr, port int) string {
	return net.JoinHostPort(ip.String(), strconv.Itoa(port))
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
