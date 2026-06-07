import { useEffect, useState, useRef, useCallback } from "react";
import { useParams, useNavigate } from "react-router";
import { agentApi, type AgentInfo, type Credential, type OTPEntry } from "../../lib/agentApi";

function TOTPDisplay({ agentId, credId }: { agentId: string; credId: string }) {
  const [code, setCode] = useState<string | null>(null);
  const [remaining, setRemaining] = useState(0);
  const [loading, setLoading] = useState(false);
  const timerRef = useRef<ReturnType<typeof setInterval>>(undefined);
  const [copied, setCopied] = useState(false);

  const fetchCode = useCallback(async () => {
    try {
      const r = await agentApi.credentials.getTOTPCode(agentId, credId);
      setCode(r.code);
      setRemaining(r.remaining);
    } catch {
      setCode(null);
    }
  }, [agentId, credId]);

  const handleReveal = async () => {
    setLoading(true);
    await fetchCode();
    setLoading(false);
  };

  useEffect(() => {
    if (code === null) return;
    timerRef.current = setInterval(() => {
      setRemaining((prev) => {
        if (prev <= 1) {
          fetchCode();
          return prev;
        }
        return prev - 1;
      });
    }, 1000);
    return () => clearInterval(timerRef.current);
  }, [code, fetchCode]);

  const handleCopy = async () => {
    if (!code) return;
    const r = await agentApi.credentials.getTOTPCode(agentId, credId);
    await navigator.clipboard.writeText(r.code);
    setCode(r.code);
    setRemaining(r.remaining);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  if (code === null) {
    return (
      <button
        onClick={handleReveal}
        disabled={loading}
        className="text-xs px-2 py-1 rounded text-blue-400 hover:bg-neutral-800"
      >
        {loading ? "..." : "TOTP"}
      </button>
    );
  }

  return (
    <div className="flex items-center gap-2">
      <span className="font-mono text-sm text-blue-300 tracking-widest">{code}</span>
      <span className="text-xs text-neutral-500 w-5 text-right">{remaining}s</span>
      <button
        onClick={handleCopy}
        className={`text-xs px-1.5 py-0.5 rounded ${
          copied ? "text-green-400 bg-green-950" : "text-neutral-500 hover:text-neutral-300 hover:bg-neutral-800"
        }`}
      >
        {copied ? "OK" : "Copy"}
      </button>
    </div>
  );
}

/** QR/URI import modal — returns a single OTPEntry to apply to the credential being edited. */
function QRImportModal({
  agentId,
  onSelect,
  onClose,
}: {
  agentId: string;
  onSelect: (entry: OTPEntry) => void;
  onClose: () => void;
}) {
  const [entries, setEntries] = useState<OTPEntry[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [mode, setMode] = useState<"upload" | "uri">("upload");
  const [uri, setUri] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);

  const handleFile = async (file: File) => {
    setLoading(true);
    setError("");
    try {
      const result = await agentApi.credentials.parseQR(agentId, file);
      if (result.length === 1) {
        onSelect(result[0]);
      } else {
        setEntries(result);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  };

  const handleURI = async () => {
    if (!uri.trim()) return;
    setLoading(true);
    setError("");
    try {
      const result = await agentApi.credentials.parseOTPURI(agentId, uri.trim());
      if (result.length === 1) {
        onSelect(result[0]);
      } else {
        setEntries(result);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="fixed inset-0 bg-black/60 flex items-center justify-center z-50 p-4">
      <div className="bg-neutral-900 border border-neutral-700 rounded-lg w-full max-w-md max-h-[80vh] flex flex-col">
        <div className="flex items-center justify-between px-4 py-3 border-b border-neutral-800">
          <h2 className="text-sm font-bold">Import TOTP</h2>
          <button onClick={onClose} className="text-neutral-500 hover:text-neutral-300">
            &times;
          </button>
        </div>

        <div className="p-4 space-y-3 overflow-y-auto flex-1">
          {entries.length === 0 ? (
            <>
              <div className="flex gap-2">
                <button
                  onClick={() => setMode("upload")}
                  className={`flex-1 py-1.5 text-xs rounded ${
                    mode === "upload" ? "bg-neutral-700 text-neutral-200" : "text-neutral-500 hover:bg-neutral-800"
                  }`}
                >
                  QR Image
                </button>
                <button
                  onClick={() => setMode("uri")}
                  className={`flex-1 py-1.5 text-xs rounded ${
                    mode === "uri" ? "bg-neutral-700 text-neutral-200" : "text-neutral-500 hover:bg-neutral-800"
                  }`}
                >
                  URI Text
                </button>
              </div>

              {mode === "upload" ? (
                <div>
                  <input
                    ref={fileRef}
                    type="file"
                    accept="image/*"
                    className="hidden"
                    onChange={(e) => {
                      const f = e.target.files?.[0];
                      if (f) handleFile(f);
                    }}
                  />
                  <button
                    onClick={() => fileRef.current?.click()}
                    disabled={loading}
                    className="w-full py-8 border-2 border-dashed border-neutral-700 rounded-lg text-neutral-500 hover:border-neutral-500 hover:text-neutral-300 text-sm"
                  >
                    {loading ? "Decoding..." : "Tap to select QR image"}
                  </button>
                </div>
              ) : (
                <div className="space-y-2">
                  <textarea
                    value={uri}
                    onChange={(e) => setUri(e.target.value)}
                    placeholder="otpauth://totp/... or otpauth-migration://..."
                    rows={3}
                    className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-xs font-mono focus:outline-none focus:border-neutral-500 resize-none"
                  />
                  <button
                    onClick={handleURI}
                    disabled={loading || !uri.trim()}
                    className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-sm font-medium disabled:opacity-40"
                  >
                    {loading ? "Parsing..." : "Parse"}
                  </button>
                </div>
              )}
            </>
          ) : (
            <>
              <div className="text-xs text-neutral-500">
                {entries.length} entries found &mdash; select one
              </div>
              {entries.map((entry, i) => (
                <button
                  key={i}
                  onClick={() => onSelect(entry)}
                  className="w-full flex items-start gap-3 p-3 rounded-lg border border-neutral-800 bg-neutral-950 hover:border-neutral-600 text-left"
                >
                  <div className="min-w-0 flex-1">
                    <div className="text-sm font-medium text-neutral-300 truncate">
                      {entry.issuer || entry.label || "Unknown"}
                    </div>
                    <div className="text-xs text-neutral-500 font-mono truncate">{entry.username}</div>
                  </div>
                </button>
              ))}
            </>
          )}

          {error && (
            <div className="p-2 bg-red-950 border border-red-800 rounded text-xs text-red-300">{error}</div>
          )}
        </div>
      </div>
    </div>
  );
}

/** Inline edit form for a single credential. */
function CredentialEdit({
  agentId,
  credential,
  isSwitching,
  onSave,
  onCancel,
}: {
  agentId: string;
  credential: Credential;
  isSwitching: boolean;
  onSave: (updated: Credential) => void;
  onCancel: () => void;
}) {
  const [label, setLabel] = useState(credential.label);
  const [username, setUsername] = useState(credential.username);
  const [password, setPassword] = useState("");
  const [totpSecret, setTotpSecret] = useState("");
  const [totpAlgorithm, setTotpAlgorithm] = useState("");
  const [totpDigits, setTotpDigits] = useState(0);
  const [totpPeriod, setTotpPeriod] = useState(0);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [showQR, setShowQR] = useState(false);

  const hasTOTP = !!credential.totpSecret;

  const handleSave = async () => {
    setSaving(true);
    setError("");
    try {
      const data: Record<string, unknown> = {};
      if (label.trim() !== credential.label) data.label = label.trim();
      if (username.trim() !== credential.username) data.username = username.trim();
      if (password) data.password = password;
      if (totpSecret.trim()) {
        data.totpSecret = totpSecret.trim();
        if (totpAlgorithm) data.totpAlgorithm = totpAlgorithm;
        if (totpDigits) data.totpDigits = totpDigits;
        if (totpPeriod) data.totpPeriod = totpPeriod;
      }

      if (Object.keys(data).length === 0) {
        onCancel();
        return;
      }

      const updated = await agentApi.credentials.update(agentId, credential.id, data);
      onSave(updated);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const handleQRSelect = (entry: OTPEntry) => {
    setTotpSecret(entry.totpSecret);
    setTotpAlgorithm(entry.algorithm || "");
    setTotpDigits(entry.digits || 0);
    setTotpPeriod(entry.period || 0);
    setShowQR(false);
  };

  return (
    <div className="p-4 bg-neutral-900 border border-neutral-700 rounded-lg space-y-3">
      <input
        type="text"
        value={label}
        onChange={(e) => setLabel(e.target.value)}
        placeholder="Label"
        className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
        autoFocus
      />
      <input
        type="text"
        value={username}
        onChange={(e) => setUsername(e.target.value)}
        placeholder="Username / ID"
        className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
      />
      <input
        type="password"
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        placeholder="New password (leave empty to keep)"
        className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
      />
      <div className="flex gap-2">
        <input
          type="password"
          value={totpSecret}
          onChange={(e) => setTotpSecret(e.target.value)}
          placeholder={hasTOTP ? "New TOTP secret (leave empty to keep)" : "TOTP Secret (optional)"}
          className="flex-1 px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
        />
        <button
          onClick={() => setShowQR(true)}
          disabled={isSwitching}
          className="px-3 py-2 bg-neutral-800 hover:bg-neutral-700 rounded text-xs whitespace-nowrap disabled:opacity-40 disabled:cursor-not-allowed"
        >
          QR
        </button>
      </div>
      {error && (
        <div className="p-2 bg-red-950 border border-red-800 rounded text-xs text-red-300">{error}</div>
      )}
      <div className="flex gap-2">
        <button
          onClick={handleSave}
          disabled={saving || isSwitching || !label.trim() || !username.trim()}
          title={isSwitching ? "デバイス転移中。完了するまで保存できない。" : undefined}
          className="flex-1 py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-sm font-medium disabled:opacity-40 disabled:cursor-not-allowed"
        >
          {saving ? "Saving..." : "Save"}
        </button>
        <button
          onClick={onCancel}
          className="px-4 py-2 text-neutral-500 hover:text-neutral-300 rounded text-sm"
        >
          Cancel
        </button>
      </div>

      {showQR && (
        <QRImportModal
          agentId={agentId}
          onSelect={handleQRSelect}
          onClose={() => setShowQR(false)}
        />
      )}
    </div>
  );
}

export function AgentCredentials() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [credentials, setCredentials] = useState<Credential[]>([]);
  const [agent, setAgent] = useState<AgentInfo | null>(null);
  const [listError, setListError] = useState("");
  const [loading, setLoading] = useState(true);
  const [label, setLabel] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [totpSecret, setTotpSecret] = useState("");
  const [addTotpAlgorithm, setAddTotpAlgorithm] = useState("");
  const [addTotpDigits, setAddTotpDigits] = useState(0);
  const [addTotpPeriod, setAddTotpPeriod] = useState(0);
  const [error, setError] = useState("");
  const [adding, setAdding] = useState(false);
  const [copied, setCopied] = useState<string | null>(null);
  const [showForm, setShowForm] = useState(false);
  const [showAddQR, setShowAddQR] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);

  // Monotonic generation counter so a stale in-flight reload() that
  // resolves after unmount or after a newer reload() was issued cannot
  // overwrite fresh state. Stored in a ref so the closure captured by
  // the .then() callback observes the latest value at resolve time.
  const reloadGen = useRef(0);

  const reload = useCallback(() => {
    if (!id) return;
    const myGen = ++reloadGen.current;
    setLoading(true);
    setListError("");
    Promise.allSettled([agentApi.credentials.list(id), agentApi.get(id)]).then(
      ([credsRes, agentRes]) => {
        if (myGen !== reloadGen.current) return; // superseded / unmounted
        if (credsRes.status === "fulfilled") {
          setCredentials(credsRes.value);
        } else {
          const err = credsRes.reason;
          setListError(err instanceof Error ? err.message : String(err));
        }
        if (agentRes.status === "fulfilled") {
          setAgent(agentRes.value);
        } else if (credsRes.status === "fulfilled") {
          // creds loaded but agent record didn't — surface so the user
          // sees that the isSwitching indicator is unknown (UI defaults
          // to enabled, which is wrong if a switch is actually running).
          const err = agentRes.reason;
          setListError(
            "agent record fetch failed: " +
              (err instanceof Error ? err.message : String(err)),
          );
        }
        setLoading(false);
      },
    );
  }, [id]);

  useEffect(() => {
    reload();
    return () => {
      // Invalidate any in-flight reload — its .then() will no-op when
      // it observes a bumped generation.
      reloadGen.current++;
    };
  }, [reload]);

  // Poll the agent record while a §3.7 device-switch is in flight so the
  // banner / disabled buttons clear automatically once SetSwitching(false)
  // lands on the server. Self-rescheduling setTimeout (NOT setInterval)
  // so a slow GET doesn't stack overlapping requests / reorder responses.
  // When isSwitching transitions true → false we also refresh the
  // credential list because the agent has moved to a remote peer and
  // future reads proxy there — the local snapshot may now be stale.
  const isSwitching = !!agent?.isSwitching;
  const prevSwitchingRef = useRef(isSwitching);
  useEffect(() => {
    if (prevSwitchingRef.current && !isSwitching) {
      // Transition true → false: refresh creds against the new holder.
      reload();
    }
    prevSwitchingRef.current = isSwitching;
  }, [isSwitching, reload]);

  useEffect(() => {
    if (!id || !isSwitching) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const tick = async () => {
      try {
        const fresh = await agentApi.get(id);
        if (cancelled) return;
        setAgent(fresh);
      } catch {
        /* keep prior state on transient failures */
      }
      if (!cancelled) {
        timer = setTimeout(tick, 5_000);
      }
    };
    timer = setTimeout(tick, 5_000);
    return () => {
      cancelled = true;
      if (timer !== null) clearTimeout(timer);
    };
  }, [id, isSwitching]);

  const handleAdd = async () => {
    if (!id || !label.trim() || !username.trim() || (!password && !totpSecret.trim())) return;
    setAdding(true);
    setError("");
    try {
      const cred = await agentApi.credentials.add(
        id,
        label.trim(),
        username.trim(),
        password,
        totpSecret.trim()
          ? {
              secret: totpSecret.trim(),
              algorithm: addTotpAlgorithm || undefined,
              digits: addTotpDigits || undefined,
              period: addTotpPeriod || undefined,
            }
          : undefined,
      );
      setCredentials((prev) => [...prev, cred]);
      setLabel("");
      setUsername("");
      setPassword("");
      setTotpSecret("");
      setAddTotpAlgorithm("");
      setAddTotpDigits(0);
      setAddTotpPeriod(0);
      setShowForm(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setAdding(false);
    }
  };

  const handleDelete = async (credId: string) => {
    if (!id || !confirm("Delete this credential?")) return;
    try {
      await agentApi.credentials.delete(id, credId);
      setCredentials((prev) => prev.filter((c) => c.id !== credId));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleCopy = async (credId: string) => {
    if (!id) return;
    try {
      const pw = await agentApi.credentials.revealPassword(id, credId);
      await navigator.clipboard.writeText(pw);
      setCopied(credId);
      setTimeout(() => setCopied(null), 2000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleEditSave = (updated: Credential) => {
    setCredentials((prev) => prev.map((c) => (c.id === updated.id ? updated : c)));
    setEditingId(null);
  };

  const handleAddQRSelect = (entry: OTPEntry) => {
    setTotpSecret(entry.totpSecret);
    setAddTotpAlgorithm(entry.algorithm || "");
    setAddTotpDigits(entry.digits || 0);
    setAddTotpPeriod(entry.period || 0);
    setShowAddQR(false);
  };

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center justify-between px-4 py-3 border-b border-neutral-800">
        <div className="flex items-center gap-2">
          <button
            onClick={() => navigate(`/agents/${id}`, { replace: true })}
            className="text-neutral-400 hover:text-neutral-200"
          >
            &larr;
          </button>
          <h1 className="text-lg font-bold">Credentials</h1>
        </div>
        <button
          onClick={() => { setShowForm((v) => !v); setEditingId(null); }}
          disabled={isSwitching}
          title={isSwitching ? "デバイス転移中。完了するまで追加できない。" : undefined}
          className="px-3 py-1.5 bg-neutral-800 hover:bg-neutral-700 rounded text-sm disabled:opacity-40 disabled:cursor-not-allowed disabled:hover:bg-neutral-800"
        >
          {showForm ? "Cancel" : "+ Add"}
        </button>
      </header>

      <main className="p-4 space-y-3 max-w-md mx-auto">
        {isSwitching && (
          <div className="p-3 bg-amber-950 border border-amber-800 rounded-lg text-sm text-amber-200">
            デバイス転移中。完了するまで credential は編集できない。
          </div>
        )}

        {listError && (
          <div className="p-3 bg-red-950 border border-red-800 rounded-lg text-sm text-red-300 flex items-center justify-between gap-2">
            <span className="break-all">{listError}</span>
            <button
              onClick={reload}
              className="px-2 py-1 bg-red-900 hover:bg-red-800 rounded text-xs whitespace-nowrap"
            >
              Retry
            </button>
          </div>
        )}

        {/* Add form */}
        {showForm && (
          <div className="p-4 bg-neutral-900 border border-neutral-700 rounded-lg space-y-3">
            <input
              type="text"
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              placeholder="Label (e.g. GitHub)"
              className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
              autoFocus
            />
            <input
              type="text"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="Username / ID"
              className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
            />
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="Password"
              className="w-full px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
            />
            <div className="flex gap-2">
              <input
                type="password"
                value={totpSecret}
                onChange={(e) => setTotpSecret(e.target.value)}
                placeholder="TOTP Secret (optional)"
                className="flex-1 px-3 py-2 bg-neutral-950 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
              />
              <button
                onClick={() => setShowAddQR(true)}
                disabled={isSwitching}
                className="px-3 py-2 bg-neutral-800 hover:bg-neutral-700 rounded text-xs whitespace-nowrap disabled:opacity-40 disabled:cursor-not-allowed"
              >
                QR
              </button>
            </div>
            <button
              onClick={handleAdd}
              disabled={
                adding || isSwitching || !label.trim() || !username.trim() || (!password && !totpSecret.trim())
              }
              title={isSwitching ? "デバイス転移中。完了するまで追加できない。" : undefined}
              className="w-full py-2 bg-neutral-700 hover:bg-neutral-600 rounded text-sm font-medium disabled:opacity-40 disabled:cursor-not-allowed"
            >
              {adding ? "Adding..." : "Add"}
            </button>
          </div>
        )}

        {/* Credential list */}
        {credentials.map((cred) =>
          editingId === cred.id && id ? (
            <CredentialEdit
              key={cred.id}
              agentId={id}
              credential={cred}
              isSwitching={isSwitching}
              onSave={handleEditSave}
              onCancel={() => setEditingId(null)}
            />
          ) : (
            <div
              key={cred.id}
              className="p-3 bg-neutral-900 border border-neutral-800 rounded-lg"
            >
              <div className="text-sm font-medium text-neutral-300">
                {cred.label}
              </div>
              <div className="text-xs text-neutral-500 mt-1 font-mono">
                {cred.username}
              </div>
              <div className="flex items-center justify-between mt-2">
                <span className="text-xs text-neutral-600 tracking-widest select-none">
                  ••••••••
                </span>
                <div className="flex gap-1">
                  <button
                    onClick={() => handleCopy(cred.id)}
                    className={`text-xs px-2 py-1 rounded ${
                      copied === cred.id
                        ? "text-green-400 bg-green-950"
                        : "text-neutral-500 hover:text-neutral-300 hover:bg-neutral-800"
                    }`}
                  >
                    {copied === cred.id ? "Copied" : "Copy PW"}
                  </button>
                  <button
                    onClick={() => { setEditingId(cred.id); setShowForm(false); }}
                    disabled={isSwitching}
                    title={isSwitching ? "デバイス転移中。完了するまで編集できない。" : undefined}
                    className="text-xs text-neutral-500 hover:text-neutral-300 px-2 py-1 rounded hover:bg-neutral-800 disabled:opacity-40 disabled:cursor-not-allowed disabled:hover:bg-transparent"
                  >
                    Edit
                  </button>
                  <button
                    onClick={() => handleDelete(cred.id)}
                    disabled={isSwitching}
                    title={isSwitching ? "デバイス転移中。完了するまで削除できない。" : undefined}
                    className="text-xs text-neutral-600 hover:text-red-400 px-2 py-1 rounded hover:bg-neutral-800 disabled:opacity-40 disabled:cursor-not-allowed disabled:hover:bg-transparent"
                  >
                    Delete
                  </button>
                </div>
              </div>
              {cred.totpSecret && id && (
                <div className="mt-2 pt-2 border-t border-neutral-800">
                  <TOTPDisplay agentId={id} credId={cred.id} />
                </div>
              )}
            </div>
          ),
        )}

        {!loading && !listError && credentials.length === 0 && !showForm && (
          <div className="text-sm text-neutral-600 text-center py-12">
            No credentials registered
          </div>
        )}

        {error && (
          <div className="p-3 bg-red-950 border border-red-800 rounded-lg text-sm text-red-300">
            {error}
          </div>
        )}
      </main>

      {showAddQR && id && (
        <QRImportModal
          agentId={id}
          onSelect={handleAddQRSelect}
          onClose={() => setShowAddQR(false)}
        />
      )}
    </div>
  );
}
