package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/loppo-llc/kojo/internal/peer"
)

// chunkedSyncBudgetBytes is the per-chunk raw-JSON budget the
// orchestrator targets when splitting an oversize payload. Stays
// well under peerAgentSyncMaxBody so a single chunk plus the JSON
// framing comfortably fits the receiver's decompressed cap and the
// gzip body fits its wire cap.
//
// 64 MiB (half the receiver cap) gives ~2× headroom for the small
// singleton repeats and JSON envelope overhead. The exact size
// doesn't change correctness — undersized chunks just add more
// round-trips, oversized chunks would 413 on target and the
// orchestrator would have to retry the split.
const chunkedSyncBudgetBytes = 64 << 20

// dispatchPeerAgentSyncChunked drives the begin/chunk/commit dance
// against target's /api/v1/peers/agent-sync/chunked/* endpoints.
// payload is the full peerAgentSyncRequest that proved too large for
// the single-shot path; this function splits it across chunks each
// bounded by chunkedSyncBudgetBytes, then commits.
//
// On any chunk-time failure the helper sends /chunked/abort so target
// reclaims the pending entry's memory without waiting for the TTL
// sweeper. abort is best-effort — a missing entry on target is a
// 200, matching the single-shot drop semantics. The original error is
// returned to the caller unchanged.
func (s *Server) dispatchPeerAgentSyncChunked(ctx context.Context, targetAddr, targetDeviceID string, payload *peerAgentSyncRequest) error {
	if payload == nil {
		return fmt.Errorf("nil payload")
	}
	if payload.OpID == "" {
		return fmt.Errorf("op_id required")
	}

	// Split into begin + N chunks. Singletons go to begin; data
	// arrays are batched up to chunkedSyncBudgetBytes per chunk.
	beginBody, dataChunks, sperr := splitAgentSyncIntoChunks(payload, chunkedSyncBudgetBytes)
	if sperr != nil {
		return fmt.Errorf("split chunks: %w", sperr)
	}

	// /chunked/begin
	if err := s.postChunkedSyncJSON(ctx, targetAddr, "/api/v1/peers/agent-sync/chunked/begin", nil, beginBody); err != nil {
		// Best-effort abort in case target accepted the entry but
		// the response was lost. A "not_found" on abort is benign.
		s.bestEffortChunkedAbort(ctx, targetAddr, payload.OpID)
		return fmt.Errorf("chunked begin: %w", err)
	}

	// /chunked/chunk (seq=0..N-1)
	for i, chunk := range dataChunks {
		q := url.Values{}
		q.Set("op_id", payload.OpID)
		q.Set("seq", strconv.Itoa(i))
		if err := s.postChunkedSyncJSON(ctx, targetAddr, "/api/v1/peers/agent-sync/chunked/chunk", q, chunk); err != nil {
			s.bestEffortChunkedAbort(ctx, targetAddr, payload.OpID)
			return fmt.Errorf("chunked chunk seq=%d: %w", i, err)
		}
	}

	// /chunked/commit — drains and applies. The response carries
	// the agent id so the orchestrator can cross-check.
	//
	// Failure modes:
	//   - Network timeout BEFORE target's handler started: target
	//     never popped the entry; it remains pending. Without an
	//     abort follow-up it lives until the TTL sweeper reclaims
	//     it (default 10 min). Best-effort abort accelerates that.
	//   - HTTP 5xx AFTER target's handler popped the entry but
	//     applyPeerAgentSync failed: the entry is already gone, so
	//     the abort is a no-op (idempotent 200).
	//   - HTTP 4xx (e.g. 409 committing): the entry stays pending
	//     under a concurrent committer. Abort would race; the
	//     committer will resolve.
	// Sending /abort on any commit error is safe in all three
	// cases — the idempotent abort semantics make double-cleanup
	// harmless.
	q := url.Values{}
	q.Set("op_id", payload.OpID)
	if err := s.postChunkedSyncJSON(ctx, targetAddr, "/api/v1/peers/agent-sync/chunked/commit", q, nil); err != nil {
		s.bestEffortChunkedAbort(ctx, targetAddr, payload.OpID)
		return fmt.Errorf("chunked commit: %w", err)
	}
	return nil
}

// postChunkedSyncJSON marshals body (if non-nil) as JSON, gzips it,
// and POSTs to the chunked-sync endpoint. Returns a non-nil error on
// any non-2xx response with the status code and (truncated) body
// included.
//
// A nil body posts an empty Content-Length: 0 request (commit takes
// only the op_id query parameter).
//
// Both raw JSON and the gzipped wire bytes are checked against
// peerAgentSyncMaxBody / peerAgentSyncMaxWireBody respectively before
// dispatch. Incompressible (high-entropy) payloads can have a gzip
// body marginally larger than the raw JSON (gzip header overhead);
// without the explicit wire check a raw-cap-passing chunk could
// still trigger target's MaxBytesReader 413.
func (s *Server) postChunkedSyncJSON(ctx context.Context, targetAddr, path string, query url.Values, body any) error {
	var reqBody io.Reader
	contentEncoding := ""
	if body != nil {
		raw, merr := json.Marshal(body)
		if merr != nil {
			return fmt.Errorf("marshal: %w", merr)
		}
		if int64(len(raw)) > peerAgentSyncMaxBody {
			return fmt.Errorf("raw body %d exceeds peerAgentSyncMaxBody %d",
				len(raw), peerAgentSyncMaxBody)
		}
		var compressed bytes.Buffer
		gz := gzip.NewWriter(&compressed)
		if _, werr := gz.Write(raw); werr != nil {
			return fmt.Errorf("gzip: %w", werr)
		}
		if cerr := gz.Close(); cerr != nil {
			return fmt.Errorf("gzip flush: %w", cerr)
		}
		if int64(compressed.Len()) > peerAgentSyncMaxWireBody {
			return fmt.Errorf("gzipped body %d exceeds peerAgentSyncMaxWireBody %d "+
				"(incompressible payload; reduce chunk budget)",
				compressed.Len(), peerAgentSyncMaxWireBody)
		}
		reqBody = bytes.NewReader(compressed.Bytes())
		contentEncoding = "gzip"
	}

	urlStr := targetAddr + path
	if query != nil && len(query) > 0 {
		urlStr += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
		req.Header.Set("Content-Type", "application/json")
	}

	client := peer.NoKeepAliveHTTPClient(switchDeviceOpTimeout)
	resp, derr := client.Do(req)
	if derr != nil {
		return fmt.Errorf("dispatch: %w", derr)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// chunkedAbortTimeout bounds the best-effort abort RPC. Kept short
// because the sweeper reclaims the entry on TTL anyway — the abort
// is just an accelerant, not a correctness requirement. 5 seconds
// covers a healthy LAN round-trip with margin while bailing fast on
// a dead target.
const chunkedAbortTimeout = 5 * time.Second

// bestEffortChunkedAbort POSTs /chunked/abort?op_id=... with a short
// context so a slow/dead target can't wedge the source goroutine.
// Errors are logged at Debug level and swallowed — the sweeper will
// reclaim the entry on TTL anyway.
//
// Roots the abort RPC in context.Background() (not the parent) so a
// cancelled / deadlined parent — the very condition that triggered
// the abort in most failure paths — does not skip the cleanup. The
// short hard timeout still bounds source-side blocking.
func (s *Server) bestEffortChunkedAbort(_ context.Context, targetAddr, opID string) {
	ctx, cancel := context.WithTimeout(context.Background(), chunkedAbortTimeout)
	defer cancel()
	q := url.Values{}
	q.Set("op_id", opID)
	if err := s.postChunkedSyncJSON(ctx, targetAddr, "/api/v1/peers/agent-sync/chunked/abort", q, nil); err != nil {
		s.logger.Debug("chunked agent-sync: best-effort abort failed (target will sweep)",
			"op_id", opID, "err", err)
	}
}

// splitAgentSyncIntoChunks turns an oversize peerAgentSyncRequest into
// a (begin, []chunks) pair where each piece marshal-size is bounded by
// budgetBytes.
//
// begin carries the singletons (agent, persona, memory, agent_token,
// grok_session, credentials, since-cursors) plus the source / op_id.
// Data chunks carry the row arrays; the splitter walks each array and
// emits a chunk every time the running budget would overflow.
//
// The order in which entities flow into chunks is deterministic
// (messages → memory_entries → workspace_files → tasks →
// claude_sessions) so the receiver-side reassembled
// peerAgentSyncRequest is byte-identical to the input modulo array
// concatenation order — which itself matches the orchestrator's
// SQL ordering, so the apply path sees the same row sequence as in
// single-shot mode.
//
// Returns an empty chunks slice when the data arrays are all empty —
// the caller is still expected to commit (begin alone applies the
// singletons).
//
// Hard caps enforced before dispatch:
//   - Each begin body (singletons + grok_session + credentials)
//     must marshal under peerAgentSyncMaxBody. GrokSession in
//     particular base64-bloats poorly; the splitter checks here
//     so source fails fast instead of round-tripping to target's
//     413.
//   - Each sealed chunk body must marshal under peerAgentSyncMaxBody.
//     A single oversized row (e.g. a 130 MiB memory_entry body)
//     can never fit in any chunk; the splitter returns an error
//     so the orchestrator can surface a useful message instead
//     of looping abort/retry against target's 413.
func splitAgentSyncIntoChunks(payload *peerAgentSyncRequest, budgetBytes int) (begin *peerAgentSyncChunkedBeginRequest, chunks []*peerAgentSyncChunkedChunkRequest, err error) {
	if payload == nil {
		return nil, nil, fmt.Errorf("nil payload")
	}
	if budgetBytes <= 0 {
		return nil, nil, fmt.Errorf("budget must be > 0")
	}

	begin = &peerAgentSyncChunkedBeginRequest{
		SourceDeviceID:            payload.SourceDeviceID,
		OpID:                      payload.OpID,
		Agent:                     payload.Agent,
		Persona:                   payload.Persona,
		Memory:                    payload.Memory,
		AgentToken:                payload.AgentToken,
		GrokSession:               payload.GrokSession,
		Credentials:               payload.Credentials,
		SinceMessageSeq:           payload.SinceMessageSeq,
		SinceMemoryEntrySeq:       payload.SinceMemoryEntrySeq,
		SinceMemoryEntryUpdatedAt: payload.SinceMemoryEntryUpdatedAt,
	}
	// Begin-size cap: grok session base64 + persona/memory body
	// can in principle bust the begin cap on their own. Marshal
	// once and check before any chunk work runs.
	beginRaw, berr := json.Marshal(begin)
	if berr != nil {
		return nil, nil, fmt.Errorf("marshal begin: %w", berr)
	}
	if int64(len(beginRaw)) > peerAgentSyncMaxBody {
		return nil, nil, fmt.Errorf("begin body raw size %d exceeds peerAgentSyncMaxBody %d "+
			"(grok_session / credentials / persona / memory too large to ride in /chunked/begin; "+
			"orchestrator must refuse the switch instead of splitting begin)",
			len(beginRaw), peerAgentSyncMaxBody)
	}

	// Greedy packer: open a chunk, append rows until the next
	// row's marshal-size would push the running total past
	// budgetBytes, then seal and open a fresh chunk. A single row
	// whose marshal-size on its own exceeds peerAgentSyncMaxBody is
	// rejected — no amount of further chunking can fit it under
	// target's per-chunk cap.
	current := &peerAgentSyncChunkedChunkRequest{}
	currentSize := 0
	seal := func() {
		if currentSize == 0 {
			return
		}
		chunks = append(chunks, current)
		current = &peerAgentSyncChunkedChunkRequest{}
		currentSize = 0
	}
	checkRow := func(rawSize int, label string) error {
		if int64(rawSize) > peerAgentSyncMaxBody {
			return fmt.Errorf("single %s row marshal-size %d exceeds peerAgentSyncMaxBody %d "+
				"(no chunking can fit it; row must be trimmed or excluded)",
				label, rawSize, peerAgentSyncMaxBody)
		}
		return nil
	}
	for _, m := range payload.Messages {
		raw, merr := json.Marshal(m)
		if merr != nil {
			return nil, nil, fmt.Errorf("marshal message: %w", merr)
		}
		if cerr := checkRow(len(raw), "message"); cerr != nil {
			return nil, nil, cerr
		}
		if currentSize > 0 && currentSize+len(raw) > budgetBytes {
			seal()
		}
		current.Messages = append(current.Messages, m)
		currentSize += len(raw)
	}
	for _, m := range payload.MemoryEntries {
		raw, merr := json.Marshal(m)
		if merr != nil {
			return nil, nil, fmt.Errorf("marshal memory_entry: %w", merr)
		}
		if cerr := checkRow(len(raw), "memory_entry"); cerr != nil {
			return nil, nil, cerr
		}
		if currentSize > 0 && currentSize+len(raw) > budgetBytes {
			seal()
		}
		current.MemoryEntries = append(current.MemoryEntries, m)
		currentSize += len(raw)
	}
	for _, f := range payload.WorkspaceFiles {
		raw, merr := json.Marshal(f)
		if merr != nil {
			return nil, nil, fmt.Errorf("marshal workspace_file: %w", merr)
		}
		if cerr := checkRow(len(raw), "workspace_file"); cerr != nil {
			return nil, nil, cerr
		}
		if currentSize > 0 && currentSize+len(raw) > budgetBytes {
			seal()
		}
		current.WorkspaceFiles = append(current.WorkspaceFiles, f)
		currentSize += len(raw)
	}
	for _, t := range payload.Tasks {
		raw, merr := json.Marshal(t)
		if merr != nil {
			return nil, nil, fmt.Errorf("marshal task: %w", merr)
		}
		if cerr := checkRow(len(raw), "task"); cerr != nil {
			return nil, nil, cerr
		}
		if currentSize > 0 && currentSize+len(raw) > budgetBytes {
			seal()
		}
		current.Tasks = append(current.Tasks, t)
		currentSize += len(raw)
	}
	for _, cs := range payload.ClaudeSessions {
		raw, merr := json.Marshal(cs)
		if merr != nil {
			return nil, nil, fmt.Errorf("marshal claude_session: %w", merr)
		}
		if cerr := checkRow(len(raw), "claude_session"); cerr != nil {
			return nil, nil, cerr
		}
		if currentSize > 0 && currentSize+len(raw) > budgetBytes {
			seal()
		}
		current.ClaudeSessions = append(current.ClaudeSessions, cs)
		currentSize += len(raw)
	}
	seal()

	// Post-seal verification: re-marshal each chunk and assert
	// the encoded size fits both caps. Catches pathological cases
	// where the greedy packer's per-row sum under-counted JSON
	// envelope/escape overhead enough to push the sealed chunk
	// past peerAgentSyncMaxBody. Source-side rejection here saves
	// a round-trip + abort cycle against target's 413.
	for i, c := range chunks {
		raw, merr := json.Marshal(c)
		if merr != nil {
			return nil, nil, fmt.Errorf("verify chunk %d marshal: %w", i, merr)
		}
		if int64(len(raw)) > peerAgentSyncMaxBody {
			return nil, nil, fmt.Errorf("sealed chunk %d marshal-size %d exceeds peerAgentSyncMaxBody %d "+
				"(reduce chunkedSyncBudgetBytes or trim payload)",
				i, len(raw), peerAgentSyncMaxBody)
		}
	}
	return begin, chunks, nil
}

// estimateAgentSyncRawSize returns the approximate raw JSON size of
// the full peerAgentSyncRequest WITHOUT building it in memory. The
// orchestrator uses this to route oversize payloads through the
// chunked dispatcher BEFORE running encodeAgentSyncWire (which would
// otherwise pin ~2× the raw size — raw + gzip — on source).
//
// "Approximate": the sum of per-entity Marshal sizes plus a fixed
// envelope overhead for the top-level JSON object. Smaller than the
// real marshal in pathological cases (deeply nested ClaudeSessions
// with redundant whitespace), but accurate to within a few % for
// the row shapes this codebase actually ships.
//
// Walks each entity in turn and marshals it individually so peak
// memory is bounded by the single largest row, not the cumulative
// payload. GrokSession.Files in particular base64-bloats poorly
// (claude transcripts often run to tens of MiB); the estimator
// walks files one-at-a-time so a 500 MiB grok session doesn't pin
// the full singleton blob during the routing decision.
func estimateAgentSyncRawSize(payload *peerAgentSyncRequest) (int64, error) {
	if payload == nil {
		return 0, fmt.Errorf("nil payload")
	}
	// Singletons WITHOUT the bulky-but-flat GrokSession.Files —
	// those are walked separately below. The grokSessionWire
	// header (SessionID) stays in the small-singleton marshal.
	var grokHeader *grokSessionWire
	if payload.GrokSession != nil {
		grokHeader = &grokSessionWire{SessionID: payload.GrokSession.SessionID}
	}
	singletons := struct {
		SourceDeviceID            string
		OpID                      string
		Agent                     any
		Persona                   any
		Memory                    any
		AgentToken                string
		GrokSession               any
		Credentials               any
		SinceMessageSeq           int64
		SinceMemoryEntrySeq       int64
		SinceMemoryEntryUpdatedAt int64
	}{
		SourceDeviceID:            payload.SourceDeviceID,
		OpID:                      payload.OpID,
		Agent:                     payload.Agent,
		Persona:                   payload.Persona,
		Memory:                    payload.Memory,
		AgentToken:                payload.AgentToken,
		GrokSession:               grokHeader,
		Credentials:               payload.Credentials,
		SinceMessageSeq:           payload.SinceMessageSeq,
		SinceMemoryEntrySeq:       payload.SinceMemoryEntrySeq,
		SinceMemoryEntryUpdatedAt: payload.SinceMemoryEntryUpdatedAt,
	}
	raw, err := json.Marshal(singletons)
	if err != nil {
		return 0, fmt.Errorf("marshal singletons estimate: %w", err)
	}
	total := int64(len(raw))
	if payload.GrokSession != nil {
		for _, f := range payload.GrokSession.Files {
			b, merr := json.Marshal(f)
			if merr != nil {
				return 0, fmt.Errorf("estimate grok_session_file: %w", merr)
			}
			total += int64(len(b)) + 1
		}
	}
	addArray := func(label string, fn func() (int64, error)) error {
		n, err := fn()
		if err != nil {
			return fmt.Errorf("estimate %s: %w", label, err)
		}
		total += n
		return nil
	}
	if err := addArray("messages", func() (int64, error) {
		var n int64
		for _, m := range payload.Messages {
			b, merr := json.Marshal(m)
			if merr != nil {
				return 0, merr
			}
			n += int64(len(b)) + 1 // +1 for comma/bracket envelope amortised
		}
		return n, nil
	}); err != nil {
		return 0, err
	}
	if err := addArray("memory_entries", func() (int64, error) {
		var n int64
		for _, m := range payload.MemoryEntries {
			b, merr := json.Marshal(m)
			if merr != nil {
				return 0, merr
			}
			n += int64(len(b)) + 1
		}
		return n, nil
	}); err != nil {
		return 0, err
	}
	if err := addArray("workspace_files", func() (int64, error) {
		var n int64
		for _, f := range payload.WorkspaceFiles {
			b, merr := json.Marshal(f)
			if merr != nil {
				return 0, merr
			}
			n += int64(len(b)) + 1
		}
		return n, nil
	}); err != nil {
		return 0, err
	}
	if err := addArray("tasks", func() (int64, error) {
		var n int64
		for _, t := range payload.Tasks {
			b, merr := json.Marshal(t)
			if merr != nil {
				return 0, merr
			}
			n += int64(len(b)) + 1
		}
		return n, nil
	}); err != nil {
		return 0, err
	}
	if err := addArray("claude_sessions", func() (int64, error) {
		var n int64
		for _, cs := range payload.ClaudeSessions {
			b, merr := json.Marshal(cs)
			if merr != nil {
				return 0, merr
			}
			n += int64(len(b)) + 1
		}
		return n, nil
	}); err != nil {
		return 0, err
	}
	return total, nil
}
