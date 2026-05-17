// Peer registry UI section (Phase G slice 3).
//
// Shows the cluster's known peers, lets the operator register a new
// peer paired off-band (the remote peer's deviceId / name / publicKey
// arrive via QR / paste), delete a retired peer, and rotate the
// long-lived Ed25519 identity key of any non-self peer.
//
// Self-row policy:
// - List: rendered with an "(this device)" badge.
// - Delete: server returns 409, surfaced as an error here too.
// - Rotate-key: server returns 409 (local-identity rotation requires
//   re-sealing the kv-stored private key, which is a separate slice).
//
// The form-input UX is intentionally minimal — peers are rarely
// added by hand, so the friction of a manual paste is fine; the
// future "scan a QR from the new peer" flow will live alongside
// this form, not replace it.

import { useCallback, useEffect, useRef, useState } from "react";
import {
  peersApi,
  type PeerInfo,
  type PeerPendingInfo,
} from "../../lib/peerApi";

interface Props {
  setError: (msg: string) => void;
  flashSuccess: () => void;
}

const STATUS_COLOR: Record<PeerInfo["status"], string> = {
  online: "text-emerald-400",
  offline: "text-neutral-500",
  degraded: "text-amber-400",
};

function formatLastSeen(ms?: number): string {
  if (!ms) return "never";
  const d = new Date(ms);
  if (Number.isNaN(d.getTime())) return "never";
  const diff = Date.now() - ms;
  if (diff < 60_000) return "just now";
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h ago`;
  return d.toLocaleString();
}

// REFRESH_INTERVAL_MS is how often the section polls the server for
// status / last_seen drift while the settings page is open. Matches
// the backend OfflineSweeper cadence so a peer flipping to offline
// shows up within one tick of the server detecting it. Polling is
// scoped to this component (cleared on unmount) so it never runs
// when the user is elsewhere in the UI.
const REFRESH_INTERVAL_MS = 30_000;

export function PeersSection({ setError, flashSuccess }: Props) {
  const [items, setItems] = useState<PeerInfo[]>([]);
  const [pending, setPending] = useState<PeerPendingInfo[]>([]);
  const [selfId, setSelfId] = useState<string>("");
  const [loading, setLoading] = useState(true);
  // unavailable=true means the server returned 404 / 503 for the
  // registry endpoint — typically because peerIdentity didn't load
  // (KEK missing, fresh install before identity bootstrap, etc).
  // The section degrades to a soft "not available" notice instead
  // of bubbling the network error up to the page-level banner.
  const [unavailable, setUnavailable] = useState(false);
  const [showAdd, setShowAdd] = useState(false);
  const [rotateFor, setRotateFor] = useState<string>(""); // deviceId
  // Form state for add: a single textarea matching the --peer-add
  // pipe-separated spec the daemon prints at startup.
  const [pairingSpec, setPairingSpec] = useState("");
  const [parseError, setParseError] = useState("");
  // Trust checkbox: opt the new peer into the privileged surface
  // (sessions / files / git on this host). Defaults to false so
  // an unmodified paste lands as a safe restricted peer.
  const [trustOnAdd, setTrustOnAdd] = useState(false);
  // Form state for rotate.
  const [newKey, setNewKey] = useState("");
  // Inline edit form per row. Only the human-friendly Name and the
  // dial URL are editable here — public_key rotates through the
  // separate rotate-key flow and device_id is immutable.
  const [editFor, setEditFor] = useState<string>("");
  const [editName, setEditName] = useState("");
  const [editURL, setEditURL] = useState("");
  const [busy, setBusy] = useState(false);
  // requestSeq is monotonically incremented before every list() call;
  // a response is only allowed to update state when its captured seq
  // is still the latest. Without this, a slow background poll racing
  // a fast post-mutation refresh could overwrite the fresh result
  // with a stale snapshot (e.g. after register/delete the item count
  // would briefly revert until the next tick). Mounted is also
  // tracked so a response that arrives after unmount doesn't write
  // through to React (no warning, but no cycles wasted either).
  const requestSeq = useRef(0);
  const mounted = useRef(true);

  // refresh is wrapped so background ticks (silent=true) don't flash
  // the loading spinner on every poll and don't bubble transient
  // errors to the page-level banner. Initial mount + post-mutation
  // refreshes pass silent=false so the user sees the spinner once.
  const refresh = useCallback(
    async (silent = false) => {
      // Early-return *before* any setState so a mutation handler that
      // races unmount can't write through the unmounted component.
      // setLoading(true) above this line would have leaked.
      if (!mounted.current) return;
      if (!silent) setLoading(true);
      const myseq = ++requestSeq.current;
      try {
        const [resp, pendResp] = await Promise.all([
          peersApi.list(),
          // pending API: swallow 404/503 (route not registered /
          // registry not initialized — same soft states the main
          // list handles via setUnavailable). Surface anything
          // else as an error banner so the Approve / Reject UI
          // doesn't silently disappear on a real failure.
          peersApi.pending().catch((e: unknown) => {
            const msg = (e as Error).message ?? "";
            if (!/^404:|^503:/.test(msg) && !silent) {
              setError(`Failed to load pending peers: ${msg}`);
            }
            return { items: [] };
          }),
        ]);
        if (!mounted.current) return;
        if (myseq === requestSeq.current) {
          setItems(resp.items ?? []);
          setSelfId(resp.selfDeviceId ?? "");
          setPending(pendResp.items ?? []);
          setUnavailable(false);
        }
      } catch (e) {
        if (!mounted.current) return;
        if (myseq !== requestSeq.current) return;
        const msg = (e as Error).message;
        // Detect "registry not registered on this server". The
        // server returns 404 (route not registered) when peerIdentity
        // is nil and 503 (registry not initialized) when the
        // registrar hasn't seeded the self-row yet. Both are soft
        // states that the UI should render as "not available", not
        // as red error banners.
        if (/^404:|^503:/.test(msg)) {
          setUnavailable(true);
        } else if (!silent) {
          setError(`Failed to load peers: ${msg}`);
        }
      } finally {
        // Clear loading regardless of seq when this *was* the
        // non-silent fetch the user is waiting on. Without this, a
        // silent poll that becomes "latest" while the user-initiated
        // load is still in-flight would prevent the user-initiated
        // load from ever clearing its spinner — the seq check below
        // would fail when our reply finally lands. silent ticks never
        // touched loading, so they have nothing to clear.
        if (mounted.current && !silent) {
          setLoading(false);
        }
      }
    },
    [setError],
  );

  useEffect(() => {
    mounted.current = true;
    void refresh();
    const handle = window.setInterval(() => {
      void refresh(true);
    }, REFRESH_INTERVAL_MS);
    return () => {
      mounted.current = false;
      window.clearInterval(handle);
    };
  }, [refresh]);

  const resetAddForm = () => {
    setPairingSpec("");
    setParseError("");
    setTrustOnAdd(false);
    setShowAdd(false);
  };

  // parsePairingSpec splits the pipe-separated spec the `--peer-add`
  // flag accepts. Same shape the daemon prints on startup so the
  // operator pastes it verbatim. Strips a surrounding pair of single
  // or double quotes so a copy that included the shell-escape
  // delimiters still parses.
  const parsePairingSpec = (raw: string) => {
    let s = raw.trim();
    if ((s.startsWith("'") && s.endsWith("'")) || (s.startsWith('"') && s.endsWith('"'))) {
      s = s.slice(1, -1).trim();
    }
    const parts = s.split("|");
    if (parts.length !== 4) {
      throw new Error(`expected 4 pipe-separated fields, got ${parts.length}`);
    }
    const [deviceId, name, url, publicKey] = parts.map((p) => p.trim());
    if (!deviceId || !name || !url || !publicKey) {
      throw new Error("every field (deviceId | name | url | publicKey) must be non-empty");
    }
    return { deviceId, name, url, publicKey };
  };

  const submitAdd = async () => {
    setParseError("");
    let parsed: { deviceId: string; name: string; url: string; publicKey: string };
    try {
      parsed = parsePairingSpec(pairingSpec);
    } catch (e) {
      setParseError((e as Error).message);
      return;
    }
    setBusy(true);
    try {
      await peersApi.register({ ...parsed, trusted: trustOnAdd });
      resetAddForm();
      flashSuccess();
      await refresh();
    } catch (e) {
      setError(`Register failed: ${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  const submitRotate = async (id: string) => {
    if (!newKey.trim()) return;
    setBusy(true);
    try {
      await peersApi.rotateKey(id, newKey.trim());
      setRotateFor("");
      setNewKey("");
      flashSuccess();
      await refresh();
    } catch (e) {
      setError(`Rotate failed: ${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  const openEdit = (p: PeerInfo) => {
    setEditFor(editFor === p.deviceId ? "" : p.deviceId);
    setEditName(p.name);
    setEditURL(p.url ?? "");
    setRotateFor("");
    setNewKey("");
  };

  const submitEdit = async (p: PeerInfo) => {
    const name = editName.trim();
    const url = editURL.trim();
    if (!name || !url) {
      setError("Edit: name と url を両方入力する必要がある");
      return;
    }
    setBusy(true);
    try {
      // Narrow PATCH: only name + url reach the server. trusted,
      // publicKey, capabilities, last_seen, status are NOT sent,
      // so a stale browser tab can't roll back a flip / refresh
      // that landed in another window or another surface.
      await peersApi.updateMetadata(p.deviceId, { name, url });
      setEditFor("");
      flashSuccess();
      await refresh();
    } catch (e) {
      setError(`Edit failed: ${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  const toggleTrust = async (p: PeerInfo) => {
    setBusy(true);
    try {
      await peersApi.setTrust(p.deviceId, !p.trusted);
      flashSuccess();
      await refresh();
    } catch (e) {
      setError(`Trust toggle failed: ${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  const approvePending = async (p: PeerPendingInfo) => {
    setBusy(true);
    try {
      await peersApi.approvePending(p.deviceId);
      flashSuccess();
      await refresh();
    } catch (e) {
      setError(`Approve failed: ${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  const rejectPending = async (p: PeerPendingInfo) => {
    if (!window.confirm(`Reject join request from "${p.name}"?`)) return;
    setBusy(true);
    try {
      await peersApi.rejectPending(p.deviceId);
      flashSuccess();
      await refresh();
    } catch (e) {
      setError(`Reject failed: ${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: string, peerName: string) => {
    if (!window.confirm(`Decommission peer "${peerName}"? This cannot be undone.`)) return;
    setBusy(true);
    try {
      await peersApi.remove(id);
      flashSuccess();
      await refresh();
    } catch (e) {
      setError(`Delete failed: ${(e as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  if (unavailable) {
    return (
      <div>
        <h2 className="text-xs font-semibold text-neutral-500 uppercase tracking-wider mb-3">
          Peers
        </h2>
        <div className="p-3 bg-neutral-900 border border-neutral-800 rounded-lg text-xs text-neutral-500">
          Peer registry is not available on this server. The local peer identity
          has not been bootstrapped yet, or the server was started without one.
        </div>
      </div>
    );
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-3">
        <h2 className="text-xs font-semibold text-neutral-500 uppercase tracking-wider">
          Peers
        </h2>
        <button
          onClick={() => setShowAdd((v) => !v)}
          className="px-2 py-1 bg-neutral-800 hover:bg-neutral-700 rounded text-xs"
        >
          {showAdd ? "Cancel" : "Register"}
        </button>
      </div>
      <p className="text-xs text-neutral-600 mb-3">
        Known cluster members. Register a peer after pairing it off-band; the local device
        cannot be added or removed from this UI. <span className="text-neutral-500">Trust</span>{" "}
        controls whether this peer can create sessions / browse files / run git on{" "}
        <em>this</em> host — pairing alone does not grant that, and the flip is per-direction
        (you also need to be marked trusted on the peer to drive its surface).
      </p>

      {showAdd && (
        <div className="p-3 bg-neutral-900 border border-neutral-800 rounded-lg mb-2 space-y-2">
          <p className="text-[11px] text-neutral-500 leading-snug">
            Paste the pairing spec the other peer prints on startup
            (<code className="font-mono">kojo --peer-add</code> argument).
            Format: <code className="font-mono">deviceId | name | url | publicKey</code>
          </p>
          <textarea
            value={pairingSpec}
            onChange={(e) => {
              setPairingSpec(e.target.value);
              if (parseError) setParseError("");
            }}
            placeholder="00000000-0000-4000-8000-000000000000|laptop|http://100.64.0.5:8080|AAAA…"
            rows={3}
            className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500"
          />
          {parseError && (
            <div className="text-xs text-red-400">Parse: {parseError}</div>
          )}
          <label className="flex items-start gap-2 text-xs text-neutral-300 cursor-pointer">
            <input
              type="checkbox"
              checked={trustOnAdd}
              onChange={(e) => setTrustOnAdd(e.target.checked)}
              className="mt-0.5 accent-amber-500"
            />
            <span>
              Trust this peer
              <span className="block text-[10px] text-neutral-500 mt-0.5">
                Required for the peer to create sessions, browse files, or run git on this host.
                Leave unchecked unless you operate both sides.
              </span>
            </span>
          </label>
          <button
            onClick={submitAdd}
            disabled={busy || !pairingSpec.trim()}
            className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-xs font-medium disabled:opacity-40"
          >
            {busy ? "Registering..." : "Register peer"}
          </button>
        </div>
      )}

      {pending.length > 0 && (
        <div className="mb-3">
          <h3 className="text-[11px] font-semibold text-amber-400 uppercase tracking-wider mb-2">
            Pending join requests
          </h3>
          <p className="text-[11px] text-neutral-500 mb-2 leading-snug">
            Peers that auto-discovered this Hub via{" "}
            <code className="font-mono">kojo --peer</code> and are waiting for
            approval. Approve grants the privileged surface (trusted=true);
            Reject drops the request — the peer may retry.
          </p>
          {pending.map((p) => (
            <div
              key={p.deviceId}
              className="p-3 bg-amber-950/30 border border-amber-900/60 rounded-lg mb-2"
            >
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0 flex-1">
                  <div className="text-sm font-medium truncate">{p.name}</div>
                  <div className="text-[11px] font-mono text-neutral-500 truncate mt-0.5">
                    {p.deviceId}
                  </div>
                  <div className="text-[11px] font-mono text-neutral-500 truncate">
                    {p.url}
                  </div>
                  <div className="text-xs text-neutral-500 mt-1">
                    seen {formatLastSeen(p.lastSeen)}
                  </div>
                </div>
                <div className="flex flex-col gap-1 shrink-0">
                  <button
                    onClick={() => approvePending(p)}
                    disabled={busy}
                    className="px-2 py-1 bg-emerald-700 hover:bg-emerald-600 rounded text-xs font-medium disabled:opacity-40"
                  >
                    {busy ? "..." : "Approve"}
                  </button>
                  <button
                    onClick={() => rejectPending(p)}
                    disabled={busy}
                    className="px-2 py-1 bg-neutral-800 hover:bg-neutral-700 rounded text-xs text-neutral-400 hover:text-red-400 disabled:opacity-40"
                  >
                    Reject
                  </button>
                </div>
              </div>
            </div>
          ))}
        </div>
      )}

      {loading ? (
        <div className="text-xs text-neutral-600">Loading...</div>
      ) : items.length === 0 ? (
        <div className="text-xs text-neutral-600">No peers registered.</div>
      ) : (
        items.map((p) => {
          const isSelf = p.isSelf || p.deviceId === selfId;
          return (
            <div
              key={p.deviceId}
              className="p-3 bg-neutral-900 border border-neutral-800 rounded-lg mb-2"
            >
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0 flex-1">
                  <div className="text-sm font-medium flex items-center gap-2">
                    <span className="truncate">{p.name}</span>
                    {isSelf && (
                      <span className="text-[10px] px-1.5 py-0.5 bg-neutral-800 text-neutral-400 rounded">
                        this device
                      </span>
                    )}
                  </div>
                  <div className="text-[11px] font-mono text-neutral-600 truncate mt-0.5">
                    {p.deviceId}
                  </div>
                  {p.url && (
                    <div className="text-[11px] font-mono text-neutral-500 truncate">
                      {p.url}
                    </div>
                  )}
                  <div className="text-xs mt-1 flex items-center gap-3">
                    <span className={STATUS_COLOR[p.status] ?? "text-neutral-500"}>
                      {p.status}
                    </span>
                    <span className="text-neutral-600">
                      seen {formatLastSeen(p.lastSeen)}
                    </span>
                    {p.trusted && (
                      <span className="text-[10px] px-1.5 py-0.5 bg-amber-900/60 text-amber-300 rounded">
                        trusted
                      </span>
                    )}
                  </div>
                </div>
                {!isSelf && (
                  <div className="flex flex-col gap-1 shrink-0">
                    <button
                      onClick={() => openEdit(p)}
                      className="px-2 py-1 bg-neutral-800 hover:bg-neutral-700 rounded text-xs"
                      title="Edit this peer's display name and dial URL"
                    >
                      {editFor === p.deviceId ? "Cancel" : "Edit"}
                    </button>
                    <button
                      onClick={() => toggleTrust(p)}
                      disabled={busy}
                      className={`px-2 py-1 rounded text-xs ${
                        p.trusted
                          ? "bg-amber-900/60 text-amber-200 hover:bg-amber-900"
                          : "bg-neutral-800 hover:bg-neutral-700"
                      }`}
                      title={
                        p.trusted
                          ? "Trusted: this peer may create sessions, browse files, and run git on this host. Click to revoke."
                          : "Not trusted: this peer is paired but cannot create sessions or read files on this host. Click to grant."
                      }
                    >
                      {p.trusted ? "Untrust" : "Trust"}
                    </button>
                    <button
                      onClick={() => {
                        setRotateFor(rotateFor === p.deviceId ? "" : p.deviceId);
                        setNewKey("");
                        // Close the edit form when opening rotate
                        // so the row never shows both panels at
                        // once — they reuse the same horizontal
                        // strip below the row header.
                        setEditFor("");
                      }}
                      className="px-2 py-1 bg-neutral-800 hover:bg-neutral-700 rounded text-xs"
                      title="Replace this peer's long-lived Ed25519 identity key (audited)"
                    >
                      {rotateFor === p.deviceId ? "Cancel" : "Rotate key"}
                    </button>
                    <button
                      onClick={() => remove(p.deviceId, p.name)}
                      className="px-2 py-1 text-neutral-600 hover:text-red-400 rounded text-xs"
                      title="Remove this peer from the registry"
                    >
                      Delete
                    </button>
                  </div>
                )}
              </div>

              {!isSelf && editFor === p.deviceId && (
                <div className="mt-3 space-y-2 border-t border-neutral-800 pt-3">
                  <div className="text-[11px] text-neutral-500">
                    Display name (free-form label; agents reference this peer by name):
                  </div>
                  <input
                    type="text"
                    value={editName}
                    onChange={(e) => setEditName(e.target.value)}
                    placeholder="laptop"
                    className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
                  />
                  <div className="text-[11px] text-neutral-500">
                    Dial URL (host:port or http(s)://host:port):
                  </div>
                  <input
                    type="text"
                    value={editURL}
                    onChange={(e) => setEditURL(e.target.value)}
                    placeholder="http://100.64.0.5:8080"
                    className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500"
                  />
                  <button
                    onClick={() => submitEdit(p)}
                    disabled={busy || !editName.trim() || !editURL.trim()}
                    className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-xs font-medium disabled:opacity-40"
                  >
                    {busy ? "Saving..." : "Save changes"}
                  </button>
                </div>
              )}

              {!isSelf && rotateFor === p.deviceId && (
                <div className="mt-3 space-y-2 border-t border-neutral-800 pt-3">
                  <div className="text-[11px] text-neutral-600">
                    Current key:{" "}
                    <span className="font-mono text-neutral-500 break-all">
                      {p.publicKey}
                    </span>
                  </div>
                  <input
                    type="text"
                    value={newKey}
                    onChange={(e) => setNewKey(e.target.value)}
                    placeholder="New public key (base64-std, 32-byte Ed25519)"
                    className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500"
                  />
                  <button
                    onClick={() => submitRotate(p.deviceId)}
                    disabled={busy || !newKey.trim()}
                    className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-xs font-medium disabled:opacity-40"
                  >
                    {busy ? "Rotating..." : "Rotate identity key"}
                  </button>
                </div>
              )}
            </div>
          );
        })
      )}
    </div>
  );
}
