package agent

import (
	"bufio"
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
)

// TruncateMemoryResult summarises what TruncateMemoryAt removed. Counts are
// best-effort; entries we couldn't parse are kept verbatim and not counted
// (same forgiving stance as rewriteMessages — never lose a malformed line
// just because its timestamp didn't parse).
type TruncateMemoryResult struct {
	// Since is the threshold instant, formatted as RFC3339. Entries whose
	// timestamp is at or after this are considered "after T" and removed.
	Since string `json:"since"`

	// MessagesRemoved is the number of kojo transcript records dropped from
	// messages.jsonl.
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
	// (in JST — diary timestamps are JST-local).
	DiaryFilesRemoved int `json:"diaryFilesRemoved"`

	// DiaryEntriesRemoved counts `- HH:MM — ...` bullet lines we removed
	// from memory/YYYY-MM-DD.md files matching the threshold's date.
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
// fence opener, so agents that write `   ```python` samples (or list-
// embedded fences) are still recognised. Tab is also accepted to match
// markdown parsers that treat `\t` as <=4 spaces. Used by
// trimDiaryFileEntries to avoid treating a `## ` inside a code block as
// a section terminator — agents and PreCompactSummarize occasionally
// embed markdown samples with their own headings inside summary bodies.
//
// Note: we don't track the opener's marker (``` vs ~~~) or fence length,
// so a `~~~` in the middle of a `~~~`-fenced block of different length
// could close it prematurely. Diary content in practice uses single ```
// fences; the spec-strict variant would require a stack and isn't worth
// the complexity for this defensive check.
var diaryFenceLine = regexp.MustCompile("^[ \t]{0,3}(```+|~~~+)")

// TruncateMemoryFromMessage truncates the agent's memory using the message
// identified by msgID. The matched message and everything sequentially after
// it in the kojo transcript are removed by index (not timestamp), so two
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
	msgs, err := loadMessages(agentID, 0)
	if err != nil {
		return nil, err
	}
	for _, msg := range msgs {
		if msg.ID != msgID {
			continue
		}
		t, perr := time.Parse(time.RFC3339, msg.Timestamp)
		if perr != nil {
			return nil, fmt.Errorf("parse message %s timestamp %q: %w", msgID, msg.Timestamp, perr)
		}
		return m.truncateMemory(agentID, t, msgID)
	}
	return nil, ErrMessageNotFound
}

// TruncateMemoryAt removes everything in the agent's memory recorded at or
// after `since`, leaving anything strictly before it untouched. Comparison
// is "timestamp >= since" — the boundary record is dropped, not kept.
//
// Targets:
//   - <agentDir>/messages.jsonl (kojo transcript)
//   - ~/.claude/projects/<encoded(agentDir)>/<sessionID>.jsonl (Claude --resume
//     state). After the timestamp filter, trailing assistant / tool_result
//     entries are also dropped so the file ends on a real user message —
//     otherwise --resume on a half-finished turn breaks claude. A file that
//     would be left empty is removed entirely.
//   - <agentDir>/memory/YYYY-MM-DD.md (daily diary). Files whose date is
//     strictly after since's JST date are deleted; the file matching
//     since's JST date keeps only `- HH:MM — ...` bullets timestamped before
//     since's HH:MM.
//
// Untouched: persona, MEMORY.md, memory/projects/*, memory/people/*,
// memory/topics/*, memory/archive/*, credentials, tasks, agent settings.
//
// Acquires the same reset guard as ResetData and waits for any in-flight
// chat — including one-shot chats (Slack / Discord / Group DM), which
// acquireResetGuard alone does NOT cancel — to finish, so cron / notify
// pollers see ErrAgentResetting in the meantime instead of racing the
// file rewrites against memory writes.
func (m *Manager) TruncateMemoryAt(agentID string, since time.Time) (*TruncateMemoryResult, error) {
	return m.truncateMemory(agentID, since, "")
}

// truncateMemory is the shared implementation behind TruncateMemoryAt and
// TruncateMemoryFromMessage. When fromMsgID is non-empty, messages.jsonl is
// truncated by message-ID position (matched ID + tail dropped) instead of
// by timestamp comparison, which lets fromMessageId distinguish between
// messages that share an RFC3339-second timestamp. Claude session JSONL
// and the daily diary still use `since` because they don't carry kojo's
// per-message ID.
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
	// race those writers against our rewrite-rename. Plain ResetData
	// removes everything wholesale and is racy against one-shots too;
	// the mismatch there is a known looseness, not a precedent we want
	// to copy for a partial truncation.
	m.cancelOneShots(agentID)

	if err := m.waitBusyClear(agentID); err != nil {
		return nil, err
	}
	if err := m.waitOneShotClear(agentID); err != nil {
		return nil, err
	}

	res := &TruncateMemoryResult{Since: since.Format(time.RFC3339)}
	dir := agentDir(agentID)

	// 1. messages.jsonl — serialised against appendMessage / rewriteMessages
	//    via transcriptLock.
	tl := transcriptLock(agentID)
	tl.Lock()
	n, err := truncateMessagesJSONL(dir, since, fromMsgID)
	tl.Unlock()
	if err != nil {
		return nil, fmt.Errorf("truncate messages.jsonl: %w", err)
	}
	res.MessagesRemoved = n

	// 2. Claude session JSONL files. Best-effort: a per-file failure is
	//    logged but does not abort the whole truncation, since messages.jsonl
	//    has already been rewritten and bailing now would leave the agent in
	//    an inconsistent half-truncated state. Same rationale as
	//    ResetData's per-step os.Remove warnings.
	entriesRm, filesRm, err := truncateClaudeSessions(dir, since, m.logger)
	if err != nil {
		m.logger.Warn("truncate: claude sessions partial failure", "agent", agentID, "err", err)
	}
	res.ClaudeSessionEntriesRemoved = entriesRm
	res.ClaudeSessionFilesRemoved = filesRm

	// 3. Daily diary (per-day .md files plus their pre-compaction summary
	//    sections).
	dEntries, dFiles, err := truncateDiary(dir, since, m.logger)
	if err != nil {
		m.logger.Warn("truncate: diary partial failure", "agent", agentID, "err", err)
	}
	res.DiaryEntriesRemoved = dEntries
	res.DiaryFilesRemoved = dFiles

	// 4. Drop memory/recent.md if anything diary-side actually changed.
	//    recent.md is a derived rolling summary that PreCompactSummarize
	//    rebuilds from the source diary on the next fire; if we leave a
	//    stale copy in place after dropping diary content, RecentDiarySummary
	//    will keep injecting that removed text into the per-turn volatile
	//    context. Same rationale as ResetData's recent.md removal.
	if dEntries > 0 || dFiles > 0 {
		memDir := filepath.Join(dir, "memory")
		recentPath := filepath.Join(memDir, recentSummaryFile)
		if rerr := os.Remove(recentPath); rerr != nil && !os.IsNotExist(rerr) {
			m.logger.Warn("truncate: remove recent.md", "agent", agentID, "err", rerr)
		} else if rerr == nil {
			// fsync the parent directory so the unlink survives a crash.
			// truncateDiary already syncs memDir for the diary rewrites,
			// but it ran before this remove, so we need a second sync to
			// cover the recent.md deletion.
			if serr := syncDir(memDir); serr != nil {
				m.logger.Warn("truncate: sync memory dir after recent.md remove",
					"agent", agentID, "err", serr)
			}
		}
	}

	// 5. Refresh LastMessage preview from the now-truncated transcript so
	//    the agent list view reflects the change immediately.
	m.refreshLastMessage(agentID)

	// 6. Reindex the memory FTS so subsequent recall searches don't return
	//    snippets from removed diary content. Reindex wipes-and-reinserts
	//    so a reduced source set produces a strictly smaller index. Safe
	//    to run while the busy / one-shot guards are still held.
	if idx := m.getOrOpenIndex(agentID); idx != nil {
		if err := idx.Reindex(agentID); err != nil {
			m.logger.Warn("truncate: memory reindex", "agent", agentID, "err", err)
		}
	}

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

// truncateMessagesJSONL streams messages.jsonl through a temp file. When
// fromMsgID is empty: drops records whose Timestamp parses cleanly and is
// at-or-after `since`. When fromMsgID is non-empty: keeps records up to
// the first occurrence of that ID, then drops the matched record and
// everything after — this is positional (by line order), so two messages
// that share the same RFC3339-second timestamp are not over-deleted. If
// fromMsgID is given but never matches, no records are removed (the file
// is rewritten byte-identical and `removed == 0`).
//
// Records with unparseable timestamps or malformed JSON are kept verbatim
// in the timestamp branch — same forgiving stance as rewriteMessages.
//
// Caller must hold transcriptLock(agentID).
func truncateMessagesJSONL(dir string, since time.Time, fromMsgID string) (int, error) {
	path := filepath.Join(dir, messagesFile)
	src, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(dir, "messages-*.jsonl.tmp")
	if err != nil {
		return 0, err
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
	r := bufio.NewReader(src)
	removed := 0
	dropping := false // once true, every subsequent record is dropped (fromMsgID branch)
	for {
		raw, readErr := r.ReadBytes('\n')
		if len(raw) > 0 {
			line := raw
			if line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
			}
			keep := true
			if dropping {
				// fromMsgID branch — already past the boundary.
				keep = false
				if len(line) > 0 {
					removed++
				}
			} else if len(line) > 0 {
				var msg Message
				if json.Unmarshal(line, &msg) == nil {
					if fromMsgID != "" {
						if msg.ID == fromMsgID {
							keep = false
							removed++
							dropping = true
						}
					} else if t, perr := time.Parse(time.RFC3339, msg.Timestamp); perr == nil && !t.Before(since) {
						keep = false
						removed++
					}
				}
			}
			if keep {
				if _, werr := w.Write(raw); werr != nil {
					cleanup()
					return 0, werr
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			cleanup()
			return 0, readErr
		}
	}
	if err := w.Flush(); err != nil {
		cleanup()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		tmpClosed = true
		os.Remove(tmpPath)
		return 0, err
	}
	tmpClosed = true
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return 0, err
	}
	if err := syncDir(dir); err != nil {
		return removed, err
	}
	return removed, nil
}

// truncateClaudeSessions iterates over every *.jsonl in the agent's Claude
// project dir and runs truncateClaudeSessionFile against each. Returns
// totals across all session files. Missing project dir is not an error
// (gemini-only / codex-only agents simply have nothing to do here).
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
	if err := syncDir(projectDir); err != nil && firstErr == nil {
		firstErr = err
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
			// Preserve blank lines verbatim — Claude doesn't emit them, but
			// a hand-edited or partially-flushed file might have them.
			keep = append(keep, kept{raw: raw})
			continue
		}
		var entry struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Message   json.RawMessage `json:"message"`
		}
		if json.Unmarshal(stripped, &entry) != nil {
			// Unparseable line — keep it; we don't know what it is and
			// dropping it could corrupt the file.
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

	// Only run the integrity trim when the timestamp pass actually
	// removed something. A no-op truncation must be byte-identical to the
	// input; a healthy session that ends on assistant or assistant-text
	// is already a valid --resume target.
	//
	// When the timestamp pass DID remove something, we walk back from the
	// end and drop entries until the tail is one of:
	//   - empty (the file is wiped further down)
	//   - a real user message (clean turn boundary for --resume)
	//   - an assistant entry whose every tool_use has a matching
	//     tool_result still in keep (a complete tool turn)
	//
	// Synthetic-user (tool_result) entries are always trimmed when at the
	// tail — Claude --resume on a session that ends with feedback is
	// awkward, and the timestamp cut is already destructive enough that
	// rewinding to the last real user message is the intuitive "back to
	// here" semantic. Each removal can re-orphan its neighbour, so we
	// recompute the matched map every pass until the tail stops changing.
	// The loop is bounded by len(keep) — each pass either drops one
	// entry or breaks.
	if timestampDropped > 0 {
		for {
			n := len(keep)
			if n == 0 {
				break
			}
			// matched: tool_use_ids cited by some kept tool_result. Used
			// to decide whether a trailing assistant has been answered.
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
				// Trailing synthetic-user (tool_result feedback) is
				// always dropped — see comment above.
				drop = true
			case last.typ == "assistant" && len(last.toolUseIDs) > 0:
				// Trailing assistant turn that issued tool_use blocks.
				// Drop it only if at least one tool_use is not yet
				// matched by a downstream tool_result — meaning the
				// matching synthetic-user was already cut.
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

	// Empty result → delete the file. Match clearClaudeSession's behaviour
	// so the next chat starts a fresh session via --session-id.
	allBlank := true
	for _, k := range keep {
		if k.typ != "" {
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
func truncateDiary(agentDirPath string, since time.Time, logger *slog.Logger) (entriesRemoved, filesRemoved int, err error) {
	memDir := filepath.Join(agentDirPath, "memory")
	entries, derr := os.ReadDir(memDir)
	if derr != nil {
		if os.IsNotExist(derr) {
			return 0, 0, nil
		}
		return 0, 0, derr
	}

	jstSince := since.In(jst)
	sinceDate := jstSince.Format("2006-01-02")
	sinceMinutes := jstSince.Hour()*60 + jstSince.Minute()

	var firstErr error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".md")
		// Diary files are named YYYY-MM-DD.md. Files that don't parse
		// (e.g. recent.md, summary.md) are not diary entries — skip.
		fileDate, perr := time.ParseInLocation("2006-01-02", stem, jst)
		if perr != nil {
			continue
		}
		path := filepath.Join(memDir, e.Name())
		fileDateStr := fileDate.Format("2006-01-02")

		if fileDateStr > sinceDate {
			if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
				logger.Warn("truncate diary remove", "path", path, "err", rerr)
				if firstErr == nil {
					firstErr = rerr
				}
				continue
			}
			filesRemoved++
			continue
		}
		if fileDateStr < sinceDate {
			continue
		}
		// Same-date file: drop bullets with HH:MM >= sinceMinutes.
		removed, rerr := trimDiaryFileEntries(path, sinceMinutes)
		entriesRemoved += removed
		if rerr != nil {
			logger.Warn("truncate diary rewrite", "path", path, "err", rerr)
			if firstErr == nil {
				firstErr = rerr
			}
		}
	}
	if err := syncDir(memDir); err != nil && firstErr == nil {
		firstErr = err
	}
	return entriesRemoved, filesRemoved, firstErr
}

// trimDiaryFileEntries rewrites a single diary file in place, dropping
// per-time entries whose minutes-of-day is >= sinceMinutes. Two entry
// shapes are handled:
//
//  1. `- HH:MM — ...` bullet lines (possibly with hanging-indent
//     continuation lines following).
//  2. `## Pre-compaction summary (HH:MM)` sections written by
//     PreCompactSummarize. The whole section — header through to the
//     next ATX heading or EOF — is dropped.
//
// Lines outside both shapes are passed through unchanged.
//
// "Continuation" handling for bullets: once we decide to drop a bullet,
// we keep dropping until a blank line or a non-indented, non-bullet line.
// This stops orphaned indented text from surviving alone after its
// leading bullet has been removed.
func trimDiaryFileEntries(path string, sinceMinutes int) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	lines := strings.SplitAfter(string(data), "\n")

	var out strings.Builder
	out.Grow(len(data))
	removed := 0
	// dropMode is one of:
	//   "" — emit normally
	//   "bullet" — dropping the hanging-indent body of a removed `- HH:MM` bullet
	//   "section" — dropping a `## Pre-compaction summary (HH:MM)` section body
	dropMode := ""
	// inFence tracks whether we're currently inside a ``` / ~~~ fenced
	// code block. ATX headings that appear inside a fenced block are
	// content, not headings, and must not terminate a section drop.
	inFence := false
	for _, line := range lines {
		// Toggle fence state on every fence delimiter we encounter,
		// regardless of dropMode. Drop-mode flow below still gets to
		// decide whether to emit or skip the line. The fence tracking
		// itself runs unconditionally so a code block that opens inside
		// a kept section and closes inside a dropped one (or vice versa)
		// doesn't leak its state into surrounding markdown.
		if diaryFenceLine.MatchString(strings.TrimRight(line, "\r\n")) {
			inFence = !inFence
		}
		if dropMode == "section" {
			// A new ATX heading at the *top* level (outside any fenced
			// code block) terminates the section. Headings inside a
			// fenced block are content. Anything else (blank lines,
			// prose, code) is section body and gets dropped with it.
			if !inFence && diaryAnyHeading.MatchString(line) {
				dropMode = ""
				// Fall through to the dispatch below so this heading
				// itself can start a new drop if it's a matching
				// pre-compaction header.
			} else {
				continue
			}
		}
		// Inside a fenced code block, treat the line as plain content
		// regardless of what it looks like — a kept section may contain
		// markdown samples that include `## Pre-compaction summary (...)`
		// or `- HH:MM` strings that we must not interpret as live diary
		// entries to drop. The bullet drop-mode (which has its own
		// indentation-based termination) is also paused inside fences;
		// the fence delimiter itself acts as a clear terminator.
		if inFence {
			if dropMode == "bullet" {
				dropMode = ""
			}
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
			// Older pre-compact section — keep header, switch out of any
			// bullet-drop in progress.
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
		if dropMode == "bullet" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.TrimSpace(trimmed) == "" {
				dropMode = ""
				out.WriteString(line)
				continue
			}
			if len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t') {
				continue
			}
			dropMode = ""
			out.WriteString(line)
			continue
		}
		out.WriteString(line)
	}

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
	if _, werr := tmp.WriteString(out.String()); werr != nil {
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

// collectToolUseIDs returns the `id` of every tool_use content block found
// inside a Claude assistant message payload. Used by the session-integrity
// trim to detect when a kept assistant entry has lost the tool_results it
// expects downstream.
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
// input is two ASCII digits, so the only way this errors is on programmer
// misuse — but returning the error keeps the parsing local and avoids
// pulling strconv into the hot loop.
func atoi2(s string) (int, error) {
	if len(s) != 2 || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return 0, fmt.Errorf("not a 2-digit number: %q", s)
	}
	return int(s[0]-'0')*10 + int(s[1]-'0'), nil
}
