package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/store"
)

// docs/multi-device-storage.md §3.7 step 4-bis — agent metadata
// sync that lands the source peer's agents / persona / memory /
// transcript / memory_entries on the target during a device switch.
// Runs BEFORE the blob pull so target has the row state by the
// time the binary bodies arrive. The store-level helper
// (store.SyncAgentFromPeer) is one tx; the HTTP layer is just an
// authentication + decode + dispatch wrapper.
//
// Route: POST /api/v1/peers/agent-sync
//
// Auth: RolePeer (source signs with its peer identity) OR
// RoleOwner (drill / monolith path). For RolePeer, the signer's
// PeerID must equal the body's source_device_id — a registered
// peer can't push another peer's agent state at us.
//
// Body:
//
//	{
//	  "source_device_id": "<source peer's device_id>",
//	  "agent":         <AgentRecord JSON>,
//	  "persona":       <AgentPersonaRecord JSON> | null,
//	  "memory":        <AgentMemoryRecord JSON> | null,
//	  "messages":      [ <MessageRecord JSON>, ... ],
//	  "memory_entries":[ <MemoryEntryRecord JSON>, ... ],
//	  "agent_token":   "<raw $KOJO_AGENT_TOKEN>"
//	}
//
// Response (200): { "agent_id": "..." }
//
// Failure modes:
//   - 400 bad_request: malformed JSON / missing required fields /
//     caller-source identity mismatch.
//   - 403 forbidden: non-peer / non-owner principal.
//   - 500 internal: store sync error (whole tx rolled back).

// itoa is a tiny strconv.Itoa shim — keeps the handler's error
// branches readable without dragging strconv into the import list
// for a single int-to-string conversion.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// decodeSyncB64 decodes one base64 payload from the pre-DB decode
// phase of applyPeerAgentSync. On malformed input it writes the shared
// 400 "<label>[<i>]: invalid base64: <err>" body and returns ok=false
// so the caller can bail with zero database side effects.
func decodeSyncB64(w http.ResponseWriter, label string, i int, b64 string) ([]byte, bool) {
	body, derr := base64.StdEncoding.DecodeString(b64)
	if derr != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			label+"["+itoa(i)+"]: invalid base64: "+derr.Error())
		return nil, false
	}
	return body, true
}

// peerAgentSyncMaxBody caps the DECOMPRESSED payload size after
// optional Content-Encoding: gzip handling. The wire size is
// bounded separately by peerAgentSyncMaxWireBody; senders gzip
// the JSON to stay under that, but the JSON itself can be much
// larger when decompressed (real ag_f71bf5.. observed ~60 MiB).
// 128 MiB covers a thousands-of-turns agent. The switch orchestrator keeps
// all history rows and fills the remaining raw-JSON capacity with native
// session artifacts in newest-activity order.
const peerAgentSyncMaxBody = 128 << 20

// peerAgentSyncMaxWireBody caps the on-the-wire body size,
// independent of Content-Encoding. The peer-auth middleware no
// longer bounds the body, so this constant is the only ceiling
// on what the handler will pull off the socket; ≤ peerAgentSyncMaxBody
// so uncompressed bodies from the owner path stay bounded too.
//
// Pinned to peerAgentSyncMaxBody. A smaller wire cap is an
// artificial bottleneck — gzip-bomb defense already lives in the
// LimitReader around the gzip reader (caps decompressed size), so
// admitting up to 128 MiB on the wire doesn't widen the memory
// attack surface. Long-lived agents accumulate claude / grok
// session JSONLs that base64-bloat poorly (high-entropy tool_use
// payloads compress at ~1.0×), and the 32 MiB cap was rejecting
// those during kojo-switch-device with "agent_too_large".
const peerAgentSyncMaxWireBody = peerAgentSyncMaxBody

type peerAgentSyncRequest struct {
	SourceDeviceID string `json:"source_device_id"`
	// OpID is the orchestrator-minted UUID identifying this
	// particular switch attempt. The same id rides along on
	// the matching finalize / drop call so target's pending
	// state map can resolve which sync to commit or roll back.
	// Required for the two-phase protocol to be safe across
	// concurrent retries.
	OpID           string                            `json:"op_id"`
	Agent          *store.AgentRecord                `json:"agent"`
	Persona        *store.AgentPersonaRecord         `json:"persona,omitempty"`
	Memory         *store.AgentMemoryRecord          `json:"memory,omitempty"`
	Messages       []*store.MessageRecord            `json:"messages,omitempty"`
	MemoryEntries  []*store.MemoryEntryRecord        `json:"memory_entries,omitempty"`
	WorkspaceFiles []*store.AgentWorkspaceFileRecord `json:"workspace_files,omitempty"`
	Tasks          []*store.AgentTaskRecord          `json:"tasks,omitempty"`
	AgentToken     string                            `json:"agent_token,omitempty"`
	// ClaudeSessions carry the source peer's
	// ~/.claude/projects/<encoded-workdir>/*.jsonl files so
	// `claude --continue` on target finds the same conversation
	// state. Content is base64 so the JSON envelope stays text-
	// only. Empty / absent for non-claude agents.
	ClaudeSessions []claudeSessionWire `json:"claude_sessions,omitempty"`

	// GrokSession carries the grok agent's primary session state:
	// the resume pointer (`.grok/session_id`) plus every file
	// under $GROK_HOME/sessions/<encoded(absAgentDir)>/<uuid>/.
	// Target replays both so `grok --resume <uuid>` on the next
	// chat picks up the conversation under target's own AgentDir-
	// derived encoded path. Empty / absent for non-grok agents,
	// for grok agents that have never started a session, and for
	// grok agents whose stored session_id no longer resolves to a
	// directory (treated as "no state to migrate" — a fresh
	// session starts on target).
	GrokSession *grokSessionWire `json:"grok_session,omitempty"`

	// GeminiSession carries the gemini agent's OpenCode session refs
	// (main `.opencode-home/kojo/session_id` plus per-SessionKey thread
	// refs) and an `opencode export` of each session they point at.
	// Target stages the exports under .opencode-home/kojo/pending-import
	// and `opencode import`s them (re-homed to ITS agent dir) before the
	// next turn, so `opencode run -s <id>` continues the conversation.
	// Absent for non-gemini agents and gemini agents with no live
	// session; the receiver treats absence as authoritative and purges
	// target-local gemini refs. The wire key is "opencode_session" (the
	// gemini-cli era used "gemini_session" with incompatible contents,
	// so a mixed-version pair degrades to a fresh session, never to a
	// misparsed one).
	GeminiSession *geminiSessionWire `json:"opencode_session,omitempty"`

	// CodexSession carries Codex app-server thread refs plus their
	// rollout JSONLs and sqlite metadata so `thread/resume` on target
	// can continue the same conversation. Empty / absent for non-codex
	// agents and for codex agents that have not yet materialized a
	// persistent thread.
	CodexSession *codexSessionWire `json:"codex_session,omitempty"`

	// SinceMessageSeq, when > 0, signals INCREMENTAL message
	// sync: the orchestrator consulted target's /agent-sync/state
	// endpoint, learned target already has messages up to this
	// seq, and is shipping only the rows with seq > this. Target
	// upserts the supplied rows by id and does NOT delete its
	// existing transcript — those rows are the same source
	// already saw, just left in place. When 0, target falls back
	// to full-replace mode (DELETE then INSERT) for safety on
	// the first-time-switch path.
	SinceMessageSeq int64 `json:"since_message_seq,omitempty"`
	// SinceMemoryEntrySeq is reserved / NOT used as a delta
	// cursor — memory_entries supports in-place body updates +
	// soft-deletes + recreations on the same seq, so a
	// seq-cursor delta would silently miss mutations. The wire
	// field exists for diagnostics; the handler rejects nonzero
	// values to prevent a future client from prematurely opting
	// in. The actual memory_entries delta is keyed off
	// SinceMemoryEntryUpdatedAt.
	SinceMemoryEntrySeq int64 `json:"since_memory_entry_seq,omitempty"`
	// SinceMemoryEntryUpdatedAt, when > 0, signals INCREMENTAL
	// memory_entries sync: source has consulted
	// /agent-sync/state, learned target's max(updated_at), and
	// is shipping rows (including tombstones) with
	// updated_at >= this value, ordered updated_at ASC so a
	// tombstone update lands before any recreation that reused
	// the same (kind,name) slot under the alive UNIQUE index.
	// `>=` (not `>`) defends against same-millisecond mutations
	// colliding on this cursor; every row sharing the cursor
	// timestamp (one or more) gets idempotently re-shipped and
	// the receiver's ON CONFLICT(id) DO UPDATE overwrites in
	// place. When 0 the legacy full-replace path runs.
	SinceMemoryEntryUpdatedAt int64 `json:"since_memory_entry_updated_at,omitempty"`
	// NOTE: workspace files (agent_workspace_files) have no
	// incremental cursor. They are tiny per-agent singletons (≤ 2
	// rows: user.md, checkin.md) so the delta is not worth the
	// silent-data-loss risk under peer clock skew. The orchestrator
	// always full-ships; syncWorkspaceFilesTx's DELETE-then-INSERT
	// is the only mode for this surface.

	// Credentials carries the agent's stored ID/password (and TOTP)
	// rows in PLAINTEXT. credentials.db is per-peer (encrypted with a
	// peer-local AES key), so cross-peer transport has to roundtrip
	// through plaintext. The receiver re-encrypts with its own key
	// before persisting.
	//
	// Pointer (NOT bare slice) so the wire distinguishes three states:
	//   - nil pointer / field absent → source is NOT authoritative
	//     (older binary, missing credentials.key, init failure). Target
	//     LEAVES its existing rows alone. Without this gate, a downgrade
	//     on source would silently clear target's credentials.
	//   - non-nil pointer to empty slice → source IS authoritative AND
	//     has no credentials for this agent. Target clears its rows
	//     to mirror — matches the "source wins, full-replace" semantics
	//     used elsewhere in this payload.
	//   - non-nil pointer to non-empty slice → source ships these rows;
	//     target replaces wholesale.
	//
	// There is no incremental cursor: credentials are tiny per-agent
	// (typically ≤ a few rows) and an updated_at delta would risk
	// silently dropping edits across peer clock skew, same rationale
	// as workspace_files.
	Credentials *[]*agent.Credential `json:"credentials,omitempty"`
	// CustomAPIKey is the agent-scoped inference credential in plaintext.
	// A non-nil pointer is authoritative; an empty string clears the target.
	// It is separate from Credentials so internal service tokens never appear
	// in the agent-visible password vault.
	CustomAPIKey *string `json:"custom_api_key,omitempty"`

	// COMPAT: both fields below are omitempty, so only degraded /
	// lossy transfers put them on the wire. An OLD target binary
	// (DisallowUnknownFields) 400s such a payload and the switch
	// aborts cleanly pre-complete — acceptable: mixed-version
	// clusters lose only the new degraded/lossy paths, never data.
	//
	// DegradedFlushes lists the source-side flushes that were
	// skipped because the orchestrator ran in opt-in degraded
	// mode ("memory_flush", "persona_flush"). Persisted into the
	// pending entry so finalize can warn the agent via the
	// arrival prompt that memory / persona may be stale.
	DegradedFlushes []string `json:"degraded_flushes,omitempty"`
	// TransferSkips lists the session files source's transfer
	// readers dropped (oversized JSONL, unreadable codex ref, …).
	// Stamped into the agent row's settings (lastTransferSkips)
	// for the owner UI and carried into the arrival prompt.
	TransferSkips []agent.SkippedSessionFile `json:"transfer_skips,omitempty"`
}

// agentRecordTool extracts the agent's backend CLI name from an
// AgentRecord. Tool is stored inside the dynamic Settings map
// (`{"tool":"grok"}`); this helper centralises the cast for source-side
// session selection. It returns "" when Settings is nil or malformed.
func agentRecordTool(rec *store.AgentRecord) string {
	if rec == nil || rec.Settings == nil {
		return ""
	}
	v, _ := rec.Settings["tool"].(string)
	// Rows written before the backend rename still carry "custom" /
	// "llama.cpp"; normalize so the transfer branches below match.
	return agent.NormalizeToolName(v)
}

// agentRecordUsesCodex reports whether the record's backend drives the
// codex CLI, and therefore owns codex thread / rollout state that has to
// be transferred (or purged) on a device switch. custom-codex delegates
// to the same CLI as codex, so it must not be left out.
func agentRecordUsesCodex(rec *store.AgentRecord) bool {
	switch agentRecordTool(rec) {
	case agent.ToolCodex, agent.ToolCustomCodex:
		return true
	default:
		return false
	}
}

func agentRecordCustomBaseURL(rec *store.AgentRecord) string {
	if rec == nil || rec.Settings == nil {
		return ""
	}
	v, _ := rec.Settings["customBaseURL"].(string)
	return v
}

// claudeSessionWire is the JSON shape of one transferred JSONL
// file. SessionID is the filename without the .jsonl suffix
// (claude's per-conversation UUID).
type claudeSessionWire struct {
	SessionID  string `json:"session_id"`
	ContentB64 string `json:"content_b64"`
}

// grokSessionWire is the JSON shape of a transferred grok session.
// SessionID identifies the conversation (UUIDv7); Files carry every
// regular file under source's
// $GROK_HOME/sessions/<encoded(absAgentDir)>/<SessionID>/ subtree.
// Each entry's RelPath is the slash-separated path RELATIVE to that
// subtree (e.g. "events.jsonl", "terminal/call-…-1.log"). Target
// replays the subtree under its OWN encoded-agentDir path so the
// next `grok --resume <SessionID>` (issued by backend_grok.go) finds
// the conversation.
type grokSessionWire struct {
	SessionID string                `json:"session_id"`
	Files     []grokSessionFileWire `json:"files,omitempty"`
}

// grokSessionFileWire is one file inside grokSessionWire.Files.
// ContentB64 is base64 so the JSON envelope stays text-only — grok
// session files include both JSONL text and small JSON blobs today,
// but base64-everywhere insulates us from a future grok release
// adding binary outputs (e.g. captured images) to the subtree.
type grokSessionFileWire struct {
	RelPath    string `json:"rel_path"`
	ContentB64 string `json:"content_b64"`
}

// geminiSessionWire is the JSON shape of a transferred gemini (OpenCode)
// session set. Refs name the resume pointers (Name "" = main ref,
// "key-<uuid>" = per-SessionKey thread ref); Files carry one
// `opencode export` JSON per referenced session, named "<sessionID>.json".
// Content is base64 so the envelope stays text-only.
type geminiSessionWire struct {
	Refs  []geminiSessionRefWire  `json:"refs,omitempty"`
	Files []geminiSessionFileWire `json:"files,omitempty"`
}

type geminiSessionRefWire struct {
	Name      string `json:"name,omitempty"`
	SessionID string `json:"session_id"`
}

type geminiSessionFileWire struct {
	RelPath    string `json:"rel_path"`
	ContentB64 string `json:"content_b64"`
}

type codexSessionWire struct {
	Threads []codexThreadWire `json:"threads,omitempty"`
}

type codexThreadWire struct {
	Goal              *agent.GoalBinding       `json:"goal,omitempty"`
	NativeGoal        *agent.CodexGoalTransfer `json:"native_goal,omitempty"`
	RefName           string                   `json:"ref_name"`
	ThreadID          string                   `json:"thread_id"`
	RolloutRelPath    string                   `json:"rollout_rel_path"`
	RolloutContentB64 string                   `json:"rollout_content_b64"`
	ThreadRow         *agent.CodexSQLiteRow    `json:"thread_row,omitempty"`
	DynamicToolRows   []agent.CodexSQLiteRow   `json:"dynamic_tool_rows,omitempty"`
}

type peerAgentSyncResponse struct {
	AgentID string `json:"agent_id"`
}

func (s *Server) handlePeerAgentSync(w http.ResponseWriter, r *http.Request) {
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
	var req peerAgentSyncRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"invalid json: "+err.Error())
		return
	}
	if !s.validatePeerAgentSyncRequest(w, r, &req, p) {
		return
	}
	s.applyPeerAgentSync(w, r, &req)
}

// agentSyncReadErrKind classifies readAgentSyncWireBody failures so
// chunk-handler callers can decide whether to poison the pending
// entry. A terminal kind (kind=size) means the orchestrator shipped
// a payload that can never fit target's caps; any follow-up commit
// against the same op_id would apply a stale prefix, so the entry
// must be dropped. Recoverable kinds (gzip / network) leave the
// accumulator intact so the orchestrator can retry the same seq.
type agentSyncReadErrKind int

const (
	agentSyncReadErrNone     agentSyncReadErrKind = iota
	agentSyncReadErrTerminal                      // size cap busted; caller should poison
	agentSyncReadErrRecover                       // decode / network failure; caller may retry
)

// readAgentSyncWireBody pulls the (optionally gzipped) request body off
// the wire honouring peerAgentSyncMaxWireBody / peerAgentSyncMaxBody.
// Returns the decompressed bytes plus an error classifier. Writes an
// appropriate HTTP error response on failure; the caller still has to
// consult `kind` to decide on side-effects (e.g. poisoning a pending
// chunked-sync entry).
//
// Wire cap and decompressed cap are equal (both peerAgentSyncMaxBody).
// To keep peak memory bounded by ONE cap (not wire + decoded
// simultaneously), gzip-encoded bodies are streamed: the
// MaxBytesReader bounds the compressed stream and gzip.NewReader
// decompresses on the fly. The fully-buffered return value only holds
// the JSON the caller needs to decode. Case-insensitive
// Content-Encoding per RFC 7230.
func (s *Server) readAgentSyncWireBody(w http.ResponseWriter, r *http.Request) ([]byte, agentSyncReadErrKind) {
	limited := http.MaxBytesReader(w, r.Body, peerAgentSyncMaxWireBody)
	enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	switch enc {
	case "gzip":
		gz, gerr := gzip.NewReader(limited)
		if gerr != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				"gzip reader: "+gerr.Error())
			return nil, agentSyncReadErrRecover
		}
		decoded, derr := io.ReadAll(io.LimitReader(gz, peerAgentSyncMaxBody+1))
		if cerr := gz.Close(); cerr != nil && derr == nil {
			derr = cerr
		}
		// http.MaxBytesReader surfaces a wire-cap bust as an
		// error from io.ReadAll on the inner gzip stream. Treat
		// it as terminal — the orchestrator shipped bytes that
		// can never land on this target.
		if derr != nil {
			var mbe *http.MaxBytesError
			if errors.As(derr, &mbe) {
				writeError(w, http.StatusRequestEntityTooLarge,
					"too_large", "wire body exceeds cap: "+derr.Error())
				return nil, agentSyncReadErrTerminal
			}
			writeError(w, http.StatusBadRequest, "bad_request",
				"gzip decompress: "+derr.Error())
			return nil, agentSyncReadErrRecover
		}
		if int64(len(decoded)) > peerAgentSyncMaxBody {
			writeError(w, http.StatusRequestEntityTooLarge,
				"too_large", "decompressed body exceeds cap")
			return nil, agentSyncReadErrTerminal
		}
		return decoded, agentSyncReadErrNone
	case "":
		raw, rerr := io.ReadAll(limited)
		if rerr != nil {
			var mbe *http.MaxBytesError
			if errors.As(rerr, &mbe) {
				writeError(w, http.StatusRequestEntityTooLarge,
					"too_large", "wire body exceeds cap: "+rerr.Error())
				return nil, agentSyncReadErrTerminal
			}
			writeError(w, http.StatusBadRequest, "bad_request",
				"read body: "+rerr.Error())
			return nil, agentSyncReadErrRecover
		}
		return raw, agentSyncReadErrNone
	default:
		writeError(w, http.StatusUnsupportedMediaType, "bad_request",
			"unsupported Content-Encoding: "+enc)
		return nil, agentSyncReadErrRecover
	}
}

// validatePeerAgentSyncRequest enforces the cross-cutting invariants
// shared by single-shot and chunked-commit ingestion: source identity,
// op_id presence, agent record well-formedness, path-safe agent id,
// and the agent_locks holder gate. Returns false (and writes an error
// response) on any violation.
func (s *Server) validatePeerAgentSyncRequest(w http.ResponseWriter, r *http.Request, req *peerAgentSyncRequest, p auth.Principal) bool {
	if req.SourceDeviceID == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"source_device_id required")
		return false
	}
	if !verifySignerIsSource(p, req.SourceDeviceID) {
		writeError(w, http.StatusForbidden, "forbidden",
			"signer peer device_id does not match source_device_id")
		return false
	}
	if req.OpID == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"op_id required (orchestrator must mint a UUID per switch attempt)")
		return false
	}
	if req.Agent == nil || req.Agent.ID == "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"agent record with id required")
		return false
	}
	if req.CustomAPIKey != nil && len(*req.CustomAPIKey) > agent.CustomAPIKeyMaxBytes {
		writeError(w, http.StatusBadRequest, "bad_request", "custom_api_key exceeds size limit")
		return false
	}
	// Path-safety gate: agent.id flows into filepath.Join for the
	// portable workspace path, AgentDir, claude session JSONL paths
	// and persona/memory file lookups. An id containing path
	// separators or `..` would let a malicious peer-sync payload
	// steer those writes outside their intended subtrees, and
	// Settings["workDir"] would store the escaped path for later
	// reuse. Reject at the boundary before any of those callers run.
	if !agent.IsPathSafeAgentID(req.Agent.ID) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"agent.id contains characters not safe for on-disk paths")
		return false
	}
	if agent.AgentSealed(req.Agent.ID) {
		writeError(w, http.StatusConflict, "sealed",
			"agent data is sealed in an encrypted archive here; refusing sync")
		return false
	}
	if s.peerID != nil && req.SourceDeviceID == s.peerID.DeviceID {
		writeError(w, http.StatusBadRequest, "bad_request",
			"source_device_id must not equal the local peer")
		return false
	}
	// Never accept a remote snapshot over a local ownership claim. A
	// different remote shadow needs delegation evidence; applyPeerAgentSync
	// resolves and fences it before any snapshot/filesystem writes. Chunked
	// staging may proceed, but commit uses that same authoritative gate.
	existingLock, lerr := s.agents.Store().GetAgentLock(r.Context(), req.Agent.ID)
	if lerr != nil && !errors.Is(lerr, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "internal",
			"lookup agent lock: "+lerr.Error())
		return false
	}
	if existingLock != nil && existingLock.HolderPeer != req.SourceDeviceID && (s.peerID == nil || existingLock.HolderPeer == s.peerID.DeviceID) {
		writeError(w, http.StatusConflict, "wrong_holder",
			"agent_locks.holder_peer does not match source_device_id; refusing sync")
		return false
	}
	return true
}

// applyPeerAgentSync runs the DB / disk / sessions / credentials
// pipeline shared by handlePeerAgentSync (single-shot) and
// handlePeerAgentSyncChunkedCommit (chunked). The caller is responsible
// for validating req via validatePeerAgentSyncRequest first.
func (s *Server) applyPeerAgentSync(w http.ResponseWriter, r *http.Request, req *peerAgentSyncRequest) {
	// Serialize receiver lifecycle changes, including force-reclaim, across
	// validation and all phase-1 side effects (single-shot AND chunked paths).
	unlock := s.lockPendingFinalize(pendingSyncKey{AgentID: req.Agent.ID})
	defer unlock()
	if !s.validatePeerAgentSyncRequest(w, r, req, auth.FromContext(r.Context())) {
		return
	}
	var incoming *store.IncomingHandoff
	if s.peerID != nil {
		prior, err := s.agents.Store().GetIncomingHandoff(r.Context(), req.Agent.ID, req.OpID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeError(w, 500, "internal", err.Error())
			return
		}
		if prior != nil && (prior.Phase != "prepared" || prior.SourcePeer != req.SourceDeviceID) {
			writeError(w, 409, "stale_handoff", "operation already accepted or retired; use a new handoff")
			return
		}
		version, delegatedBy, err := s.authorizeIncomingSource(r.Context(), req.Agent.ID, req.SourceDeviceID)
		if err != nil {
			writeError(w, 409, "wrong_holder", err.Error())
			return
		}
		incoming = &store.IncomingHandoff{AgentID: req.Agent.ID, OpID: req.OpID, SourcePeer: req.SourceDeviceID, TargetPeer: s.peerID.DeviceID, Expected: version, DelegatedBy: delegatedBy}
		if prior != nil && prior.Expected != version {
			writeError(w, 409, "stale_handoff", "ownership changed since phase-1")
			return
		}
		if err := s.agents.Store().ValidateIncomingHandoff(r.Context(), incoming); err != nil {
			if errors.Is(err, store.ErrStaleHandoff) {
				writeError(w, 409, "stale_handoff", "operation cancelled, superseded, or another handoff is pending")
			} else {
				writeError(w, 500, "internal", err.Error())
			}
			return
		}
	}
	// Cross-platform workDir: the user-facing Settings.workDir
	// (peer-local per docs §3.8) is rewritten to a portable
	// default so a /Users/alice/... path from a macOS source
	// doesn't end up on a Windows target. This is independent of
	// claude session JSONL placement — those follow AgentDir,
	// not Settings.workDir (see ClaudeBackend's cmd.Dir =
	// agentDir(agent.ID) wiring).
	targetWorkDir, werr := agent.DefaultAgentWorkDir(req.Agent.ID)
	if werr != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"resolve target workDir: "+werr.Error())
		return
	}
	if req.Agent.Settings == nil {
		req.Agent.Settings = map[string]any{}
	}
	req.Agent.Settings["workDir"] = targetWorkDir
	// MkdirAll for the portable default workDir runs AFTER the
	// base64 decode loop below so a 400 on malformed claude_sessions
	// doesn't leave a stub directory behind on target. The
	// Settings["workDir"] rewrite above is purely in-memory.

	// Pre-decode claude_sessions BEFORE the DB sync runs. A
	// malformed base64 payload caught here returns 400 with
	// zero database side effects. Without this ordering a bad
	// ContentB64 would land all the agent rows / tasks /
	// memory_entries first and then 400 — leaving target with
	// a half-applied switch and source still believing the
	// switch is pending.
	var decodedSessions []agent.ClaudeSessionFile
	if len(req.ClaudeSessions) > 0 {
		decodedSessions = make([]agent.ClaudeSessionFile, 0, len(req.ClaudeSessions))
		for i, cs := range req.ClaudeSessions {
			body, ok := decodeSyncB64(w, "claude_sessions", i, cs.ContentB64)
			if !ok {
				return
			}
			decodedSessions = append(decodedSessions, agent.ClaudeSessionFile{
				SessionID: cs.SessionID,
				Content:   body,
			})
		}
	}

	// grok session: optional. Decode here so a malformed base64
	// surfaces as 400 before any DB / disk state is touched.
	var decodedGrok *agent.GrokSessionTransfer
	if req.GrokSession != nil && len(req.GrokSession.Files) > 0 {
		decodedGrok = &agent.GrokSessionTransfer{
			SessionID: req.GrokSession.SessionID,
			Files:     make([]agent.GrokSessionFile, 0, len(req.GrokSession.Files)),
		}
		for i, gf := range req.GrokSession.Files {
			body, ok := decodeSyncB64(w, "grok_session.files", i, gf.ContentB64)
			if !ok {
				return
			}
			decodedGrok.Files = append(decodedGrok.Files, agent.GrokSessionFile{
				RelPath: gf.RelPath,
				Content: body,
			})
		}
	}

	// gemini session: optional; decode before touching disk / DB.
	var decodedGemini *agent.GeminiSessionTransfer
	if req.GeminiSession != nil && len(req.GeminiSession.Files) > 0 {
		decodedGemini = &agent.GeminiSessionTransfer{
			Refs:  make([]agent.GeminiSessionRef, 0, len(req.GeminiSession.Refs)),
			Files: make([]agent.GeminiSessionFile, 0, len(req.GeminiSession.Files)),
		}
		for _, r := range req.GeminiSession.Refs {
			decodedGemini.Refs = append(decodedGemini.Refs, agent.GeminiSessionRef{
				Name: r.Name, SessionID: r.SessionID,
			})
		}
		for i, gf := range req.GeminiSession.Files {
			body, ok := decodeSyncB64(w, "opencode_session.files", i, gf.ContentB64)
			if !ok {
				return
			}
			decodedGemini.Files = append(decodedGemini.Files, agent.GeminiSessionFile{
				RelPath: gf.RelPath,
				Content: body,
			})
		}
	}

	// codex session: optional. Decode rollout JSONLs before any DB /
	// disk state is touched so malformed base64 fails atomically.
	var decodedCodex *agent.CodexSessionTransfer
	if req.CodexSession != nil && len(req.CodexSession.Threads) > 0 {
		decodedCodex = &agent.CodexSessionTransfer{
			Threads: make([]agent.CodexThreadTransfer, 0, len(req.CodexSession.Threads)),
		}
		for i, ct := range req.CodexSession.Threads {
			body, ok := decodeSyncB64(w, "codex_session.threads", i, ct.RolloutContentB64)
			if !ok {
				return
			}
			decodedCodex.Threads = append(decodedCodex.Threads, ct.toTransfer(body))
		}
	}

	// Materialise the portable default workDir on disk so a
	// subsequent Settings save (validateUpdateConfigPure absolute
	// path check + the post-existence stat in Update) doesn't 400
	// with "workDir does not exist" on the first edit after the
	// switch. Deferred to here so the base64 decode 400 path above
	// short-circuits before we touch the filesystem.
	if merr := agent.EnsureAgentWorkspaceDirIfDefault(targetWorkDir, req.Agent.ID); merr != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"create target workDir: "+merr.Error())
		return
	}

	// Two-phase sync to make sessions + DB atomic-ish across
	// failures:
	//   1. StageClaudeSessionSnapshot writes the selected JSONLs and
	//      moves pre-existing/omitted files aside as backups.
	//      Returns commit (drop backups) and rollback (restore
	//      backups) callbacks.
	//   2. SyncAgentFromPeer runs the DB write.
	//   3. On DB success → commit() retires the backups.
	//      On DB failure → rollback() restores the prior JSONLs
	//      so target isn't left with new sessions for an agent
	//      whose DB rows never landed.
	// This eliminates the "DB failed after sessions committed"
	// hole the prior order had: abort/drop on source still
	// can't reach across to target's filesystem, but the
	// rollback callback fired inline does.
	// Always apply an authoritative Claude snapshot. A non-Claude source sends
	// no Claude artifacts, which deliberately tombstones target-local leftovers
	// from a previous backend. Otherwise switching back to Claude later could
	// resume divergent target history instead of bootstrapping from the
	// canonical Kojo transcript.
	sessionCommit, sessionRollback, serr := agent.StageClaudeSessionSnapshot(req.Agent.ID, decodedSessions)
	if serr != nil {
		s.logger.Error("peer agent-sync: claude session stage failed",
			"agent", req.Agent.ID, "err", serr)
		writeError(w, http.StatusInternalServerError, "internal",
			"claude session stage: "+serr.Error())
		return
	}

	// grok session: stage in the same two-phase fashion as claude.
	// On stage failure we roll back the already-staged claude
	// files so target is not left with a partial cross-backend
	// switch. On DB failure later we roll back BOTH; on success
	// we commit BOTH.
	//
	// Tombstone branch: no GrokSession means either the active Grok backend has
	// no session yet, or Grok is inactive. In both cases target-local Grok state
	// is non-authoritative and must be purged; otherwise a later backend switch
	// could resume divergent history.
	var grokCommit, grokRollback func()
	var gserr error
	if decodedGrok == nil {
		grokCommit, grokRollback, gserr = agent.StageGrokSessionCleanup(req.Agent.ID)
	} else {
		grokCommit, grokRollback, gserr = agent.StageGrokSession(req.Agent.ID, decodedGrok)
	}
	if gserr != nil {
		if sessionRollback != nil {
			sessionRollback()
		}
		s.logger.Error("peer agent-sync: grok session stage failed",
			"agent", req.Agent.ID, "err", gserr)
		writeError(w, http.StatusInternalServerError, "internal",
			"grok session stage: "+gserr.Error())
		return
	}

	// Codex follows the same authoritative-snapshot rule. Absence means either
	// no active Codex session or an inactive backend, so stale target refs are
	// removed and any future Codex turn uses canonical-history fallback.
	var codexCommit, codexRollback func()
	var cserr error
	if decodedCodex == nil {
		codexCommit, codexRollback, cserr = agent.StageCodexSessionCleanup(req.Agent.ID)
	} else {
		codexCommit, codexRollback, cserr = agent.StageCodexSession(req.Agent.ID, decodedCodex)
	}
	if cserr != nil {
		if sessionRollback != nil {
			sessionRollback()
		}
		if grokRollback != nil {
			grokRollback()
		}
		s.logger.Error("peer agent-sync: codex session stage failed",
			"agent", req.Agent.ID, "err", cserr)
		writeError(w, http.StatusInternalServerError, "internal",
			"codex session stage: "+cserr.Error())
		return
	}

	// Gemini follows the same authoritative-snapshot rule: absence purges
	// target-local refs and chat logs so a later gemini turn starts fresh
	// (with the canonical-history fallback) instead of resuming divergent
	// target history.
	var geminiCommit, geminiRollback func()
	var gmerr error
	if decodedGemini == nil {
		geminiCommit, geminiRollback, gmerr = agent.StageGeminiSessionCleanup(req.Agent.ID)
	} else {
		geminiCommit, geminiRollback, gmerr = agent.StageGeminiSession(req.Agent.ID, decodedGemini)
	}
	if gmerr != nil {
		if sessionRollback != nil {
			sessionRollback()
		}
		if grokRollback != nil {
			grokRollback()
		}
		if codexRollback != nil {
			codexRollback()
		}
		s.logger.Error("peer agent-sync: gemini session stage failed",
			"agent", req.Agent.ID, "err", gmerr)
		writeError(w, http.StatusInternalServerError, "internal",
			"gemini session stage: "+gmerr.Error())
		return
	}

	// Incremental message sync: SinceMessageSeq > 0 means source
	// has consulted /agent-sync/state, confirmed source's
	// transcript is append-only (no tombstones, no edits), and
	// is shipping rows with seq > target's max. When 0 the
	// legacy full-replace path runs (first-time switch /
	// edited-source downgrade / legacy caller).
	//
	// Incremental memory_entries sync: SinceMemoryEntryUpdatedAt
	// > 0 means source has filtered by updated_at AND included
	// tombstones AND ordered updated_at ASC so the row order
	// resolves the alive UNIQUE index correctly. Target upserts
	// by id, leaving rows outside the delta intact.
	//
	// SinceMemoryEntrySeq is rejected if > 0: that field was a
	// dead-end early-draft cursor that would silently miss
	// in-place mutations. A client that sets it is buggy and we
	// fail loud rather than corrupt target's state.
	//
	// A pending full-sync marker (written when another device
	// force-reclaimed this agent away from us) means our rows diverged
	// from the source's history; a delta merge would drop rows or
	// collide on the memory natural-key index. The state probe already
	// answers Known=false in that case, so an incremental payload here
	// means the marker landed after the probe — refuse it (fail closed
	// on lookup errors) and let the orchestrator retry with a full sync.
	fullSyncRequired := false
	if req.SinceMessageSeq > 0 || req.SinceMemoryEntryUpdatedAt > 0 {
		marked, merr := s.agents.Store().HasAgentFullSyncMarker(r.Context(), req.Agent.ID)
		fullSyncRequired = merr != nil || marked
	}
	if req.SinceMemoryEntrySeq > 0 || fullSyncRequired {
		if sessionRollback != nil {
			sessionRollback()
		}
		if grokRollback != nil {
			grokRollback()
		}
		if codexRollback != nil {
			codexRollback()
		}
		if geminiRollback != nil {
			geminiRollback()
		}
		if fullSyncRequired {
			writeError(w, http.StatusConflict, "full_sync_required",
				"target history diverged after a force-reclaim; retry the switch so it ships a full snapshot")
			return
		}
		writeError(w, http.StatusBadRequest, "unsupported",
			"since_memory_entry_seq is not a valid delta cursor; use since_memory_entry_updated_at")
		return
	}

	incrementalMessages := req.SinceMessageSeq > 0
	incrementalMemoryEntries := req.SinceMemoryEntryUpdatedAt > 0

	// Serialize settings owned by the receiving peer against their mutation
	// endpoints. This protects the owner-facing transfer-warning
	// acknowledgement and the leftover built-in Slack settings. Keep the
	// established LockPatch -> memorySync order used by the settings/persona
	// paths.
	releasePatch := s.agents.LockPatch(req.Agent.ID)
	// Preserve the repository-wide LockPatch -> memorySync order used by
	// memory/persona writes. Hold memorySyncMu across BOTH the DB write and
	// disk materialize so prepareChat cannot scan stale disk between them and
	// overwrite the freshly synced rows.
	releaseMemSync := agent.LockAgentMemorySync(req.Agent.ID)
	// Re-check under the locks: an Archive that sealed after the
	// pre-validation must not have its DB rows replaced.
	if agent.AgentSealed(req.Agent.ID) {
		releasePatch()
		releaseMemSync()
		if sessionRollback != nil {
			sessionRollback()
		}
		if grokRollback != nil {
			grokRollback()
		}
		if codexRollback != nil {
			codexRollback()
		}
		if geminiRollback != nil {
			geminiRollback()
		}
		writeError(w, http.StatusConflict, "sealed",
			"agent data is sealed in an encrypted archive here; refusing sync")
		return
	}
	if err := s.applyReceiverOwnedSettingsToSyncRecord(r.Context(), req.Agent, req.OpID, req.TransferSkips); err != nil {
		releasePatch()
		releaseMemSync()
		// Restore the staged session snapshots: their stage held the
		// per-agent transfer locks, which only commit/rollback release.
		if sessionRollback != nil {
			sessionRollback()
		}
		if grokRollback != nil {
			grokRollback()
		}
		if codexRollback != nil {
			codexRollback()
		}
		if geminiRollback != nil {
			geminiRollback()
		}
		writeError(w, http.StatusInternalServerError, "internal",
			"merge receiver-owned settings: "+err.Error())
		return
	}

	if err := s.agents.Store().SyncAgentFromPeer(r.Context(), store.AgentSyncPayload{
		IncomingHandoff: incoming,
		Agent:           req.Agent,
		Persona:         req.Persona,
		Memory:          req.Memory,
		Messages:        req.Messages,
		MemoryEntries:   req.MemoryEntries,
		// Workspace files: always full-replace. See the
		// peerAgentSyncRequest doc-comment for the rationale.
		WorkspaceFiles:           req.WorkspaceFiles,
		Tasks:                    req.Tasks,
		IncrementalMessages:      incrementalMessages,
		IncrementalMemoryEntries: incrementalMemoryEntries,
	}); err != nil {
		releasePatch()
		releaseMemSync()
		if sessionRollback != nil {
			sessionRollback()
		}
		if grokRollback != nil {
			grokRollback()
		}
		if codexRollback != nil {
			codexRollback()
		}
		if geminiRollback != nil {
			geminiRollback()
		}
		s.logger.Error("peer agent-sync: store apply failed; rolled back session files",
			"agent", req.Agent.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal",
			"sync apply: "+err.Error())
		return
	}
	releasePatch()
	if sessionCommit != nil {
		sessionCommit()
	}
	if grokCommit != nil {
		grokCommit()
	}
	if codexCommit != nil {
		codexCommit()
	}
	if geminiCommit != nil {
		geminiCommit()
	}

	// Hub-local settings overrides (internal/agent/hub_local_update.go):
	// settings PATCHed on this peer while the source held the agent
	// were acknowledged with 200 and must survive the source-wins row
	// ingest above. Per-field 3-way merge: fields the source never
	// touched get the local edit re-applied; fields the source changed
	// later keep the source value (logged, never silent). A re-apply
	// failure keeps the kv row and returns 500 so the idempotent sync
	// request is retried rather than completing with a lost edit.
	// No-op on peers with no pending override row.
	appliedOv, droppedOv, ovErr := s.agents.ReapplyHubLocalOverrides(r.Context(), req.Agent.ID)
	if ovErr != nil {
		releaseMemSync()
		s.logger.Error("peer agent-sync: hub-local override re-apply failed",
			"agent", req.Agent.ID, "err", ovErr)
		writeError(w, http.StatusInternalServerError, "internal",
			"hub-local override re-apply: "+ovErr.Error())
		return
	}
	if len(appliedOv) > 0 {
		s.logger.Info("peer agent-sync: re-applied hub-local settings edits made during remote holdership",
			"agent", req.Agent.ID, "fields", appliedOv)
	}
	if len(droppedOv) > 0 {
		s.logger.Info("peer agent-sync: dropped hub-local settings edits; source changed the same fields later",
			"agent", req.Agent.ID, "fields", droppedOv)
	}

	// Credentials sync: re-encrypt and replace target's credentials
	// for this agent. credentials.db lives in a separate SQLite file
	// with a peer-local AES key, so this can't ride inside the
	// SyncAgentFromPeer tx — it runs after the main DB commit
	// succeeded. A failure here returns 500 so the orchestrator
	// retries; ReplaceCredentials is idempotent (DELETE-then-INSERT
	// inside its own tx) so the retry lands cleanly. The retry path
	// also re-applies the main DB rows via SyncAgentFromPeer's UPSERTs,
	// keeping the two SQLite files convergent.
	//
	// req.Credentials==nil means source is NOT authoritative (legacy
	// binary, missing credentials store, init failure). Skip the
	// replace entirely — clearing target on the absence of a field
	// would let a source-side downgrade silently wipe target's rows.
	// req.Credentials!=nil but len==0 IS authoritative and clears
	// target.
	if req.Credentials != nil {
		creds := *req.Credentials
		if s.agents == nil || !s.agents.HasCredentials() {
			// Source has authority and shipped rows (or an
			// authoritative empty), but target has no store. Surface
			// loud rather than silently dropping them — operator
			// needs to see that credentials.key is missing on target
			// before the switch can be considered successful.
			// releaseMemSync MUST fire before the early return — the
			// per-agent memory-sync lock was acquired above and is
			// released after the disk reconcile on the success path;
			// leaking it here would wedge every future agent-sync
			// retry for this agentID.
			releaseMemSync()
			s.logger.Error("peer agent-sync: credentials present but target has no credential store",
				"agent", req.Agent.ID, "count", len(creds))
			writeError(w, http.StatusServiceUnavailable, "unavailable",
				"target credential store unavailable; cannot land synced credentials")
			return
		}
		if cerr := s.agents.Credentials().ReplaceCredentials(req.Agent.ID, creds); cerr != nil {
			// Same lock-leak risk as the no-store branch above.
			releaseMemSync()
			s.logger.Error("peer agent-sync: credentials replace failed",
				"agent", req.Agent.ID, "err", cerr)
			writeError(w, http.StatusInternalServerError, "internal",
				"replace credentials: "+cerr.Error())
			return
		}
	}
	if req.CustomAPIKey != nil {
		if s.agents == nil || !s.agents.HasCredentials() {
			releaseMemSync()
			writeError(w, http.StatusServiceUnavailable, "unavailable",
				"target credential store unavailable; cannot land custom API key")
			return
		}
		if cerr := agent.StoreCustomAPIKey(s.agents.Credentials(), req.Agent.ID, agentRecordCustomBaseURL(req.Agent), *req.CustomAPIKey); cerr != nil {
			releaseMemSync()
			writeError(w, http.StatusInternalServerError, "internal",
				"store custom API key: "+cerr.Error())
			return
		}
	}

	// Reconcile target's MEMORY.md + memory/* tree against the
	// authoritative post-commit DB state. Without this, target's
	// STALE local files (left over from a previous time it hosted
	// the agent) look "canonical" to the next prepareChat / Load
	// sync, which walks the disk and UPSERTs the old bodies back
	// into the DB — silently rolling back peer→hub's new state
	// (notably today's diary entries).
	//
	// Reading from DB (rather than the wire payload's delta) makes
	// the incremental path safe too: source might have shipped
	// only the changed rows, but target still needs every UNCHANGED
	// row's disk file to match its DB body — otherwise stale disk
	// for those rows triggers the same rollback bug.
	//
	// A reconcile failure here is a hard failure — leaving stale
	// disk in place would re-trigger the rollback bug on the next
	// prepareChat. The 500 lets the orchestrator retry agent-sync;
	// SyncAgentFromPeer is idempotent (UPSERT-by-id) so the DB
	// side replays cleanly.
	//
	// Use a fresh Background-rooted ctx (NOT r.Context()) so a
	// client cancel / HTTP timeout between SyncAgentFromPeer's
	// commit and the reconcile can't strand target with the DB
	// updated but disk still stale — that's the exact state the
	// reconciler exists to prevent. 60s is generous; the typical
	// agent has fewer than a thousand memory_entries and reads
	// finish in milliseconds.
	reconcileCtx, reconcileCancel := context.WithTimeout(context.Background(), 60*time.Second)
	merr := agent.ReconcileAgentDiskFromDBHeld(reconcileCtx, s.agents.Store(), req.Agent.ID, s.logger)
	reconcileCancel()
	releaseMemSync()
	if merr != nil {
		s.logger.Error("peer agent-sync: disk reconcile failed; surface 500 so orchestrator retries",
			"agent", req.Agent.ID, "err", merr)
		writeError(w, http.StatusInternalServerError, "internal",
			"reconcile disk: "+merr.Error())
		return
	}

	// Minimal post-write hook: refresh agent.Manager's in-memory
	// cache so /api/v1/agents/{id} surfaces the new row right
	// away. Token adoption + AgentLockGuard registration are
	// DEFERRED to the finalize endpoint (POST
	// /api/v1/peers/agent-sync/finalize) so an aborted switch
	// can't strand target with a valid token + lock guard for
	// an agent whose blobs never landed. The raw token is
	// stashed in pendingAgentSyncs keyed by (agent_id, op_id)
	// so a stale drop from a previous attempt can't erase the
	// fresh retry's entry.
	if err := s.recordPendingAgentSync(r.Context(), req.Agent.ID, req.OpID, pendingSyncEntry{
		SourceDeviceID: req.SourceDeviceID, IncomingFenced: incoming != nil,
		RawToken:        req.AgentToken,
		DegradedFlushes: req.DegradedFlushes,
		TransferSkips:   req.TransferSkips,
	}); err != nil {
		s.logger.Error("peer agent-sync: persist pending entry failed",
			"agent", req.Agent.ID, "op_id", req.OpID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal",
			"persist pending entry: "+err.Error())
		return
	}
	if s.onAgentSynced != nil {
		if err := s.onAgentSynced(r.Context(), req.Agent.ID); err != nil {
			s.logger.Error("peer agent-sync: in-memory reload failed",
				"agent", req.Agent.ID, "err", err)
			writeError(w, http.StatusInternalServerError, "internal",
				"in-memory reload: "+err.Error())
			return
		}
	}

	writeJSONResponse(w, http.StatusOK, peerAgentSyncResponse{AgentID: req.Agent.ID})
}

// applyReceiverOwnedSettingsToSyncRecord merges every settings field owned by
// the receiving peer, then refreshes record metadata exactly once. Combining
// transfer acknowledgement and the legacy Slack settings in one pass is important:
// a retry of the same sync must converge on the current row rather than bumping
// version/ETag once per independently merged field.
func (s *Server) applyReceiverOwnedSettingsToSyncRecord(ctx context.Context, rec *store.AgentRecord, opID string, skips []agent.SkippedSessionFile) error {
	if rec == nil {
		return errors.New("nil agent record")
	}
	if rec.Settings == nil {
		rec.Settings = make(map[string]any)
	}
	originalSettings := make(map[string]any, len(rec.Settings))
	for key, value := range rec.Settings {
		originalSettings[key] = value
	}

	current, err := s.agents.Store().GetAgent(ctx, rec.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err := mergeTransferSkipsSettings(rec.Settings, current, opID, skips); err != nil {
		return err
	}
	keepLocalLegacySlackSettings(rec.Settings, current)
	finalizeReceiverOwnedSettingsMetadata(rec, current, originalSettings)
	return nil
}

// mergeTransferSkipsSettings replaces the source snapshot's prior transfer
// warning with this switch's result. A retry of the same op_id preserves the
// receiver's acknowledgement; a new or clean transfer clears it.
func mergeTransferSkipsSettings(settings map[string]any, current *store.AgentRecord, opID string, skips []agent.SkippedSessionFile) error {
	for _, key := range []string{
		"lastTransferSkips",
		"lastTransferSkipsOpID",
		"lastTransferSkipsDismissedGeneration",
	} {
		takeSettingFold(settings, key)
	}
	if len(skips) > 0 {
		raw, err := json.Marshal(skips)
		if err != nil {
			return err
		}
		var normalized any
		if err := json.Unmarshal(raw, &normalized); err != nil {
			return err
		}
		settings["lastTransferSkips"] = normalized
		settings["lastTransferSkipsOpID"] = opID
		if current != nil {
			currentOp, _ := current.Settings["lastTransferSkipsOpID"].(string)
			currentDismissed, _ := current.Settings["lastTransferSkipsDismissedGeneration"].(string)
			if currentOp == opID && currentDismissed == opID {
				settings["lastTransferSkipsDismissedGeneration"] = opID
			}
		}
	}
	return nil
}

// keepLocalLegacySlackSettings keeps the built-in Slack bot's leftover
// settings from travelling between devices: whatever the sender put on
// the wire is dropped and the receiver's own value, if it has one, is
// kept.
//
// The settings were always private to the device the bot was configured
// on, because its tokens are in that device's credential store. That is
// what lets the Slack migration act without coordinating with peers
// (see slack_migration.go): exactly one device ever holds them, and it
// must not lose them to a sync before it has migrated them.
func keepLocalLegacySlackSettings(settings map[string]any, current *store.AgentRecord) {
	takeSettingFold(settings, "slackBot")
	if current == nil {
		return
	}
	if value, has := findSettingFold(current.Settings, "slackBot"); has {
		settings["slackBot"] = value
	}
}

func finalizeReceiverOwnedSettingsMetadata(rec, current *store.AgentRecord, originalSettings map[string]any) {
	if reflect.DeepEqual(originalSettings, rec.Settings) {
		return
	}
	maxVersion := rec.Version
	if current != nil && current.Version > maxVersion {
		maxVersion = current.Version
	}
	// Agent-sync is retried as a whole after failures in later phases. If a
	// previous attempt already persisted these merged bytes, retain its
	// metadata instead of bumping version/ETag on every retry.
	currentMetadataCoversSource := current != nil &&
		(current.Version > rec.Version ||
			current.Version == rec.Version && current.UpdatedAt >= rec.UpdatedAt)
	if currentMetadataCoversSource && sameSyncedAgentContent(rec, current) {
		rec.Seq = current.Seq
		rec.Version = current.Version
		rec.UpdatedAt = current.UpdatedAt
		rec.CreatedAt = current.CreatedAt
		rec.ETag = current.ETag
		return
	}
	rec.Version = maxVersion + 1
	mergedUpdatedAt := store.NowMillis()
	if rec.UpdatedAt > mergedUpdatedAt {
		mergedUpdatedAt = rec.UpdatedAt
	}
	if current != nil && current.UpdatedAt > mergedUpdatedAt {
		mergedUpdatedAt = current.UpdatedAt
	}
	rec.UpdatedAt = mergedUpdatedAt
	rec.ETag = "" // SyncAgentFromPeer recomputes it from the merged record.
}

func findSettingFold(settings map[string]any, wanted string) (any, bool) {
	for key, value := range settings {
		if strings.EqualFold(key, wanted) {
			return value, true
		}
	}
	return nil, false
}

func sameSyncedAgentContent(a, b *store.AgentRecord) bool {
	if a == nil || b == nil {
		return false
	}
	// SyncAgentFromPeer persists empty persona/workspace references as the
	// agent ID. Compare that canonical form so a retry of the same sync does
	// not manufacture another version merely because the wire used the
	// shorthand empty value.
	canonicalRef := func(value, agentID string) string {
		if value == "" {
			return agentID
		}
		return value
	}
	return a.ID == b.ID && a.Name == b.Name &&
		canonicalRef(a.PersonaRef, a.ID) == canonicalRef(b.PersonaRef, b.ID) &&
		canonicalRef(a.WorkspaceID, a.ID) == canonicalRef(b.WorkspaceID, b.ID) &&
		a.PeerID == b.PeerID && a.Seq == b.Seq &&
		reflect.DeepEqual(a.Settings, b.Settings) && reflect.DeepEqual(a.DeletedAt, b.DeletedAt)
}

func takeSettingFold(settings map[string]any, wanted string) (any, bool) {
	var value any
	found := false
	for key, candidate := range settings {
		if strings.EqualFold(key, wanted) {
			if !found {
				value = candidate
				found = true
			}
			delete(settings, key)
		}
	}
	return value, found
}

func codexThreadToWire(th agent.CodexThreadTransfer) codexThreadWire {
	return codexThreadWire{Goal: th.Goal, NativeGoal: th.NativeGoal, RefName: th.RefName, ThreadID: th.ThreadID, RolloutRelPath: th.RolloutRelPath, RolloutContentB64: base64.StdEncoding.EncodeToString(th.RolloutContent), ThreadRow: th.ThreadRow, DynamicToolRows: th.DynamicToolRows}
}
func (ct codexThreadWire) toTransfer(body []byte) agent.CodexThreadTransfer {
	return agent.CodexThreadTransfer{Goal: ct.Goal, NativeGoal: ct.NativeGoal, RefName: ct.RefName, ThreadID: ct.ThreadID, RolloutRelPath: ct.RolloutRelPath, RolloutContent: body, ThreadRow: ct.ThreadRow, DynamicToolRows: ct.DynamicToolRows}
}
