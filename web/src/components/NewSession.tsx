import { useEffect, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { api, type ServerInfo } from "../lib/api";
import { peersApi, type PeerInfo } from "../lib/peerApi";

export function NewSession() {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const [info, setInfo] = useState<ServerInfo>();
  const [tool, setTool] = useState("claude");
  const [model, setModel] = useState("");
  const [workDir, setWorkDir] = useState("");
  const [args, setArgs] = useState("");
  const [yoloMode, setYoloMode] = useState(false);
  const [simpleSystemPrompt, setSimpleSystemPrompt] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [suggestions, setSuggestions] = useState<string[]>([]);
  const [showSuggestions, setShowSuggestions] = useState(false);
  const suggestTimer = useRef<ReturnType<typeof setTimeout>>(null);
  const wrapperRef = useRef<HTMLDivElement>(null);
  // Peer selector. Empty selectedPeerId = "this host". The list
  // includes every registry row (including self) so the operator
  // can explicitly confirm "this device" — the dropdown shows
  // human-friendly names, not URLs.
  const [peers, setPeers] = useState<PeerInfo[]>([]);
  const [selfPeerId, setSelfPeerId] = useState("");
  const [selectedPeerId, setSelectedPeerId] = useState("");

  // Peer list is best-effort: a server without peer-identity wiring
  // returns 404 / 503 and we just hide the selector. Runs once.
  useEffect(() => {
    peersApi.list().then((resp) => {
      setPeers(resp.items ?? []);
      setSelfPeerId(resp.selfDeviceId ?? "");
    }).catch(() => { /* peer registry unavailable */ });
  }, []);

  // Fetch server info from the SELECTED host (Hub default, or the
  // remote peer when one is chosen) so tool availability / homeDir
  // reflect what's actually installed on that machine. Re-runs on
  // peer switch — the Hub proxy forwards the info call when
  // ?peer=<id> is present and the peer is paired (registered on
  // both sides via the join-request flow).
  //
  // Concurrency: a rapid peer switch can spawn two info() promises;
  // without a guard the slower (older) reply could overwrite the
  // newer one and leave the form on the wrong host's tools / homeDir.
  // The infoSeq counter discards stale replies. On failure we clear
  // `info` so the Create button (which gates on `tool` + `workDir`)
  // can't fire against the previous host's settings.
  const infoSeq = useRef(0);
  const [infoLoading, setInfoLoading] = useState(false);
  useEffect(() => {
    const peerId = selectedPeerId && selectedPeerId !== selfPeerId ? selectedPeerId : undefined;
    const seq = ++infoSeq.current;
    setError("");
    // Clear the form during the switch so a click on Start lands
    // on whichever host the user picked LAST. Otherwise the
    // previous host's tool / workDir stays selectable while the
    // new info() is still in flight.
    setInfo(undefined);
    setTool("");
    setWorkDir("");
    setInfoLoading(true);
    api.info(peerId).then((info) => {
      if (seq !== infoSeq.current) return;
      setInfo(info);
      const paramTool = searchParams.get("tool");
      const paramDir = searchParams.get("workDir");
      if (paramTool && info.tools?.[paramTool]?.available) {
        setTool(paramTool);
      } else if (info.tools) {
        const available = Object.entries(info.tools).find(([, t]) => t.available);
        if (available) setTool(available[0]);
        else setTool("");
      }
      setWorkDir(paramDir || info.homeDir || "");
      setInfoLoading(false);
    }).catch((err) => {
      if (seq !== infoSeq.current) return;
      console.error(err);
      setInfoLoading(false);
      if (selectedPeerId && selectedPeerId !== selfPeerId) {
        setError("Peer info unavailable. Is the peer online and paired with this host?");
      }
    });
  }, [searchParams, selectedPeerId, selfPeerId]);

  // close suggestions on outside click
  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (wrapperRef.current && !wrapperRef.current.contains(e.target as Node)) {
        setShowSuggestions(false);
      }
    };
    document.addEventListener("mousedown", handler);
    return () => document.removeEventListener("mousedown", handler);
  }, []);

  const fetchSuggestions = (value: string) => {
    if (suggestTimer.current) clearTimeout(suggestTimer.current);
    if (!value || value.length < 2) {
      setSuggestions([]);
      setShowSuggestions(false);
      return;
    }
    suggestTimer.current = setTimeout(() => {
      const peerId = selectedPeerId && selectedPeerId !== selfPeerId ? selectedPeerId : undefined;
      api.dirSuggest(value, peerId).then((dirs) => {
        setSuggestions(dirs);
        setShowSuggestions(dirs.length > 0);
      }).catch(console.error);
    }, 150);
  };

  const handleWorkDirChange = (value: string) => {
    setWorkDir(value);
    fetchSuggestions(value);
  };

  const selectSuggestion = (dir: string) => {
    setWorkDir(dir);
    setShowSuggestions(false);
    fetchSuggestions(dir + "/");
  };

  const handleCreate = async () => {
    if (!tool || !workDir) return;
    setLoading(true);
    setError("");
    try {
      let parsedArgs = args.trim() ? args.trim().split(/\s+/) : [];
      if (model && !parsedArgs.includes("--model")) {
        parsedArgs = ["--model", model, ...parsedArgs];
      }
      const peerId = selectedPeerId && selectedPeerId !== selfPeerId ? selectedPeerId : undefined;
      const session = await api.sessions.create({
        tool,
        workDir,
        args: parsedArgs.length > 0 ? parsedArgs : undefined,
        yoloMode,
        simpleSystemPrompt,
        peerId,
      });
      const target = peerId
        ? `/session/${session.id}?peer=${encodeURIComponent(peerId)}`
        : `/session/${session.id}`;
      navigate(target, { replace: true });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setLoading(false);
    }
  };

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center justify-between px-4 py-3 border-b border-neutral-800">
        <div className="flex items-center gap-2">
          <button onClick={() => navigate("/", { replace: true })} className="text-neutral-400 hover:text-neutral-200">
            &larr;
          </button>
          <h1 className="text-lg font-bold">New Session</h1>
        </div>
      </header>

      <main className="p-4 space-y-6 max-w-md mx-auto">
        {/* Peer selection (only when 2+ peers are registered) */}
        {peers.length > 1 && (
          <div>
            <label className="block text-sm text-neutral-400 mb-2">Host</label>
            <select
              value={selectedPeerId || selfPeerId}
              onChange={(e) => setSelectedPeerId(e.target.value)}
              className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
            >
              {peers.map((p) => {
                const isSelf = p.deviceId === selfPeerId;
                const offline = p.status !== "online";
                // A non-self peer needs to be paired (registered on
                // both sides via the join-request flow). The local
                // UI can only hint at what we know: status + name.
                // The Hub-side proxy will 403 if the pairing is
                // missing on the other side — surface it as a
                // runtime error rather than disabling here.
                const disabled = !isSelf && offline;
                return (
                  <option key={p.deviceId} value={p.deviceId} disabled={disabled}>
                    {p.name}
                    {isSelf ? " (this device)" : ""}
                    {offline && !isSelf ? " — offline" : ""}
                  </option>
                );
              })}
            </select>
          </div>
        )}

        {/* Tool selection */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Tool</label>
          <div className="space-y-2">
            {info &&
              Object.entries(info.tools).map(([name, t]) => (
                <label
                  key={name}
                  className={`flex items-center gap-3 p-3 rounded-lg border cursor-pointer ${
                    tool === name
                      ? "border-neutral-500 bg-neutral-800"
                      : "border-neutral-800 bg-neutral-900"
                  } ${!t.available ? "opacity-40 cursor-not-allowed" : ""}`}
                >
                  <input
                    type="radio"
                    name="tool"
                    value={name}
                    checked={tool === name}
                    disabled={!t.available}
                    onChange={() => {
                      setTool(name);
                      setModel("");
                    }}
                    className="accent-neutral-400"
                  />
                  <span className="font-mono">{name}</span>
                  {!t.available && (
                    <span className="text-xs text-neutral-600">(not available)</span>
                  )}
                </label>
              ))}
          </div>
        </div>

        {/* Working directory */}
        <div ref={wrapperRef} className="relative">
          <label className="block text-sm text-neutral-400 mb-2">Working Directory</label>
          <input
            type="text"
            value={workDir}
            onChange={(e) => handleWorkDirChange(e.target.value)}
            onFocus={() => suggestions.length > 0 && setShowSuggestions(true)}
            placeholder="/path/to/your/project"
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
          />
          {showSuggestions && (
            <ul className="absolute z-10 left-0 right-0 mt-1 bg-neutral-900 border border-neutral-700 rounded max-h-48 overflow-y-auto">
              {suggestions.map((dir) => (
                <li key={dir}>
                  <button
                    type="button"
                    onClick={() => selectSuggestion(dir)}
                    className="w-full text-left px-3 py-2 text-sm font-mono hover:bg-neutral-800 truncate"
                  >
                    {dir}
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>

        {/* Additional arguments */}
        <div>
          <label className="block text-sm text-neutral-400 mb-2">Additional Arguments</label>
          <input
            type="text"
            value={args}
            onChange={(e) => setArgs(e.target.value)}
            placeholder="--model opus"
            className="w-full px-3 py-2 bg-neutral-900 border border-neutral-700 rounded text-sm focus:outline-none focus:border-neutral-500"
          />
        </div>

        {/* Yolo mode */}
        <label className="flex items-center gap-3 p-3 bg-neutral-900 rounded-lg border border-neutral-800 cursor-pointer">
          <input
            type="checkbox"
            checked={yoloMode}
            onChange={(e) => setYoloMode(e.target.checked)}
            className="accent-yellow-500"
          />
          <span>&#x26A1; Yolo Mode</span>
          <span className="text-xs text-neutral-500 ml-auto">{yoloMode ? "ON" : "OFF"}</span>
        </label>

        {/* Minimal system prompt (claude / custom only) */}
        {(tool === "claude" || tool === "custom") && (
          <div>
            <label className="flex items-center gap-3 p-3 bg-neutral-900 rounded-lg border border-neutral-800 cursor-pointer">
              <input
                type="checkbox"
                checked={simpleSystemPrompt}
                onChange={(e) => setSimpleSystemPrompt(e.target.checked)}
                className="accent-neutral-400"
              />
              <span>Minimal System Prompt</span>
              <span className="text-xs text-neutral-500 ml-auto">{simpleSystemPrompt ? "ON" : "OFF"}</span>
            </label>
            <p className="text-xs text-neutral-500 mt-1 px-1">
              Override claude&apos;s default system prompt with just a working-directory note (<code>--system-prompt</code>).
            </p>
          </div>
        )}

        {error && (
          <div className="p-3 bg-red-950 border border-red-800 rounded-lg text-sm text-red-300">
            {error}
          </div>
        )}

        <button
          onClick={handleCreate}
          disabled={loading || infoLoading || !tool || !workDir}
          className="w-full py-3 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-sm font-medium disabled:opacity-40"
        >
          {loading ? "Starting..." : "Start Session"}
        </button>
      </main>
    </div>
  );
}
