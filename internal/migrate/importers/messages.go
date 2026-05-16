package importers

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// messagesImporter walks every <v0>/agents/<id>/messages.jsonl and
// streams the records into agent_messages. Domain key: "messages".
// Depends on the agents importer having run first (FK on agent_id).
type messagesImporter struct{}

func (messagesImporter) Domain() string { return "messages" }

// v0Message decodes one line of v0's messages.jsonl. Tool/attachment/usage
// fields are kept as raw JSON so the v1 store can persist them verbatim
// without re-encoding.
type v0Message struct {
	ID          string          `json:"id"`
	Role        string          `json:"role"`
	Content     string          `json:"content,omitempty"`
	Thinking    string          `json:"thinking,omitempty"`
	ToolUses    json.RawMessage `json:"toolUses,omitempty"`
	Attachments json.RawMessage `json:"attachments,omitempty"`
	Timestamp   string          `json:"timestamp,omitempty"`
	Usage       json.RawMessage `json:"usage,omitempty"`
}

func (messagesImporter) Run(ctx context.Context, st *store.Store, opts migrate.Options) error {
	if done, err := alreadyImported(ctx, st, "messages"); err != nil {
		return err
	} else if done {
		return nil
	}

	logger := slog.Default().With("importer", "messages")

	srcPaths, err := collectMessagesSourcePaths(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("collect source paths: %w", err)
	}
	checksum, err := domainChecksum(opts.V0Dir, srcPaths)
	if err != nil {
		return fmt.Errorf("checksum messages sources: %w", err)
	}

	agents, err := st.ListAgents(ctx)
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}

	// Cross-agent id collision detection: agent_messages.id is a
	// global PRIMARY KEY in v1, but v0 only enforced uniqueness
	// within each agent. The most common collision source is `Fork
	// --include-transcript` in v0, which copies messages.jsonl
	// line-for-line preserving every id. Pre-load the set of all
	// already-imported ids so importAgentMessages can rewrite a
	// colliding id to a fresh one before the bulk insert lands the
	// PK conflict. The set grows as we import each agent (the per-
	// agent helper updates it in place) so the second occurrence of
	// a forked id sees it as already-in-use.
	globalExisting, err := loadAllMessageIDs(ctx, st)
	if err != nil {
		return fmt.Errorf("preload global ids: %w", err)
	}

	total := 0
	for _, a := range agents {
		n, err := importAgentMessages(ctx, st, opts.V0Dir, a.ID, globalExisting, logger)
		if err != nil {
			return fmt.Errorf("agent %s: %w", a.ID, err)
		}
		total += n
	}
	return markImported(ctx, st, "messages", total, checksum)
}

// importBatchSize bounds the per-flush slice for streaming messages.jsonl
// through BulkAppendMessages by record count. Picked to cut the commit
// count for a 100k-row file by ~20× vs. per-row AppendMessage. Past
// ~10000 the SQLite writer lock starts stalling concurrent readers;
// below ~1000 most of the speedup erodes.
const importBatchSize = 5000

// importBatchBytes bounds the same buffer by raw line size so a few
// pathologically large messages (a single PDF transcript can carry
// multi-MB tool_uses payloads) can't push memory toward 1 GB while we
// wait to hit importBatchSize. 32 MiB is the same buffer ceiling we
// give the bufio.Scanner per-line, so by construction at least one
// record always fits before flushing.
const importBatchBytes = 32 << 20

// importAgentMessages streams messages.jsonl for one agent into agent_messages.
//
// Idempotency: a v0 id is unique across the agent's transcript, so we
// skip when the row already exists for THIS agent. To avoid 100k×
// GetMessage round-trips on a large transcript, the existing ids for
// this agent are loaded once up front into an in-memory set; the
// per-line check is then a map lookup.
//
// Cross-agent id collision: agent_messages.id is a global PRIMARY KEY
// in v1, but v0 only enforced uniqueness within each agent. The
// canonical collision source is `Fork --include-transcript` in v0,
// which copies messages.jsonl line-for-line preserving every id. When
// we see a v0 id that's already in `globalExisting` (some prior
// agent's import claimed it) we generate a fresh id and rewrite the
// record before insert — v0 message bodies don't carry inter-message
// references (Content is plain text, ToolUses opaque LLM tool-call
// ids in their own namespace, Attachments opaque) so the rewrite
// doesn't break anything downstream. The original id is logged at
// Warn so an operator can correlate.
//
// Inserts are funneled through BulkAppendMessages in chunks of
// importBatchSize. Each chunk is one transaction, so a 100k-row
// transcript pays ~20 commits instead of 100k. A crash mid-import only
// loses the most recent un-flushed chunk; the next run rebuilds
// `existing` from the rows that did commit and resumes from the first
// unrecorded id.
func importAgentMessages(ctx context.Context, st *store.Store, v0Dir, agentID string, globalExisting map[string]bool, logger *slog.Logger) (int, error) {
	path := filepath.Join(agentDir(v0Dir, agentID), "messages.jsonl")
	f, err := openV0(v0Dir, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	existing, err := loadExistingMessageIDs(ctx, st, agentID)
	if err != nil {
		return 0, fmt.Errorf("preload existing ids: %w", err)
	}
	// Seed globalExisting with this agent's own ids so the
	// collision detector below doesn't mistake them for foreign
	// rows (they're "ours, already imported", not "claimed by
	// someone else"). Done lazily to avoid a second query —
	// loadExistingMessageIDs already touched these rows.
	for id := range existing {
		globalExisting[id] = true
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 32<<20)

	pending := make([]*store.MessageRecord, 0, importBatchSize)
	pendingBytes := 0
	count := 0
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		// Cross-agent id collisions are caught and rewritten in the
		// per-line loop below before they reach this flush; a
		// PRIMARY KEY conflict reaching here would mean the
		// globalExisting set diverged from the on-disk row set
		// (e.g. another writer hit the table mid-import). Keeping
		// the old hint so an operator can trace it back.
		n, err := st.BulkAppendMessages(ctx, agentID, pending, store.MessageInsertOptions{})
		if err != nil {
			return fmt.Errorf("bulk append (%d records, possible cross-agent id collision): %w",
				len(pending), err)
		}
		count += n
		pending = pending[:0]
		pendingBytes = 0
		return nil
	}

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var m v0Message
		if err := json.Unmarshal(line, &m); err != nil {
			logger.Warn("messages: skipping malformed line",
				"agent", agentID, "line", lineNo, "err", err)
			continue
		}
		if m.ID == "" {
			logger.Warn("messages: skipping line without id",
				"agent", agentID, "line", lineNo)
			continue
		}
		if !validRole(m.Role) {
			logger.Warn("messages: skipping line with invalid role",
				"agent", agentID, "line", lineNo, "id", m.ID, "role", m.Role)
			continue
		}

		if existing[m.ID] {
			continue
		}

		// Cross-agent collision: the v0 id is already claimed by a
		// row this importer ran wrote (or pre-existed) under some
		// OTHER agent. Rewrite to a fresh id before insert. Logged
		// at Warn so an operator can correlate the synthetic id
		// back to the v0 source if needed.
		insertID := m.ID
		if globalExisting[m.ID] {
			fresh, gerr := generateFreshMessageID(globalExisting)
			if gerr != nil {
				return 0, fmt.Errorf("agent %s line %d: rewrite colliding id %q: %w",
					agentID, lineNo, m.ID, gerr)
			}
			logger.Warn("messages: cross-agent id collision; rewriting",
				"agent", agentID, "line", lineNo, "v0_id", m.ID, "v1_id", fresh)
			insertID = fresh
		}

		ts := parseRFC3339Millis(m.Timestamp)
		if ts == 0 {
			ts = store.NowMillis()
		}
		pending = append(pending, &store.MessageRecord{
			ID:          insertID,
			AgentID:     agentID,
			Role:        m.Role,
			Content:     m.Content,
			Thinking:    m.Thinking,
			ToolUses:    m.ToolUses,
			Attachments: m.Attachments,
			Usage:       m.Usage,
			CreatedAt:   ts,
			UpdatedAt:   ts,
		})
		// Mark seen on BOTH sets:
		//   - existing[m.ID]: per-agent dedup so a duplicate v0 id
		//     within the same messages.jsonl drops on the next
		//     occurrence (matching the per-row AppendMessage path's
		//     skip-after-insert behaviour).
		//   - globalExisting[insertID]: cross-agent collision set so
		//     the rewritten id (or the original, if no rewrite) is
		//     reserved against subsequent agents' imports.
		existing[m.ID] = true
		globalExisting[insertID] = true
		pendingBytes += len(line)
		if len(pending) >= importBatchSize || pendingBytes >= importBatchBytes {
			if err := flush(); err != nil {
				return count, fmt.Errorf("line %d: %w", lineNo, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return count, fmt.Errorf("scan %s: %w", path, err)
	}
	if err := flush(); err != nil {
		return count, fmt.Errorf("final flush: %w", err)
	}
	return count, nil
}

// loadExistingMessageIDs returns the set of agent_messages ids already
// stored for agentID (live or tombstoned). Tombstones are included so a
// resurrected v0 id doesn't INSERT-conflict against a row the importer
// previously soft-deleted via UpdateMessage. The query is keyed on the
// (agent_id, seq) index that backs ListMessages, so it stays O(rows)
// even when many other agents are present.
func loadExistingMessageIDs(ctx context.Context, st *store.Store, agentID string) (map[string]bool, error) {
	rows, err := st.DB().QueryContext(ctx,
		`SELECT id FROM agent_messages WHERE agent_id = ?`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// loadAllMessageIDs returns the set of every agent_messages id
// currently in the DB (live or tombstoned, across every agent).
// Used by the cross-agent collision detector — see Run's preload
// comment. O(rows total) memory; for a single agent's importer it's
// the same shape as loadExistingMessageIDs but without the agent
// filter.
func loadAllMessageIDs(ctx context.Context, st *store.Store) (map[string]bool, error) {
	rows, err := st.DB().QueryContext(ctx, `SELECT id FROM agent_messages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// generateFreshMessageID returns a v0-shape "m_<16hex>" identifier
// guaranteed not to collide with any id in `taken` at call time.
// The caller is responsible for marking the returned id in `taken`
// before generating the next one. 64 random bits + an in-memory
// taken-set check makes a runtime collision astronomically unlikely
// even for a single import that rewrites every id (taken would have
// to fill ~2^32 entries before birthday-paradox math kicks in).
//
// Falls back with a clean error if crypto/rand can't produce
// entropy or if a long retry loop somehow exhausts plausible ids
// (the latter is a safety bound, not a realistic outcome).
func generateFreshMessageID(taken map[string]bool) (string, error) {
	var buf [8]byte
	for tries := 0; tries < 32; tries++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", fmt.Errorf("rand: %w", err)
		}
		id := "m_" + hex.EncodeToString(buf[:])
		if !taken[id] {
			return id, nil
		}
	}
	return "", errors.New("could not generate non-colliding message id after 32 tries")
}

// validRole mirrors the store's CHECK constraint set ("user", "assistant",
// "system", "tool"). v0 only emits the first three but we accept "tool" so
// custom-built records do not desync the importer from the schema.
func validRole(r string) bool {
	switch r {
	case "user", "assistant", "system", "tool":
		return true
	}
	return false
}

