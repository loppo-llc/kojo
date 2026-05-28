package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
	"golang.org/x/text/unicode/norm"
)

// exportZipMaxBytes caps the in-memory zip we build before streaming
// to the response. Single-agent exports are bounded by the data the
// agent itself accumulated (transcripts + persona + memory) — a
// chatty agent's transcript is the dominant term. 256 MiB is the
// soft cap for the v1 zip export; agents pushing larger should be
// migrated via the per-table HTTP endpoints (or future paginated
// export).
const exportZipMaxBytes = 256 << 20

// exportPaginationLimit caps each ListMessages page when streaming
// transcripts into the zip. Aligned with the daemon-side sync's 500
// page so the export contract matches what other endpoints use.
const exportPaginationLimit = 500

// handleExportAgent streams a zip archive of one agent's
// canonical state: persona, MEMORY.md, memory/<kind>/<name>.md,
// and the full transcript as JSON. Owner-only — the export
// includes prose the agent would otherwise gate behind
// per-message authorization.
//
// Layout:
//
//	persona.md
//	MEMORY.md
//	memory/<kind>/<name>.md
//	messages.json   ← {"messages":[...]}
//	export.json     ← {"agentId":"...", "exportedAt":"...", "version":1}
//
// JSON files use 2-space indented output for human readability.
// The zip is built in-memory then written to the response so the
// archive's central directory is correct (streaming a partial zip
// with no central directory is unparseable). exportZipMaxBytes
// guards against runaway memory.
func (s *Server) handleExportAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := auth.FromContext(r.Context())
	if !p.IsOwner() && !p.IsPeer() {
		writeError(w, http.StatusForbidden, "forbidden", "export is owner-only")
		return
	}
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	st := s.agents.Store()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "store not initialized")
		return
	}

	// Pre-build the archive in a byte buffer. zip.Writer can stream,
	// but the central directory at the end mandates a Close() call
	// that needs all sizes known — buffering is the simplest correct
	// shape. We bound the buffer below.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// Pin a single export timestamp for the manifest, the
	// Content-Disposition filename, AND every zip header's Modified
	// field. The within-call consistency guarantees the manifest's
	// exportedAt matches the zip's modtimes, but two distinct
	// exports of unchanged data still produce different archives
	// because exportTime advances each call. True content-addressed
	// determinism (e.g. modtime derived from the latest source
	// row's UpdatedAt) is a future hardening the caller can layer
	// on if reproducible-build semantics matter.
	exportTime := time.Now().UTC()

	dbCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if err := s.writeExportContents(dbCtx, zw, id, st, exportTime); err != nil {
		s.logger.Error("export handler: build zip failed", "agent", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}
	if err := zw.Close(); err != nil {
		s.logger.Error("export handler: close zip failed", "agent", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		return
	}

	if buf.Len() > exportZipMaxBytes {
		s.logger.Warn("export handler: zip exceeds soft cap",
			"agent", id, "size", buf.Len(), "cap", exportZipMaxBytes)
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			"export exceeds soft cap; use per-table endpoints")
		return
	}

	filename := fmt.Sprintf("kojo-export-%s-%s.zip", sanitizeFilenameSegment(id),
		exportTime.Format("20060102T150405Z"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
	if _, err := io.Copy(w, &buf); err != nil {
		// Connection died mid-stream. Logging is the only meaningful
		// recovery — the headers are already on the wire.
		s.logger.Warn("export handler: stream copy failed", "agent", id, "err", err)
	}
}

// writeExportContents emits the zip entries for one agent. Returns
// the first error; partial zips are not surfaced to the caller (the
// outer handler aborts before sending headers when err != nil).
func (s *Server) writeExportContents(ctx context.Context, zw *zip.Writer, agentID string, st *store.Store, exportTime time.Time) error {
	// 1. persona
	if rec, err := st.GetAgentPersona(ctx, agentID); err == nil && rec != nil {
		if err := writeZipFile(zw, "persona.md", []byte(rec.Body), exportTime); err != nil {
			return fmt.Errorf("write persona: %w", err)
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("read persona: %w", err)
	}

	// 2. MEMORY.md
	if rec, err := st.GetAgentMemory(ctx, agentID); err == nil && rec != nil {
		if err := writeZipFile(zw, "MEMORY.md", []byte(rec.Body), exportTime); err != nil {
			return fmt.Errorf("write MEMORY.md: %w", err)
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("read MEMORY.md: %w", err)
	}

	// 3. memory_entries — emit each row as memory/<kind>/<name>.md
	//    (or memory/<name>.md for kind=daily, mirroring the canonical
	//    layout). Page through in 500-row chunks.
	var cursor int64
	for {
		opts := store.MemoryEntryListOptions{Limit: exportPaginationLimit, Cursor: cursor}
		rows, err := st.ListMemoryEntries(ctx, agentID, opts)
		if err != nil {
			return fmt.Errorf("list memory entries: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, rec := range rows {
			cursor = rec.Seq
			path := memoryEntryExportPath(rec.Kind, rec.Name)
			if path == "" {
				// Defensive: skip rows whose kind/name can't map to a
				// safe path. Such rows are corrupt or legacy
				// fall-through; the per-row HTTP endpoint can recover
				// them individually.
				s.logger.Warn("export: skipping memory entry with unmappable kind/name",
					"agent", agentID, "kind", rec.Kind, "name", rec.Name)
				continue
			}
			if err := writeZipFile(zw, path, []byte(rec.Body), exportTime); err != nil {
				return fmt.Errorf("write memory entry %s: %w", path, err)
			}
		}
		if len(rows) < exportPaginationLimit {
			break
		}
	}

	// 4. transcript — full conversation as messages.json. We aggregate
	//    in a slice to keep the wire format simple (one JSON array)
	//    and bounded by exportZipMaxBytes overall.
	type exportMessage struct {
		ID          string          `json:"id"`
		Seq         int64           `json:"seq"`
		Role        string          `json:"role"`
		Content     string          `json:"content,omitempty"`
		Thinking    string          `json:"thinking,omitempty"`
		ToolUses    json.RawMessage `json:"toolUses,omitempty"`
		Attachments json.RawMessage `json:"attachments,omitempty"`
		Usage       json.RawMessage `json:"usage,omitempty"`
		ETag        string          `json:"etag"`
		CreatedAt   int64           `json:"createdAt"`
		UpdatedAt   int64           `json:"updatedAt"`
	}
	var msgs []exportMessage
	var msgCursor int64
	for {
		opts := store.MessageListOptions{Limit: exportPaginationLimit, SinceSeq: msgCursor}
		rows, err := st.ListMessages(ctx, agentID, opts)
		if err != nil {
			return fmt.Errorf("list messages: %w", err)
		}
		if len(rows) == 0 {
			break
		}
		for _, m := range rows {
			msgCursor = m.Seq
			msgs = append(msgs, exportMessage{
				ID:          m.ID,
				Seq:         m.Seq,
				Role:        m.Role,
				Content:     m.Content,
				Thinking:    m.Thinking,
				ToolUses:    json.RawMessage(m.ToolUses),
				Attachments: json.RawMessage(m.Attachments),
				Usage:       json.RawMessage(m.Usage),
				ETag:        m.ETag,
				CreatedAt:   m.CreatedAt,
				UpdatedAt:   m.UpdatedAt,
			})
		}
		if len(rows) < exportPaginationLimit {
			break
		}
	}
	msgsJSON, err := json.MarshalIndent(map[string]any{"messages": msgs}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal messages: %w", err)
	}
	if err := writeZipFile(zw, "messages.json", msgsJSON, exportTime); err != nil {
		return fmt.Errorf("write messages.json: %w", err)
	}

	// 5. export manifest with provenance.
	manifest := map[string]any{
		"agentId":      agentID,
		"exportedAt":   exportTime.Format(time.RFC3339),
		"version":      1,
		"messageCount": len(msgs),
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := writeZipFile(zw, "export.json", manifestJSON, exportTime); err != nil {
		return fmt.Errorf("write export.json: %w", err)
	}

	return nil
}

// writeZipFile adds one file to the zip with a caller-supplied
// modification timestamp. Centralizing the timestamp here (rather
// than calling time.Now in each entry) keeps the manifest's
// exportedAt in lockstep with every entry's modtime, so a single
// archive is internally consistent. Cross-call determinism is NOT
// guaranteed — see handleExportAgent's comment for why.
func writeZipFile(zw *zip.Writer, name string, data []byte, modTime time.Time) error {
	header := &zip.FileHeader{
		Name:     name,
		Method:   zip.Deflate,
		Modified: modTime,
	}
	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// memoryEntryExportPath returns the in-zip path for a memory entry
// row. Mirrors agent.memoryEntryCanonicalPath but operates only on
// the (kind, name) tuple. Refuses anything that would escape the
// memory/ prefix or that fails the same validation the CRUD
// endpoints enforce (NUL, control chars, Win32 reserved punctuation
// + device names, trailing dot/space, malformed daily dates, etc).
// Returns "" for unmappable rows; the caller skips with a logged
// warning so a single corrupt row doesn't fail the whole export.
//
// We re-implement the validation surface here rather than calling
// agent.validateMemoryEntryName (unexported) — duplicating the rule
// list is a one-line synchronization burden vs exporting the
// validator out of the agent package.
func memoryEntryExportPath(kind, name string) string {
	if !exportNameSafe(kind, name) {
		return ""
	}
	switch kind {
	case "daily":
		return "memory/" + name + ".md"
	case "project":
		return "memory/projects/" + name + ".md"
	case "topic":
		return "memory/topics/" + name + ".md"
	case "people":
		return "memory/people/" + name + ".md"
	case "archive":
		return "memory/archive/" + name + ".md"
	}
	return ""
}

// exportNameSafe enforces the same rules as agent.validateMemoryEntryName.
// Kept narrow + explicit so a corrupt or peer-replicated row doesn't
// land in the zip with a name that breaks unzip on a Windows peer.
func exportNameSafe(kind, name string) bool {
	if name == "" || len(name) > 200 {
		return false
	}
	if !norm.NFC.IsNormalString(name) {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	if strings.HasPrefix(name, ".") {
		return false
	}
	if strings.ContainsAny(name, "/\\") {
		return false
	}
	if strings.ContainsRune(name, 0) {
		return false
	}
	if strings.ContainsAny(name, `<>:"|?*`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7F || unicode.IsControl(r) {
			return false
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	// Win32 device-name stem check.
	stem := strings.ToLower(name)
	if dot := strings.Index(stem, "."); dot >= 0 {
		stem = stem[:dot]
	}
	switch stem {
	case "con", "prn", "aux", "nul":
		return false
	}
	for i := 1; i <= 9; i++ {
		if stem == fmt.Sprintf("com%d", i) || stem == fmt.Sprintf("lpt%d", i) {
			return false
		}
	}
	if kind == "daily" {
		if _, err := time.Parse("2006-01-02", name); err != nil {
			return false
		}
	}
	return true
}

// sanitizeFilenameSegment scrubs the agent ID for safe inclusion in
// the Content-Disposition filename. Agents are id-prefixed (`ag_*`)
// with hex characters, but defense in depth: drop anything outside
// [a-zA-Z0-9_-].
func sanitizeFilenameSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "agent"
	}
	return b.String()
}
