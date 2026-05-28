import { useState, useRef, useCallback, useEffect } from "react";
import { agentApi } from "../../lib/agentApi";

interface AgentAvatarProps {
  agentId: string;
  name: string;
  size?: "sm" | "md" | "lg" | "xl";
  className?: string;
  preview?: boolean;
  /** Append ?t=<value> to bust browser cache after avatar change */
  cacheBust?: string | number;
}

const sizes = {
  sm: "w-12 h-12",
  md: "w-14 h-14",
  lg: "w-16 h-16",
  xl: "w-24 h-24",
};

// Thumbnail resolution per size tier. 2x DPR so Retina doesn't blur.
// sm=48px → 128, md=56px → 128, lg=64px → 128, xl=96px → 256.
const thumbRes: Record<string, number> = {
  sm: 128,
  md: 128,
  lg: 128,
  xl: 256,
};

function appendCacheBust(url: string, cb: string | number): string {
  return `${url}${url.includes("?") ? "&" : "?"}t=${cb}`;
}

export function AgentAvatar({
  agentId,
  name,
  size = "md",
  className = "",
  preview = true,
  cacheBust,
}: AgentAvatarProps) {
  const [show, setShow] = useState(false);
  const [style, setStyle] = useState<React.CSSProperties>({});
  const [natSize, setNatSize] = useState<{ w: number; h: number } | null>(null);
  const ref = useRef<HTMLDivElement>(null);
  const timerRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  // Icon src: low-res thumbnail via ?size=<n>
  const thumbBase = agentApi.avatarUrl(agentId, thumbRes[size] ?? 128);
  const thumbSrc = cacheBust != null ? appendCacheBust(thumbBase, cacheBust) : thumbBase;

  // Hover preview src: full resolution (no size param)
  const fullBase = agentApi.avatarUrl(agentId);
  const fullSrc = cacheBust != null ? appendCacheBust(fullBase, cacheBust) : fullBase;

  // Preload natural size from the full image for hover layout
  useEffect(() => {
    if (!preview) return;
    const img = new Image();
    img.onload = () => setNatSize({ w: img.naturalWidth, h: img.naturalHeight });
    img.src = fullSrc;
  }, [fullSrc, preview]);

  const handleEnter = useCallback(() => {
    if (!preview || !natSize) return;
    timerRef.current = setTimeout(() => {
      if (!ref.current) return;
      const rect = ref.current.getBoundingClientRect();
      const pad = 12;
      const maxW = Math.min(natSize.w, window.innerWidth - pad * 2, 480);
      const maxH = Math.min(natSize.h, window.innerHeight - pad * 2, 480);
      const scale = Math.min(maxW / natSize.w, maxH / natSize.h, 1);
      const w = natSize.w * scale;
      const h = natSize.h * scale;

      // Try below, then above, then centered vertically
      let top = rect.bottom + 8;
      if (top + h > window.innerHeight - pad) {
        top = rect.top - h - 8;
      }
      if (top < pad) {
        top = Math.max(pad, (window.innerHeight - h) / 2);
      }

      // Center horizontally on avatar, clamp to viewport
      let left = rect.left + rect.width / 2 - w / 2;
      left = Math.max(pad, Math.min(left, window.innerWidth - w - pad));

      setStyle({ left, top, width: w, height: h });
      setShow(true);
    }, 300);
  }, [preview, natSize]);

  const handleLeave = useCallback(() => {
    clearTimeout(timerRef.current);
    setShow(false);
  }, []);

  return (
    <div
      ref={ref}
      className={`relative shrink-0 ${sizes[size]} ${className}`}
      onMouseEnter={handleEnter}
      onMouseLeave={handleLeave}
    >
      <img
        src={thumbSrc}
        alt={name}
        className={`${sizes[size]} rounded-full object-cover bg-neutral-800`}
        onError={(e) => {
          (e.target as HTMLImageElement).style.display = "none";
        }}
      />
      {show && (
        <div
          className="fixed z-50 pointer-events-none"
          style={style}
        >
          <div className="w-full h-full rounded-2xl overflow-hidden shadow-2xl shadow-black/60 border border-neutral-700/50 bg-neutral-900">
            <img
              src={fullSrc}
              alt={name}
              className="w-full h-full object-cover"
            />
          </div>
        </div>
      )}
    </div>
  );
}
