import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Routes, Route } from "react-router";
import { Dashboard } from "./components/Dashboard";
import { TerminalView } from "./components/TerminalView";
import { NewSession } from "./components/NewSession";
import { FileBrowser } from "./components/FileBrowser";
import "./index.css";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<Dashboard />} />
        <Route path="/session/:id" element={<TerminalView />} />
        <Route path="/new" element={<NewSession />} />
        <Route path="/files" element={<FileBrowser />} />
      </Routes>
    </BrowserRouter>
  </StrictMode>,
);
