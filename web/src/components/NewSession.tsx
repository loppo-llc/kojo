import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { api, type ServerInfo } from "../lib/api";

export function NewSession() {
  const navigate = useNavigate();
  const [info, setInfo] = useState<ServerInfo>();
  const [tool, setTool] = useState("claude");
  const [workDir, setWorkDir] = useState("");
  const [args, setArgs] = useState("");
  const [yoloMode, setYoloMode] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [suggestions, setSuggestions] = useState<string[]>([]);
  const [showSuggestions, setShowSuggestions] = useState(false);
  const suggestTimer = useRef<ReturnType<typeof setTimeout>>(null);
  const wrapperRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    api.info().then((info) => {
      setInfo(info);
      const available = Object.entries(info.tools).find(([, t]) => t.available);
      if (available) setTool(available[0]);
      if (info.homeDir) setWorkDir(info.homeDir);
    }).catch(console.error);
  }, []);

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
      api.dirSuggest(value).then((dirs) => {
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
      const parsedArgs = args.trim() ? args.trim().split(/\s+/) : undefined;
      const session = await api.sessions.create({ tool, workDir, args: parsedArgs, yoloMode });
      navigate(`/session/${session.id}`, { replace: true });
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
                    onChange={() => setTool(name)}
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

        {error && (
          <div className="p-3 bg-red-950 border border-red-800 rounded-lg text-sm text-red-300">
            {error}
          </div>
        )}

        <button
          onClick={handleCreate}
          disabled={loading || !tool || !workDir}
          className="w-full py-3 bg-neutral-800 hover:bg-neutral-700 rounded-lg text-sm font-medium disabled:opacity-40"
        >
          {loading ? "Starting..." : "Start Session"}
        </button>
      </main>
    </div>
  );
}
