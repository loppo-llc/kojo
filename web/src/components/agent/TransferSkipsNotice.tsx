import { useState } from "react";
import type { TransferSkip } from "../../lib/agentApi";

// TransferSkipsNotice renders the owner-facing "skipped during
// transfer" warning for an agent whose most recent §3.7 device-switch
// left session files behind (oversized JSONL, unreadable codex ref,
// …). The server stamps AgentInfo.lastTransferSkips on the target
// during agent-sync and clears it on the next clean transfer, so the
// notice disappears by itself once a lossless switch happens.
//
// Collapsed by default to a one-line summary; click to expand the
// per-file detail (path, reason, size).
export function TransferSkipsNotice({ skips }: { skips?: TransferSkip[] }) {
  const [open, setOpen] = useState(false);
  if (!skips || skips.length === 0) return null;
  return (
    <div className="mt-1 rounded border border-lamp-warn/40 bg-lamp-warn/5 px-2 py-1 text-[11px] text-lamp-warn">
      <button
        type="button"
        onClick={(e) => {
          e.stopPropagation();
          setOpen((v) => !v);
        }}
        className="flex w-full items-center gap-1 text-left"
        title="直前のデバイス転移でスキップされたセッションファイルがある"
      >
        <span aria-hidden>⚠</span>
        <span>転移時にスキップされたファイル: {skips.length}件</span>
        <span className="ml-auto" aria-hidden>{open ? "▾" : "▸"}</span>
      </button>
      {open && (
        <ul className="mt-1 space-y-0.5">
          {skips.map((s) => (
            <li key={s.path} className="truncate font-mono">
              {s.path}
              <span className="ml-1 text-lamp-warn/70">
                ({s.reason}
                {s.sizeBytes ? `, ${formatBytes(s.sizeBytes)}` : ""})
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function formatBytes(n: number): string {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GiB`;
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MiB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KiB`;
  return `${n} B`;
}
