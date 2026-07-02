package server

import (
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/loppo-llc/kojo/internal/agent"
)

// --- Agent-scoped file browser handlers ---
//
// These endpoints expose the agent's data directory contents without allowing
// traversal outside of it. The client sends a relative `path` query (forward-
// slash separated). The server rejects any segment containing ".." or a
// platform path separator, then joins with filepath.Join so Windows paths
// work correctly.

// resolveAgentPath returns the absolute filesystem path corresponding to
// rel (forward-slash separated, relative to agent data dir). It rejects
// ".." segments and absolute paths to prevent escape, and resolves
// symlinks so that a symlink inside the agent dir can't point at something
// outside of it.
func resolveAgentPath(agentID, rel string) (absDir, absPath string, err error) {
	absDir = agent.AgentDir(agentID)
	// Normalize: drop leading/trailing slashes, split on "/", reject ".." and
	// empty-after-trim segments. Reject embedded OS separators too so Windows
	// back-slashes can't be smuggled in.
	clean := strings.Trim(rel, "/")
	if clean == "" {
		return absDir, absDir, nil
	}
	if strings.ContainsRune(clean, 0) {
		return "", "", fmt.Errorf("invalid path")
	}
	parts := strings.Split(clean, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", "", fmt.Errorf("invalid path segment: %q", p)
		}
		if strings.ContainsAny(p, `\/`) {
			return "", "", fmt.Errorf("invalid path segment: %q", p)
		}
	}
	absPath = filepath.Join(append([]string{absDir}, parts...)...)

	// Resolve symlinks on both sides, then use filepath.Rel to confirm the
	// resolved path is under the resolved agent dir. If the target doesn't
	// exist yet (view endpoints wouldn't call this, but list may), resolve
	// the parent instead — same pattern as filebrowser.validatePath.
	resolvedRoot, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		return "", "", fmt.Errorf("cannot resolve agent directory: %w", err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", "", fmt.Errorf("cannot resolve path: %w", err)
		}
		parent, perr := filepath.EvalSymlinks(filepath.Dir(absPath))
		if perr != nil {
			return "", "", fmt.Errorf("cannot resolve path: %w", perr)
		}
		resolvedTarget = filepath.Join(parent, filepath.Base(absPath))
	}
	relCheck, err := filepath.Rel(resolvedRoot, resolvedTarget)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path escapes agent directory")
	}
	return absDir, absPath, nil
}

// agentRelURL builds a URL for the agent-scoped raw endpoint with proper
// query-string escaping.
func agentRelURL(agentID, rel string) string {
	v := url.Values{}
	v.Set("path", rel)
	return fmt.Sprintf("/api/v1/agents/%s/files/raw?%s", agentID, v.Encode())
}

func (s *Server) handleListAgentFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	rel := r.URL.Query().Get("path")
	hidden := r.URL.Query().Get("hidden") == "true"

	_, abs, err := resolveAgentPath(id, rel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	result, err := s.files.List(abs, hidden)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Return both the relative sub-path (for UI state) and the absolute
	// path (for copy-to-clipboard). Assembling the abs path server-side
	// keeps OS-specific separators correct on Windows.
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"path":    rel,
		"absPath": abs,
		"entries": result.Entries,
	})
}

func (s *Server) handleViewAgentFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	rel := r.URL.Query().Get("path")
	_, abs, err := resolveAgentPath(id, rel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	view, err := s.files.View(abs)
	if err != nil {
		writeFileViewError(w, err)
		return
	}
	// Replace absolute paths with relative ones in the response.
	view.Path = rel
	if view.URL != "" {
		// Point at the agent-scoped raw endpoint with proper URL encoding.
		view.URL = agentRelURL(id, rel)
	}
	// Attach absPath as a sibling field so the UI can offer
	// "copy path" without reassembling OS-specific separators.
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"path":     view.Path,
		"type":     view.Type,
		"content":  view.Content,
		"language": view.Language,
		"mime":     view.Mime,
		"size":     view.Size,
		"url":      view.URL,
		"absPath":  abs,
	})
}

func (s *Server) handleRawAgentFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	rel := r.URL.Query().Get("path")
	_, abs, err := resolveAgentPath(id, rel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Must be a real file, not a directory.
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
			"filename": filepath.Base(abs),
		}))
	}
	if err := s.files.ServeRaw(w, r, abs); err != nil {
		writeServeErr(w, err)
	}
}

// handleThumbAgentFile serves a JPEG thumbnail for an agent-scoped image.
// Resolution + access control match handleRawAgentFile; the thumbnail
// itself comes from Browser.ServeThumb, which re-validates the absolute
// path under the allowed roots (so this is defence-in-depth, not the
// primary check).
func (s *Server) handleThumbAgentFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	rel := r.URL.Query().Get("path")
	_, abs, err := resolveAgentPath(id, rel)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "not_found", "file not found")
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	if err := s.files.ServeThumb(w, r, abs, size); err != nil {
		writeServeErr(w, err)
	}
}
