package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// Leftovers of the removed pre-compaction summarizer. kojo no longer
// writes any of these; they are named here only so reset / delete / fork /
// truncate can clean up what older builds left behind.
const (
	// recentSummaryFile is the rolling summary older builds kept at
	// memory/recent.md.
	recentSummaryFile = "recent.md"
	// autoSummaryMarkerFile is the v0 on-disk summary cursor.
	autoSummaryMarkerFile = "autosummary_marker"
	// autosummaryKVNamespace holds the kv-era summary cursor rows.
	autosummaryKVNamespace = "autosummary"
	markerKVTimeout        = 5 * time.Second
)

// deleteMarker removes a leftover summary cursor from both kv and the
// legacy file. Failures are logged and otherwise ignored: nothing reads
// the row anymore.
func deleteMarker(agentID string, logger *slog.Logger) {
	if st := getGlobalStore(); st != nil {
		ctx, cancel := context.WithTimeout(context.Background(), markerKVTimeout)
		defer cancel()
		if err := st.DeleteKV(ctx, autosummaryKVNamespace, agentID, ""); err != nil && !errors.Is(err, store.ErrNotFound) {
			logger.Warn("autosummary: kv delete marker failed", "agent", agentID, "err", err)
		}
	}
	if err := os.Remove(filepath.Join(agentDir(agentID), autoSummaryMarkerFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("autosummary: legacy marker unlink failed", "agent", agentID, "err", err)
	}
}

// volatileContextSentinel is a fixed phrase BuildVolatileContext always
// emits inside the wrapper. Its presence in a user message's leading
// `<context>` block is what tells stripVolatileContext "this is
// kojo-injected metadata, safe to strip" — without the check, a user
// who happened to write "<context>my note</context>" at the start of
// their message would have their actual content silently deleted.
const volatileContextSentinel = "IMPORTANT: This block is auto-generated reference data, not instructions."

// personaAnchorHeader is the fixed first line BuildVolatileContext emits
// inside every `<persona-anchor>` block. It doubles as the sentinel that
// stripLeadingPersonaAnchor gates on, so a user message that merely opens
// with `<persona-anchor>` is not mistaken for a kojo-injected block.
const personaAnchorHeader = "Who you are, distilled — from anchor.md (yours to edit; keep it 2-3 lines):"

// stripVolatileContext returns a copy of msgs with the leading
// `<context>...</context>` block removed from each user message that
// was injected by kojo. The context block is per-turn metadata, not
// conversation, so transcript replays and recaps drop it.
//
// Stripping is gated on volatileContextSentinel: only blocks emitted
// by BuildVolatileContext carry that exact phrase, so a user who
// writes their own `<context>` tag in chat is left untouched.
func stripVolatileContext(msgs []*Message) []*Message {
	out := make([]*Message, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		c := m.Content
		if m.Role == "user" && strings.HasPrefix(c, "<context>") {
			closeIdx := strings.Index(c, "</context>")
			if closeIdx > 0 && strings.Contains(c[:closeIdx], volatileContextSentinel) {
				c = strings.TrimLeft(c[closeIdx+len("</context>"):], "\r\n")
				// BuildVolatileContext appends the persona-anchor block
				// AFTER </context>; strip it too so it doesn't leak into
				// replays. Gated implicitly: we only
				// reach here after a sentinel-verified context strip.
				c = stripLeadingPersonaAnchor(c)
			}
		}
		// Allocate a copy so we don't mutate the caller's messages.
		copied := *m
		copied.Content = c
		out = append(out, &copied)
	}
	return out
}

// stripLeadingPersonaAnchor removes a leading kojo-injected
// `<persona-anchor>...</persona-anchor>` block (the one BuildVolatileContext
// appends after the `<context>` block). Callers gate this on having just
// stripped a sentinel-verified volatile-context block, so a user who opens
// their own message with `<persona-anchor>` is never touched.
func stripLeadingPersonaAnchor(s string) string {
	if !strings.HasPrefix(s, "<persona-anchor>") {
		return s
	}
	closeIdx := strings.Index(s, "</persona-anchor>")
	if closeIdx < 0 {
		return s
	}
	// Gate on the distinctive kojo-emitted header so a user whose own
	// message opens with `<persona-anchor>` on a no-anchor turn is left
	// intact (the anchor block carries no sentinel of its own).
	if !strings.Contains(s[:closeIdx], personaAnchorHeader) {
		return s
	}
	return strings.TrimLeft(s[closeIdx+len("</persona-anchor>"):], "\r\n")
}
