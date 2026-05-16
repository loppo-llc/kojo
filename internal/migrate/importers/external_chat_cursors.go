package importers

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// externalChatCursorsImporter walks <v0>/agents/<agentID>/chat_history/
// <platform>/<channelID>/<threadID>.jsonl and inserts one cursor row
// per thread into the v1 external_chat_cursors table. Domain key:
// "external_chat_cursors".
//
// v0 layout (from internal/chathistory/store.go):
//
//	<v0>/agents/<agentID>/chat_history/<platform>/<channelID>/_channel.jsonl
//	<v0>/agents/<agentID>/chat_history/<platform>/<channelID>/<threadID>.jsonl
//
// _channel.jsonl is intentionally NOT imported. v0's FetchChannelHistory
// is a sliding-window fetch that overwrites the file every poll and
// never consults LastPlatformTS as a delta cursor — the file's last ts
// is therefore not a "where to resume from" cursor in any meaningful
// sense. Importing it as a v1 cursor would invite a future v1 channel
// poll to mistake it for a delta starting point and silently drop
// messages between last-real-ts and now. Per-thread JSONLs (which DO
// drive cursor-based delta fetch in v0's FetchThreadHistory) are the
// only files that turn into cursor rows.
//
// For each per-thread file the cursor is the messageId of the last
// entry whose id is a real platform timestamp (numeric + dots). Bot-
// synthesised ids ("...bot") and the "_incomplete" sentinel are not
// real cursors and are skipped — matches v0's chathistory.LastPlatformTS
// exactly so a re-import yields the same cursor the v0 runtime would
// have used on its next poll.
//
// v1 schema (0001_initial.sql §3.3):
//
//	id          PRIMARY KEY            (composite — see below)
//	source      TEXT NOT NULL          (platform: 'slack' | ...)
//	agent_id    TEXT (nullable)
//	channel_id  TEXT (nullable)
//	cursor      TEXT NOT NULL
//
// The schema deliberately omits a thread_id column; thread information
// lives only in the composite id:
//
//	thread-level cursor:   "<agent>:<source>:<channel>:<thread>"
//
// All four segments are colon-free in practice — Slack channel ids are
// alphanumeric (Cxxxxxxxx), thread ids are numeric-with-dot
// (1712345678.123456), platform names are kojo-controlled, and agent
// ids are random hex-ish. The importer fails closed (skip + warn) on
// any segment that does contain ':' so a future plugin that violates
// this can't smuggle a row that aliases another conversation's id.
//
// peer_id is stamped from opts.HomePeer for the same reason as
// notify_cursors: cursors are global-scoped (every peer must agree on
// the same cursor to avoid double-fetching the same external history on
// device switch — see design doc §2.3) but the row remembers which peer
// last advanced it.
type externalChatCursorsImporter struct{}

func (externalChatCursorsImporter) Domain() string { return "external_chat_cursors" }

func (externalChatCursorsImporter) Run(ctx context.Context, st *store.Store, opts migrate.Options) error {
	if done, err := alreadyImported(ctx, st, "external_chat_cursors"); err != nil {
		return err
	} else if done {
		return nil
	}

	logger := slog.Default().With("importer", "external_chat_cursors")

	srcPaths, err := collectExternalChatCursorsSourcePaths(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("collect source paths: %w", err)
	}
	checksum, err := domainChecksum(opts.V0Dir, srcPaths)
	if err != nil {
		return fmt.Errorf("checksum external_chat_cursors sources: %w", err)
	}

	// Build the composite-id-safe agent filter. agentsImporter itself
	// does NOT reject ':' in agent ids today (see agents.go:108 — only
	// id+name presence is checked), so this set can be a strict *subset*
	// of what landed in v1's agents table. The narrower filter is on
	// purpose: the v1 cursor primary key is composite ("<agent>:<source>:
	// <channel>"), and accepting an agent id with ':' would let two
	// distinct (agent, source, channel) tuples collide on the composite.
	// notify_cursors applies the same filter for the same reason, so
	// cross-domain joins by agent_id stay consistent across the two
	// cursor tables.
	//
	// Missing agents.json (os.ErrNotExist) is tolerated and returns an
	// empty set, which downgrades every chat_history dir to "orphan agent"
	// and yields markImported(0). Malformed JSON is fatal — same posture
	// as notify_cursors: we'd rather surface the corruption than silently
	// drop every cursor.
	validAgents, err := loadValidV0AgentIDs(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("load valid agent ids: %w", err)
	}

	base := agentsBaseDir(opts.V0Dir)
	entries, err := readDirV0(opts.V0Dir, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return markImported(ctx, st, "external_chat_cursors", 0, checksum)
		}
		return fmt.Errorf("readdir agents: %w", err)
	}

	// importBatchSize bounds memory and the size of the SQL transaction
	// when an operator has accumulated tens of thousands of chat_history
	// JSONL files (1 agent × N channels × M threads can balloon quickly).
	// Each batch is its own short transaction; ON CONFLICT DO NOTHING in
	// BulkInsertExternalChatCursors keeps the operation idempotent across
	// batch boundaries, so a crash mid-domain converges to the same final
	// state on the next --migrate-resume run.
	const importBatchSize = 1000

	flush := func(batch []*store.ExternalChatCursorRecord) (int, error) {
		if len(batch) == 0 {
			return 0, nil
		}
		return st.BulkInsertExternalChatCursors(ctx, batch, store.ExternalChatCursorInsertOptions{PeerID: opts.HomePeer})
	}

	var batch []*store.ExternalChatCursorRecord
	// freshlyInserted counts rows BulkInsert reported as newly-inserted
	// across all batches; useful for the post-run log line. importable
	// counts every candidate that reached a batch (including those that
	// already existed and were skipped via ON CONFLICT) — that is the
	// "rows in v1 that this domain owns" total, which is what
	// migration_status.imported_count should reflect even after a crash-
	// resume cycle re-walks the v0 tree and finds every row already
	// committed. The single-batch importers (notify_cursors etc.) collapse
	// these two counts because their bulk call is atomic; this importer
	// commits in chunks, so reporting freshlyInserted alone would under-
	// count after a partial-progress crash.
	freshlyInserted := 0
	importable := 0
	skipped := 0
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "groupdms" {
			continue
		}
		agentID := e.Name()
		if !validAgents[agentID] {
			// Orphan agent dir on disk that agentsImporter wouldn't have
			// inserted. Skip silently (the disk-vs-manifest mismatch is
			// already surfaced in the agents domain checksum).
			continue
		}
		chatRoot := filepath.Join(base, agentID, "chat_history")
		if _, err := os.Lstat(chatRoot); err != nil {
			// No chat_history dir for this agent — common case.
			// Tolerate only os.ErrNotExist; surface EACCES / IO so a
			// permission glitch doesn't silently drop every cursor for
			// the affected agent.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("lstat chat_history %s: %w", chatRoot, err)
		}
		platforms, err := readDirV0(opts.V0Dir, chatRoot)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("readdir chat_history %s: %w", chatRoot, err)
		}
		for _, p := range platforms {
			if !p.IsDir() {
				continue
			}
			platform := p.Name()
			if strings.ContainsRune(platform, ':') {
				logger.Warn("external_chat_cursors: skipping platform dir with ':' in name",
					"agent_id", agentID, "platform", platform)
				skipped++
				continue
			}
			platformDir := filepath.Join(chatRoot, platform)
			channels, err := readDirV0(opts.V0Dir, platformDir)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return fmt.Errorf("readdir %s: %w", platformDir, err)
			}
			for _, c := range channels {
				if !c.IsDir() {
					continue
				}
				channelID := c.Name()
				if strings.ContainsRune(channelID, ':') {
					logger.Warn("external_chat_cursors: skipping channel dir with ':' in name",
						"agent_id", agentID, "platform", platform, "channel", channelID)
					skipped++
					continue
				}
				channelDir := filepath.Join(platformDir, channelID)
				files, err := readDirV0(opts.V0Dir, channelDir)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						continue
					}
					return fmt.Errorf("readdir %s: %w", channelDir, err)
				}
				for _, f := range files {
					if f.IsDir() {
						continue
					}
					name := f.Name()
					if !strings.HasSuffix(name, ".jsonl") {
						continue
					}
					threadID := strings.TrimSuffix(name, ".jsonl")
					// _channel.jsonl is the channel-level rollup that v0
					// fetches via the sliding-window FetchChannelHistory
					// — that path overwrites the file every poll and
					// does NOT consult LastPlatformTS as a delta cursor.
					// Importing the file's last ts as a v1 cursor would
					// invite a future v1 channel poll to mistake a
					// non-cursor file for a delta-fetch starting point
					// and silently drop messages between last-real-ts
					// and now. Skip it: thread JSONLs (which DO use
					// cursor-based delta fetch in v0's FetchThreadHistory)
					// are still imported below.
					if threadID == "_channel" {
						continue
					}
					if strings.ContainsRune(threadID, ':') {
						logger.Warn("external_chat_cursors: skipping thread file with ':' in name",
							"agent_id", agentID, "platform", platform,
							"channel", channelID, "thread", threadID)
						skipped++
						continue
					}
					filePath := filepath.Join(channelDir, name)
					cursor, err := lastPlatformTSFromV0(opts.V0Dir, filePath)
					if err != nil {
						// Read failure: log and skip this file. The
						// scan-vs-import gap note in domainChecksum
						// applies — the file IS in the checksum, but
						// the row doesn't get written, which is exactly
						// what we want for unreadable inputs.
						logger.Warn("external_chat_cursors: skipping unreadable file",
							"path", filePath, "err", err)
						skipped++
						continue
					}
					if cursor == "" {
						// No real platform-stamped messages — only
						// bot/incomplete entries, or empty file. Skip:
						// re-importing an empty cursor would still be
						// an empty cursor, and the v1 runtime's first
						// poll will full-fetch as if nothing was saved.
						continue
					}
					mtime := fileMTimeMillis(filePath)

					agentIDLocal := agentID
					channelIDLocal := channelID
					batch = append(batch, &store.ExternalChatCursorRecord{
						ID:        agentID + ":" + platform + ":" + channelID + ":" + threadID,
						Source:    platform,
						AgentID:   &agentIDLocal,
						ChannelID: &channelIDLocal,
						Cursor:    cursor,
						CreatedAt: mtime,
						UpdatedAt: mtime,
					})
					importable++
					if len(batch) >= importBatchSize {
						n, err := flush(batch)
						if err != nil {
							return fmt.Errorf("bulk insert external_chat_cursors: %w", err)
						}
						freshlyInserted += n
						batch = batch[:0]
					}
				}
			}
		}
	}

	// Final flush for any remainder. Empty batch is a no-op.
	if len(batch) > 0 {
		n, err := flush(batch)
		if err != nil {
			return fmt.Errorf("bulk insert external_chat_cursors: %w", err)
		}
		freshlyInserted += n
	}

	if skipped > 0 || freshlyInserted != importable {
		logger.Info("external_chat_cursors: import complete",
			"importable", importable,
			"freshly_inserted", freshlyInserted,
			"skipped", skipped)
	}
	return markImported(ctx, st, "external_chat_cursors", importable, checksum)
}

// lastPlatformTSFromV0 mirrors chathistory.LastPlatformTS but routes the
// read through openV0 so the O_RDONLY + root-escape guards apply. We
// can't reuse internal/chathistory directly because its LoadHistory uses
// os.Open and would silently follow a hostile symlink past the v0 root.
//
// Returns the messageId of the last entry whose id is a real platform
// timestamp (digits + dots only — see isNumericTSChars). Synthetic ids
// like "1712345.bot" or "_incomplete" are skipped.
//
// Scanner err handling matches v0's chathistory.LastPlatformTS: a scan
// error (oversize line past 1MB, mid-file truncation, etc.) returns
// whatever lastReal we accumulated up to that point rather than nil.
// The alternative — propagate the error and let Run skip the file —
// would silently lose the cursor for any conversation containing one
// pathological line, regressing v0 behaviour. The downside is that a
// totally broken file may surface a stale cursor; the v1 runtime's
// first poll re-fetches enough to catch up either way.
func lastPlatformTSFromV0(v0Root, path string) (string, error) {
	f, err := openV0(v0Root, path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var lastReal string
	sc := bufio.NewScanner(f)
	// Match chathistory.LastPlatformTS buffer sizing — long lines (large
	// blocks of pasted text) are common in Slack messages.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		// Decode just enough of the JSON object to read messageId. Going
		// through HistoryMessage would also work but pulls the whole
		// chathistory type system into the importer for one field.
		var m struct {
			MessageID string `json:"messageId"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			continue // skip corrupt lines, same as LoadHistory
		}
		if isNumericTSChars(m.MessageID) {
			lastReal = m.MessageID
		}
	}
	// Intentionally ignore sc.Err() per the docstring above.
	return lastReal, nil
}

// isNumericTSChars returns true if id contains only digits and dots
// (e.g. "1712345678.123456"). Mirrors chathistory.isNumericTS — kept
// inline so the importer does not depend on internal/chathistory for
// one private predicate.
func isNumericTSChars(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if c != '.' && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// loadValidV0AgentIDs returns the set of agent ids the v1 cursor table's
// composite primary key can safely accept. Specifically: id+name both
// present, AND id colon-free. agentsImporter today does NOT enforce the
// colon-free constraint (it inserts any agent whose id+name are
// present), so the set returned here is a strict *subset* of what
// landed in v1's agents table. The narrower filter is on purpose —
// the v1 cursor primary key is composite ("<agent>:<source>:<channel>:
// <thread>"), and accepting an agent id with ':' would let two distinct
// (agent, source, channel, thread) tuples collide on the composite.
//
// Reads agents.json directly rather than the v1 agents table for the
// same reason as notify_cursors' loadNotifySourceTypes — it makes the
// importer self-contained and side-steps any subtle differences between
// the on-disk file and the v1 row that crept in through a future bulk-
// insert tweak.
//
// Skip rules:
//   - missing agents.json (os.ErrNotExist) → empty set (every agent
//     dir on disk downgrades to "orphan agent" and contributes zero rows)
//   - empty / malformed JSON → hard error. We can't tell "no agents
//     declared" from "agents.json corrupted" with an empty set, and
//     orphaning every cursor in those cases would silently lose every
//     external poll's resume point on migration.
//   - empty agentID/name → skip the agent
//   - colon-bearing agentID → skip the agent (the v1 composite id uses
//     ':' as separator; an id with ':' would alias another id's
//     composite)
func loadValidV0AgentIDs(v0Dir string) (map[string]bool, error) {
	path := filepath.Join(agentsBaseDir(v0Dir), "agents.json")
	data, err := readV0(v0Dir, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		// Mirror loadNotifySourceTypes: zero-byte agents.json on a disk
		// that *has* the file is a truncation signal, not v0 contract.
		return nil, fmt.Errorf("agents.json is empty")
	}
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("agents.json malformed: %w", err)
	}
	out := make(map[string]bool, len(raw))
	for _, ag := range raw {
		agentID, _ := ag["id"].(string)
		name, _ := ag["name"].(string)
		if agentID == "" || name == "" {
			continue
		}
		if strings.ContainsRune(agentID, ':') {
			continue
		}
		out[agentID] = true
	}
	return out, nil
}

// collectExternalChatCursorsSourcePaths enumerates the per-thread JSONL
// files under <v0>/agents/<id>/chat_history/<platform>/<channel>/ that
// the importer's row-copy loop ingests. Returned list is the input to
// domainChecksum. agents.json is included too because the "valid agent"
// filter depends on it — a future edit that adds/removes an agent flips
// which chat_history dirs the importer reads.
//
// Excluded by design (so the checksum strictly tracks what gets imported):
//   - _channel.jsonl: not a cursor source (sliding-window fetch in v0).
//
// Same scan-vs-import gap as the other importers: a JSONL that *does*
// reach the checksum may still be skipped by Run for soft reasons —
// orphan agentID (chat_history dir present, agentID absent or invalid
// in agents.json), threadID containing ':' (composite-id collision
// guard), or an empty/unreadable file. The hash records what was
// scanned (modulo the _channel exclusion), not what was inserted; an
// operator using the checksum to detect drift wants the broader signal.
func collectExternalChatCursorsSourcePaths(v0Dir string) ([]string, error) {
	var paths []string

	manifest := filepath.Join(agentsBaseDir(v0Dir), "agents.json")
	updated, err := addLeafIfRegular(v0Dir, manifest, paths)
	if err != nil {
		return nil, err
	}
	paths = updated

	base := agentsBaseDir(v0Dir)
	entries, err := readDirV0(v0Dir, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return paths, nil
		}
		return nil, fmt.Errorf("readdir agents: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() || e.Name() == "groupdms" {
			continue
		}
		chatRoot := filepath.Join(base, e.Name(), "chat_history")
		if _, err := os.Lstat(chatRoot); err != nil {
			// Tolerate missing dir; surface EACCES / IO so the
			// checksum doesn't silently miss files we couldn't reach.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("lstat chat_history %s: %w", chatRoot, err)
		}
		walkErr := walkDirV0(v0Dir, chatRoot, func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				// Symlink-dir rejection from walkDirV0 surfaces here;
				// skip the offending entry rather than abort. Mirrors
				// collectAgentsSourcePaths.
				return nil
			}
			if d == nil || d.IsDir() {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			// Mirror Run's _channel.jsonl exclusion. Keeping the
			// channel-rollup file out of the source_checksum makes the
			// hash strictly reflect "what gets imported", so an
			// operator who mutates a v0 channel file (e.g. local
			// cleanup of bot tails) does not register as drift on the
			// next migration audit.
			if d.Name() == "_channel.jsonl" {
				return nil
			}
			rel, err := filepath.Rel(v0Dir, p)
			if err != nil {
				return nil
			}
			paths = append(paths, filepath.ToSlash(rel))
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("walk chat_history %s: %w", chatRoot, walkErr)
		}
	}
	return paths, nil
}
