import { useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { api, type DirEntry, type FileView } from "../lib/api";

export function FileBrowser() {
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const [entries, setEntries] = useState<DirEntry[]>([]);
  const [currentPath, setCurrentPath] = useState("");
  const [fileView, setFileView] = useState<FileView | null>(null);
  const [showHidden, setShowHidden] = useState(false);

  const path = searchParams.get("path") || "";

  useEffect(() => {
    api.files
      .list(path || undefined, showHidden)
      .then((result) => {
        setCurrentPath(result.path);
        setEntries(result.entries);
      })
      .catch(console.error);
  }, [path, showHidden]);

  const navigateTo = (entry: DirEntry) => {
    const newPath = currentPath + "/" + entry.name;
    if (entry.type === "dir") {
      setSearchParams({ path: newPath });
      setFileView(null);
    } else {
      api.files.view(newPath).then(setFileView).catch(console.error);
    }
  };

  const goUp = () => {
    const parent = currentPath.split("/").slice(0, -1).join("/") || "/";
    setSearchParams({ path: parent });
    setFileView(null);
  };

  if (fileView) {
    return (
      <div className="min-h-full bg-neutral-950 text-neutral-200">
        <header className="flex items-center gap-2 px-4 py-3 border-b border-neutral-800">
          <button onClick={() => setFileView(null)} className="text-neutral-400 hover:text-neutral-200">
            &larr;
          </button>
          <span className="text-sm truncate">{fileView.path.split("/").pop()}</span>
        </header>
        <main className="p-4">
          {fileView.type === "text" && (
            <pre className="text-xs font-mono overflow-x-auto whitespace-pre p-4 bg-neutral-900 rounded-lg border border-neutral-800">
              {fileView.content}
            </pre>
          )}
          {fileView.type === "image" && fileView.url && (
            <div className="flex flex-col items-center gap-4">
              <img src={fileView.url} alt="" className="max-w-full rounded" />
              <div className="text-xs text-neutral-500">
                {formatSize(fileView.size)} &middot; {fileView.mime}
              </div>
            </div>
          )}
        </main>
      </div>
    );
  }

  return (
    <div className="min-h-full bg-neutral-950 text-neutral-200">
      <header className="flex items-center gap-2 px-4 py-3 border-b border-neutral-800">
        <button onClick={() => navigate("/")} className="text-neutral-400 hover:text-neutral-200">
          &larr;
        </button>
        <span className="text-sm truncate flex-1">Files &mdash; {currentPath}</span>
        <button
          onClick={() => setShowHidden(!showHidden)}
          className={`px-2 py-0.5 text-xs rounded ${
            showHidden ? "bg-neutral-700 text-neutral-300" : "bg-neutral-800 text-neutral-500"
          }`}
        >
          .*
        </button>
      </header>
      <main className="divide-y divide-neutral-800/50">
        <button
          onClick={goUp}
          className="w-full text-left px-4 py-3 hover:bg-neutral-900 text-sm flex items-center gap-2"
        >
          <span>&#x1F4C1;</span>
          <span>..</span>
        </button>
        {entries.map((entry) => (
          <button
            key={entry.name}
            onClick={() => navigateTo(entry)}
            className="w-full text-left px-4 py-3 hover:bg-neutral-900 text-sm flex items-center gap-2"
          >
            <span>{entry.type === "dir" ? "\u{1F4C1}" : "\u{1F4C4}"}</span>
            <span className="truncate">{entry.name}</span>
          </button>
        ))}
      </main>
    </div>
  );
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(0)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
