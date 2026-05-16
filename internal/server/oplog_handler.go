package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/oplog"
	"github.com/loppo-llc/kojo/internal/store"
)

// docs/multi-device-storage.md §3.13.1 — the Hub-side receive surface
// for op-log replays. A peer that buffered agent-runtime writes during
// a Hub-partition replays them via this endpoint when the partition
// recovers. The endpoint enforces two preconditions in order:
//
//  1. fencing_token gate: EVERY entry's (agent_id, peer_id,
//     fencing_token) tuple must match the current agent_locks row.
//     If ANY entry mismatches, the WHOLE batch is rejected
//     (docs §3.13.1 step 5.1 — a single stale token means the lock
//     was lost mid-partition and the peer's accumulated writes are
//     no longer authoritative). The peer is expected to archive
//     the batch for forensic review and surface "partition writes
//     lost" to the UI.
//  2. per-entry dispatch: entries are applied to the appropriate
//     store path in op_id order. Each entry is idempotent on the
//     dispatch side (insert paths use the op_id as the row ID and
//     treat PK conflicts as success).
//
// Auth: Owner-only. Op-log replay is an admin-grade operation —
// only the operator (or future peer-Hub mTLS) drives it.
//
// Batch size: capped at oplogFlushMaxEntries entries and
// oplogFlushMaxBytes total bytes so a malicious / buggy peer can't
// pin the Hub on a single request. Larger backlogs are flushed in
// multiple batches.

const (
	// oplogFlushMaxBytes caps the total request body size. 64 MiB
	// generously fits a per-agent op-log at its 10 MB rotation
	// limit × 6 (the default backlog cap is much smaller). A
	// hostile peer would have to chain very large requests to
	// approach the Hub's memory ceiling.
	oplogFlushMaxBytes = 64 << 20

	// oplogFlushMaxEntries caps the number of entries per request.
	// Bounded so the per-entry dispatch loop has predictable wall
	// time and so a runaway peer can't drown the Hub in
	// rate-limited dispatches in one HTTP turn.
	oplogFlushMaxEntries = 5000

	// oplogPeerIDPattern bounds the peer_id shape; same alphabet
	// peer_registry.device_id uses. Validated up-front so a
	// malformed peer_id can't shape a kv-key from the fencing
	// branch in unexpected ways.
	oplogPeerIDMaxLen = 64
)

// oplogFlushRequest is the wire shape for POST /api/v1/oplog/flush.
// peer_id identifies the flushing peer (must match every entry's
// expected holder_peer in agent_locks). entries is the batch in
// op_id order.
type oplogFlushRequest struct {
	PeerID  string         `json:"peer_id"`
	Entries []*oplog.Entry `json:"entries"`
}

// oplogFlushResult is one per-entry outcome returned in the response.
// status is "ok" on a successful (or idempotent) dispatch; "error"
// means the dispatch surfaced a non-fencing error (the batch as a
// whole was NOT rejected — the peer is expected to retry just this
// entry after correcting the underlying issue).
type oplogFlushResult struct {
	OpID   string `json:"op_id"`
	Status string `json:"status"`
	ETag   string `json:"etag,omitempty"`
	Error  string `json:"error,omitempty"`
}

// oplogFlushResponse is the response envelope. Rejected=true means
// the WHOLE batch was refused (fencing failure); peer archives and
// abandons. Rejected=false means each entry has an individual result.
type oplogFlushResponse struct {
	Rejected     bool               `json:"rejected"`
	RejectReason string             `json:"reject_reason,omitempty"`
	Results      []oplogFlushResult `json:"results,omitempty"`
}

// handleOplogFlush implements POST /api/v1/oplog/flush. The function
// is split into:
//
//   - parseOplogFlushBody → strict body validation.
//   - oplogValidateBatch  → fencing precondition for ALL entries.
//   - oplogDispatchBatch  → per-entry dispatch with op_id-keyed
//     idempotency.
//
// Splitting keeps the handler itself a thin orchestrator and lets
// each step be tested in isolation.
func (s *Server) handleOplogFlush(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil || s.agents.Store() == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "op-log replay requires store")
		return
	}
	if !auth.FromContext(r.Context()).IsOwner() {
		writeError(w, http.StatusForbidden, "forbidden", "owner-only")
		return
	}
	req, err := parseOplogFlushBody(w, r)
	if err != nil {
		// parseOplogFlushBody already wrote the response.
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Phase 0: ledger probe. Entries whose op_id has already been
	// applied (with a matching fingerprint) bypass the fencing gate
	// — their dispatch is a no-op that returns the saved etag, so
	// fencing rotated since the original write must NOT cause the
	// retry to fail. Mismatched fingerprints (op_id reuse) AND
	// any non-NotFound DB error tag the entry as a per-entry
	// failure that won't be re-fenced.
	entryStates := s.oplogClassifyByLedger(ctx, req)

	// Phase 1: fencing gate, restricted to entries that still need
	// dispatch. A single mismatch fails the batch with 409 and a
	// reject_reason naming the offending entry. Already-applied or
	// pre-failed entries skip the gate.
	if reject := s.oplogValidateBatch(ctx, req, entryStates); reject != "" {
		writeJSONResponse(w, http.StatusConflict, oplogFlushResponse{
			Rejected:     true,
			RejectReason: reject,
		})
		return
	}

	// Phase 2: dispatch. Already-applied entries return their saved
	// etag without a second write; needs-dispatch entries call the
	// store helper with an Idempotency tag so the ledger insert
	// lands in the same tx as the write.
	results := s.oplogDispatchBatch(ctx, req, entryStates)
	writeJSONResponse(w, http.StatusOK, oplogFlushResponse{
		Rejected: false,
		Results:  results,
	})
}

// parseOplogFlushBody enforces the request body cap, parses JSON
// with DisallowUnknownFields (so a typo in the wire shape surfaces
// rather than silently being ignored), and validates every static
// constraint that doesn't need the DB. On any failure the response
// has already been written; the caller returns without further
// processing.
func parseOplogFlushBody(w http.ResponseWriter, r *http.Request) (*oplogFlushRequest, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, oplogFlushMaxBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large")
			return nil, err
		}
		writeError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
		return nil, err
	}
	var req oplogFlushRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid json: "+err.Error())
		return nil, err
	}
	// Trailing-data check: a second Decode must return io.EOF.
	// Without this, a peer that double-encodes the batch (a
	// frequent bug when wrapping JSON-Lines into a single body)
	// would have its first object silently honoured and the rest
	// discarded.
	var trail json.RawMessage
	if err := dec.Decode(&trail); err != io.EOF {
		writeError(w, http.StatusBadRequest, "bad_request", "request body has trailing data")
		return nil, errors.New("trailing data")
	}
	if req.PeerID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "peer_id required")
		return nil, errors.New("peer_id required")
	}
	if len(req.PeerID) > oplogPeerIDMaxLen {
		writeError(w, http.StatusBadRequest, "bad_request", "peer_id too long")
		return nil, errors.New("peer_id too long")
	}
	if !isPrintableASCII(req.PeerID) {
		writeError(w, http.StatusBadRequest, "bad_request", "peer_id has control characters")
		return nil, errors.New("peer_id bad chars")
	}
	if len(req.Entries) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "entries required")
		return nil, errors.New("entries required")
	}
	if len(req.Entries) > oplogFlushMaxEntries {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("too many entries (%d > %d)", len(req.Entries), oplogFlushMaxEntries))
		return nil, errors.New("entries cap")
	}
	// Per-entry static gate. Anything that fails here is a
	// malformed peer client; we surface 400 with the offending
	// op_id so the bug is locatable.
	// op_id-monotonic check: docs §3.13.1 step 5.2 says "op_id 順
	// に Hub の write API を idempotency-key 付きで叩く". UUIDv7
	// sorts naturally by time-component, so we accept any
	// lexicographically non-decreasing sequence. A peer that
	// shuffled entries (e.g. parallel goroutines without a
	// rendez-vous) surfaces here as a 400 instead of being
	// silently applied out of order — which would matter for
	// agent_messages.insert (the per-agent seq column is allocated
	// at Hub-write time, not from the op-log).
	seenOpIDs := make(map[string]struct{}, len(req.Entries))
	prevOpID := ""
	for i, ent := range req.Entries {
		if ent == nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("entries[%d] null", i))
			return nil, errors.New("null entry")
		}
		if ent.OpID == "" || !isPrintableASCII(ent.OpID) || len(ent.OpID) > 128 {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("entries[%d] op_id invalid", i))
			return nil, errors.New("op_id invalid")
		}
		if _, dup := seenOpIDs[ent.OpID]; dup {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("entries[%d] duplicate op_id %q", i, ent.OpID))
			return nil, errors.New("duplicate op_id")
		}
		seenOpIDs[ent.OpID] = struct{}{}
		if prevOpID != "" && ent.OpID < prevOpID {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("entries[%d] op_id %q < previous %q (batch must be lexicographically non-decreasing)",
					i, ent.OpID, prevOpID))
			return nil, errors.New("op_id order")
		}
		prevOpID = ent.OpID
		if ent.AgentID == "" {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("entries[%d] agent_id required", i))
			return nil, errors.New("agent_id required")
		}
		if ent.FencingToken <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("entries[%d] fencing_token must be > 0", i))
			return nil, errors.New("fencing_token")
		}
		if ent.Table == "" || ent.Op == "" {
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("entries[%d] table+op required", i))
			return nil, errors.New("table/op")
		}
	}
	return &req, nil
}

// entryState classifies a single batch entry after the ledger probe.
type entryState int

const (
	// stateNeedsDispatch — no prior ledger row; gate by fencing
	// and run the dispatch.
	stateNeedsDispatch entryState = iota
	// stateAlreadyApplied — ledger has a row whose fingerprint
	// matches; return saved etag, skip fencing, skip dispatch.
	stateAlreadyApplied
	// stateLedgerError — probe surfaced a per-entry error (op_id
	// reuse with different fingerprint, or DB error). The entry
	// short-circuits to error in dispatch; fencing skips it.
	stateLedgerError
)

// oplogEntryState pairs a state with the data needed by phase 2.
type oplogEntryState struct {
	state       entryState
	priorRec    *store.OplogAppliedRecord // populated for stateAlreadyApplied
	probeError  string                     // populated for stateLedgerError
	fingerprint string                     // sha256(table, op, body) — used by all phases
}

// oplogClassifyByLedger probes the ledger for every entry and
// records its classification. The fingerprint is computed once and
// re-used by phase 2 (passed to the store helper as the
// IdempotencyTag.Fingerprint).
func (s *Server) oplogClassifyByLedger(ctx context.Context, req *oplogFlushRequest) []oplogEntryState {
	out := make([]oplogEntryState, len(req.Entries))
	for i, ent := range req.Entries {
		fp := computeEntryFingerprint(ent)
		out[i].fingerprint = fp
		prior, err := s.agents.Store().GetOplogApplied(ctx, ent.OpID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			out[i].state = stateNeedsDispatch
		case err != nil:
			out[i].state = stateLedgerError
			out[i].probeError = "ledger probe: " + err.Error()
		case prior.AgentID != ent.AgentID || prior.Fingerprint != fp:
			out[i].state = stateLedgerError
			out[i].probeError = fmt.Sprintf("op_id %s already applied with different agent/fingerprint (peer-side bug)", ent.OpID)
		default:
			out[i].state = stateAlreadyApplied
			out[i].priorRec = prior
		}
	}
	return out
}

// oplogValidateBatch returns "" if every needs-dispatch entry's
// fencing matches the current agent_locks row for its agent. On
// the first mismatch it returns a human-readable reject reason
// (the WHOLE batch is then refused per docs §3.13.1).
//
// Already-applied entries skip the fencing gate — their write
// has been committed long ago and the peer's retry must succeed
// even after a lock rotation. Ledger-error entries also skip the
// fencing gate (they'll surface their probe error in phase 2).
//
// The check uses CheckFencing (not CheckFencingTx) at this layer
// because the dispatch path re-checks fencing inside its own tx
// via FencingPredicate. The convenience wrapper's TOCTOU window
// is closed by the in-tx re-check.
func (s *Server) oplogValidateBatch(ctx context.Context, req *oplogFlushRequest, states []oplogEntryState) string {
	for i, ent := range req.Entries {
		if states[i].state != stateNeedsDispatch {
			continue
		}
		err := s.agents.Store().CheckFencing(ctx, ent.AgentID, req.PeerID, ent.FencingToken)
		switch {
		case err == nil:
			continue
		case errors.Is(err, store.ErrFencingMismatch):
			return fmt.Sprintf("entries[%d] (op_id=%s, agent_id=%s): fencing_token=%d does not match holder",
				i, ent.OpID, ent.AgentID, ent.FencingToken)
		case errors.Is(err, store.ErrNotFound):
			return fmt.Sprintf("entries[%d] (op_id=%s, agent_id=%s): no agent_locks row (lease expired or never acquired)",
				i, ent.OpID, ent.AgentID)
		default:
			return fmt.Sprintf("entries[%d] (op_id=%s): fencing check failed: %v", i, ent.OpID, err)
		}
	}
	return ""
}

// computeEntryFingerprint folds the entry's (table, op, body) into
// a hex sha256. 0x1f (unit separator) is the field delimiter — its
// canonical "outside ASCII printable" status keeps it from
// colliding with any byte the body itself can carry without
// escaping. Used as the ledger fingerprint so an op_id reused for
// a different write surfaces as ErrOplogOpIDReused.
func computeEntryFingerprint(ent *oplog.Entry) string {
	h := sha256.New()
	h.Write([]byte(ent.Table))
	h.Write([]byte{0x1f})
	h.Write([]byte(ent.Op))
	h.Write([]byte{0x1f})
	h.Write([]byte(ent.Body))
	return hex.EncodeToString(h.Sum(nil))
}

// oplogDispatchBatch applies each entry to its target table. Errors
// on individual entries are recorded in the per-entry result; the
// loop continues so the peer learns the success boundary.
//
// Per-entry idempotency is enforced INSIDE the store helpers via
// the IdempotencyTag option — the ledger probe + write + ledger
// insert all live in the same tx so a crash between dispatch
// commit and ledger record is impossible.
//
// Currently dispatched:
//
//   - agent_messages.insert
//   - agent_memory.update
//   - memory_entries.insert / .update
//
// Unsupported (op, table) tuples surface as per-entry errors so the
// peer can quarantine those entries; the design doc (§3.13.1)
// limits op-log to agent-runtime writes, so a non-listed
// (table, op) combination is a peer-side bug.
func (s *Server) oplogDispatchBatch(ctx context.Context, req *oplogFlushRequest, states []oplogEntryState) []oplogFlushResult {
	out := make([]oplogFlushResult, 0, len(req.Entries))
	for i, ent := range req.Entries {
		res := s.oplogDispatchOne(ctx, req.PeerID, ent, states[i])
		out = append(out, res)
	}
	return out
}

// oplogDispatchOne runs a single entry through its handler. Returns
// per-entry result; never throws.
//
// Routing by state:
//   - stateAlreadyApplied: ledger has a prior matching row; return
//     its saved etag, no second write.
//   - stateLedgerError: surface the probe error.
//   - stateNeedsDispatch: call the per-table dispatcher with an
//     IdempotencyTag (op_id + fingerprint) so the ledger record
//     lands in the SAME tx as the write — closes the crash window
//     between dispatch commit and ledger insert.
func (s *Server) oplogDispatchOne(ctx context.Context, peerID string, ent *oplog.Entry, st oplogEntryState) oplogFlushResult {
	res := oplogFlushResult{OpID: ent.OpID}
	switch st.state {
	case stateAlreadyApplied:
		res.Status = "ok"
		res.ETag = st.priorRec.ResultETag
		return res
	case stateLedgerError:
		res.Status = "error"
		res.Error = st.probeError
		return res
	}
	tag := &store.IdempotencyTag{OpID: ent.OpID, Fingerprint: st.fingerprint}
	var (
		etag string
		err  error
	)
	switch ent.Table + "." + ent.Op {
	case "agent_messages.insert":
		etag, err = s.oplogDispatchMessageInsert(ctx, peerID, ent, tag)
	case "agent_memory.update":
		etag, err = s.oplogDispatchMemoryUpdate(ctx, peerID, ent, tag)
	case "memory_entries.insert":
		etag, err = s.oplogDispatchMemoryEntryInsert(ctx, peerID, ent, tag)
	case "memory_entries.update":
		etag, err = s.oplogDispatchMemoryEntryUpdate(ctx, peerID, ent, tag)
	default:
		res.Status = "error"
		res.Error = fmt.Sprintf("unsupported (table=%q, op=%q) — op-log only carries agent-runtime writes", ent.Table, ent.Op)
		return res
	}
	if err != nil {
		res.Status = "error"
		res.Error = err.Error()
		return res
	}
	res.Status = "ok"
	res.ETag = etag
	return res
}

// oplogDispatchMessageInsert appends an agent_messages row using
// ent.OpID as the row ID so a replay against the same op_id is a
// no-op (PK conflict → treated as idempotent success; we re-read
// the existing row to return its etag).
//
// Required body shape:
//
//	{"role":"user|assistant|system|tool","content":"...",
//	 "thinking":"...","tool_uses":..., "attachments":..., "usage":...}
//
// The op-log writer (peer side) is expected to assemble the body
// directly from the agent runtime's MessageRecord serialization.
func (s *Server) oplogDispatchMessageInsert(ctx context.Context, peerID string, ent *oplog.Entry, tag *store.IdempotencyTag) (string, error) {
	var body struct {
		Role        string          `json:"role"`
		Content     string          `json:"content"`
		Thinking    string          `json:"thinking"`
		ToolUses    json.RawMessage `json:"tool_uses,omitempty"`
		Attachments json.RawMessage `json:"attachments,omitempty"`
		Usage       json.RawMessage `json:"usage,omitempty"`
	}
	if err := decodeEntryBodyStrict(ent.Body, &body); err != nil {
		return "", fmt.Errorf("decode body: %w", err)
	}
	rec := &store.MessageRecord{
		ID:          ent.OpID,
		AgentID:     ent.AgentID,
		Role:        body.Role,
		Content:     body.Content,
		Thinking:    body.Thinking,
		ToolUses:    body.ToolUses,
		Attachments: body.Attachments,
		Usage:       body.Usage,
	}
	opts := store.MessageInsertOptions{
		Now:    ent.ClientTS,
		PeerID: peerID,
		Fencing: &store.FencingPredicate{
			AgentID:      ent.AgentID,
			Peer:         peerID,
			FencingToken: ent.FencingToken,
		},
		Idempotency: tag,
	}
	out, err := s.agents.Store().AppendMessage(ctx, rec, opts)
	if err != nil {
		if errors.Is(err, store.ErrOplogOpIDReused) {
			return "", err
		}
		if isPKConflict(err) {
			// PK conflict with an Idempotency tag set is an
			// invariant violation: the in-tx ledger probe would
			// have short-circuited an exact replay before reaching
			// the INSERT. Reaching here means there is an
			// agent_messages row with this op_id but NO ledger
			// row — which can only happen if the op_id collides
			// with a message ID written outside the op-log path
			// (e.g. a legacy importer) or with another agent's
			// op_id. Either way, treating it as success would
			// silently return an unrelated row's etag; refuse
			// instead.
			return "", fmt.Errorf("op_id %s collides with an existing agent_messages row that has no ledger entry (peer-side bug)", ent.OpID)
		}
		return "", err
	}
	return out.ETag, nil
}

// oplogDispatchMemoryUpdate replaces an agent_memory row body. The
// upsert path is naturally idempotent for the COMMON case
// (re-applying the same body yields the same body_sha256 and
// therefore the same etag), but the row's UpdatedAt does advance
// each call. A peer that retries the same op_id should observe
// stable etag/body but a moving UpdatedAt — acceptable because
// agent_memory etag is content-derived and the UI keys on etag,
// not on UpdatedAt, for invalidation.
//
// Replay-idempotency for UPDATE ops is otherwise a peer-side
// responsibility per docs §3.13.1 step 5: the peer removes
// successful entries from the op-log AFTER the dispatch ack lands,
// so a crash mid-ack at worst re-applies a no-op. The Hub does NOT
// maintain an applied-op ledger keyed on op_id in v1; a future
// slice may add one if a measurable problem emerges.
func (s *Server) oplogDispatchMemoryUpdate(ctx context.Context, peerID string, ent *oplog.Entry, tag *store.IdempotencyTag) (string, error) {
	var body struct {
		Body       string  `json:"body"`
		BodySHA256 string  `json:"body_sha256,omitempty"`
		LastTxID   *string `json:"last_tx_id,omitempty"`
		IfMatch    string  `json:"if_match,omitempty"`
	}
	if err := decodeEntryBodyStrict(ent.Body, &body); err != nil {
		return "", fmt.Errorf("decode body: %w", err)
	}
	opts := store.AgentMemoryInsertOptions{
		Now:            ent.ClientTS,
		PeerID:         peerID,
		LastTxID:       body.LastTxID,
		AllowOverwrite: true,
		Fencing: &store.FencingPredicate{
			AgentID:      ent.AgentID,
			Peer:         peerID,
			FencingToken: ent.FencingToken,
		},
		Idempotency: tag,
	}
	out, err := s.agents.Store().UpsertAgentMemory(ctx, ent.AgentID, body.Body, body.IfMatch, opts)
	if err != nil {
		return "", err
	}
	return out.ETag, nil
}

// oplogDispatchMemoryEntryInsert creates a memory_entries row keyed
// on op_id. Same PK-conflict-as-idempotent posture as the message
// path.
func (s *Server) oplogDispatchMemoryEntryInsert(ctx context.Context, peerID string, ent *oplog.Entry, tag *store.IdempotencyTag) (string, error) {
	var body struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		Body string `json:"body"`
	}
	if err := decodeEntryBodyStrict(ent.Body, &body); err != nil {
		return "", fmt.Errorf("decode body: %w", err)
	}
	rec := &store.MemoryEntryRecord{
		ID:      ent.OpID,
		AgentID: ent.AgentID,
		Kind:    body.Kind,
		Name:    body.Name,
		Body:    body.Body,
	}
	opts := store.MemoryEntryInsertOptions{
		Now:    ent.ClientTS,
		PeerID: peerID,
		Fencing: &store.FencingPredicate{
			AgentID:      ent.AgentID,
			Peer:         peerID,
			FencingToken: ent.FencingToken,
		},
		Idempotency: tag,
	}
	out, err := s.agents.Store().InsertMemoryEntry(ctx, rec, opts)
	if err != nil {
		if errors.Is(err, store.ErrOplogOpIDReused) {
			return "", err
		}
		if isPKConflict(err) {
			// Same invariant as agent_messages.insert: with the
			// Idempotency tag set, the in-tx ledger probe would
			// have short-circuited a legitimate replay before
			// reaching INSERT. A PK conflict here means op_id
			// collides with a memory_entries row that has no
			// ledger entry — refuse rather than silently return
			// an unrelated row's etag.
			return "", fmt.Errorf("op_id %s collides with an existing memory_entries row that has no ledger entry (peer-side bug)", ent.OpID)
		}
		return "", err
	}
	return out.ETag, nil
}

// oplogDispatchMemoryEntryUpdate patches an existing memory_entries
// row. The peer is expected to thread the row's prior etag through
// the body's if_match field so a concurrent owner-admin update on
// the same row surfaces as a conflict and the entry is retried with
// a fresh read.
//
// CRITICAL: before dispatching, we re-read the target row and verify
// its agent_id matches ent.AgentID. Without this, an entry whose
// fencing_token is valid for agent A could carry a body.id pointing
// at agent B's memory entry — cross-agent escape via valid op-log
// auth.
func (s *Server) oplogDispatchMemoryEntryUpdate(ctx context.Context, peerID string, ent *oplog.Entry, tag *store.IdempotencyTag) (string, error) {
	var body struct {
		ID      string  `json:"id"`
		IfMatch string  `json:"if_match"`
		Kind    *string `json:"kind,omitempty"`
		Name    *string `json:"name,omitempty"`
		Body    *string `json:"body,omitempty"`
	}
	if err := decodeEntryBodyStrict(ent.Body, &body); err != nil {
		return "", fmt.Errorf("decode body: %w", err)
	}
	if body.ID == "" {
		return "", errors.New("body.id required")
	}
	// if_match is REQUIRED for update ops: without it a replay of a
	// stale partition write could overwrite a fresh owner-admin
	// edit. The peer pulls the etag at the time of the original
	// write and threads it through.
	if body.IfMatch == "" {
		return "", errors.New("body.if_match required for memory_entries.update")
	}
	// A no-op patch (every field nil) wouldn't change the row.
	// The store would return the current record as a success but
	// would NOT record an oplog_applied row — leaving a peer
	// retry with the same op_id able to repeat the no-op write.
	// Refuse the no-op up-front so the op-log payload is always
	// meaningful; the peer should not have queued an empty patch.
	if body.Kind == nil && body.Name == nil && body.Body == nil {
		return "", errors.New("memory_entries.update body must set at least one of kind/name/body")
	}
	// Authorization scope check: the row must belong to the agent
	// whose fencing_token we validated. Re-read the row before
	// patching so a body.id targeting a different agent's entry
	// is refused.
	cur, err := s.agents.Store().GetMemoryEntry(ctx, body.ID)
	if err != nil {
		return "", fmt.Errorf("scope check: %w", err)
	}
	if cur.AgentID != ent.AgentID {
		return "", fmt.Errorf("scope check: entry %s belongs to %s, fencing was for %s",
			body.ID, cur.AgentID, ent.AgentID)
	}
	// MemoryEntryPatch does not carry peer_id / now (the store
	// stamps now from NowMillis() unconditionally). The op-log
	// replay therefore loses the wall-clock of the original write —
	// the daemon's clock at replay time is recorded instead. For
	// correctness this is fine (etag is content-derived) but the
	// operator's "when did this entry change" UI shows the replay
	// instant rather than the partition-time edit. Acceptable for
	// v1; a future MemoryEntryPatch extension can add Now/PeerID
	// fields the same way MessageInsertOptions did.
	_ = peerID
	patch := store.MemoryEntryPatch{
		Kind: body.Kind,
		Name: body.Name,
		Body: body.Body,
		Fencing: &store.FencingPredicate{
			AgentID:      ent.AgentID,
			Peer:         peerID,
			FencingToken: ent.FencingToken,
		},
		Idempotency: tag,
	}
	out, err := s.agents.Store().UpdateMemoryEntry(ctx, body.ID, body.IfMatch, patch)
	if err != nil {
		return "", err
	}
	return out.ETag, nil
}

// decodeEntryBodyStrict parses an entry body with strict semantics:
// empty body fails, unknown fields fail (a peer-side typo on a
// field name silently dropped would be a foot-gun — a "content"
// →"contnet" typo on a stale binary would otherwise persist a
// message with empty content), trailing JSON values fail (a
// `{"a":1} {"b":2}` body would otherwise drop the second object
// silently). Used by every dispatcher.
func decodeEntryBodyStrict(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return errors.New("body empty")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	// Decoder.Decode reads ONE value. A subsequent Decode must
	// return io.EOF; anything else is trailing data (Decoder.More
	// alone would miss e.g. trailing whitespace + a second value
	// where the buffered scanner has already pulled past the
	// first value's closing brace).
	var trail json.RawMessage
	if err := dec.Decode(&trail); err != io.EOF {
		return errors.New("body has trailing data")
	}
	return nil
}

// isPKConflict reports whether err looks like a primary-key uniqueness
// violation. modernc.org/sqlite surfaces these as a generic error
// whose message contains "UNIQUE constraint failed"; matching the
// string is the lowest-friction way to detect the idempotent-replay
// case without leaking driver internals into store/.
func isPKConflict(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// isPrintableASCII rejects any byte outside the printable ASCII
// range (so a peer_id / op_id can't smuggle newlines or NULs into
// log output / kv keys). Mirrors the slackbot label gate's posture.
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}
