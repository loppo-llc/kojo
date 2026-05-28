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
//   status (online/offline)
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
	fmt.Fprintln(w, "DEVICE_ID\tNAME\tURL\tSTATUS\tLAST_SEEN\tSELF")
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			idShort, p.Name, urlCol, p.Status, lastSeen, self)
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
func runPeerAddCommand(logger *slog.Logger, configDir, spec string) int {
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
	if _, err := st.RegisterPeerMetadata(ctx, &store.PeerRecord{
		DeviceID: deviceID,
		Name:     name,
		URL:      peerURL,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "peer-add: register: %v\n", err)
		return 1
	}
	fmt.Printf("peer added: %s (%s, %s)\n", deviceID, name, peerURL)
	fmt.Fprintln(os.Stderr,
		"\n  WARNING: --peer-add writes peer_registry metadata only.\n"+
			"  Bearer tokens are minted only by the auto-pairing Approve\n"+
			"  flow (peer side runs `kojo --peer --hub <hub>`, operator\n"+
			"  clicks Approve in Settings). Until that's done, inter-peer\n"+
			"  auth against this row will return 401.")
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

// printPairingSpec prints the pairing identity to stderr at startup.
// With Ed25519 signing retired (docs/peer-simplify-plan.md) the
// authoritative pairing channel is the auto-pairing flow: peer hosts
// run `kojo --peer` against this Hub's URL and the Owner approves
// the pending join request in Settings. That flow mints + delivers
// the Bearer pair end-to-end with no manual paste.
//
// `--peer-add` survives only as a metadata-only escape hatch (writes
// peer_registry without Bearer tokens). Until a follow-up adds
// matching `--peer-mint-bearer` / `--peer-import-bearer` commands,
// rows added that way cannot authenticate inter-peer requests.
//
// role is "hub" or "peer" — wording in the banner shifts so the
// operator knows which side they are sitting on.
func printPairingSpec(id *peer.Identity, peerURL, role string) {
	if id == nil {
		return
	}
	spec := fmt.Sprintf("%s|%s|%s", id.DeviceID, id.Name, peerURL)
	hubURL := peerURL
	if role == "peer" {
		hubURL = "<hub-url>"
	}
	fmt.Fprintf(os.Stderr,
		"  Pairing — recommended (auto-pairing via Hub Approve):\n\n"+
			"    On EACH peer host:\n"+
			"        kojo --peer --hub %s\n"+
			"    Then on the Hub, Settings → Pending → Approve.\n\n"+
			"  This host's identity (for diagnostics / manual offline rows):\n"+
			"        %s\n\n"+
			"  Manual `--peer-add '<spec>'` writes the registry row only;\n"+
			"  it does NOT mint Bearer tokens, so the resulting peer cannot\n"+
			"  authenticate until a future --peer-mint-bearer command lands.\n\n",
		hubURL, spec)
}
