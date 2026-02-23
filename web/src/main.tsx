import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Routes, Route } from "react-router";
import { Dashboard } from "./components/Dashboard";
import { TerminalView } from "./components/TerminalView";
import { NewSession } from "./components/NewSession";
import { FileBrowser } from "./components/FileBrowser";
import { GitPanel } from "./components/GitPanel";
import "./index.css";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<Dashboard />} />
        <Route path="/session/:id" element={<TerminalView />} />
        <Route path="/new" element={<NewSession />} />
        <Route path="/files" element={<FileBrowser />} />
        <Route path="/session/:id/git" element={<GitPanel />} />
      </Routes>
    </BrowserRouter>
  </StrictMode>,
);
