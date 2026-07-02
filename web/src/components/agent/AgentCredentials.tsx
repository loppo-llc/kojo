import { useEffect, useState, useRef, useCallback } from "react";
import { useParams, useNavigate } from "react-router";
import { agentApi, type AgentInfo, type Credential, type OTPEntry } from "../../lib/agentApi";
import { errMsg } from "../../lib/utils";
import { PageHeader } from "../ui/PageHeader";
import { Input } from "../ui/Input";
import { Textarea } from "../ui/Textarea";
import { Button } from "../ui/Button";
import { Banner } from "../ui/Banner";

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
        className="rounded-md px-2 py-1 text-[12px] text-copper transition-colors hover:bg-hover"
      >
        {loading ? "..." : "TOTP"}
      </button>
    );
  }

  return (
    <div className="flex items-center gap-2">
      <span className="font-mono text-[14px] tracking-widest text-copper-bright">{code}</span>
      <span className="w-5 text-right text-[12px] text-ink-faint">{remaining}s</span>
      <button
        onClick={handleCopy}
        className={`rounded-md px-1.5 py-0.5 text-[12px] transition-colors ${
          copied ? "text-lamp-run" : "text-ink-faint hover:bg-hover hover:text-ink-dim"
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
      setError(errMsg(err));
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
      setError(errMsg(err));
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
      <div className="flex max-h-[80vh] w-full max-w-md flex-col rounded-[10px] border border-hairline bg-raised">
        <div className="flex items-center justify-between border-b border-hairline px-4 py-3">
          <h2 className="text-[14px] font-semibold text-ink">Import TOTP</h2>
          <button onClick={onClose} className="text-ink-faint transition-colors hover:text-ink" aria-label="Close">
            &times;
          </button>
        </div>

        <div className="flex-1 space-y-3 overflow-y-auto p-4">
          {entries.length === 0 ? (
            <>
              <div className="flex gap-1 rounded-lg border border-hairline bg-app p-1">
                <button
                  onClick={() => setMode("upload")}
                  className={`flex-1 rounded-md py-1.5 text-[12px] transition-colors ${
                    mode === "upload" ? "bg-copper/15 text-copper-bright" : "text-ink-dim hover:text-ink"
                  }`}
                >
                  QR Image
                </button>
                <button
                  onClick={() => setMode("uri")}
                  className={`flex-1 rounded-md py-1.5 text-[12px] transition-colors ${
                    mode === "uri" ? "bg-copper/15 text-copper-bright" : "text-ink-dim hover:text-ink"
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
                    className="w-full rounded-lg border-2 border-dashed border-hairline py-8 text-[14px] text-ink-faint transition-colors hover:border-ink-faint hover:text-ink-dim"
                  >
                    {loading ? "Decoding..." : "Tap to select QR image"}
                  </button>
                </div>
              ) : (
                <div className="space-y-2">
                  <Textarea
                    mono
                    value={uri}
                    onChange={(e) => setUri(e.target.value)}
                    placeholder="otpauth://totp/... or otpauth-migration://..."
                    rows={3}
                  />
                  <Button
                    variant="primary"
                    onClick={handleURI}
                    disabled={loading || !uri.trim()}
                    className="w-full"
                  >
                    {loading ? "Parsing..." : "Parse"}
                  </Button>
                </div>
              )}
            </>
          ) : (
            <>
              <div className="text-[12px] text-ink-dim">
                {entries.length} entries found &mdash; select one
              </div>
              {entries.map((entry, i) => (
                <button
                  key={i}
                  onClick={() => onSelect(entry)}
                  className="flex w-full items-start gap-3 rounded-[10px] border border-hairline bg-app p-3 text-left transition-colors hover:border-ink-faint"
                >
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-[14px] font-medium text-ink">
                      {entry.issuer || entry.label || "Unknown"}
                    </div>
                    <div className="truncate font-mono text-[12px] text-ink-faint">{entry.username}</div>
                  </div>
                </button>
              ))}
            </>
          )}

          {error && <Banner tone="error">{error}</Banner>}
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
      setError(errMsg(err));
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
    <div className="space-y-3 rounded-[10px] border border-hairline bg-surface p-4">
      <Input
        value={label}
        onChange={(e) => setLabel(e.target.value)}
        placeholder="Label"
        autoFocus
      />
      <Input
        value={username}
        onChange={(e) => setUsername(e.target.value)}
        placeholder="Username / ID"
      />
      <Input
        type="password"
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        placeholder="New password (leave empty to keep)"
      />
      <div className="flex gap-2">
        <Input
          type="password"
          value={totpSecret}
          onChange={(e) => setTotpSecret(e.target.value)}
          placeholder={hasTOTP ? "New TOTP secret (leave empty to keep)" : "TOTP Secret (optional)"}
          className="flex-1"
        />
        <Button onClick={() => setShowQR(true)} disabled={isSwitching} className="shrink-0">
          QR
        </Button>
      </div>
      {error && <Banner tone="error">{error}</Banner>}
      <div className="flex gap-2">
        <Button
          variant="primary"
          onClick={handleSave}
          disabled={saving || isSwitching || !label.trim() || !username.trim()}
          title={isSwitching ? "デバイス転移中。完了するまで保存できない。" : undefined}
          className="flex-1"
        >
          {saving ? "Saving..." : "Save"}
        </Button>
        <Button onClick={onCancel}>Cancel</Button>
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
          setListError(errMsg(err));
        }
        if (agentRes.status === "fulfilled") {
          setAgent(agentRes.value);
        } else if (credsRes.status === "fulfilled") {
          // creds loaded but agent record didn't — surface so the user
          // sees that the isSwitching indicator is unknown (UI defaults
          // to enabled, which is wrong if a switch is actually running).
          const err = agentRes.reason;
          setListError(
            "agent record fetch failed: " + errMsg(err),
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
      setError(errMsg(err));
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
      setError(errMsg(err));
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
      setError(errMsg(err));
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
    <div className="min-h-full bg-app text-ink">
      <PageHeader title="Credentials" onBack={() => navigate(`/agents/${id}`, { replace: true })}>
        <Button
          onClick={() => { setShowForm((v) => !v); setEditingId(null); }}
          disabled={isSwitching}
          title={isSwitching ? "デバイス転移中。完了するまで追加できない。" : undefined}
        >
          {showForm ? "Cancel" : "+ Add"}
        </Button>
      </PageHeader>

      <main className="mx-auto max-w-[560px] space-y-3 px-4 py-6">
        {isSwitching && (
          <Banner tone="warn">
            デバイス転移中。完了するまで credential は編集できない。
          </Banner>
        )}

        {listError && (
          <Banner
            tone="error"
            action={
              <Button onClick={reload} variant="danger" className="shrink-0">
                Retry
              </Button>
            }
          >
            {listError}
          </Banner>
        )}

        {/* Add form */}
        {showForm && (
          <div className="space-y-3 rounded-[10px] border border-hairline bg-surface p-4">
            <Input
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              placeholder="Label (e.g. GitHub)"
              autoFocus
            />
            <Input
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="Username / ID"
            />
            <Input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="Password"
            />
            <div className="flex gap-2">
              <Input
                type="password"
                value={totpSecret}
                onChange={(e) => setTotpSecret(e.target.value)}
                placeholder="TOTP Secret (optional)"
                className="flex-1"
              />
              <Button onClick={() => setShowAddQR(true)} disabled={isSwitching} className="shrink-0">
                QR
              </Button>
            </div>
            <Button
              variant="primary"
              onClick={handleAdd}
              disabled={
                adding || isSwitching || !label.trim() || !username.trim() || (!password && !totpSecret.trim())
              }
              title={isSwitching ? "デバイス転移中。完了するまで追加できない。" : undefined}
              className="w-full"
            >
              {adding ? "Adding..." : "Add"}
            </Button>
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
              className="rounded-[10px] border border-hairline bg-surface p-3"
            >
              <div className="text-[14px] font-medium text-ink">
                {cred.label}
              </div>
              <div className="mt-1 font-mono text-[12px] text-ink-faint">
                {cred.username}
              </div>
              <div className="mt-2 flex items-center justify-between">
                <span className="select-none tracking-widest text-ink-faint">
                  ••••••••
                </span>
                <div className="flex gap-1">
                  <button
                    onClick={() => handleCopy(cred.id)}
                    className={`rounded-md px-2 py-1 text-[12px] transition-colors ${
                      copied === cred.id
                        ? "text-lamp-run"
                        : "text-ink-faint hover:bg-hover hover:text-ink-dim"
                    }`}
                  >
                    {copied === cred.id ? "Copied" : "Copy PW"}
                  </button>
                  <button
                    onClick={() => { setEditingId(cred.id); setShowForm(false); }}
                    disabled={isSwitching}
                    title={isSwitching ? "デバイス転移中。完了するまで編集できない。" : undefined}
                    className="rounded-md px-2 py-1 text-[12px] text-ink-faint transition-colors hover:bg-hover hover:text-ink-dim disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:bg-transparent"
                  >
                    Edit
                  </button>
                  <button
                    onClick={() => handleDelete(cred.id)}
                    disabled={isSwitching}
                    title={isSwitching ? "デバイス転移中。完了するまで削除できない。" : undefined}
                    className="rounded-md px-2 py-1 text-[12px] text-ink-faint transition-colors hover:bg-hover hover:text-lamp-err disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:bg-transparent"
                  >
                    Delete
                  </button>
                </div>
              </div>
              {cred.totpSecret && id && (
                <div className="mt-2 border-t border-hairline pt-2">
                  <TOTPDisplay agentId={id} credId={cred.id} />
                </div>
              )}
            </div>
          ),
        )}

        {!loading && !listError && credentials.length === 0 && !showForm && (
          <div className="py-12 text-center text-[14px] text-ink-faint">
            No credentials registered
          </div>
        )}

        {error && <Banner tone="error">{error}</Banner>}
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
