import { useEffect, useRef, useState } from "react";
import { Link } from "react-router";
import { Wordmark } from "./Wordmark";

const NEW_ITEMS = [
  { to: "/agents/new", label: "New agent" },
  { to: "/new", label: "New session" },
] as const;

/**
 * Sticky app-shell header: wordmark left; a "+" menu (New agent / New
 * session) and the settings gear on the right. The menu follows the
 * menu-button pattern — opens on click, first item focused, Arrow keys
 * move between items, Escape closes and restores focus to the trigger,
 * and outside click / focus leaving the group closes it.
 */
export function Header() {
  const [menuOpen, setMenuOpen] = useState(false);
  const menuWrapRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const itemRefs = useRef<(HTMLAnchorElement | null)[]>([]);

  const closeMenu = (restoreFocus = false) => {
    setMenuOpen(false);
    if (restoreFocus) triggerRef.current?.focus();
  };

  useEffect(() => {
    if (!menuOpen) return;
    const onPointer = (e: MouseEvent) => {
      if (menuWrapRef.current && !menuWrapRef.current.contains(e.target as Node)) {
        setMenuOpen(false);
      }
    };
    document.addEventListener("mousedown", onPointer);
    // Land keyboard focus on the first item when the menu opens.
    itemRefs.current[0]?.focus();
    return () => document.removeEventListener("mousedown", onPointer);
  }, [menuOpen]);

  const onMenuKeyDown = (e: React.KeyboardEvent) => {
    const items = itemRefs.current.filter(Boolean) as HTMLAnchorElement[];
    const idx = items.indexOf(document.activeElement as HTMLAnchorElement);
    if (e.key === "Escape") {
      e.preventDefault();
      closeMenu(true);
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      items[(idx + 1) % items.length]?.focus();
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      items[(idx - 1 + items.length) % items.length]?.focus();
    } else if (e.key === "Tab") {
      // Let focus leave naturally, then collapse the menu.
      setMenuOpen(false);
    }
  };

  return (
    <header className="sticky top-0 z-40 border-b border-hairline bg-app/85 backdrop-blur">
      <div className="mx-auto flex h-14 max-w-[720px] items-center justify-between px-4">
        <Link to="/" aria-label="kojo home" className="rounded-[10px]">
          <Wordmark className="text-lg" />
        </Link>

        <div className="flex items-center gap-1">
          <div
            className="relative"
            ref={menuWrapRef}
            onBlur={(e) => {
              // Close when focus moves outside the menu group (e.g. Tab).
              if (!e.currentTarget.contains(e.relatedTarget as Node)) setMenuOpen(false);
            }}
          >
            <button
              ref={triggerRef}
              type="button"
              onClick={() => setMenuOpen((o) => !o)}
              aria-haspopup="menu"
              aria-expanded={menuOpen}
              aria-label="New"
              className="rounded-[10px] p-2 text-ink-dim transition-colors hover:bg-hover hover:text-ink"
            >
              <svg viewBox="0 0 20 20" fill="currentColor" className="h-5 w-5">
                <path d="M10.75 4.75a.75.75 0 00-1.5 0v4.5h-4.5a.75.75 0 000 1.5h4.5v4.5a.75.75 0 001.5 0v-4.5h4.5a.75.75 0 000-1.5h-4.5v-4.5z" />
              </svg>
            </button>
            {menuOpen && (
              <div
                role="menu"
                aria-label="Create new"
                onKeyDown={onMenuKeyDown}
                className="absolute right-0 z-50 mt-1 w-44 overflow-hidden rounded-[10px] border border-hairline bg-raised py-1 shadow-xl shadow-black/40"
              >
                {NEW_ITEMS.map((item, i) => (
                  <Link
                    key={item.to}
                    ref={(el) => {
                      itemRefs.current[i] = el;
                    }}
                    role="menuitem"
                    to={item.to}
                    onClick={() => closeMenu()}
                    className="block px-3 py-2 text-sm text-ink transition-colors hover:bg-hover focus:bg-hover focus:outline-none"
                  >
                    {item.label}
                  </Link>
                ))}
              </div>
            )}
          </div>

          <Link
            to="/settings"
            aria-label="Settings"
            title="Settings"
            className="rounded-[10px] p-2 text-ink-faint transition-colors hover:bg-hover hover:text-ink-dim"
          >
            <svg viewBox="0 0 20 20" fill="currentColor" className="h-5 w-5">
              <path
                fillRule="evenodd"
                d="M7.84 1.804A1 1 0 018.82 1h2.36a1 1 0 01.98.804l.331 1.652a6.993 6.993 0 011.929 1.115l1.598-.54a1 1 0 011.186.447l1.18 2.044a1 1 0 01-.205 1.251l-1.267 1.113a7.047 7.047 0 010 2.228l1.267 1.113a1 1 0 01.206 1.25l-1.18 2.045a1 1 0 01-1.187.447l-1.598-.54a6.993 6.993 0 01-1.929 1.115l-.33 1.652a1 1 0 01-.98.804H8.82a1 1 0 01-.98-.804l-.331-1.652a6.993 6.993 0 01-1.929-1.115l-1.598.54a1 1 0 01-1.186-.447l-1.18-2.044a1 1 0 01.205-1.251l1.267-1.114a7.05 7.05 0 010-2.227L1.821 7.773a1 1 0 01-.206-1.25l1.18-2.045a1 1 0 011.187-.447l1.598.54A6.993 6.993 0 017.51 3.456l.33-1.652zM10 13a3 3 0 100-6 3 3 0 000 6z"
                clipRule="evenodd"
              />
            </svg>
          </Link>
        </div>
      </div>
    </header>
  );
}
