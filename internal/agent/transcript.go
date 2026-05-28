package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// ErrMessageNotFound is returned when a message with the given ID does not exist.
var ErrMessageNotFound = errors.New("message not found")

// ErrMessageETagMismatch is returned when an If-Match precondition on a
// message-mutation API does not match the current row's etag. Maps to
// HTTP 412 at the handler layer (RFC 7232).
var ErrMessageETagMismatch = errors.New("message etag mismatch")

// ErrMemoryEntryExists is returned when CreateAgentMemoryEntry would
// collide with an existing live row under the same (kind, name).
// Maps to HTTP 409 at the handler.
var ErrMemoryEntryExists = errors.New("memory entry already exists")

// ErrInvalidMemoryEntry is the sentinel for client-supplied data that
// fails validation (bad kind, malformed name, oversized body). Maps to
// HTTP 400 at the handler. Operational failures (DB errors, file I/O
// errors, sync errors) do NOT wrap this — they fall through to 500 so
// the caller can distinguish their bug from ours.
var ErrInvalidMemoryEntry = errors.New("invalid memory entry")

// ErrMemoryEntryRenameUnsupported is returned by UpdateAgentMemoryEntry
// when the patch attempts to change kind or name. Rename via PATCH is
// genuinely hard to make crash-safe without an intent-file protocol —
// callers should DELETE + CREATE instead. Maps to HTTP 400.
var ErrMemoryEntryRenameUnsupported = errors.New("rename via PATCH is not supported; use DELETE + CREATE")

// ErrMemoryEntryStoredCorrupt signals a DB row whose persisted kind
// or name fails validation when read back (peer-replicated junk, an
// importer bug, manual SQL surgery). It is NOT a client error —
// the request is well-formed; the server has a bad row. Maps to
// HTTP 500 so monitoring catches it.
var ErrMemoryEntryStoredCorrupt = errors.New("stored memory entry data is invalid")

// ErrMemoryEntryNotCanonical signals an attempt to UPDATE a row whose
// backing file lives at a non-canonical path (e.g. legacy fall-through
// `memory/foo.md` for a kind=topic row). PATCH would silently mint a
// new file at the canonical path and orphan the old one — which the
// next sync would resurrect as a duplicate row. Caller should DELETE
// the legacy entry and CREATE it fresh under the canonical layout.
// Maps to HTTP 409.
var ErrMemoryEntryNotCanonical = errors.New("memory entry is at a non-canonical path; delete and recreate")

// ErrInvalidPersona is the sentinel for client-supplied persona data
// that fails validation (oversized body). Maps to HTTP 400.
var ErrInvalidPersona = errors.New("invalid persona")

// errStoreNotReady is returned when the package-level *store.Store handle
// is nil — typically because a test constructed *Manager without going
// through NewManager. Surfacing the condition explicitly is better than
// nil-deref'ing on st.db deep in the call stack.
var errStoreNotReady = errors.New("agent store: not initialized (NewManager has not run)")

// transcriptCtx returns a per-call context with a write timeout. Callers
// in the request path already wrap their handlers in a context, but the
// transcript helpers are also reachable from cron/notify/slack code that
// passes through context.Background — without a timeout a stuck DB write
// would wedge that worker indefinitely.
func transcriptCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// boundedCtx derives a per-call context that combines parent
// cancellation with a maximum wait of 30s. Used by handlers that
// already carry a request ctx (so cancel-on-disconnect propagates)
// but still want the transcriptCtx-style ceiling so a stuck DB query
// can't hold a connection open indefinitely. nil parent (defensive)
// falls back to transcriptCtx semantics.
func boundedCtx(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		return transcriptCtx()
	}
	return context.WithTimeout(parent, 30*time.Second)
}

// appendMessage inserts msg into the agent's transcript via the
// agent_messages table. Seq is allocated by the store. Idempotency on
// retry is the caller's responsibility — a duplicate ID will fail the
// PRIMARY KEY constraint.
func appendMessage(agentID string, msg *Message) error {
	if msg == nil {
		return errors.New("appendMessage: nil message")
	}
	db := getGlobalStore()
	if db == nil {
		return errStoreNotReady
	}

	rec, err := messageToRecord(agentID, msg)
	if err != nil {
		return err
	}
	ts := parseAgentRFC3339Millis(msg.Timestamp)
	if ts == 0 {
		ts = store.NowMillis()
	}

	ctx, cancel := transcriptCtx()
	defer cancel()
	out, err := db.AppendMessage(ctx, rec, store.MessageInsertOptions{
		CreatedAt: ts,
		UpdatedAt: ts,
	})
	if err != nil {
		return err
	}
	// Reflect store-allocated timestamp back into the caller's *Message
	// so subsequent reads (LastMessage preview, broadcaster fan-out)
	// observe the same RFC3339 string the DB row will produce.
	if msg.Timestamp == "" && out.CreatedAt != 0 {
		msg.Timestamp = millisToRFC3339(out.CreatedAt)
	}
	// Reflect the just-allocated etag too — without this, messages
	// fanned out via the chat broadcaster (WebSocket subscribers) would
	// arrive without an etag, and the Web UI's first edit on the new
	// message would PATCH unconditionally (no If-Match), defeating
	// optimistic concurrency for the entire fresh-message lifecycle
	// until a transcript reload.
	msg.ETag = out.ETag
	return nil
}

// loadMessages reads the last N messages from the agent's transcript.
// If limit <= 0, all messages are returned, oldest-first (the legacy
// JSONL semantics).
//
// Implementation note: ListMessages with order=desc + limit returns
// newest-first; we reverse to keep the caller's "oldest-first" contract
// stable across the file→DB cutover.
func loadMessages(agentID string, limit int) ([]*Message, error) {
	ctx, cancel := transcriptCtx()
	defer cancel()
	msgs, _, err := loadMessagesPaginatedCtx(ctx, agentID, limit, "")
	return msgs, err
}

// loadMessagesCtx is the ctx-aware variant of loadMessages used by the
// request-driven paths (Regenerate) so a client disconnect propagates
// to the underlying SQLite query. The caller's ctx is passed through
// without an extra timeout — wrap with boundedCtx if a ceiling is
// required.
func loadMessagesCtx(ctx context.Context, agentID string, limit int) ([]*Message, error) {
	msgs, _, err := loadMessagesPaginatedCtx(ctx, agentID, limit, "")
	return msgs, err
}

// loadMessagesPaginated reads messages with cursor-based pagination.
// If before is non-empty, returns the last `limit` messages strictly
// before that ID. Returns the messages (oldest-first) and whether
// older messages remain beyond the returned window.
//
// Uses transcriptCtx for the DB ceiling — callers in request paths
// that need cancellation should use loadMessagesPaginatedCtx instead.
func loadMessagesPaginated(agentID string, limit int, before string) ([]*Message, bool, error) {
	ctx, cancel := transcriptCtx()
	defer cancel()
	return loadMessagesPaginatedCtx(ctx, agentID, limit, before)
}

// loadMessagesPaginatedCtx is the ctx-aware variant. The ctx is used
// for every store call in the paginated read so the caller's
// cancellation propagates.
func loadMessagesPaginatedCtx(ctx context.Context, agentID string, limit int, before string) ([]*Message, bool, error) {
	db := getGlobalStore()
	if db == nil {
		return nil, false, errStoreNotReady
	}

	var beforeSeq int64
	if before != "" {
		// Resolve the cursor message's seq. ErrNotFound here matches the
		// legacy behavior: the caller passed an ID we don't have, so we
		// treat it as "before nothing" rather than failing the listing.
		ref, err := db.GetMessage(ctx, before)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, false, fmt.Errorf("resolve before cursor: %w", err)
		}
		if ref != nil && ref.AgentID == agentID {
			beforeSeq = ref.Seq
		}
	}

	// Fetch limit+1 in DESC order so we can tell whether more older rows
	// remain without an extra query. We trim the +1 sentinel before
	// returning.
	listOpts := store.MessageListOptions{
		BeforeSeq: beforeSeq,
		Order:     "desc",
	}
	if limit > 0 {
		listOpts.Limit = limit + 1
	}
	recs, err := db.ListMessages(ctx, agentID, listOpts)
	if err != nil {
		return nil, false, err
	}

	hasMore := false
	if limit > 0 && len(recs) > limit {
		hasMore = true
		recs = recs[:limit]
	}

	// Match the legacy file-based contract: nil (not an empty slice) when
	// the agent has no messages. Callers test "len(msgs) > 0" everywhere
	// but a small handful of tests pin the nil-vs-empty distinction.
	if len(recs) == 0 {
		return nil, false, nil
	}

	// Reverse for oldest-first.
	out := make([]*Message, len(recs))
	for i, rec := range recs {
		m, err := recordToMessage(rec)
		if err != nil {
			return nil, false, err
		}
		out[len(recs)-1-i] = m
	}
	return out, hasMore, nil
}

// updateMessageContent replaces the content of the message with the given
// ID. Returns ErrMessageNotFound if no matching message exists or if it
// does not belong to agentID (a cross-agent update would silently
// re-attach the message; the agent-scope check guards against that).
//
// ifMatchETag is forwarded to store.UpdateMessage so the precondition
// check is atomic with the SQLite UPDATE — no TOCTOU window between
// the load and the write. Empty ifMatchETag skips the check (used by
// daemon-internal callers that do not expose optimistic concurrency).
//
// Returns the post-update etag alongside the message so callers can
// surface it in HTTP ETag headers without a separate GetMessage round-
// trip (which would race against any concurrent edit that landed
// between the UPDATE returning and the subsequent SELECT).
func updateMessageContent(agentID, msgID, content, ifMatchETag string) (*Message, string, error) {
	db := getGlobalStore()
	if db == nil {
		return nil, "", errStoreNotReady
	}

	ctx, cancel := transcriptCtx()
	defer cancel()

	cur, err := db.GetMessage(ctx, msgID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", ErrMessageNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if cur.AgentID != agentID {
		// Treat cross-agent ID as not-found rather than mismatch: the
		// caller has no business mentioning an etag for a message that
		// doesn't exist within the agent they own. Leaking
		// ErrETagMismatch here would let an attacker probe whether a
		// given msgID exists under any agent.
		return nil, "", ErrMessageNotFound
	}

	rec, err := db.UpdateMessage(ctx, msgID, ifMatchETag, store.MessagePatch{Content: &content})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, "", ErrMessageNotFound
	case errors.Is(err, store.ErrETagMismatch):
		return nil, "", ErrMessageETagMismatch
	case err != nil:
		return nil, "", err
	}
	msg, err := recordToMessage(rec)
	if err != nil {
		return nil, "", err
	}
	return msg, rec.ETag, nil
}

// deleteMessage removes the message with the given ID from the transcript.
// Returns ErrMessageNotFound if no matching message exists or if it does
// not belong to agentID.
//
// ifMatchETag is forwarded to the store for optimistic locking. Empty
// disables the check (legacy daemon-internal callers, e.g. truncation).
// A non-empty If-Match against a vanished row maps to ErrMessageNotFound
// (the resource is gone — refetch); a non-empty If-Match against a row
// whose etag has moved on maps to ErrMessageETagMismatch.
func deleteMessage(agentID, msgID, ifMatchETag string) error {
	db := getGlobalStore()
	if db == nil {
		return errStoreNotReady
	}

	ctx, cancel := transcriptCtx()
	defer cancel()

	cur, err := db.GetMessage(ctx, msgID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrMessageNotFound
	}
	if err != nil {
		return err
	}
	// Cross-agent guard runs before the conditional delete so a caller
	// cannot probe another agent's etag space via a 412/404 oracle.
	if cur.AgentID != agentID {
		return ErrMessageNotFound
	}
	if err := db.SoftDeleteMessage(ctx, msgID, ifMatchETag); err != nil {
		switch {
		case errors.Is(err, store.ErrETagMismatch):
			return ErrMessageETagMismatch
		case errors.Is(err, store.ErrNotFound):
			return ErrMessageNotFound
		}
		return err
	}
	return nil
}

// findRegenerateTarget locates the user message that should drive a
// regeneration rooted at msgID, and computes how many leading messages
// of the transcript to keep.
//
// regenerateTarget is the result of findRegenerateTarget: the source
// message whose content drives the regeneration, plus the pivot id the
// user actually clicked on. The pivot's seq is intentionally NOT
// captured here — it's read inside the truncate transaction (alongside
// the etag re-validation) so a cross-device prefix mutation between
// the click and the truncate cannot shift the boundary onto the wrong
// row. SourceID is the id whose content/role we'll re-run the chat
// from; for "assistant" pivots that's the preceding user row, for
// "user" pivots that's the pivot itself.
type regenerateTarget struct {
	SourceID   string // id of the row whose content drives the regen
	Source     *Message
	PivotID    string
	KillPivot  bool   // true: kill pivot and after (assistant mode); false: keep pivot, kill after (user mode)
	SourceMode string // "user" | "assistant" (informational)
}

//   - assistant msgID: keeps messages before msgID, returns the nearest
//     preceding user message as the regeneration source.
//   - user msgID: keeps messages up to and including msgID, returns msgID
//     itself as the regeneration source.
//   - system/tool/unknown: ErrInvalidRegenerate.
//
// ifMatchETag, when non-empty, is compared against the *clicked*
// message's etag — i.e. the row the user is regenerating from. Mismatch
// returns ErrMessageETagMismatch so the UI can refetch instead of
// truncating against a stale view. The check is intentionally on the
// clicked message rather than the derived regeneration source: the user
// only saw msgID, so that's the only etag they can meaningfully
// precondition on. This call is a fast-fail UX optimisation — the
// authoritative re-check happens inside the truncate transaction.
//
// Source-side staleness for assistant-mode regenerate (another device
// editing the preceding user message between the click and this call)
// is NOT detected by the etag check — the API only carries one etag.
// Manager.Regenerate compensates by re-reading the source content via
// store.GetMessage right before invoking the backend, so the chat
// runs against the currently committed source rather than the snapshot
// captured here.
func findRegenerateTarget(parent context.Context, agentID, msgID, ifMatchETag string) (*regenerateTarget, error) {
	// boundedCtx layers a 30s ceiling on the request ctx so a slow
	// DB scan can't hold the editing mutex forever even when the
	// HTTP connection is still alive. Cancel-on-disconnect still
	// propagates because boundedCtx derives from parent.
	ctx, cancel := boundedCtx(parent)
	defer cancel()
	msgs, err := loadMessagesCtx(ctx, agentID, 0)
	if err != nil {
		return nil, err
	}
	idx := -1
	for i, m := range msgs {
		if m.ID == msgID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, ErrMessageNotFound
	}
	pivot := msgs[idx]
	if ifMatchETag != "" && pivot.ETag != ifMatchETag {
		return nil, ErrMessageETagMismatch
	}
	switch pivot.Role {
	case "assistant":
		for i := idx - 1; i >= 0; i-- {
			if msgs[i].Role == "user" {
				return &regenerateTarget{
					SourceID:   msgs[i].ID,
					Source:     msgs[i],
					PivotID:    pivot.ID,
					KillPivot:  true,
					SourceMode: "assistant",
				}, nil
			}
		}
		return nil, ErrInvalidRegenerate
	case "user":
		return &regenerateTarget{
			SourceID:   pivot.ID,
			Source:     pivot,
			PivotID:    pivot.ID,
			KillPivot:  false,
			SourceMode: "user",
		}, nil
	default:
		return nil, ErrInvalidRegenerate
	}
}

// truncateMessagesTo keeps only the first keepCount messages and
// soft-deletes the rest. keepCount = 0 truncates the entire transcript.
// Used only by lifecycle paths (Reset) — the regenerate flow goes
// through truncateForRegenerate, which derives the boundary from the
// pivot row's seq inside the truncate transaction.
//
// Implementation: ListMessages(asc, limit=keepCount) to get the boundary
// seq, then TruncateMessagesAfterSeq tombstones every message past it
// in a single statement.
func truncateMessagesTo(agentID string, keepCount int) error {
	db := getGlobalStore()
	if db == nil {
		return errStoreNotReady
	}

	ctx, cancel := transcriptCtx()
	defer cancel()

	if keepCount <= 0 {
		_, err := db.TruncateMessagesAfterSeq(ctx, agentID, 0, "", "")
		return err
	}

	recs, err := db.ListMessages(ctx, agentID, store.MessageListOptions{
		Limit: keepCount,
		Order: "asc",
	})
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		// Empty transcript: legacy behaviour returned ErrMessageNotFound
		// from rewriteMessages because it couldn't open messages.jsonl.
		return ErrMessageNotFound
	}
	if len(recs) < keepCount {
		return nil
	}
	boundarySeq := recs[len(recs)-1].Seq
	if _, err := db.TruncateMessagesAfterSeq(ctx, agentID, boundarySeq, "", ""); err != nil {
		return err
	}
	return nil
}

// truncateForRegenerate atomically validates the pivot row's etag,
// re-validates the source row's etag (assistant-mode only — user-mode
// regenerate has source == pivot), derives afterSeq from the pivot's
// seq + killPivot, and tombstones the suffix — all inside one store
// transaction. This closes two cross-device TOCTOU windows:
//
//  1. Pivot-side: a prefix mutation between findRegenerateTarget and
//     the truncate cannot shift the boundary onto the wrong row,
//     because the boundary is derived from the pivot's *immutable* seq
//     inside the same TX that validates the pivot etag.
//  2. Source-side: a concurrent edit / tombstone on the source row
//     (the user message that drives the chat in assistant-mode) lands
//     between Manager.Regenerate's GetMessage(SourceID) and this call
//     would otherwise commit a regen against stale content. Passing
//     sourceETag here makes the source's freshness atomic with the
//     truncate.
//
// sourceID/sourceETag may be empty when the caller does not want the
// source-side check (e.g. CLI internal callers without optimistic
// locking); in that case only the pivot is validated.
//
// killPivot:
//   - true (assistant-mode regenerate): tombstone the pivot itself and
//     everything after — afterSeq = pivot.Seq - 1.
//   - false (user-mode regenerate): keep the pivot, tombstone
//     everything after — afterSeq = pivot.Seq.
func truncateForRegenerate(parent context.Context, agentID, pivotID, pivotETag, sourceID, sourceETag string, killPivot bool) error {
	db := getGlobalStore()
	if db == nil {
		return errStoreNotReady
	}
	// boundedCtx layers a 30s ceiling on the parent so a stuck DB
	// can't hold the edit mutex indefinitely even when the caller's
	// ctx has no deadline.
	ctx, cancel := boundedCtx(parent)
	defer cancel()
	if err := db.TruncateForRegenerate(ctx, agentID, pivotID, pivotETag, sourceID, sourceETag, killPivot); err != nil {
		switch {
		case errors.Is(err, store.ErrETagMismatch):
			return ErrMessageETagMismatch
		case errors.Is(err, store.ErrNotFound):
			return ErrMessageNotFound
		default:
			return err
		}
	}
	return nil
}

// messageToRecord converts the in-memory *Message into the row shape the
// store expects. Tool/Attachment/Usage payloads round-trip as opaque
// JSON so the store doesn't need to know their schemas — that's by
// design (see MessageRecord doc).
func messageToRecord(agentID string, msg *Message) (*store.MessageRecord, error) {
	rec := &store.MessageRecord{
		ID:       msg.ID,
		AgentID:  agentID,
		Role:     msg.Role,
		Content:  msg.Content,
		Thinking: msg.Thinking,
	}
	if len(msg.ToolUses) > 0 {
		buf, err := json.Marshal(msg.ToolUses)
		if err != nil {
			return nil, fmt.Errorf("marshal toolUses: %w", err)
		}
		rec.ToolUses = buf
	}
	if len(msg.Attachments) > 0 {
		buf, err := json.Marshal(msg.Attachments)
		if err != nil {
			return nil, fmt.Errorf("marshal attachments: %w", err)
		}
		rec.Attachments = buf
	}
	if msg.Usage != nil {
		buf, err := json.Marshal(msg.Usage)
		if err != nil {
			return nil, fmt.Errorf("marshal usage: %w", err)
		}
		rec.Usage = buf
	}
	return rec, nil
}

// recordToMessage rehydrates an in-memory *Message from a stored row.
// Empty / null JSON payloads decode to nil slices/pointers so the
// caller can rely on len(toolUses)==0 etc. without a separate nil check.
func recordToMessage(rec *store.MessageRecord) (*Message, error) {
	m := &Message{
		ID:        rec.ID,
		Role:      rec.Role,
		Content:   rec.Content,
		Thinking:  rec.Thinking,
		Timestamp: normalizeTimestamp(millisToRFC3339(rec.CreatedAt)),
		ETag:      rec.ETag,
	}
	if len(rec.ToolUses) > 0 && string(rec.ToolUses) != "null" {
		if err := json.Unmarshal(rec.ToolUses, &m.ToolUses); err != nil {
			return nil, fmt.Errorf("unmarshal toolUses for %s: %w", rec.ID, err)
		}
	}
	if len(rec.Attachments) > 0 && string(rec.Attachments) != "null" {
		if err := json.Unmarshal(rec.Attachments, &m.Attachments); err != nil {
			return nil, fmt.Errorf("unmarshal attachments for %s: %w", rec.ID, err)
		}
	}
	if len(rec.Usage) > 0 && string(rec.Usage) != "null" {
		if err := json.Unmarshal(rec.Usage, &m.Usage); err != nil {
			return nil, fmt.Errorf("unmarshal usage for %s: %w", rec.ID, err)
		}
	}
	return m, nil
}
