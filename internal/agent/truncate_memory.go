package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// TruncateMemoryResult summarises what TruncateMemoryAt removed. Counts are
// best-effort; entries we couldn't parse are kept verbatim and not counted
// (same forgiving stance as ResetData — never lose a malformed line just
// because its timestamp didn't parse).
type TruncateMemoryResult struct {
	// Since is the threshold instant, formatted as RFC3339. Entries whose
	// timestamp is at or after this are considered "after T" and removed.
	Since string `json:"since"`

	// MessagesRemoved is the number of kojo transcript records tombstoned
	// in the agent_messages table.
	MessagesRemoved int `json:"messagesRemoved"`

	// ClaudeSessionEntriesRemoved counts JSONL records dropped from
	// ~/.claude/projects/<encoded(agentDir)>/<sessionID>.jsonl across every
	// session file we touched. Includes records dropped during the
	// turn-boundary trim that runs after the timestamp filter.
	ClaudeSessionEntriesRemoved int `json:"claudeSessionEntriesRemoved"`

	// ClaudeSessionFilesRemoved counts session files we deleted because
	// every record in them was at or after the threshold (and so the file
	// would have been left empty).
	ClaudeSessionFilesRemoved int `json:"claudeSessionFilesRemoved"`

	// DiaryFilesRemoved counts memory/YYYY-MM-DD.md files we deleted
	// outright because their date is strictly after the threshold's date
	// (in JST — diary timestamps are JST-local). Also counts memory_entries
	// rows whose name matched a removed file.
	DiaryFilesRemoved int `json:"diaryFilesRemoved"`

	// DiaryEntriesRemoved counts `- HH:MM — ...` bullet lines we removed
	// from memory/YYYY-MM-DD.md files matching the threshold's date (or
	// from their memory_entries body when the row outlives the file).
	DiaryEntriesRemoved int `json:"diaryEntriesRemoved"`
}

// diaryEntryHHMM matches the leading `- HH:MM` portion of a diary bullet.
// memory.go's writing-discipline directive standardises on
// `- HH:MM — <one-line summary>` (em-dash separator), but we tolerate any
// non-digit run after the time so a stray `- HH:MM - foo` or `- HH:MM: foo`
// still gets caught — the agent occasionally drifts from the spec and we'd
// rather over-trim than leave post-T entries in place.
var diaryEntryHHMM = regexp.MustCompile(`^\s*-\s+(\d{2}):(\d{2})\b`)

// diaryPreCompactSection matches the header autosummary.go writes when it
// flushes a pre-compaction summary into a diary file. The format is
// `## Pre-compaction summary (HH:MM)` — same minute-precision as bullets
// so the same minutes-of-day comparison applies.
var diaryPreCompactSection = regexp.MustCompile(`^##\s+Pre-compaction summary\s+\((\d{2}):(\d{2})\)\s*$`)

// diaryAnyHeading matches any markdown ATX heading at depth 1-6. Used by
// the section-drop pass to decide where a section terminates.
var diaryAnyHeading = regexp.MustCompile(`^#{1,6}\s`)

// diaryFenceLine matches a fenced-code-block delimiter (``` or ~~~). The
// CommonMark spec permits up to 3 leading spaces of indentation before a
// fence opener, so agents that write `   ` + "```" + `python` samples (or list-
// embedded fences) are still recognised. Tab is also accepted to match
// markdown parsers that treat `\t` as <=4 spaces.
var diaryFenceLine = regexp.MustCompile("^[ \t]{0,3}(```+|~~~+)")

// syncDir fsyncs a directory inode so unlink/rename ops survive a crash.
// Mirrors internal/oplog.fsyncDir — Windows / some network FSes refuse
// fsync on dirs and we swallow that as non-fatal.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return nil
	}
	return nil
}

// TruncateMemoryFromMessage truncates the agent's memory using the message
// identified by msgID. The matched message and everything sequentially after
// it in the kojo transcript are removed by seq (not timestamp), so two
// messages that share an RFC3339-second timestamp are not over-deleted; the
// matched message's timestamp is still used as the threshold for Claude
// session JSONL records and daily diary bullets, where positional
// equivalents are not available.
//
// Returns ErrMessageNotFound if no such message exists.
func (m *Manager) TruncateMemoryFromMessage(agentID, msgID string) (*TruncateMemoryResult, error) {
	if strings.TrimSpace(msgID) == "" {
		return nil, fmt.Errorf("messageID is required")
	}
	return m.truncateMemory(agentID, time.Time{}, msgID)
}

// TruncateMemoryAt removes everything in the agent's memory recorded at or
// after `since`, leaving anything strictly before it untouched. Comparison
// is "timestamp >= since" — the boundary record is dropped, not kept.
//
// Targets:
//   - agent_messages rows for this agent (kojo transcript) — tombstoned via
//     Store.TruncateMessagesFromCreatedAt.
//   - ~/.claude/projects/<encoded(agentDir)>/<sessionID>.jsonl (Claude --resume
//     state). After the timestamp filter, trailing assistant / tool_result
//     entries are also dropped so the file ends on a real user message —
//     otherwise --resume on a half-finished turn breaks claude. A file that
//     would be left empty is removed entirely.
//   - <agentDir>/memory/YYYY-MM-DD.md (daily diary) AND the matching
//     memory_entries rows. Files whose date is strictly after since's JST
//     date are deleted (and their DB row tombstoned); the file matching
//     since's JST date keeps only `- HH:MM — ...` bullets timestamped before
//     since's HH:MM (with the DB body updated to match).
//
// Untouched: persona, MEMORY.md, memory/projects/*, memory/people/*,
// memory/topics/*, memory/archive/*, credentials, tasks, agent settings.
//
// Acquires the same reset guard as ResetData and waits for any in-flight
// chat — including one-shot chats (Slack / Discord / Group DM), which
// acquireResetGuard alone does NOT cancel — to finish, so cron / notify
// pollers see ErrAgentResetting in the meantime instead of racing the
// rewrites against memory writes.
func (m *Manager) TruncateMemoryAt(agentID string, since time.Time) (*TruncateMemoryResult, error) {
	return m.truncateMemory(agentID, since, "")
}

// truncateMemory is the shared implementation behind TruncateMemoryAt and
// TruncateMemoryFromMessage. When fromMsgID is non-empty, the boundary is
// derived from the matched message's seq (not its timestamp), which lets
// fromMessageId distinguish between messages that share an RFC3339-second
// timestamp. Claude session JSONL and the daily diary still use the
// derived `since` because they don't carry kojo's per-message ID.
func (m *Manager) truncateMemory(agentID string, since time.Time, fromMsgID string) (*TruncateMemoryResult, error) {
	m.mu.Lock()
	_, ok := m.agents[agentID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, agentID)
	}

	cleanup, err := m.acquireResetGuard(agentID)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// acquireResetGuard cancels m.busy chats but not one-shot chats
	// (Slack, Discord, Group DM) which can keep writing to MEMORY.md /
	// memory/*.md / persona / index. Match Fork's pattern so we don't
	// race those writers against the rewrites below.
	m.cancelOneShots(agentID)

	if err := m.waitBusyClear(agentID); err != nil {
		return nil, err
	}
	if err := m.waitOneShotClear(agentID); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1. Transcript: derive boundary, tombstone via the store. fromMsgID
	//    path also overrides `since` with the matched message's timestamp
	//    so steps 2-3 use the same boundary.
	var messagesRemoved int
	if fromMsgID != "" {
		derivedSince, n, err := truncateMessagesFromMsgID(ctx, agentID, fromMsgID)
		if err != nil {
			return nil, err
		}
		since = derivedSince
		messagesRemoved = n
	} else {
		n, err := truncateMessagesByTime(ctx, agentID, since)
		if err != nil {
			return nil, fmt.Errorf("truncate messages: %w", err)
		}
		messagesRemoved = n
	}

	res := &TruncateMemoryResult{
		Since:           since.Format(time.RFC3339),
		MessagesRemoved: messagesRemoved,
	}
	dir := agentDir(agentID)

	// 2. Claude session JSONL files. Best-effort: a per-file failure is
	//    logged but does not abort the whole truncation, since the DB
	//    truncate has already committed and bailing now would leave the
	//    agent in an inconsistent half-truncated state.
	entriesRm, filesRm, err := truncateClaudeSessions(dir, since, m.logger)
	if err != nil {
		m.logger.Warn("truncate: claude sessions partial failure", "agent", agentID, "err", err)
	}
	res.ClaudeSessionEntriesRemoved = entriesRm
	res.ClaudeSessionFilesRemoved = filesRm

	// 3. Daily diary (per-day .md files plus their pre-compaction summary
	//    sections AND the corresponding memory_entries rows). Held under
	//    memorySyncMu so a concurrent syncMemoryEntriesToDB can't race
	//    our file rewrite against the DB body it just observed.
	releaseSync := lockMemorySync(agentID)
	dEntries, dFiles, err := m.truncateDiary(ctx, agentID, since)
	if err != nil {
		m.logger.Warn("truncate: diary partial failure", "agent", agentID, "err", err)
	}
	res.DiaryEntriesRemoved = dEntries
	res.DiaryFilesRemoved = dFiles

	// 4. Drop memory/recent.md if anything diary-side actually changed.
	//    recent.md is a derived rolling summary that PreCompactSummarize
	//    rebuilds on the next fire; if we leave a stale copy in place
	//    after dropping diary content, RecentDiarySummary will keep
	//    injecting that removed text into the per-turn volatile context.
	//
	//    Tombstone the matching memory_entries row too — scanMemoryDir
	//    classifies memory/recent.md as (kind=topic, name=recent), so
	//    syncAgentMemoryHeld below would either (a) tombstone it
	//    automatically when the on-disk scan finds the file gone OR
	//    (b) hydrate it back to disk from the stale DB body if the
	//    sync hits the diskUninitialized branch (memory/ left empty by
	//    an aggressive full-history truncate). Doing the tombstone
	//    explicitly here closes case (b).
	if dEntries > 0 || dFiles > 0 {
		memDir := filepath.Join(dir, "memory")
		// scanMemoryDir collapses BOTH memory/recent.md (autosummary's
		// own path) and memory/topics/recent.md (the canonical path
		// memoryEntryDiskPath hydrates a (topic, recent) row to) into
		// the same natural key. Either copy left behind would let the
		// next syncMemoryEntriesToDB upsert the stale body and undo our
		// tombstone. Walk both.
		// Each unlink needs an fsync of its parent directory (NOT just
		// memDir) so a crash between unlink and the next sync can't
		// resurrect the stale path. Tracking the dirs we actually wrote
		// to lets us skip an unnecessary topics/ stat when only root
		// recent.md was present.
		dirsToSync := map[string]struct{}{}
		for _, p := range []string{
			filepath.Join(memDir, recentSummaryFile),
			filepath.Join(memDir, "topics", recentSummaryFile),
		} {
			rerr := os.Remove(p)
			switch {
			case rerr == nil:
				dirsToSync[filepath.Dir(p)] = struct{}{}
			case os.IsNotExist(rerr):
				// nothing to remove — fine.
			default:
				m.logger.Warn("truncate: remove recent.md",
					"agent", agentID, "path", p, "err", rerr)
			}
		}
		for d := range dirsToSync {
			if serr := syncDir(d); serr != nil {
				m.logger.Warn("truncate: sync dir after recent.md remove",
					"agent", agentID, "dir", d, "err", serr)
			}
		}
		// Tombstone the matching memory_entries row regardless of whether
		// either disk file was present. A peer-replicated (topic, recent)
		// row would otherwise hydrate stale content back onto disk on the
		// next sync, including the diskUninitialized branch (memory/ left
		// empty by an aggressive full-history truncate).
		if st := getGlobalStore(); st != nil {
			if rec, ferr := st.FindMemoryEntryByName(ctx, agentID, "topic", "recent"); ferr == nil {
				if derr := st.SoftDeleteMemoryEntry(ctx, rec.ID, ""); derr != nil {
					m.logger.Warn("truncate: tombstone recent.md row",
						"agent", agentID, "err", derr)
				}
			} else if !errors.Is(ferr, store.ErrNotFound) {
				m.logger.Warn("truncate: lookup recent.md row",
					"agent", agentID, "err", ferr)
			}
		}
	}

	// 5. Final post-truncate sync FIRST so the on-disk diary edits we just
	//    made are reflected back into the DB (covers same-date partial
	//    rewrites whose memory_entries row we couldn't find by name).
	//    Reindex needs to read the post-sync state, otherwise rows hydrated
	//    by the sync miss the FTS rebuild and search keeps returning stale
	//    snippets until the next reindex trigger.
	if st := getGlobalStore(); st != nil {
		syncCtx, syncCancel := context.WithTimeout(context.Background(), 60*time.Second)
		if serr := syncAgentMemoryHeld(syncCtx, st, agentID, m.logger); serr != nil {
			m.logger.Warn("truncate: post-sync failed", "agent", agentID, "err", serr)
		}
		syncCancel()
	}

	// 6. Reindex FTS while we still hold memorySyncMu — a concurrent
	//    memory edit racing the rebuild would either be lost (predates
	//    the wipe) or propagated twice (lands after Reindex's stems
	//    already re-read it).
	if idx := m.getOrOpenIndex(agentID); idx != nil {
		if rerr := idx.Reindex(agentID); rerr != nil {
			m.logger.Warn("truncate: memory reindex", "agent", agentID, "err", rerr)
		}
	}
	releaseSync()

	// 7. Refresh LastMessage preview from the now-truncated transcript so
	//    the agent list view reflects the change immediately.
	m.refreshLastMessage(agentID)

	m.logger.Info("agent memory truncated",
		"agent", agentID,
		"since", res.Since,
		"fromMsg", fromMsgID,
		"messages", res.MessagesRemoved,
		"claudeEntries", res.ClaudeSessionEntriesRemoved,
		"claudeFiles", res.ClaudeSessionFilesRemoved,
		"diaryEntries", res.DiaryEntriesRemoved,
		"diaryFiles", res.DiaryFilesRemoved,
	)
	return res, nil
}

// truncateMessagesByTime tombstones every live agent_messages row whose
// created_at >= since. Returns the count of rows affected.
func truncateMessagesByTime(ctx context.Context, agentID string, since time.Time) (int, error) {
	st := getGlobalStore()
	if st == nil {
		return 0, errStoreNotReady
	}
	n, err := st.TruncateMessagesFromCreatedAt(ctx, agentID, since.UnixMilli())
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// truncateMessagesFromMsgID looks up the pivot message by ID, derives the
// effective `since` from its created_at, and tombstones every message at or
// after the pivot (using the pivot's etag as a soft precondition). Returns
// the derived threshold, the count of tombstoned rows, and any error.
//
// Maps store.ErrNotFound → ErrMessageNotFound; a cross-agent ID hit is
// treated as not-found so a privileged agent can't reach into another
// agent's row by guessing IDs.
func truncateMessagesFromMsgID(ctx context.Context, agentID, msgID string) (time.Time, int, error) {
	st := getGlobalStore()
	if st == nil {
		return time.Time{}, 0, errStoreNotReady
	}
	rec, err := st.GetMessage(ctx, msgID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return time.Time{}, 0, ErrMessageNotFound
		}
		return time.Time{}, 0, err
	}
	if rec.AgentID != agentID {
		return time.Time{}, 0, ErrMessageNotFound
	}
	since := time.UnixMilli(rec.CreatedAt).UTC()
	n, err := st.TruncateMessagesAfterSeq(ctx, agentID, rec.Seq-1, rec.ID, rec.ETag)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return time.Time{}, 0, ErrMessageNotFound
		}
		if errors.Is(err, store.ErrETagMismatch) {
			return time.Time{}, 0, ErrMessageETagMismatch
		}
		return time.Time{}, 0, err
	}
	return since, int(n), nil
}

// truncateClaudeSessions walks every .jsonl file under
// claudeProjectDir(absDir) and trims records at-or-after `since`, plus the
// trailing-tool-call cleanup detailed on truncateClaudeSessionFile.
// Best-effort across files — a per-file failure is logged and the walk
// continues; the first error is returned so the caller can surface it.
//
// (gemini-only / codex-only agents simply have nothing to do here.)
func truncateClaudeSessions(agentDirPath string, since time.Time, logger *slog.Logger) (entriesRemoved, filesRemoved int, err error) {
	absDir, aerr := filepath.Abs(agentDirPath)
	if aerr != nil {
		return 0, 0, aerr
	}
	projectDir := claudeProjectDir(absDir)
	entries, derr := os.ReadDir(projectDir)
	if derr != nil {
		if os.IsNotExist(derr) {
			return 0, 0, nil
		}
		return 0, 0, derr
	}
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(projectDir, e.Name())
		removed, deleted, ferr := truncateClaudeSessionFile(path, since)
		entriesRemoved += removed
		if deleted {
			filesRemoved++
		}
		if ferr != nil {
			logger.Warn("truncate claude session file", "path", path, "err", ferr)
			if firstErr == nil {
				firstErr = ferr
			}
		}
	}
	if serr := syncDir(projectDir); serr != nil && firstErr == nil {
		firstErr = serr
	}
	return entriesRemoved, filesRemoved, firstErr
}

// truncateClaudeSessionFile rewrites a single Claude session JSONL, dropping
// records whose `timestamp` field is at-or-after since. When the timestamp
// pass actually drops at least one record, a follow-up trim removes any
// trailing synthetic-user (tool_result) entries that would now reference a
// dropped tool_use. The trim does NOT touch trailing assistant entries by
// itself — a healthy session that ends on assistant content is a valid
// --resume target — so a no-op truncation does not damage healthy files.
// Specifically: trailing tool_result entries that reference a tool_use_id
// no longer present are dropped; once that loop stops, any assistant entry
// it has just orphaned (i.e., that has unmatched tool_use blocks) is
// dropped too. This catches the "we cut a multi-tool turn at the
// timestamp boundary" case without nibbling at completed turns.
//
// If every line would be dropped, the file is removed instead.
func truncateClaudeSessionFile(path string, since time.Time) (removed int, deleted bool, err error) {
	src, oerr := os.Open(path)
	if oerr != nil {
		if os.IsNotExist(oerr) {
			return 0, false, nil
		}
		return 0, false, oerr
	}
	rawLines := make([][]byte, 0, 64)
	r := bufio.NewReader(src)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			cp := make([]byte, len(line))
			copy(cp, line)
			rawLines = append(rawLines, cp)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			src.Close()
			return 0, false, rerr
		}
	}
	src.Close()

	type kept struct {
		raw           []byte
		typ           string
		real          bool     // for type=="user": real user msg vs tool_result feedback
		toolUseIDs    []string // tool_use ids contained in an assistant message
		toolResultIDs []string // tool_use_id refs contained in a synthetic user message
	}
	keep := make([]kept, 0, len(rawLines))
	timestampDropped := 0
	for _, raw := range rawLines {
		stripped := raw
		if len(stripped) > 0 && stripped[len(stripped)-1] == '\n' {
			stripped = stripped[:len(stripped)-1]
			if len(stripped) > 0 && stripped[len(stripped)-1] == '\r' {
				stripped = stripped[:len(stripped)-1]
			}
		}
		if len(stripped) == 0 {
			keep = append(keep, kept{raw: raw})
			continue
		}
		var entry struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Message   json.RawMessage `json:"message"`
		}
		if json.Unmarshal(stripped, &entry) != nil {
			keep = append(keep, kept{raw: raw})
			continue
		}
		if entry.Timestamp != "" {
			if t, perr := time.Parse(time.RFC3339Nano, entry.Timestamp); perr == nil && !t.Before(since) {
				removed++
				timestampDropped++
				continue
			}
		}
		k := kept{raw: raw, typ: entry.Type}
		switch entry.Type {
		case "user":
			k.real = isRealUserEntry(entry.Message)
			if !k.real {
				k.toolResultIDs = collectToolResultIDs(entry.Message)
			}
		case "assistant":
			k.toolUseIDs = collectToolUseIDs(entry.Message)
		}
		keep = append(keep, k)
	}

	if timestampDropped > 0 {
		for {
			n := len(keep)
			if n == 0 {
				break
			}
			matched := make(map[string]bool)
			for _, k := range keep {
				for _, id := range k.toolResultIDs {
					if id != "" {
						matched[id] = true
					}
				}
			}
			last := keep[n-1]
			drop := false
			switch {
			case last.typ == "user" && !last.real:
				drop = true
			case last.typ == "assistant" && len(last.toolUseIDs) > 0:
				for _, id := range last.toolUseIDs {
					if id != "" && !matched[id] {
						drop = true
						break
					}
				}
			}
			if !drop {
				break
			}
			keep = keep[:n-1]
			removed++
		}
	}

	// No-op early return: a session whose timestamps were all pre-T must
	// be byte-identical after this call. The session file's mtime is
	// consulted by sessionFileUsable / idle-window logic, and rewriting
	// it would invalidate those decisions for sessions we never touched.
	if removed == 0 {
		return 0, false, nil
	}

	// Use the raw bytes (not k.typ) to decide whether the file is empty:
	// kept k.typ == "" includes blank-line and unparseable-line preserves,
	// which we promised to keep verbatim. Treating those as "blank" would
	// silently drop a file whose only surviving content is malformed JSON
	// — the opposite of the parser's forgiving stance.
	allBlank := true
	for _, k := range keep {
		if strings.TrimSpace(string(k.raw)) != "" {
			allBlank = false
			break
		}
	}
	if allBlank {
		if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
			return removed, false, rerr
		}
		return removed, true, nil
	}

	dir := filepath.Dir(path)
	tmp, terr := os.CreateTemp(dir, "session-*.jsonl.tmp")
	if terr != nil {
		return removed, false, terr
	}
	tmpPath := tmp.Name()
	tmpClosed := false
	cleanup := func() {
		if !tmpClosed {
			tmp.Close()
		}
		os.Remove(tmpPath)
	}
	w := bufio.NewWriter(tmp)
	for _, k := range keep {
		if _, werr := w.Write(k.raw); werr != nil {
			cleanup()
			return removed, false, werr
		}
	}
	if werr := w.Flush(); werr != nil {
		cleanup()
		return removed, false, werr
	}
	if serr := tmp.Sync(); serr != nil {
		cleanup()
		return removed, false, serr
	}
	if cerr := tmp.Close(); cerr != nil {
		tmpClosed = true
		os.Remove(tmpPath)
		return removed, false, cerr
	}
	tmpClosed = true
	if rerr := os.Rename(tmpPath, path); rerr != nil {
		os.Remove(tmpPath)
		return removed, false, rerr
	}
	return removed, false, nil
}

// truncateDiary walks <agentDir>/memory/, deleting daily files whose date is
// strictly after since's JST date and rewriting the file matching since's
// JST date to drop bullet entries timestamped at-or-after since's HH:MM
// (also JST). Subdirectories (projects/, people/, topics/, archive/) are
// untouched — they're per-topic notes without a per-line timestamp scheme,
// so a "delete everything after T" pass would have to be all-or-nothing.
//
// For every disk mutation we also mutate the matching memory_entries row
// (kind="daily", name=YYYY-MM-DD): full-file deletion → SoftDeleteMemoryEntry;
// same-date trim → UpdateMemoryEntry with the rewritten body. After the
// disk walk, ListMemoryEntries(kind=daily) catches any DB row whose
// on-disk file no longer exists (e.g. a peer-synced row that hasn't been
// materialised here yet) and tombstones it via the same predicate.
//
// Caller must hold lockMemorySync(agentID).
func (m *Manager) truncateDiary(ctx context.Context, agentID string, since time.Time) (entriesRemoved, filesRemoved int, err error) {
	dir := agentDir(agentID)
	memDir := filepath.Join(dir, "memory")
	st := getGlobalStore()

	jstSince := since.In(jst)
	sinceDate := jstSince.Format("2006-01-02")
	sinceMinutes := jstSince.Hour()*60 + jstSince.Minute()

	// Track which YYYY-MM-DD names we already handled via the disk walk
	// so the DB-only fallback pass doesn't double-count.
	handled := make(map[string]bool)
	var firstErr error

	entries, derr := os.ReadDir(memDir)
	if derr != nil && !os.IsNotExist(derr) {
		return 0, 0, derr
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".md")
		fileDate, perr := time.ParseInLocation("2006-01-02", stem, jst)
		if perr != nil {
			continue
		}
		path := filepath.Join(memDir, e.Name())
		fileDateStr := fileDate.Format("2006-01-02")
		handled[stem] = true

		if fileDateStr > sinceDate {
			if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
				m.logger.Warn("truncate diary remove", "path", path, "err", rerr)
				if firstErr == nil {
					firstErr = rerr
				}
				continue
			}
			filesRemoved++
			if st != nil {
				if rec, ferr := st.FindMemoryEntryByName(ctx, agentID, "daily", stem); ferr == nil {
					if derr := st.SoftDeleteMemoryEntry(ctx, rec.ID, ""); derr != nil {
						m.logger.Warn("truncate diary db tombstone",
							"agent", agentID, "name", stem, "err", derr)
						if firstErr == nil {
							firstErr = derr
						}
					}
				} else if !errors.Is(ferr, store.ErrNotFound) {
					m.logger.Warn("truncate diary db lookup",
						"agent", agentID, "name", stem, "err", ferr)
					if firstErr == nil {
						firstErr = ferr
					}
				}
			}
			continue
		}
		if fileDateStr < sinceDate {
			continue
		}
		// Same-date file: trim bullets in place, then push the trimmed
		// body into memory_entries so a cross-device reader sees the
		// same content.
		removed, rerr := trimDiaryFileEntries(path, sinceMinutes)
		entriesRemoved += removed
		if rerr != nil {
			m.logger.Warn("truncate diary rewrite", "path", path, "err", rerr)
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		if removed > 0 && st != nil {
			if rec, ferr := st.FindMemoryEntryByName(ctx, agentID, "daily", stem); ferr == nil {
				body, berr := os.ReadFile(path)
				if berr == nil {
					bodyStr := string(body)
					if _, uerr := st.UpdateMemoryEntry(ctx, rec.ID, "", store.MemoryEntryPatch{Body: &bodyStr}); uerr != nil {
						m.logger.Warn("truncate diary db update",
							"agent", agentID, "name", stem, "err", uerr)
						if firstErr == nil {
							firstErr = uerr
						}
					}
				}
			} else if !errors.Is(ferr, store.ErrNotFound) {
				m.logger.Warn("truncate diary db lookup",
					"agent", agentID, "name", stem, "err", ferr)
				if firstErr == nil {
					firstErr = ferr
				}
			}
		}
	}

	// DB-only fallback: tombstone memory_entries rows whose name is a
	// YYYY-MM-DD strictly after sinceDate and that we didn't already
	// process via the disk walk. Covers peer-synced rows that haven't
	// been materialised onto this peer's disk yet.
	if st != nil {
		var cursor int64
		for {
			rows, lerr := st.ListMemoryEntries(ctx, agentID, store.MemoryEntryListOptions{
				Kind:   "daily",
				Limit:  200,
				Cursor: cursor,
			})
			if lerr != nil {
				m.logger.Warn("truncate diary db list",
					"agent", agentID, "err", lerr)
				if firstErr == nil {
					firstErr = lerr
				}
				break
			}
			if len(rows) == 0 {
				break
			}
			for _, rec := range rows {
				cursor = rec.Seq
				if handled[rec.Name] {
					continue
				}
				if _, perr := time.ParseInLocation("2006-01-02", rec.Name, jst); perr != nil {
					continue
				}
				if rec.Name > sinceDate {
					if derr := st.SoftDeleteMemoryEntry(ctx, rec.ID, ""); derr != nil {
						m.logger.Warn("truncate diary db-only tombstone",
							"agent", agentID, "name", rec.Name, "err", derr)
						if firstErr == nil {
							firstErr = derr
						}
						continue
					}
					filesRemoved++
				} else if rec.Name == sinceDate {
					newBody, n := trimDiaryStringEntries(rec.Body, sinceMinutes)
					if n > 0 {
						if _, uerr := st.UpdateMemoryEntry(ctx, rec.ID, "", store.MemoryEntryPatch{Body: &newBody}); uerr != nil {
							m.logger.Warn("truncate diary db-only body trim",
								"agent", agentID, "name", rec.Name, "err", uerr)
							if firstErr == nil {
								firstErr = uerr
							}
							continue
						}
						entriesRemoved += n
					}
				}
			}
			if len(rows) < 200 {
				break
			}
		}
	}

	if serr := syncDir(memDir); serr != nil && firstErr == nil && !os.IsNotExist(serr) {
		firstErr = serr
	}
	return entriesRemoved, filesRemoved, firstErr
}

// trimDiaryFileEntries reads a diary file from disk, trims it, and writes
// back atomically. Returns the number of entries removed. A no-op (no
// bullet matched the threshold) keeps the file byte-identical and returns
// 0.
//
// Caller must hold lockMemorySync(agentID).
func trimDiaryFileEntries(path string, sinceMinutes int) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	out, removed := trimDiaryStringEntries(string(data), sinceMinutes)
	if removed == 0 {
		return 0, nil
	}

	dir := filepath.Dir(path)
	tmp, terr := os.CreateTemp(dir, "diary-*.md.tmp")
	if terr != nil {
		return removed, terr
	}
	tmpPath := tmp.Name()
	tmpClosed := false
	cleanup := func() {
		if !tmpClosed {
			tmp.Close()
		}
		os.Remove(tmpPath)
	}
	if _, werr := tmp.WriteString(out); werr != nil {
		cleanup()
		return removed, werr
	}
	if serr := tmp.Sync(); serr != nil {
		cleanup()
		return removed, serr
	}
	if cerr := tmp.Close(); cerr != nil {
		tmpClosed = true
		os.Remove(tmpPath)
		return removed, cerr
	}
	tmpClosed = true
	if rerr := os.Rename(tmpPath, path); rerr != nil {
		os.Remove(tmpPath)
		return removed, rerr
	}
	return removed, nil
}

// trimDiaryStringEntries is the pure-function core of the diary trim. Given
// a diary body and the threshold (minutes since midnight, JST), returns the
// trimmed body and the number of removed entries.
//
// Entry shapes handled:
//
//  1. `- HH:MM — ...` bullet lines (possibly with hanging-indent
//     continuation lines following).
//  2. `## Pre-compaction summary (HH:MM)` sections written by
//     PreCompactSummarize. The whole section — header through to the
//     next ATX heading or EOF — is dropped.
//
// Lines outside both shapes are passed through unchanged. Heading and
// bullet detection is paused inside fenced code blocks (``` / ~~~) so an
// embedded markdown sample with its own headings or bullets isn't
// mis-parsed as live diary content.
func trimDiaryStringEntries(body string, sinceMinutes int) (string, int) {
	lines := strings.SplitAfter(body, "\n")

	var out strings.Builder
	out.Grow(len(body))
	removed := 0
	dropMode := ""
	inFence := false
	for _, line := range lines {
		// Bullet-drop continuation handling runs BEFORE fence toggling.
		// Otherwise a fence delimiter that appears inside a dropped
		// bullet's continuation (e.g. `- 14:00 — work` followed by an
		// indented `   ` + "```" + `\n` code sample) would flip inFence into
		// "kept code" mode and start emitting the post-T content. The
		// continuation is content of the dropped bullet, not a real
		// markdown structure.
		if dropMode == "bullet" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.TrimSpace(trimmed) == "" {
				dropMode = ""
				out.WriteString(line)
				continue
			}
			if len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t') {
				// continuation of the dropped bullet — keep dropping,
				// keep `inFence` untouched (the fence delimiter, if
				// any, is content, not structure).
				continue
			}
			// Non-indented, non-blank line — bullet drop terminates
			// here and the line falls through to the normal dispatch
			// below (which may itself open a section / start a new
			// bullet drop).
			dropMode = ""
		}
		if diaryFenceLine.MatchString(strings.TrimRight(line, "\r\n")) {
			inFence = !inFence
		}
		if dropMode == "section" {
			if !inFence && diaryAnyHeading.MatchString(line) {
				dropMode = ""
			} else {
				continue
			}
		}
		if inFence {
			out.WriteString(line)
			continue
		}
		if matches := diaryPreCompactSection.FindStringSubmatch(line); matches != nil {
			hh, _ := atoi2(matches[1])
			mm, _ := atoi2(matches[2])
			if hh*60+mm >= sinceMinutes {
				removed++
				dropMode = "section"
				continue
			}
			dropMode = ""
			out.WriteString(line)
			continue
		}
		if matches := diaryEntryHHMM.FindStringSubmatch(line); matches != nil {
			hh, _ := atoi2(matches[1])
			mm, _ := atoi2(matches[2])
			if hh*60+mm >= sinceMinutes {
				removed++
				dropMode = "bullet"
				continue
			}
			dropMode = ""
			out.WriteString(line)
			continue
		}
		out.WriteString(line)
	}
	return out.String(), removed
}

// collectToolUseIDs returns the `id` of every tool_use content block found
// inside a Claude assistant message payload.
func collectToolUseIDs(msgRaw json.RawMessage) []string {
	if len(msgRaw) == 0 {
		return nil
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msgRaw, &msg) != nil {
		return nil
	}
	var blocks []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID != "" {
			out = append(out, b.ID)
		}
	}
	return out
}

// collectToolResultIDs returns the `tool_use_id` of every tool_result content
// block inside a Claude (synthetic) user message payload.
func collectToolResultIDs(msgRaw json.RawMessage) []string {
	if len(msgRaw) == 0 {
		return nil
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msgRaw, &msg) != nil {
		return nil
	}
	var blocks []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
	}
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "tool_result" && b.ToolUseID != "" {
			out = append(out, b.ToolUseID)
		}
	}
	return out
}

// atoi2 parses a 2-digit decimal string. The diary regex guarantees the
// input is two ASCII digits.
func atoi2(s string) (int, error) {
	if len(s) != 2 || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return 0, fmt.Errorf("not a 2-digit number: %q", s)
	}
	return int(s[0]-'0')*10 + int(s[1]-'0'), nil
}
