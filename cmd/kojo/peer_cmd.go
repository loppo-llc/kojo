package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// runPeerListCommand prints the peer_registry rows to stdout in a
// human-readable table. Mirrors the --snapshot / --clean pattern: it
// runs against a short-lived *store.Store connection (NO long-running
// configdir lock) so the operator can list peers while the main
// daemon is also running.
//
// Output columns:
//   device_id (8-byte prefix for compactness)
//   name
//   status (online/offline/degraded)
//   last_seen (relative — "5s ago", "2m ago", "—" for never)
//   self ("*" for the local binary's row, blank otherwise)
//
// Exit codes:
//   0 — success (even with zero rows; first-ever boot is legitimate)
//   1 — store open / list failure
//
// The local peer's row is identified by reading the same kv namespace
// that peer.LoadOrCreate writes; if the kv lookup fails we still
// print the table without the self marker — degraded but useful.
func runPeerListCommand(logger *slog.Logger, configDir string) int {
	st, closeFn, err := openStoreReadOnly(logger, configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-list: %v\n", err)
		return 1
	}
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	peers, err := st.ListPeers(ctx, store.ListPeersOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-list: list: %v\n", err)
		return 1
	}

	selfID := readSelfDeviceID(ctx, st)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DEVICE_ID\tNAME\tURL\tSTATUS\tLAST_SEEN\tTRUST\tSELF")
	now := store.NowMillis()
	for _, p := range peers {
		idShort := p.DeviceID
		if len(idShort) > 8 {
			idShort = idShort[:8]
		}
		var lastSeen string
		if p.LastSeen == 0 {
			lastSeen = "—"
		} else {
			lastSeen = relativeTime(now - p.LastSeen)
		}
		self := ""
		if selfID != "" && p.DeviceID == selfID {
			self = "*"
		}
		urlCol := p.URL
		if urlCol == "" {
			urlCol = "—"
		}
		trust := "—"
		if p.Trusted {
			trust = "trusted"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			idShort, p.Name, urlCol, p.Status, lastSeen, trust, self)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "peer-list: flush: %v\n", err)
		return 1
	}
	if len(peers) == 0 {
		fmt.Fprintln(os.Stdout, "(no peers registered)")
	}
	return 0
}

// readSelfDeviceID returns the device_id this binary uses, or "" if
// it isn't recorded yet (first-ever boot before peer.LoadOrCreate
// runs) or the kv read fails. Best-effort: a missing self marker
// just means the operator can't tell which row is "us" in the
// printout — not a fatal error for the listing command.
func readSelfDeviceID(ctx context.Context, st *store.Store) string {
	rec, err := st.GetKV(ctx, peer.KVNamespace, peer.KeyDeviceID)
	if err != nil {
		return ""
	}
	return rec.Value
}

// openStoreReadOnly opens a *store.Store handle pointing at the
// kojo.db under configDir without acquiring the configdir lock.
// Mirrors runSnapshotCommand's approach — multiple readers on
// kojo.db are safe under SQLite WAL mode, so co-existing with the
// running daemon is OK. Note that the readOnly bit is more about
// "skip migrations" than literal mode-O_RDONLY: --peer-add and
// --peer-remove DO write rows, so the store must be opened RW.
//
// Caller MUST invoke the returned closer to release the SQLite
// handle.
func openStoreReadOnly(logger *slog.Logger, configDir string) (*store.Store, func(), error) {
	dbPath := filepath.Join(configDir, "kojo.db")
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("no kojo.db at %s — run kojo once to initialize", dbPath)
		}
		return nil, nil, err
	}
	openCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := store.Open(openCtx, store.Options{Path: dbPath})
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	_ = logger // reserved for future log lines on open path
	return st, func() { _ = st.Close() }, nil
}

// relativeTime renders a duration in millis as a coarse "X ago"
// string. Granularity matches what an operator would care about:
// seconds for "actively connected", minutes for "recently up",
// hours / days for "definitely down". Negative values (clock skew
// between peers) round to "0s" rather than confusing the reader.
func relativeTime(deltaMillis int64) string {
	if deltaMillis < 0 {
		deltaMillis = 0
	}
	d := time.Duration(deltaMillis) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// runPeerAddCommand inserts a remote peer row into peer_registry so
// the local binary can address it by device_id. Format of the spec
// argument:
//
//	<device_id>|<name>|<url>
//
// All three components are required. `name` is the human-friendly
// device label; `url` is the dial address other peers reach it on
// (`host:port` for tsnet/HTTPS or `http://host:port` for peer-mode).
// The Ed25519 public_key field that used to live in the fourth slot
// was retired in docs/peer-simplify-plan.md step 9 — Bearer tokens
// delivered through the auto-pairing approve flow now carry the
// identity material.
//
// Status defaults to "offline" — the operator only asserted the
// peer's identity, not its current reachability. The Hub flips it
// to "online" on the first successful inbound heartbeat from that
// peer.
func runPeerAddCommand(logger *slog.Logger, configDir, spec string, trusted bool) int {
	st, closeFn, err := openStoreReadOnly(logger, configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-add: %v\n", err)
		return 1
	}
	defer closeFn()

	parts := strings.SplitN(spec, "|", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		fmt.Fprintf(os.Stderr, "peer-add: spec must be <device_id>|<name>|<url>\n")
		return 1
	}
	deviceID, name, peerURL := parts[0], parts[1], parts[2]

	// Shape gates shared with the HTTP handler so a typo doesn't
	// reach UpsertPeer.
	if err := peer.ValidateDeviceID(deviceID); err != nil {
		fmt.Fprintf(os.Stderr, "peer-add: %v\n", err)
		return 1
	}
	if err := peer.ValidateName(name); err != nil {
		fmt.Fprintf(os.Stderr, "peer-add: %v\n", err)
		return 1
	}
	if !peer.IsDialAddress(peerURL) {
		fmt.Fprintf(os.Stderr, "peer-add: url must look like host:port or http(s)://host:port (got %q)\n", peerURL)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// RegisterPeerMetadata (not UpsertPeer) so a re-add against
	// an already-known peer doesn't clobber the heartbeat's
	// last_seen / status — UpsertPeer would reset them to
	// (offline, 0) and the operator-visible `peer-list` would
	// flip the peer offline until the next heartbeat.
	if _, err := st.RegisterPeerMetadata(ctx, &store.PeerRecord{
		DeviceID: deviceID,
		Name:     name,
		URL:      peerURL,
		Trusted:  trusted,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "peer-add: register: %v\n", err)
		return 1
	}
	// RegisterPeerMetadata's preserve-on-conflict contract leaves
	// trusted alone on re-add. Apply the flip explicitly so the
	// CLI argument is authoritative for both insert AND update.
	if err := st.UpdatePeerTrust(ctx, deviceID, trusted); err != nil {
		fmt.Fprintf(os.Stderr, "peer-add: set trust: %v\n", err)
		return 1
	}
	trustLabel := ""
	if trusted {
		trustLabel = " [trusted]"
	}
	fmt.Printf("peer added: %s (%s, %s)%s\n", deviceID, name, peerURL, trustLabel)
	return 0
}

// runPeerTrustCommand flips the trusted bit on an existing
// peer_registry row. Decoupled from peer-add so the operator can
// promote / demote a previously-paired peer without re-typing the
// full spec. Refuses self (same reasoning as peer-remove —
// flipping the local trust state would be a meaningless self-write
// because the principal never authenticates against its own
// trusted column).
func runPeerTrustCommand(logger *slog.Logger, configDir, deviceID string, trusted bool) int {
	st, closeFn, err := openStoreReadOnly(logger, configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-trust: %v\n", err)
		return 1
	}
	defer closeFn()
	if deviceID == "" {
		fmt.Fprintf(os.Stderr, "peer-trust: device_id required\n")
		return 1
	}
	if err := peer.ValidateDeviceID(deviceID); err != nil {
		fmt.Fprintf(os.Stderr, "peer-trust: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if self := readSelfDeviceID(ctx, st); self != "" && self == deviceID {
		fmt.Fprintf(os.Stderr, "peer-trust: refusing to flip self (%s)\n", deviceID)
		return 1
	}
	if err := st.UpdatePeerTrust(ctx, deviceID, trusted); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "peer-trust: %s not in peer_registry\n", deviceID)
			return 1
		}
		fmt.Fprintf(os.Stderr, "peer-trust: %v\n", err)
		return 1
	}
	state := "trusted"
	if !trusted {
		state = "untrusted"
	}
	fmt.Printf("peer %s: %s\n", deviceID, state)
	return 0
}

// runPeerRemoveCommand deletes a peer_registry row by device_id.
// Idempotent — removing a non-existent device_id is reported as a
// no-op but still exits 0 so scripts can call it without
// pre-checking.
//
// Refuses to remove the local binary's own row (self): doing so
// would leave the next start with a phantom row mismatch (kv knows
// the device_id but peer_registry doesn't, breaking the home_peer
// → name lookup that some Phase 4 surfaces depend on).
func runPeerRemoveCommand(logger *slog.Logger, configDir, deviceID string) int {
	st, closeFn, err := openStoreReadOnly(logger, configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-remove: %v\n", err)
		return 1
	}
	defer closeFn()

	if deviceID == "" {
		fmt.Fprintf(os.Stderr, "peer-remove: device_id required\n")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if self := readSelfDeviceID(ctx, st); self != "" && self == deviceID {
		fmt.Fprintf(os.Stderr, "peer-remove: refusing to remove self (%s)\n", deviceID)
		return 1
	}
	if err := st.DeletePeer(ctx, deviceID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Printf("peer-remove: %s not found (no-op)\n", deviceID)
			return 0
		}
		fmt.Fprintf(os.Stderr, "peer-remove: %v\n", err)
		return 1
	}
	fmt.Printf("peer removed: %s\n", deviceID)
	return 0
}

// printPairingSpec prints the pairing triple (device_id|name|url|pubkey)
// to stderr at startup so the operator can paste it into the OTHER
// host's `kojo --peer-add` flag. Pairing is bidirectional: each
// host stores the other's row so signed inter-peer requests pass
// the receiver's PeerAuth middleware.
//
// The pipe `|` separator is shell-active in bash/zsh/cmd/PowerShell,
// so paste-without-quotes turns the spec into two phantom commands —
// exactly the failure mode the operator hits when they copy the
// printed line and run it verbatim. Spell out the single-quote form,
// with paths for every shell we expect operators to use.
//
// role is "Hub" or "peer" — wording in the banner shifts so the
// operator knows which side they are sitting on and which side to
// run --peer-add on.
func printPairingSpec(id *peer.Identity, peerURL, role string) {
	if id == nil {
		return
	}
	// Pairing spec carries only metadata now — the Ed25519 public key
	// that used to occupy the fourth field was retired in
	// docs/peer-simplify-plan.md step 9. Bearer tokens replace it via
	// the auto-pairing flow.
	spec := fmt.Sprintf("%s|%s|%s", id.DeviceID, id.Name, peerURL)
	// Hub-side pairing must combine `--peer-add <spec>` with the
	// bool flag `--peer-add-trusted` so the peer admits the Hub
	// on the privileged surface (session create, files, git, ...).
	// A plain `--peer-add` leaves the row as trusted=0 and every
	// Hub→peer proxy call would 403. Peer-side pairing on the Hub
	// uses plain `--peer-add` because the Hub should NOT trust an
	// arbitrary peer to drive its own session / file surface; the
	// operator can flip `--peer-trust <device_id>` later if they
	// decide to.
	var headline, suffix string
	switch role {
	case "hub":
		headline = "Hub pairing spec — run on every peer host so the peer admits Hub-driven session/file/git proxy:"
		suffix = " --peer-add-trusted"
	case "peer":
		headline = "peer pairing spec — run on the Hub to register this peer (Hub stays default-untrusted; use `kojo --peer-trust <device_id>` to promote):"
		suffix = ""
	default:
		headline = "peer pairing spec — run on the other host:"
		suffix = ""
	}
	fmt.Fprintf(os.Stderr,
		"  %s\n\n"+
			"    bash/zsh:    kojo --peer-add '%[3]s'%[2]s\n"+
			"    cmd.exe:     kojo --peer-add \"%[3]s\"%[2]s\n"+
			"    PowerShell:  kojo --peer-add '%[3]s'%[2]s\n\n",
		headline, suffix, spec)
}
