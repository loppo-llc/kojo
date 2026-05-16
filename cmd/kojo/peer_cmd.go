package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
	"github.com/loppo-llc/kojo/internal/store/secretcrypto"
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
	fmt.Fprintln(w, "DEVICE_ID\tNAME\tSTATUS\tLAST_SEEN\tSELF")
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			idShort, p.Name, p.Status, lastSeen, self)
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
//	<device_id>:<name>:<base64-public-key>
//
// All three components are required. The public key is the remote
// peer's Ed25519 public key, base64-encoded the same way the
// remote's --peer-list output reports it. Passing a key that doesn't
// round-trip through base64 → 32 bytes returns an error and the row
// is NOT inserted (mismatched key size would later fail the
// signature verification path with a confusing low-level error).
//
// Status defaults to "offline" — the operator only asserted the
// peer's identity, not its current reachability. The Hub flips it
// to "online" on the first successful inbound heartbeat from that
// peer (Phase G slice 2+).
func runPeerAddCommand(logger *slog.Logger, configDir, spec string) int {
	st, closeFn, err := openStoreReadOnly(logger, configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-add: %v\n", err)
		return 1
	}
	defer closeFn()

	// Pipe separator (not colon) so <name> can hold a
	// `host:port` form. The base64 alphabet doesn't include `|`
	// and peer name validation refuses control chars, so `|`
	// is safe as a delimiter against both fields' contents.
	parts := strings.SplitN(spec, "|", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		fmt.Fprintf(os.Stderr, "peer-add: spec must be <device_id>|<name>|<base64-public-key>\n")
		return 1
	}
	deviceID, name, pubB64 := parts[0], parts[1], parts[2]

	// Shape gates shared with the HTTP handler so a typo doesn't
	// reach UpsertPeer (which would store a junk row that later
	// auth attempts surface as `public_key shape invalid` 500s).
	if err := peer.ValidateDeviceID(deviceID); err != nil {
		fmt.Fprintf(os.Stderr, "peer-add: %v\n", err)
		return 1
	}
	if err := peer.ValidateName(name); err != nil {
		fmt.Fprintf(os.Stderr, "peer-add: %v\n", err)
		return 1
	}
	if err := peer.ValidatePublicKey(pubB64); err != nil {
		fmt.Fprintf(os.Stderr, "peer-add: %v\n", err)
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
		DeviceID:  deviceID,
		Name:      name,
		PublicKey: pubB64,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "peer-add: register: %v\n", err)
		return 1
	}
	fmt.Printf("peer added: %s (%s)\n", deviceID, name)
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

// isPeerRegistryDialAddress is the shape gate peer-self uses to
// decide whether a peer_registry self-row's Name field looks like a
// dial address other peers can actually reach. The runtime stamps
// exactly two shapes:
//
//   - Hub (tsnet): "<fqdn>:<port>" — no scheme; the historical
//     Tailscale TLS form. peerSubscriberTargetsLoop's
//     normalizeSubscriberAddress fills in "https://" for these.
//   - Peer (`--peer`): "http://<ts-ipv4-or-host>:<port>" — explicit
//     scheme so the Subscriber dials plain HTTP. Set in main.go's
//     peer-mode SetPublicName path.
//
// Anything else — bare hostname (`TVT-DEV-0000`), path/query/
// fragment, unsupported scheme — would silently land in a Hub
// operator's peer-add and produce an unreachable row the
// Subscriber loops over forever. Refuse and force the operator
// to start the daemon once so the row gets the canonical form.
//
// `https://host[:port]` is allowed too for an operator who hand-
// stamps a non-default explicit scheme into the row; we just won't
// generate it ourselves.
func isPeerRegistryDialAddress(name string) bool {
	if name == "" {
		return false
	}
	// Scheme branch: anything with "://" must parse cleanly as
	// http/https, expose a host with a port, and carry no path /
	// query / fragment cruft.
	if strings.Contains(name, "://") {
		u, err := url.Parse(name)
		if err != nil {
			return false
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "http" && scheme != "https" {
			return false
		}
		if u.Host == "" {
			return false
		}
		if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return false
		}
		if _, _, err := net.SplitHostPort(u.Host); err != nil {
			return false
		}
		return true
	}
	// Scheme-less branch: must be `host:port`. SplitHostPort is the
	// authoritative parser for that shape — bare hostnames (no
	// colon, no port) and "host:" with empty port are rejected.
	host, port, err := net.SplitHostPort(name)
	if err != nil || host == "" || port == "" {
		return false
	}
	return true
}

// runPeerSelfCommand prints the local binary's identity in a form
// suitable for paste-into-other-peer's `--peer-add` flag:
//
//	<device_id>|<name>|<base64-public-key>
//
// Pipe separator (not colon) so the middle <name> field can hold
// a `host:port` Tailscale FQDN without colliding with the
// delimiter — the base64 alphabet doesn't include `|` and peer
// name validation refuses control chars.
//
// First-ever boot path: if peer.LoadOrCreate hasn't been called yet
// (kv has no peer/* rows), this command refuses with a hint to start
// the daemon once. Mirrors --peer-list's degraded behaviour around
// missing self markers; Phase G's design assumes identity is
// generated by the daemon's main flow, not by ad-hoc subcommands.
func runPeerSelfCommand(logger *slog.Logger, configDir string) int {
	st, closeFn, err := openStoreReadOnly(logger, configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-self: %v\n", err)
		return 1
	}
	defer closeFn()

	authDir := filepath.Join(configDir, "auth")
	kek, err := secretcrypto.LoadOrCreateKEK(authDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-self: KEK setup failed: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := peer.LoadOrCreate(ctx, st, kek)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer-self: load identity: %v\n", err)
		return 1
	}
	// Prefer the peer_registry self-row's Name: a running daemon
	// will have called Registrar.RefreshPublicName after tsnet
	// (Hub) or peerBindAndAdvertise (--peer) populated the dial
	// address, so the row holds the canonical
	// `[scheme://]<host>:<port>` form other peers can dial.
	//
	// If the row hasn't been written yet, the binary's identity
	// name (= OS hostname, no port, no scheme) is the only thing
	// available — and that is exactly the broken paste a remote
	// operator's --peer-add would silently accept, only for
	// Subscriber dial to fail later because the address has no
	// port (and on a peer-mode host, no `http://` either). Refuse
	// instead so the operator runs the daemon once before pairing.
	rec, gerr := st.GetPeer(ctx, id.DeviceID)
	if gerr != nil || rec == nil || !isPeerRegistryDialAddress(rec.Name) {
		fmt.Fprintf(os.Stderr,
			"peer-self: this binary has not advertised a dial address yet.\n"+
				"  Start the daemon once first (so it can stamp the peer_registry\n"+
				"  self-row with the Tailscale FQDN / Tailscale IPv4 the Hub will\n"+
				"  reach this host on) and re-run --peer-self:\n"+
				"    Hub:   kojo            # tsnet → <host>.<tailnet>.ts.net:<port>\n"+
				"    Peer:  kojo --peer     # plain HTTP on Tailscale IPv4\n")
		return 1
	}
	spec := fmt.Sprintf("%s|%s|%s", id.DeviceID, rec.Name, id.PublicKeyBase64())
	fmt.Println(spec)
	// Usage hint on stderr so piping the stdout into another tool
	// stays clean (machine-readable spec on stdout, human-readable
	// guidance on stderr). The pipe `|` separator is shell-active
	// in bash/zsh/cmd/PowerShell, so paste-without-quotes turns the
	// spec into two phantom commands — exactly the failure mode the
	// operator hits when they copy the printed line and run it
	// verbatim. Spell out the single-quote form, with paths for
	// every shell we expect operators to use.
	fmt.Fprintf(os.Stderr,
		"\n  to register this peer on the other host, run (quotes required — `|` is a shell pipe):\n"+
			"    bash/zsh:    kojo --peer-add '%[1]s'\n"+
			"    cmd.exe:     kojo --peer-add \"%[1]s\"\n"+
			"    PowerShell:  kojo --peer-add '%[1]s'\n\n",
		spec)
	return 0
}
