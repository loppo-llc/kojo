package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/store"
)

// Chunked agent-sync protocol.
//
// The single-shot endpoint (POST /api/v1/peers/agent-sync) marshals the
// entire peerAgentSyncRequest into one gzipped POST capped at
// peerAgentSyncMaxBody (128 MiB decompressed). Long-running agents with
// large transcripts / memory_entries / claude session JSONLs blow past
// that cap; without chunking the orchestrator has no path forward and
// the device-switch is permanently impossible.
//
// The chunked protocol splits the same payload into:
//
//   1. /chunked/begin — opens an op_id slot on target with the
//      singletons (agent record, persona, memory, agent_token,
//      grok session, credentials, since-cursors). Target reserves a
//      chunkedSyncEntry but does not touch the DB / disk yet.
//   2. /chunked/chunk — one or more calls appending entity rows
//      (messages, memory_entries, workspace_files, tasks,
//      claude_sessions) to the pending entry. Each chunk respects
//      peerAgentSyncMaxBody so peak memory remains bounded by one
//      chunk at a time.
//   3. /chunked/commit — target hands the accumulated request to
//      applyPeerAgentSync (same code path as the single-shot
//      handler), then drops the pending entry.
//   4. /chunked/abort — orchestrator can drop the pending entry
//      explicitly (e.g. switch was aborted upstream). Best-effort:
//      a missing entry is a 200, since the sweeper would have GC'd
//      it eventually anyway.
//
// Lifetime: the sweeper drops entries idle past chunkedSyncEntryTTL so
// an orchestrator that crashes between begin and commit doesn't pin
// target memory forever. The accumulated raw-size also caps at
// chunkedSyncMaxAccumulatedBytes; once exceeded the chunk handler
// returns 413 and the orchestrator must abort.

const (
	// chunkedSyncEntryTTL is how long an idle pending entry lives
	// before the sweeper drops it. The orchestrator typically
	// finishes a multi-chunk upload within seconds; 10 minutes
	// covers a slow inter-peer link without letting an abandoned
	// switch pin memory indefinitely.
	chunkedSyncEntryTTL = 10 * time.Minute

	// chunkedSyncSweepInterval is the cadence at which the sweeper
	// scans for expired pending entries.
	chunkedSyncSweepInterval = 1 * time.Minute

	// chunkedSyncMaxAccumulatedBytes caps the total raw (decompressed)
	// JSON across all chunks for one op_id. Defends against a runaway
	// source that ships unbounded data. 2 GiB matches what the local
	// SQLite store can chew through in one transaction without OOM on
	// a low-spec peer.
	chunkedSyncMaxAccumulatedBytes int64 = 2 << 30

	// chunkedSyncMaxChunks caps the number of /chunked/chunk calls per
	// op_id. Defends against a degenerate slicer that ships one row
	// per chunk. 4096 × peerAgentSyncMaxBody = 512 GiB of theoretical
	// headroom; in practice we run into chunkedSyncMaxAccumulatedBytes
	// first. Either limit hit returns 413.
	chunkedSyncMaxChunks = 4096
)

// chunkedSyncEntry accumulates one in-flight chunked agent-sync. The
// orchestrator opens it via /chunked/begin, appends data via
// /chunked/chunk, and drains it via /chunked/commit (or drops it via
// /chunked/abort). Idle entries past chunkedSyncEntryTTL are swept.
type chunkedSyncEntry struct {
	// sourceDeviceID pins the entry to one peer; subsequent chunk /
	// commit / abort calls MUST originate from the same peer.
	// Defends against a peer-spoofing race where peer A starts an
	// upload and peer B tries to slip rows into it.
	sourceDeviceID string

	// req is the accumulator. begin populates the singletons +
	// since-cursors + credentials/grok_session pointers. Each chunk
	// appends to the slice fields.
	req *peerAgentSyncRequest

	// nextChunkSeq is the seq number the orchestrator is expected to
	// send next. Begin initialises it to 0; a chunk that arrives with
	// a different seq is rejected (no out-of-order, no replay). The
	// orchestrator submits chunks sequentially over a single
	// connection, so strict ordering is realistic and the simplest
	// defence against a duplicate-chunk replay.
	nextChunkSeq int

	// accumulatedBytes is the running total of raw (decompressed)
	// JSON bytes accepted across all chunks. Compared against
	// chunkedSyncMaxAccumulatedBytes on each chunk arrival.
	accumulatedBytes int64

	// lastTouched is updated on every successful begin / chunk call.
	// The sweeper compares against time.Now() to identify expired
	// entries.
	lastTouched time.Time

	// committing is set to 1 (CAS) when commit starts running so a
	// concurrent abort/chunk call cannot tear the apply midway. The
	// map's entry is removed once commit returns regardless of
	// success/failure.
	committing atomic.Bool
}

// peerAgentSyncChunkedBeginRequest opens a chunked upload slot. The
// singletons mirror peerAgentSyncRequest but with the bulky array
// fields omitted — those land via /chunked/chunk.
type peerAgentSyncChunkedBeginRequest struct {
	SourceDeviceID string `json:"source_device_id"`
	OpID           string `json:"op_id"`

	Agent       *store.AgentRecord        `json:"agent"`
	Persona     *store.AgentPersonaRecord `json:"persona,omitempty"`
	Memory      *store.AgentMemoryRecord  `json:"memory,omitempty"`
	AgentToken  string                    `json:"agent_token,omitempty"`
	GrokSession *grokSessionWire          `json:"grok_session,omitempty"`
	Credentials *[]*agent.Credential      `json:"credentials,omitempty"`

	SinceMessageSeq           int64 `json:"since_message_seq,omitempty"`
	SinceMemoryEntrySeq       int64 `json:"since_memory_entry_seq,omitempty"`
	SinceMemoryEntryUpdatedAt int64 `json:"since_memory_entry_updated_at,omitempty"`

	// Degraded / skip metadata — singleton-sized, rides begin.
	DegradedFlushes []string                   `json:"degraded_flushes,omitempty"`
	TransferSkips   []agent.SkippedSessionFile `json:"transfer_skips,omitempty"`
}

// peerAgentSyncChunkedChunkRequest appends one batch of rows to a
// pending entry. Each chunk carries the op_id + seq in the URL query
// string; the body is the JSON-encoded data payload alone.
type peerAgentSyncChunkedChunkRequest struct {
	Messages       []*store.MessageRecord            `json:"messages,omitempty"`
	MemoryEntries  []*store.MemoryEntryRecord        `json:"memory_entries,omitempty"`
	WorkspaceFiles []*store.AgentWorkspaceFileRecord `json:"workspace_files,omitempty"`
	Tasks          []*store.AgentTaskRecord          `json:"tasks,omitempty"`
	ClaudeSessions []claudeSessionWire               `json:"claude_sessions,omitempty"`
	CodexThreads   []codexThreadWire                 `json:"codex_threads,omitempty"`
}

// peerAgentSyncChunkedResponse is returned by begin / chunk / commit.
// AgentID echoes the row identifier on the commit path; AccumulatedBytes
// surfaces the running total on chunk acks so the orchestrator can log
// progress.
type peerAgentSyncChunkedResponse struct {
	OpID             string `json:"op_id,omitempty"`
	AgentID          string `json:"agent_id,omitempty"`
	AccumulatedBytes int64  `json:"accumulated_bytes,omitempty"`
	NextChunkSeq     int    `json:"next_chunk_seq,omitempty"`
}

// handlePeerAgentSyncChunkedBegin opens a pending slot for one op_id.
// The body is the same wire shape as the single-shot handler minus
// the bulk data arrays. Returns 200 with the next-expected seq (= 0)
// on success; 409 if the op_id is already reserved by a still-pending
// upload (a retry with the same op_id should call /abort first).
func (s *Server) handlePeerAgentSyncChunkedBegin(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePeerOrOwner(w, r)
	if !ok {
		return
	}
	if _, ok := s.requireAgentStore(w, "agent store not configured"); !ok {
		return
	}

	body, kind := s.readAgentSyncWireBody(w, r)
	if kind != agentSyncReadErrNone {
		return
	}
	var bReq peerAgentSyncChunkedBeginRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&bReq); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid json: "+err.Error())
		return
	}

	// Materialise into the full peerAgentSyncRequest shape so the
	// same validatePeerAgentSyncRequest gate runs as the single-shot
	// path. Data arrays start empty and grow per-chunk.
	req := &peerAgentSyncRequest{
		SourceDeviceID:            bReq.SourceDeviceID,
		OpID:                      bReq.OpID,
		Agent:                     bReq.Agent,
		Persona:                   bReq.Persona,
		Memory:                    bReq.Memory,
		AgentToken:                bReq.AgentToken,
		GrokSession:               bReq.GrokSession,
		Credentials:               bReq.Credentials,
		SinceMessageSeq:           bReq.SinceMessageSeq,
		SinceMemoryEntrySeq:       bReq.SinceMemoryEntrySeq,
		SinceMemoryEntryUpdatedAt: bReq.SinceMemoryEntryUpdatedAt,
		DegradedFlushes:           bReq.DegradedFlushes,
		TransferSkips:             bReq.TransferSkips,
	}
	if !s.validatePeerAgentSyncRequest(w, r, req, p) {
		return
	}

	// Reserve the slot. Refuse if an existing entry for this op_id
	// is still pending — the orchestrator must call /abort first to
	// reclaim a stuck op_id.
	s.chunkedSyncMu.Lock()
	if _, exists := s.chunkedAgentSyncs[bReq.OpID]; exists {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusConflict, "already_pending",
			"op_id is already reserved by another chunked upload; abort first")
		return
	}
	entry := &chunkedSyncEntry{
		sourceDeviceID:   bReq.SourceDeviceID,
		req:              req,
		nextChunkSeq:     0,
		accumulatedBytes: int64(len(body)),
		lastTouched:      time.Now(),
	}
	if entry.accumulatedBytes > chunkedSyncMaxAccumulatedBytes {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			"begin body alone exceeds accumulated cap")
		return
	}
	s.chunkedAgentSyncs[bReq.OpID] = entry
	s.chunkedSyncMu.Unlock()

	s.logger.Info("chunked agent-sync: begin",
		"agent", req.Agent.ID, "op_id", bReq.OpID,
		"source", bReq.SourceDeviceID, "begin_bytes", entry.accumulatedBytes)

	writeJSONResponse(w, http.StatusOK, peerAgentSyncChunkedResponse{
		OpID:             bReq.OpID,
		AccumulatedBytes: entry.accumulatedBytes,
		NextChunkSeq:     0,
	})
}

// handlePeerAgentSyncChunkedChunk appends one batch of rows to a
// pending entry. Query parameters: op_id (required), seq (required,
// must equal nextChunkSeq), final (optional bool — if true the
// orchestrator should immediately call /chunked/commit; the chunk
// handler does not auto-commit, leaving the commit step explicit so a
// commit-time auth failure can't strand the apply).
func (s *Server) handlePeerAgentSyncChunkedChunk(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePeerOrOwner(w, r)
	if !ok {
		return
	}
	if _, ok := s.requireAgentStore(w, "agent store not configured"); !ok {
		return
	}

	opID := r.URL.Query().Get("op_id")
	if opID == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"op_id query parameter required")
		return
	}
	seqStr := r.URL.Query().Get("seq")
	if seqStr == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"seq query parameter required")
		return
	}
	seq, perr := strconv.Atoi(seqStr)
	if perr != nil || seq < 0 {
		writeError(w, http.StatusBadRequest, "bad_request",
			"seq must be a non-negative integer")
		return
	}

	// Preflight on cheap query-string + map state BEFORE touching
	// the body. A wrong-peer / replay / nonexistent-op_id request
	// gets rejected without forcing readAgentSyncWireBody through
	// 128 MiB of decode work — closes a CPU/bandwidth amplification
	// path where a malicious authenticated peer could pin target by
	// flooding /chunk for op_ids it never begun.
	//
	// preflightEntry captures the pointer observed at preflight
	// time. The append-side re-acquire compares pointer identity
	// (NOT just opID presence) so an abort + re-begin race during
	// the body read can't slip our chunk into the freshly-minted
	// successor entry — which would belong to a different peer
	// with a different source check that already passed.
	s.chunkedSyncMu.Lock()
	preflightEntry, ok := s.chunkedAgentSyncs[opID]
	if !ok {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusNotFound, "not_found",
			"no pending chunked upload for op_id (expired, never begun, or already committed)")
		return
	}
	if preflightEntry.committing.Load() {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusConflict, "committing",
			"commit is in progress; refusing further chunks")
		return
	}
	if !verifySignerIsSource(p, preflightEntry.sourceDeviceID) {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusForbidden, "forbidden",
			"signer peer device_id does not match the begin source_device_id")
		return
	}
	if seq != preflightEntry.nextChunkSeq {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusConflict, "out_of_order",
			fmt.Sprintf("chunk seq %d does not match expected %d", seq, preflightEntry.nextChunkSeq))
		return
	}
	if preflightEntry.nextChunkSeq+1 > chunkedSyncMaxChunks {
		// Terminal cap: poison the entry so a subsequent commit
		// can't apply a truncated prefix. The orchestrator must
		// restart the switch with a fresh op_id.
		delete(s.chunkedAgentSyncs, opID)
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusRequestEntityTooLarge, "too_many_chunks",
			fmt.Sprintf("chunk count would exceed cap %d; orchestrator must batch more aggressively",
				chunkedSyncMaxChunks))
		return
	}
	s.chunkedSyncMu.Unlock()

	body, kind := s.readAgentSyncWireBody(w, r)
	if kind != agentSyncReadErrNone {
		// readAgentSyncWireBody already wrote the error. A
		// terminal-kind failure (peerAgentSyncMaxBody bust) means
		// the orchestrator shipped a chunk that can never fit
		// target's caps, so any follow-up commit would apply a
		// stale prefix. Poison the entry so commit gets 404.
		// Recoverable failures (network hiccup, malformed gzip)
		// leave the entry alone — those don't grow the
		// accumulator, and the orchestrator can retry the same
		// seq.
		//
		// Pointer-identity check on the delete so an abort +
		// re-begin race during the body read doesn't drop the
		// successor entry that already passed its own preflight.
		if kind == agentSyncReadErrTerminal {
			s.chunkedSyncMu.Lock()
			if cur, stillThere := s.chunkedAgentSyncs[opID]; stillThere && cur == preflightEntry {
				delete(s.chunkedAgentSyncs, opID)
			}
			s.chunkedSyncMu.Unlock()
		}
		return
	}
	var cReq peerAgentSyncChunkedChunkRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cReq); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid json: "+err.Error())
		return
	}

	// Re-acquire the map lock to commit the append. State could
	// have changed between the preflight unlock and now (concurrent
	// commit / abort / next chunk arriving on the same op_id under
	// a buggy orchestrator). Recheck the same invariants — AND
	// compare pointer identity against preflightEntry so an
	// abort + re-begin race during the body read can't leak this
	// chunk into a successor entry (potentially owned by a
	// different peer).
	s.chunkedSyncMu.Lock()
	entry, ok := s.chunkedAgentSyncs[opID]
	if !ok {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusNotFound, "not_found",
			"pending entry vanished between preflight and append (concurrent commit/abort)")
		return
	}
	if entry != preflightEntry {
		// op_id was aborted and re-begun while we were reading
		// the body. The new entry belongs to whoever called
		// /begin; refuse the chunk so the new owner's flow stays
		// uncorrupted. Source must restart with a fresh op_id.
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusConflict, "entry_replaced",
			"pending entry was re-begun mid-flight; restart with fresh op_id")
		return
	}
	if entry.committing.Load() {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusConflict, "committing",
			"commit started between preflight and append; refusing further chunks")
		return
	}
	if !verifySignerIsSource(p, entry.sourceDeviceID) {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusForbidden, "forbidden",
			"signer peer device_id does not match the begin source_device_id (re-acquire)")
		return
	}
	if seq != entry.nextChunkSeq {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusConflict, "out_of_order",
			fmt.Sprintf("chunk seq %d does not match expected %d (raced with another chunk)",
				seq, entry.nextChunkSeq))
		return
	}
	newTotal := entry.accumulatedBytes + int64(len(body))
	if newTotal > chunkedSyncMaxAccumulatedBytes {
		// Terminal cap: drop the entry so a follow-up commit can
		// not silently apply a truncated payload. Orchestrator
		// MUST restart with a fresh op_id.
		delete(s.chunkedAgentSyncs, opID)
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("accumulated bytes %d would exceed cap %d; entry dropped, restart with fresh op_id",
				newTotal, chunkedSyncMaxAccumulatedBytes))
		return
	}

	// Append the chunk's arrays to the accumulator. The full
	// validation (path-safe agent.id, holder check) ran at begin;
	// per-chunk we trust the singletons and only grow the data.
	entry.req.Messages = append(entry.req.Messages, cReq.Messages...)
	entry.req.MemoryEntries = append(entry.req.MemoryEntries, cReq.MemoryEntries...)
	entry.req.WorkspaceFiles = append(entry.req.WorkspaceFiles, cReq.WorkspaceFiles...)
	entry.req.Tasks = append(entry.req.Tasks, cReq.Tasks...)
	entry.req.ClaudeSessions = append(entry.req.ClaudeSessions, cReq.ClaudeSessions...)
	if len(cReq.CodexThreads) > 0 {
		if entry.req.CodexSession == nil {
			entry.req.CodexSession = &codexSessionWire{}
		}
		entry.req.CodexSession.Threads = append(entry.req.CodexSession.Threads, cReq.CodexThreads...)
	}
	entry.accumulatedBytes = newTotal
	entry.nextChunkSeq++
	entry.lastTouched = time.Now()
	nextSeq := entry.nextChunkSeq
	s.chunkedSyncMu.Unlock()

	s.logger.Debug("chunked agent-sync: chunk accepted",
		"op_id", opID, "seq", seq, "chunk_bytes", len(body),
		"accumulated", newTotal, "next_seq", nextSeq)

	writeJSONResponse(w, http.StatusOK, peerAgentSyncChunkedResponse{
		OpID:             opID,
		AccumulatedBytes: newTotal,
		NextChunkSeq:     nextSeq,
	})
}

// handlePeerAgentSyncChunkedCommit drains the pending entry and runs
// applyPeerAgentSync against the accumulated payload. The pending
// entry is removed from the map BEFORE the apply runs so a retry
// from the orchestrator (same op_id) is rejected with 404 instead of
// double-applying; a failed apply leaves the orchestrator with no
// retry path on target and must restart the switch with a fresh
// op_id (matching the single-shot handler's idempotency posture —
// pendingAgentSyncs already keys finalize / drop off (agent_id,
// op_id)).
func (s *Server) handlePeerAgentSyncChunkedCommit(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePeerOrOwner(w, r)
	if !ok {
		return
	}
	if _, ok := s.requireAgentStore(w, "agent store not configured"); !ok {
		return
	}

	opID := r.URL.Query().Get("op_id")
	if opID == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"op_id query parameter required")
		return
	}

	s.chunkedSyncMu.Lock()
	entry, ok := s.chunkedAgentSyncs[opID]
	if !ok {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusNotFound, "not_found",
			"no pending chunked upload for op_id")
		return
	}
	if !verifySignerIsSource(p, entry.sourceDeviceID) {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusForbidden, "forbidden",
			"signer peer device_id does not match the begin source_device_id")
		return
	}
	// CAS the committing flag so a concurrent chunk call (rare —
	// the orchestrator should serialise) sees committing=true and
	// gets 409, while abort sees the same flag and bails out.
	if !entry.committing.CompareAndSwap(false, true) {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusConflict, "committing",
			"commit is already running for this op_id")
		return
	}
	// Pop the entry before releasing the map lock so concurrent
	// abort can't double-free, but keep a reference to the
	// in-progress apply via `entry`. A late chunk that races in
	// between this delete and the response will get 404, which is
	// the right answer — its data wouldn't have made it into the
	// apply anyway.
	delete(s.chunkedAgentSyncs, opID)
	s.chunkedSyncMu.Unlock()

	s.logger.Info("chunked agent-sync: commit",
		"agent", entry.req.Agent.ID, "op_id", opID,
		"messages", len(entry.req.Messages),
		"memory_entries", len(entry.req.MemoryEntries),
		"workspace_files", len(entry.req.WorkspaceFiles),
		"tasks", len(entry.req.Tasks),
		"claude_sessions", len(entry.req.ClaudeSessions),
		"codex_threads", func() int {
			if entry.req.CodexSession == nil {
				return 0
			}
			return len(entry.req.CodexSession.Threads)
		}(),
		"accumulated_bytes", entry.accumulatedBytes)

	// applyPeerAgentSync re-runs the request through the same
	// staging / DB / disk / credentials pipeline as the single-shot
	// handler. Validation already ran at begin time; we re-validate
	// the holder lock here in case agent_locks rotated mid-upload
	// (rare but possible — defends against a peer that won begin
	// but lost the lock to a force-reclaim before committing).
	if !s.validatePeerAgentSyncRequest(w, r, entry.req, p) {
		return
	}
	s.applyPeerAgentSync(w, r, entry.req)
}

// handlePeerAgentSyncChunkedAbort drops a pending entry. Idempotent:
// a missing entry returns 200, since the sweeper would have GC'd it
// anyway. Used by the orchestrator on switch abort to free target
// memory without waiting for TTL.
func (s *Server) handlePeerAgentSyncChunkedAbort(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePeerOrOwner(w, r)
	if !ok {
		return
	}
	opID := r.URL.Query().Get("op_id")
	if opID == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"op_id query parameter required")
		return
	}

	s.chunkedSyncMu.Lock()
	entry, ok := s.chunkedAgentSyncs[opID]
	if !ok {
		s.chunkedSyncMu.Unlock()
		// Idempotent — sweeper would have collected it anyway.
		writeJSONResponse(w, http.StatusOK, peerAgentSyncChunkedResponse{OpID: opID})
		return
	}
	if !verifySignerIsSource(p, entry.sourceDeviceID) {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusForbidden, "forbidden",
			"signer peer device_id does not match the begin source_device_id")
		return
	}
	if entry.committing.Load() {
		s.chunkedSyncMu.Unlock()
		writeError(w, http.StatusConflict, "committing",
			"commit is in progress; refusing to abort")
		return
	}
	delete(s.chunkedAgentSyncs, opID)
	s.chunkedSyncMu.Unlock()

	s.logger.Info("chunked agent-sync: abort",
		"op_id", opID, "agent", entry.req.Agent.ID,
		"accumulated_bytes", entry.accumulatedBytes)

	writeJSONResponse(w, http.StatusOK, peerAgentSyncChunkedResponse{OpID: opID})
}

// runChunkedSyncSweeper periodically scans chunkedAgentSyncs for
// entries idle past chunkedSyncEntryTTL and drops them. Started by
// New(); stopped by Shutdown closing chunkedSyncSweepDone.
func (s *Server) runChunkedSyncSweeper() {
	t := time.NewTicker(chunkedSyncSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.chunkedSyncSweepDone:
			return
		case <-t.C:
			s.sweepExpiredChunkedSyncs(time.Now())
		}
	}
}

// sweepExpiredChunkedSyncs removes pending entries whose lastTouched
// is older than now - chunkedSyncEntryTTL. Tests can invoke this
// directly with a fixed `now` to assert sweep behaviour without
// waiting for the ticker.
func (s *Server) sweepExpiredChunkedSyncs(now time.Time) {
	threshold := now.Add(-chunkedSyncEntryTTL)
	s.chunkedSyncMu.Lock()
	expired := make([]string, 0)
	for opID, entry := range s.chunkedAgentSyncs {
		if entry.committing.Load() {
			continue // never sweep an in-flight commit
		}
		if entry.lastTouched.Before(threshold) {
			expired = append(expired, opID)
		}
	}
	for _, opID := range expired {
		entry := s.chunkedAgentSyncs[opID]
		delete(s.chunkedAgentSyncs, opID)
		s.logger.Info("chunked agent-sync: sweeper dropped expired entry",
			"op_id", opID, "agent", entry.req.Agent.ID,
			"idle_seconds", int(now.Sub(entry.lastTouched).Seconds()),
			"accumulated_bytes", entry.accumulatedBytes)
	}
	s.chunkedSyncMu.Unlock()
}

// ErrNoPendingChunkedSync sentinel used by orchestrator helpers when
// a /chunked/{op_id} call returns 404. Exported via type assertion on
// the dispatch error path so the source can distinguish "target lost
// our pending entry" from a generic HTTP failure.
var ErrNoPendingChunkedSync = errors.New("no pending chunked upload for op_id")
