package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// newChunkedSyncTestServer builds a Server with just enough wiring for
// the chunked sync handlers. agents is required by every handler's
// `s.agents.Store()` precheck; we point configdir at a per-test temp
// directory via XDG_CONFIG_HOME so NewManager's internal store lands
// in an isolated scratch tree without leaking into the user's real
// config dir. The chunked map + mu are initialised the same way
// New() does.
func newChunkedSyncTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mgr, mErr := agent.NewManager(slog.Default())
	if mErr != nil {
		t.Fatalf("agent.NewManager: %v", mErr)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	srv := &Server{
		agents:               mgr,
		logger:               slog.Default(),
		chunkedAgentSyncs:    make(map[string]*chunkedSyncEntry),
		chunkedSyncSweepDone: make(chan struct{}),
	}
	// LIFO: runs BEFORE mgr.Close above, so a fire-and-forget
	// queue-drain kick from a handler under test (handoff complete,
	// enqueue, finalize) can't touch the store during TempDir
	// cleanup (see stopHandoffDrainForTest).
	t.Cleanup(func() { stopHandoffDrainForTest(t, srv) })
	return srv
}

// authedRequest wraps r in a context that stamps the given Principal
// — bypasses the middleware chain so handler-only tests don't need to
// spin up a listener.
func authedRequest(r *http.Request, p auth.Principal) *http.Request {
	return r.WithContext(auth.WithPrincipal(r.Context(), p))
}

// gzipJSON marshals v and returns the gzipped bytes. The chunked
// handlers accept identity-encoded bodies too, but exercising the
// gzip path matches production behaviour.
func gzipJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// readJSONResponse decodes the response body as JSON into out.
func readJSONResponse(t *testing.T, rr *httptest.ResponseRecorder, out any) {
	t.Helper()
	body, _ := io.ReadAll(rr.Body)
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode response (status %d, body %q): %v", rr.Code, string(body), err)
	}
}

const (
	testChunkedSource  = "src-device"
	testChunkedAgentID = "ag_chunked_test"
)

// makeBeginRequest builds a minimal but valid begin payload bound to
// the test source/agent. opID is unique per call so the parallel
// tests don't collide on the chunkedAgentSyncs map.
func makeBeginRequest(opID string) *peerAgentSyncChunkedBeginRequest {
	return &peerAgentSyncChunkedBeginRequest{
		SourceDeviceID: testChunkedSource,
		OpID:           opID,
		Agent: &store.AgentRecord{
			ID:   testChunkedAgentID,
			Name: "chunked test agent",
		},
	}
}

// postChunked invokes one of the chunked handlers via httptest with
// the gzipped body and an injected RolePeer principal.
func postChunked(t *testing.T, srv *Server, path string, query url.Values, body any, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	urlStr := path
	if query != nil && len(query) > 0 {
		urlStr += "?" + query.Encode()
	}
	var reader io.Reader
	var ce string
	if body != nil {
		reader = bytes.NewReader(gzipJSON(t, body))
		ce = "gzip"
	}
	r := httptest.NewRequest(http.MethodPost, urlStr, reader)
	if ce != "" {
		r.Header.Set("Content-Encoding", ce)
		r.Header.Set("Content-Type", "application/json")
	}
	r = authedRequest(r, auth.Principal{Role: auth.RolePeer, PeerID: testChunkedSource})
	rr := httptest.NewRecorder()
	handler(rr, r)
	return rr
}

// TestChunkedSync_BeginAcceptsValidRequest pins the begin happy path:
// a well-formed request reserves a slot and returns NextChunkSeq=0.
func TestChunkedSync_BeginAcceptsValidRequest(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	rr := postChunked(t, srv, "/api/v1/peers/agent-sync/chunked/begin", nil,
		makeBeginRequest("op-begin-1"), srv.handlePeerAgentSyncChunkedBegin)
	if rr.Code != http.StatusOK {
		t.Fatalf("begin status: got %d, body %s", rr.Code, rr.Body.String())
	}
	var resp peerAgentSyncChunkedResponse
	readJSONResponse(t, rr, &resp)
	if resp.OpID != "op-begin-1" || resp.NextChunkSeq != 0 {
		t.Errorf("begin response: got %+v", resp)
	}
	srv.chunkedSyncMu.Lock()
	_, ok := srv.chunkedAgentSyncs["op-begin-1"]
	srv.chunkedSyncMu.Unlock()
	if !ok {
		t.Errorf("entry not stored after begin")
	}
}

// TestChunkedSync_BeginRejectsDuplicateOpID: a second begin with the
// same op_id is 409, defending against a retry collision that would
// silently merge data into another upload.
func TestChunkedSync_BeginRejectsDuplicateOpID(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	op := "op-dup"
	if rr := postChunked(t, srv, "/api/v1/peers/agent-sync/chunked/begin", nil,
		makeBeginRequest(op), srv.handlePeerAgentSyncChunkedBegin); rr.Code != http.StatusOK {
		t.Fatalf("first begin: %d %s", rr.Code, rr.Body.String())
	}
	rr := postChunked(t, srv, "/api/v1/peers/agent-sync/chunked/begin", nil,
		makeBeginRequest(op), srv.handlePeerAgentSyncChunkedBegin)
	if rr.Code != http.StatusConflict {
		t.Errorf("second begin: got %d want 409 (%s)", rr.Code, rr.Body.String())
	}
}

// TestChunkedSync_ChunkAppendsAndCommit walks the full begin→chunk→
// commit flow with synthetic messages. The commit hits applyPeerAgentSync
// which we don't drive end-to-end here (it requires sessions / disk
// staging); the test asserts the chunk handler accumulates correctly
// up to the point where commit would dispatch.
func TestChunkedSync_ChunkAppendsRows(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	op := "op-chunks"
	if rr := postChunked(t, srv, "/api/v1/peers/agent-sync/chunked/begin", nil,
		makeBeginRequest(op), srv.handlePeerAgentSyncChunkedBegin); rr.Code != http.StatusOK {
		t.Fatalf("begin: %d %s", rr.Code, rr.Body.String())
	}

	// Two chunks. First has messages, second has memory_entries.
	chunk0 := &peerAgentSyncChunkedChunkRequest{
		Messages: []*store.MessageRecord{
			{ID: "m1", AgentID: testChunkedAgentID, Seq: 1, Role: "user", Content: "hi"},
			{ID: "m2", AgentID: testChunkedAgentID, Seq: 2, Role: "assistant", Content: "hello"},
		},
	}
	chunk1 := &peerAgentSyncChunkedChunkRequest{
		MemoryEntries: []*store.MemoryEntryRecord{
			{ID: "me1", AgentID: testChunkedAgentID, Kind: "fact", Name: "n1", Body: "b1"},
		},
	}
	for i, chunk := range []any{chunk0, chunk1} {
		q := url.Values{"op_id": []string{op}, "seq": []string{strconv.Itoa(i)}}
		rr := postChunked(t, srv, "/api/v1/peers/agent-sync/chunked/chunk", q, chunk,
			srv.handlePeerAgentSyncChunkedChunk)
		if rr.Code != http.StatusOK {
			t.Fatalf("chunk %d: got %d (%s)", i, rr.Code, rr.Body.String())
		}
		var resp peerAgentSyncChunkedResponse
		readJSONResponse(t, rr, &resp)
		if resp.NextChunkSeq != i+1 {
			t.Errorf("chunk %d: next_seq = %d want %d", i, resp.NextChunkSeq, i+1)
		}
	}
	srv.chunkedSyncMu.Lock()
	entry := srv.chunkedAgentSyncs[op]
	srv.chunkedSyncMu.Unlock()
	if entry == nil {
		t.Fatalf("entry vanished mid-flight")
	}
	if len(entry.req.Messages) != 2 {
		t.Errorf("Messages len = %d want 2", len(entry.req.Messages))
	}
	if len(entry.req.MemoryEntries) != 1 {
		t.Errorf("MemoryEntries len = %d want 1", len(entry.req.MemoryEntries))
	}
}

// TestChunkedSync_ChunkRejectsOutOfOrder ensures the strict-ordering
// gate fires: a chunk with seq != nextChunkSeq is 409. Defends against
// replays and a slicer bug that double-sends a chunk.
func TestChunkedSync_ChunkRejectsOutOfOrder(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	op := "op-order"
	if rr := postChunked(t, srv, "/api/v1/peers/agent-sync/chunked/begin", nil,
		makeBeginRequest(op), srv.handlePeerAgentSyncChunkedBegin); rr.Code != http.StatusOK {
		t.Fatalf("begin: %d %s", rr.Code, rr.Body.String())
	}
	// First chunk must be seq=0; seq=1 should be rejected.
	q := url.Values{"op_id": []string{op}, "seq": []string{"1"}}
	rr := postChunked(t, srv, "/api/v1/peers/agent-sync/chunked/chunk", q,
		&peerAgentSyncChunkedChunkRequest{}, srv.handlePeerAgentSyncChunkedChunk)
	if rr.Code != http.StatusConflict {
		t.Errorf("out-of-order chunk: got %d want 409 (%s)", rr.Code, rr.Body.String())
	}
}

// TestChunkedSync_ChunkRejectsWrongSource: a different peer trying to
// append to an existing op_id is 403. The map-key reservation is
// per-peer.
func TestChunkedSync_ChunkRejectsWrongSource(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	op := "op-wrong-src"
	if rr := postChunked(t, srv, "/api/v1/peers/agent-sync/chunked/begin", nil,
		makeBeginRequest(op), srv.handlePeerAgentSyncChunkedBegin); rr.Code != http.StatusOK {
		t.Fatalf("begin: %d %s", rr.Code, rr.Body.String())
	}
	q := url.Values{"op_id": []string{op}, "seq": []string{"0"}}
	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/peers/agent-sync/chunked/chunk?"+q.Encode(),
		bytes.NewReader(gzipJSON(t, &peerAgentSyncChunkedChunkRequest{})))
	r.Header.Set("Content-Encoding", "gzip")
	r = authedRequest(r, auth.Principal{Role: auth.RolePeer, PeerID: "other-device"})
	rr := httptest.NewRecorder()
	srv.handlePeerAgentSyncChunkedChunk(rr, r)
	if rr.Code != http.StatusForbidden {
		t.Errorf("wrong-source chunk: got %d want 403 (%s)", rr.Code, rr.Body.String())
	}
}

// TestChunkedSync_AbortIsIdempotent: aborting a non-existent op_id
// returns 200. The orchestrator can call /abort without first
// checking whether a slot was even reserved.
func TestChunkedSync_AbortIsIdempotent(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	q := url.Values{"op_id": []string{"never-began"}}
	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/peers/agent-sync/chunked/abort?"+q.Encode(), nil)
	r = authedRequest(r, auth.Principal{Role: auth.RolePeer, PeerID: testChunkedSource})
	rr := httptest.NewRecorder()
	srv.handlePeerAgentSyncChunkedAbort(rr, r)
	if rr.Code != http.StatusOK {
		t.Errorf("abort of missing op_id: got %d (%s)", rr.Code, rr.Body.String())
	}
}

// TestChunkedSync_SweeperDropsExpiredEntries pokes the sweeper with a
// synthetic time so we don't have to wait minutes. Entries older than
// chunkedSyncEntryTTL get evicted; fresh ones survive.
func TestChunkedSync_SweeperDropsExpiredEntries(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	now := time.Now()
	// Seed two entries: one stale, one fresh.
	srv.chunkedSyncMu.Lock()
	srv.chunkedAgentSyncs["stale"] = &chunkedSyncEntry{
		sourceDeviceID: testChunkedSource,
		req:            &peerAgentSyncRequest{Agent: &store.AgentRecord{ID: "stale-agent"}},
		lastTouched:    now.Add(-2 * chunkedSyncEntryTTL),
	}
	srv.chunkedAgentSyncs["fresh"] = &chunkedSyncEntry{
		sourceDeviceID: testChunkedSource,
		req:            &peerAgentSyncRequest{Agent: &store.AgentRecord{ID: "fresh-agent"}},
		lastTouched:    now.Add(-time.Second),
	}
	srv.chunkedSyncMu.Unlock()

	srv.sweepExpiredChunkedSyncs(now)

	srv.chunkedSyncMu.Lock()
	_, hasStale := srv.chunkedAgentSyncs["stale"]
	_, hasFresh := srv.chunkedAgentSyncs["fresh"]
	srv.chunkedSyncMu.Unlock()
	if hasStale {
		t.Errorf("stale entry not swept")
	}
	if !hasFresh {
		t.Errorf("fresh entry incorrectly swept")
	}
}

// TestChunkedSync_AccumulatedSizeCap: a chunk that would push the
// running total past chunkedSyncMaxAccumulatedBytes is 413 AND the
// pending entry is dropped so a follow-up commit can't apply a
// truncated prefix. The orchestrator must restart with a fresh op_id.
func TestChunkedSync_AccumulatedSizeCap(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	op := "op-cap"
	if rr := postChunked(t, srv, "/api/v1/peers/agent-sync/chunked/begin", nil,
		makeBeginRequest(op), srv.handlePeerAgentSyncChunkedBegin); rr.Code != http.StatusOK {
		t.Fatalf("begin: %d %s", rr.Code, rr.Body.String())
	}
	srv.chunkedSyncMu.Lock()
	srv.chunkedAgentSyncs[op].accumulatedBytes = chunkedSyncMaxAccumulatedBytes - 10
	srv.chunkedSyncMu.Unlock()

	// Send a chunk that's > 10 bytes after gzip. JSON-encoding a
	// 100-byte content string gets us over comfortably.
	chunk := &peerAgentSyncChunkedChunkRequest{
		Messages: []*store.MessageRecord{
			{ID: "m_big", AgentID: testChunkedAgentID, Seq: 1, Role: "user",
				Content: strings.Repeat("x", 1024)},
		},
	}
	q := url.Values{"op_id": []string{op}, "seq": []string{"0"}}
	rr := postChunked(t, srv, "/api/v1/peers/agent-sync/chunked/chunk", q, chunk,
		srv.handlePeerAgentSyncChunkedChunk)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("cap-busting chunk: got %d want 413 (%s)", rr.Code, rr.Body.String())
	}
	// Entry MUST be dropped — a stale entry would let a follow-up
	// commit apply the unflushed prefix.
	srv.chunkedSyncMu.Lock()
	_, stillThere := srv.chunkedAgentSyncs[op]
	srv.chunkedSyncMu.Unlock()
	if stillThere {
		t.Errorf("cap-busting chunk left entry in map; commit could apply truncated prefix")
	}
	// Commit must 404 after the entry is poisoned.
	commitQ := url.Values{"op_id": []string{op}}
	commitRR := postChunked(t, srv, "/api/v1/peers/agent-sync/chunked/commit", commitQ, nil,
		srv.handlePeerAgentSyncChunkedCommit)
	if commitRR.Code != http.StatusNotFound {
		t.Errorf("commit after cap-buster: got %d want 404 (%s)", commitRR.Code, commitRR.Body.String())
	}
}

// TestChunkedSync_ChunkOnMissingOpIDDoesNotDecodeBody: a chunk POST
// for an op_id that was never begun returns 404 BEFORE the body is
// read. Defends against a wrong-peer / replay request forcing target
// to spend the full peerAgentSyncMaxBody decode budget. We verify
// this indirectly by sending a body that would otherwise fail
// gzip-decode and observing the 404 status (not 400 bad_request).
func TestChunkedSync_ChunkOnMissingOpIDDoesNotDecodeBody(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	// Garbage bytes that would fail gzip decode if read.
	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/peers/agent-sync/chunked/chunk?op_id=never-began&seq=0",
		bytes.NewReader([]byte("not-valid-gzip-bytes")))
	r.Header.Set("Content-Encoding", "gzip")
	r = authedRequest(r, auth.Principal{Role: auth.RolePeer, PeerID: testChunkedSource})
	rr := httptest.NewRecorder()
	srv.handlePeerAgentSyncChunkedChunk(rr, r)
	if rr.Code != http.StatusNotFound {
		t.Errorf("chunk on missing op_id: got %d want 404 (%s) — body decoded before map check",
			rr.Code, rr.Body.String())
	}
}

// TestEstimateAgentSyncRawSize roughly tracks the actual marshal
// size. Doesn't have to be exact — within ±20% across mixed
// payloads — but must not under-report by orders of magnitude
// (would defeat its purpose as an oversize-detection preflight).
func TestEstimateAgentSyncRawSize(t *testing.T) {
	payload := &peerAgentSyncRequest{
		SourceDeviceID: testChunkedSource,
		OpID:           "op-est",
		Agent:          &store.AgentRecord{ID: testChunkedAgentID, Name: "est test"},
	}
	for i := 0; i < 50; i++ {
		payload.Messages = append(payload.Messages, &store.MessageRecord{
			ID: "m" + strconv.Itoa(i), AgentID: testChunkedAgentID, Seq: int64(i + 1),
			Role: "user", Content: strings.Repeat("x", 200),
		})
	}
	est, err := estimateAgentSyncRawSize(payload)
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	actual, _ := json.Marshal(payload)
	ratio := float64(est) / float64(len(actual))
	if ratio < 0.8 || ratio > 1.3 {
		t.Errorf("estimate %d vs actual %d: ratio %.2f outside [0.8, 1.3]", est, len(actual), ratio)
	}
}

// TestSplitAgentSyncIntoChunks_RejectsOversizeRow: a single row whose
// marshal-size exceeds peerAgentSyncMaxBody is rejected by the
// splitter (no amount of chunking can fit it under target's per-chunk
// cap). Defends against the orchestrator looping abort/retry against
// a 413 target would otherwise return on the chunk POST.
func TestSplitAgentSyncIntoChunks_RejectsOversizeRow(t *testing.T) {
	payload := &peerAgentSyncRequest{
		SourceDeviceID: testChunkedSource,
		OpID:           "op-oversize-row",
		Agent:          &store.AgentRecord{ID: testChunkedAgentID, Name: "oversize"},
		Messages: []*store.MessageRecord{
			{ID: "m_huge", AgentID: testChunkedAgentID, Seq: 1, Role: "user",
				Content: strings.Repeat("x", peerAgentSyncMaxBody+1)},
		},
	}
	_, _, err := splitAgentSyncIntoChunks(payload, chunkedSyncBudgetBytes)
	if err == nil {
		t.Fatalf("expected error on single oversize row, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds peerAgentSyncMaxBody") {
		t.Errorf("error %v does not mention the cap; orchestrator can't diagnose", err)
	}
}

// TestSplitAgentSyncIntoChunks_RespectsBudget: the splitter never
// emits a chunk whose marshal-size exceeds budgetBytes (modulo the
// minimum case of one row > budget, where a single-row chunk is
// unavoidable).
func TestSplitAgentSyncIntoChunks_RespectsBudget(t *testing.T) {
	// Build a payload with messages that each marshal to ~1KB.
	const (
		rows     = 100
		rowBytes = 1024
		budget   = 10 * 1024 // 10 rows per chunk → 10 chunks expected
	)
	payload := &peerAgentSyncRequest{
		SourceDeviceID: testChunkedSource,
		OpID:           "op-split",
		Agent:          &store.AgentRecord{ID: testChunkedAgentID, Name: "split test"},
	}
	for i := 0; i < rows; i++ {
		payload.Messages = append(payload.Messages, &store.MessageRecord{
			ID:      "m" + strconv.Itoa(i),
			AgentID: testChunkedAgentID,
			Seq:     int64(i + 1),
			Role:    "user",
			Content: strings.Repeat("x", rowBytes-200), // leave room for envelope
		})
	}
	_, chunks, err := splitAgentSyncIntoChunks(payload, budget)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(chunks) < 5 || len(chunks) > 20 {
		t.Errorf("chunk count %d outside expected band [5,20]", len(chunks))
	}
	for i, c := range chunks {
		raw, _ := json.Marshal(c)
		// Permit overshoot up to one row's worth since the
		// greedy packer only seals AFTER detecting overflow.
		if len(raw) > budget+rowBytes {
			t.Errorf("chunk %d size %d > budget %d + row %d", i, len(raw), budget, rowBytes)
		}
	}
	// Reassembled total row count must match input.
	total := 0
	for _, c := range chunks {
		total += len(c.Messages)
	}
	if total != rows {
		t.Errorf("reassembled message count %d != input %d", total, rows)
	}
}

// TestSplitAgentSyncIntoChunks_EmptyPayload: a payload with only
// singletons returns zero data chunks. The orchestrator still
// commits — begin alone applies the singletons.
func TestSplitAgentSyncIntoChunks_EmptyPayload(t *testing.T) {
	payload := &peerAgentSyncRequest{
		SourceDeviceID: testChunkedSource,
		OpID:           "op-empty",
		Agent:          &store.AgentRecord{ID: testChunkedAgentID, Name: "empty"},
	}
	begin, chunks, err := splitAgentSyncIntoChunks(payload, 1024)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if begin == nil {
		t.Fatalf("begin nil")
	}
	if len(chunks) != 0 {
		t.Errorf("chunks len = %d want 0", len(chunks))
	}
}
