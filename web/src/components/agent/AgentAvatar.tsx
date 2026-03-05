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

  const base = agentApi.avatarUrl(agentId);
  const src = cacheBust != null ? `${base}?t=${cacheBust}` : base;

  // Preload natural size once
  useEffect(() => {
    if (!preview) return;
    const img = new Image();
    img.onload = () => setNatSize({ w: img.naturalWidth, h: img.naturalHeight });
    img.src = src;
  }, [src, preview]);

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
      className={`relative shrink-0 ${className}`}
      onMouseEnter={handleEnter}
      onMouseLeave={handleLeave}
    >
      <img
        src={src}
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
              src={src}
              alt={name}
              className="w-full h-full object-cover"
            />
          </div>
        </div>
      )}
    </div>
  );
}
