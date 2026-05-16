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
import { peersApi, type PeerInfo } from "../../lib/peerApi";

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
  // Form state for add.
  const [deviceId, setDeviceId] = useState("");
  const [name, setName] = useState("");
  const [publicKey, setPublicKey] = useState("");
  const [capabilities, setCapabilities] = useState("");
  // Form state for rotate.
  const [newKey, setNewKey] = useState("");
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
        const resp = await peersApi.list();
        if (!mounted.current) return;
        if (myseq === requestSeq.current) {
          setItems(resp.items ?? []);
          setSelfId(resp.selfDeviceId ?? "");
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
    setDeviceId("");
    setName("");
    setPublicKey("");
    setCapabilities("");
    setShowAdd(false);
  };

  const submitAdd = async () => {
    if (!deviceId.trim() || !name.trim() || !publicKey.trim()) return;
    setBusy(true);
    try {
      await peersApi.register({
        deviceId: deviceId.trim(),
        name: name.trim(),
        publicKey: publicKey.trim(),
        capabilities: capabilities.trim() || undefined,
      });
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
        cannot be added or removed from this UI.
      </p>

      {showAdd && (
        <div className="p-3 bg-neutral-900 border border-neutral-800 rounded-lg mb-2 space-y-2">
          <input
            type="text"
            value={deviceId}
            onChange={(e) => setDeviceId(e.target.value)}
            placeholder="Device ID (UUID)"
            className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500"
          />
          <input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Name"
            className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs focus:outline-none focus:border-neutral-500"
          />
          <input
            type="text"
            value={publicKey}
            onChange={(e) => setPublicKey(e.target.value)}
            placeholder="Public key (base64-std, 32-byte Ed25519)"
            className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500"
          />
          <textarea
            value={capabilities}
            onChange={(e) => setCapabilities(e.target.value)}
            placeholder='Capabilities (optional JSON object, e.g. {"os":"macos"})'
            rows={2}
            className="w-full px-3 py-2 bg-neutral-800 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500"
          />
          <button
            onClick={submitAdd}
            disabled={busy || !deviceId.trim() || !name.trim() || !publicKey.trim()}
            className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-xs font-medium disabled:opacity-40"
          >
            {busy ? "Registering..." : "Register peer"}
          </button>
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
                  <div className="text-xs mt-1 flex items-center gap-3">
                    <span className={STATUS_COLOR[p.status] ?? "text-neutral-500"}>
                      {p.status}
                    </span>
                    <span className="text-neutral-600">
                      seen {formatLastSeen(p.lastSeen)}
                    </span>
                  </div>
                </div>
                {!isSelf && (
                  <div className="flex flex-col gap-1 shrink-0">
                    <button
                      onClick={() => {
                        setRotateFor(rotateFor === p.deviceId ? "" : p.deviceId);
                        setNewKey("");
                      }}
                      className="px-2 py-1 bg-neutral-800 hover:bg-neutral-700 rounded text-xs"
                    >
                      {rotateFor === p.deviceId ? "Cancel" : "Rotate key"}
                    </button>
                    <button
                      onClick={() => remove(p.deviceId, p.name)}
                      className="px-2 py-1 text-neutral-600 hover:text-red-400 rounded text-xs"
                    >
                      Delete
                    </button>
                  </div>
                )}
              </div>

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
