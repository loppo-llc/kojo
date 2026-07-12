import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Routes, Route, Navigate } from "react-router";
import { bootstrapTokenFromURL } from "./lib/auth";
import { AppLayout, EmptyPane } from "./components/AppLayout";
import { SessionPage } from "./components/SessionPage";
import { NewSession } from "./components/NewSession";
import { FileBrowser } from "./components/FileBrowser";
import { AgentChat } from "./components/agent/AgentChat";
import { AgentCreate } from "./components/agent/AgentCreate";
import { AgentSettings } from "./components/agent/AgentSettings";
import { AgentCredentials } from "./components/agent/AgentCredentials";
import { AgentDataBrowser } from "./components/agent/AgentDataBrowser";
import { AgentTodos } from "./components/agent/AgentTodos";
import { GroupDMChat } from "./components/groupdm/GroupDMChat";
import { GlobalSettings } from "./components/GlobalSettings";
import { ReloadPrompt } from "./components/ui/ReloadPrompt";
import { UpdatePrompt } from "./components/ui/UpdatePrompt";
import "@fontsource/ibm-plex-mono/400.css";
import "@fontsource/ibm-plex-mono/500.css";
import "@fontsource/ibm-plex-mono/600.css";
import "./index.css";

// Pull the Owner token out of `?token=…` and stash it before any
// component mounts and starts hitting /api/v1/*.
bootstrapTokenFromURL();

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    {/* useTransitions={false}: keep router state updates synchronous.
        BrowserRouter wraps navigations in React.startTransition by
        default, and Dashboard polling (3-5s setState) interrupts those
        transitions — flashing UI / wrong destination after back. Prop
        was unstable_useTransitions until react-router 7.15 stabilized
        it (the unstable_ name is gone, not aliased). */}
    <BrowserRouter useTransitions={false}>
      <Routes>
        {/* Two-pane shell: at lg+ the Dashboard list is a persistent
            sidebar and these render in the right pane; below lg they
            drill in full-page exactly as before. */}
        <Route element={<AppLayout />}>
          <Route path="/" element={<EmptyPane />} />
          <Route path="/session/:id" element={<SessionPage />} />
          <Route path="/session/:id/terminal" element={<SessionPage />} />
          <Route path="/session/:id/files" element={<SessionPage />} />
          <Route path="/session/:id/git" element={<SessionPage />} />
          <Route path="/session/:id/attachments" element={<SessionPage />} />
          <Route path="/agents/:id" element={<AgentChat />} />
          <Route path="/groupdms/new" element={<GroupDMChat />} />
          <Route path="/groupdms/:id" element={<GroupDMChat />} />
          {/* Static "new / settings" panes: at lg+ they render in the right
              pane beside the persistent Dashboard sidebar; below lg they drill
              in full-page. React Router ranks static segments above dynamic
              ones, so /agents/new wins over /agents/:id regardless of order. */}
          <Route path="/new" element={<NewSession />} />
          <Route path="/agents/new" element={<AgentCreate />} />
          <Route path="/settings" element={<GlobalSettings />} />
        </Route>
        {/* Full-page routes — intentionally outside the 2-pane shell. */}
        <Route path="/files" element={<FileBrowser />} />
        <Route path="/agents" element={<Navigate to="/" replace />} />
        <Route path="/agents/:id/settings" element={<AgentSettings />} />
        <Route path="/agents/:id/credentials" element={<AgentCredentials />} />
        <Route path="/agents/:id/data" element={<AgentDataBrowser />} />
        <Route path="/agents/:id/todos" element={<AgentTodos />} />
      </Routes>
      {/* Shared fixed stack: both prompts are max-w-md cards; flex-col
          keeps them stacked when both are visible (Update above Reload). */}
      <div className="pointer-events-none fixed inset-x-0 bottom-0 z-50 flex flex-col items-center gap-2 p-3">
        <UpdatePrompt />
        <ReloadPrompt />
      </div>
    </BrowserRouter>
  </StrictMode>,
);
