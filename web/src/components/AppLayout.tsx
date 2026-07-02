import { Outlet, useLocation } from "react-router";
import { Dashboard } from "./Dashboard";
import { Wordmark } from "./ui/Wordmark";
import { useMediaQuery } from "../hooks/useMediaQuery";

/**
 * AppLayout is the messenger-style shell for the primary 2-pane routes
 * (`/`, `/agents/:id`, `/groupdms/:id`, `/session/:id…`).
 *
 * Below lg (<1024px) it reproduces the pre-P4 full-page drill-in exactly:
 * at `/` the Dashboard list fills the screen; on a detail route the
 * Dashboard is not rendered at all (so there is no hidden list DOM and no
 * background polling on mobile) and the matched detail view takes over.
 *
 * At lg+ it becomes a persistent two-pane grid: a ~360px sidebar holding
 * the Dashboard list (scrolling independently) and the detail view on the
 * right. At `/` the right pane shows the empty state.
 *
 * The detail subtree is keyed by the route's id so switching rows in the
 * sidebar remounts the detail view. AgentChat and SessionPage keep async
 * per-conversation state (transcript, streaming refs, terminal/WS) that is
 * not cleared synchronously on an id change; remounting avoids a flash of
 * the previous conversation and any cross-conversation state bleed. Session
 * tab changes (`/session/:id/files` …) keep the same id, so the key is
 * stable and the terminal/WebSocket survive tab switches.
 */
export function AppLayout() {
  const location = useLocation();
  const isIndex = location.pathname === "/";
  const isDesktop = useMediaQuery("(min-width: 1024px)");
  // Render the sidebar list only where it is actually shown: always at lg+,
  // and at `/` below lg (where it is the full-page list). On a mobile detail
  // route it is not rendered — no hidden polling Dashboard.
  const showSidebar = isDesktop || isIndex;

  // Remount the detail pane when the conversation/session id changes, but
  // not on session tab-only changes (same id → stable key).
  const m = location.pathname.match(/^\/(agents|groupdms|session)\/([^/]+)/);
  const detailKey = m ? `${m[1]}/${m[2]}` : "index";

  return (
    <div className="h-full bg-app lg:grid lg:grid-cols-[360px_minmax(0,1fr)] lg:grid-rows-1">
      {showSidebar && (
        <aside className="h-full min-w-0 lg:min-h-0 lg:overflow-y-auto lg:border-r lg:border-hairline">
          <Dashboard variant="sidebar" />
        </aside>
      )}
      <main className={`h-full min-h-0 min-w-0 ${isIndex ? "hidden lg:block" : ""}`}>
        <div key={detailKey} className="h-full min-h-0">
          <Outlet />
        </div>
      </main>
    </div>
  );
}

/**
 * Empty state shown in the right pane at `/` on lg+. Never visible below
 * lg — the parent <main> is `hidden lg:block` at the index route.
 */
export function EmptyPane() {
  return (
    <div className="flex h-full flex-col items-center justify-center gap-2 bg-app px-6 text-center">
      <Wordmark className="text-2xl opacity-70" />
      <p className="font-mono text-[13px] text-ink-faint">Select an agent or session</p>
    </div>
  );
}
