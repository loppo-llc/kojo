package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Background-subagent activity visibility (Option A live tail + Option C backfill).
//
// A Task tool call launched with run_in_background keeps running after the
// parent turn's "result" event finalizes the turn accumulator — its later
// events can no longer attach to the parent ToolUse.Children via the normal
// stream path, so they go invisible. The Claude CLI writes each background
// subagent's own transcript to
//
//	<projectDir>/<sessionID>/subagents/agent-<agentID>.jsonl
//
// alongside agent-<agentID>.meta.json, whose toolUseId maps back to the Task
// tool_use kojo already renders. This file tails those child JSONLs while the
// persistent session is alive, converts appended lines into ChatEvents (tagged
// with ParentToolUseID = the Task tool_use id) and durable ToolUse children,
// and hands them to the Manager to (a) push live to chat watchers and (b)
// durably attach to the owning persisted message.

const (
	// subagentPollInterval is how often the tailer re-scans the subagents dir
	// for child-file growth. Polling (no fsnotify) keeps the dependency
	// surface flat; 1.5s is a good latency/overhead trade for a background
	// activity feed.
	subagentPollInterval = 1500 * time.Millisecond

	// subagentMaxChildren caps how many children a single background subagent
	// contributes, bounding both the persisted message row and live pushes.
	// Consistent with the flat, one-level-deep Children model the in-turn
	// subagent accumulator produces.
	subagentMaxChildren = 1000

	// subagentScanBytesPerTick bounds how much of a single child file is read
	// per poll so a fast-writing subagent can't force an unbounded read into
	// memory. Any remainder is picked up on the next tick (offset advances
	// only past fully-read lines).
	subagentScanBytesPerTick = 4 * 1024 * 1024
)

// subagentActivity is one batch of newly-observed background-subagent output
// for a single Task tool_use. Events are the live-push representation; Children
// is the durable representation merged onto the owning message's ToolUse.
type subagentActivity struct {
	ToolUseID string
	Events    []ChatEvent
	Children  []ToolUse
}

// subagentActivityFunc consumes a batch of background-subagent activity. Set by
// the Manager; nil disables surfacing (events are still parsed but dropped).
type subagentActivityFunc func(agentID string, act subagentActivity)

// subagentTranscriptLine is one line of a subagent's own JSONL transcript. It
// reuses claudeContentBlock so tool_use / tool_result / text extraction matches
// the in-turn stream path exactly. isSidechain marks subagent (non-main) lines;
// uuid is the per-entry id used for idempotent de-duplication.
type subagentTranscriptLine struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	IsSidechain bool   `json:"isSidechain"`
	Message     struct {
		Content []claudeContentBlock `json:"content"`
	} `json:"message"`
}

// readSubagentMeta returns the Task tool_use id a subagent transcript belongs
// to, read from its sibling agent-<id>.meta.json. Returns "" (no error surfaced
// beyond that) when the meta file is missing/unreadable/lacks the field — the
// caller then skips the child until its meta lands.
func readSubagentMeta(metaPath string) string {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return ""
	}
	var meta struct {
		ToolUseID string `json:"toolUseId"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return ""
	}
	return meta.ToolUseID
}

// subagentEventsFromLine converts one subagent transcript line into live
// ChatEvents (tagged with parentToolUseID) and durable ToolUse children.
// idx-derived stable ids let text bubbles de-dupe by id on re-scan, so the
// merge stays idempotent across a process restart that re-reads from offset 0.
//
// Returns (nil, nil) for lines that carry no renderable content (summaries,
// bookkeeping, unparseable JSON).
func subagentEventsFromLine(line []byte, parentToolUseID string) (events []ChatEvent, children []ToolUse) {
	var entry subagentTranscriptLine
	if err := json.Unmarshal(line, &entry); err != nil {
		return nil, nil
	}
	switch entry.Type {
	case "assistant":
		for i, block := range entry.Message.Content {
			switch block.Type {
			case "text":
				if block.Text == "" {
					continue
				}
				events = append(events, ChatEvent{
					Type:            "text",
					Delta:           block.Text,
					ParentToolUseID: parentToolUseID,
				})
				children = append(children, ToolUse{
					ID:   textBubbleID(entry.UUID, i),
					Text: block.Text,
				})
			case "tool_use":
				input := string(block.Input)
				events = append(events, ChatEvent{
					Type:            "tool_use",
					ToolUseID:       block.ID,
					ToolName:        block.Name,
					ToolInput:       input,
					ParentToolUseID: parentToolUseID,
				})
				children = append(children, ToolUse{
					ID:    block.ID,
					Name:  block.Name,
					Input: input,
				})
			}
		}
	case "user":
		for _, block := range entry.Message.Content {
			if block.Type != "tool_result" || block.ToolUseID == "" {
				continue
			}
			output := block.contentText()
			events = append(events, ChatEvent{
				Type:            "tool_result",
				ToolUseID:       block.ToolUseID,
				ToolOutput:      output,
				ParentToolUseID: parentToolUseID,
			})
			// A tool_result mutates the matching tool_use child's Output
			// rather than adding a new child; carry it as an Output-only
			// entry that mergeSubagentChildren folds onto the existing id.
			children = append(children, ToolUse{ID: block.ToolUseID, Output: output})
		}
	}
	return events, children
}

// textBubbleID derives a stable child id for a subagent text block so re-scans
// (e.g. after a restart re-reads the file from the start) de-dupe idempotently.
func textBubbleID(uuid string, idx int) string {
	if uuid == "" {
		return ""
	}
	return "txt:" + uuid + ":" + fmt.Sprint(idx)
}

// mergeSubagentChildren folds incoming children into existing, idempotently:
//   - a tool_use / text child with a new id is appended (subject to the cap);
//   - a child whose id already exists updates the stored entry's newly-non-empty
//     fields (Output from a later tool_result, a late-arriving Name/Input);
//   - entries without an id (unattributable) are dropped to preserve idempotency.
//
// Returns the merged slice and whether anything changed (so callers can skip a
// durable write when a re-scan produced no net-new content).
func mergeSubagentChildren(existing, incoming []ToolUse) (merged []ToolUse, changed bool) {
	merged = existing
	idx := make(map[string]int, len(existing))
	for i, c := range existing {
		if c.ID != "" {
			idx[c.ID] = i
		}
	}
	for _, in := range incoming {
		if in.ID == "" {
			continue
		}
		if at, ok := idx[in.ID]; ok {
			cur := merged[at]
			if in.Output != "" && in.Output != cur.Output {
				cur.Output = in.Output
				changed = true
			}
			if cur.Name == "" && in.Name != "" {
				cur.Name = in.Name
				changed = true
			}
			if cur.Input == "" && in.Input != "" {
				cur.Input = in.Input
				changed = true
			}
			if cur.Text == "" && in.Text != "" {
				cur.Text = in.Text
				changed = true
			}
			merged[at] = cur
			continue
		}
		// An Output-only entry with no matching tool_use is a tool_result that
		// arrived before (or without) its tool_use line — skip rather than add a
		// nameless, inputless child.
		if in.Name == "" && in.Text == "" && in.Output != "" {
			continue
		}
		if len(merged) >= subagentMaxChildren {
			continue
		}
		idx[in.ID] = len(merged)
		merged = append(merged, in)
		changed = true
	}
	return merged, changed
}

// scanAppendedLines reads complete (newline-terminated) lines from f starting
// at fromOffset, up to maxBytes, and returns them along with the offset just
// past the last complete line. A trailing partial line (still being written) is
// left unconsumed so it is re-read whole on the next tick.
func scanAppendedLines(f io.ReaderAt, fromOffset, maxBytes int64) (lines [][]byte, newOffset int64) {
	sr := io.NewSectionReader(f, fromOffset, maxBytes)
	br := bufio.NewReader(sr)
	newOffset = fromOffset
	for {
		chunk, err := br.ReadBytes('\n')
		if err != nil {
			// A non-nil err means no terminating newline was found: this is a
			// partial trailing line still being written, or EOF / the maxBytes
			// boundary. Leave it unconsumed so it is re-read whole next tick;
			// offset stays at the end of the last complete line.
			break
		}
		newOffset += int64(len(chunk))
		trimmed := trimRightCR(chunk[:len(chunk)-1])
		if len(trimmed) > 0 {
			// Copy: bufio reuses its buffer across ReadBytes calls.
			cp := make([]byte, len(trimmed))
			copy(cp, trimmed)
			lines = append(lines, cp)
		}
	}
	return lines, newOffset
}

func trimRightCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}

// subagentFileState is the per-child-file tail cursor: the byte offset already
// consumed and the resolved Task tool_use id from meta.json (cached so we don't
// re-read the meta every tick once known).
type subagentFileState struct {
	offset    int64
	toolUseID string
}

// subagentTailer polls a persistent session's subagents directory and surfaces
// background-subagent output. One tailer is owned by one claudeSession and
// stops when that session's process context is cancelled (no goroutine leak).
type subagentTailer struct {
	agentID string
	dir     string // agent working dir (agentDir); project dir is derived from it
	logger  *slog.Logger

	// sessionIDFn returns the live session id (authoritative once a result
	// event lands, else the deterministic id). Called each tick so a session
	// that learns its id mid-life starts tailing the right directory.
	sessionIDFn func() string
	emit        subagentActivityFunc
	interval    time.Duration

	// scanMu serializes whole scans so the periodic poll and the turn-boundary
	// backfill kick can't run concurrently and double-read the same appended
	// bytes (which would double-emit live text bubbles).
	scanMu sync.Mutex

	mu    sync.Mutex
	files map[string]*subagentFileState // keyed by child file absolute path
}

func newSubagentTailer(agentID, dir string, logger *slog.Logger, sessionIDFn func() string, emit subagentActivityFunc) *subagentTailer {
	return &subagentTailer{
		agentID:     agentID,
		dir:         dir,
		logger:      logger,
		sessionIDFn: sessionIDFn,
		emit:        emit,
		interval:    subagentPollInterval,
		files:       make(map[string]*subagentFileState),
	}
}

// run polls until ctx is cancelled. Tied to the session's procCtx so it dies
// with the process.
func (t *subagentTailer) run(ctx context.Context) {
	if t.emit == nil {
		return // nothing to surface to; skip the whole poll loop
	}
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	// One immediate scan so a subagent that already finished by the time the
	// session spawned (recovery/backfill) is picked up without a full interval
	// of latency.
	t.scanOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.scanOnce()
		}
	}
}

// subagentsDir resolves the directory holding this session's child transcripts,
// tolerating both the per-session layout (<projectDir>/<sessionID>/subagents)
// and a flat one (<projectDir>/subagents), and the macOS symlink-resolved cwd
// the CLI actually encodes. Returns "" when none exists yet.
func (t *subagentTailer) subagentsDir() string {
	sid := ""
	if t.sessionIDFn != nil {
		sid = t.sessionIDFn()
	}
	var bases []string
	seen := map[string]bool{}
	addBase := func(d string) {
		if abs, err := filepath.Abs(d); err == nil {
			pd := claudeProjectDir(abs)
			if !seen[pd] {
				seen[pd] = true
				bases = append(bases, pd)
			}
		}
	}
	addBase(t.dir)
	if resolved, err := filepath.EvalSymlinks(t.dir); err == nil {
		addBase(resolved)
	}
	for _, pd := range bases {
		if sid != "" {
			if d := filepath.Join(pd, sid, "subagents"); dirExists(d) {
				return d
			}
		}
		if d := filepath.Join(pd, "subagents"); dirExists(d) {
			return d
		}
	}
	return ""
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// scanOnce walks the subagents dir once, tailing each agent-*.jsonl from its
// stored offset and emitting any new activity. Idempotent: an unchanged file
// produces no offset advance and no emit.
func (t *subagentTailer) scanOnce() {
	t.scanMu.Lock()
	defer t.scanMu.Unlock()
	dir := t.subagentsDir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// Deterministic order keeps emitted batches stable across ticks/tests.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "agent-") || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		t.scanFile(dir, name)
	}
}

func (t *subagentTailer) scanFile(dir, name string) {
	path := filepath.Join(dir, name)

	t.mu.Lock()
	st := t.files[path]
	if st == nil {
		st = &subagentFileState{}
		t.files[path] = st
	}
	toolUseID := st.toolUseID
	offset := st.offset
	t.mu.Unlock()

	if toolUseID == "" {
		metaPath := filepath.Join(dir, strings.TrimSuffix(name, ".jsonl")+".meta.json")
		toolUseID = readSubagentMeta(metaPath)
		if toolUseID == "" {
			return // meta not ready yet; retry next tick
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	if info.Size() <= offset {
		// Nothing new (or the file was truncated/rotated — reset to re-read).
		if info.Size() < offset {
			offset = 0
		} else {
			t.mu.Lock()
			t.files[path].toolUseID = toolUseID
			t.mu.Unlock()
			return
		}
	}

	lines, newOffset := scanAppendedLines(f, offset, subagentScanBytesPerTick)

	var events []ChatEvent
	var children []ToolUse
	for _, line := range lines {
		evs, chs := subagentEventsFromLine(line, toolUseID)
		events = append(events, evs...)
		children = append(children, chs...)
	}

	t.mu.Lock()
	t.files[path].offset = newOffset
	t.files[path].toolUseID = toolUseID
	t.mu.Unlock()

	if len(events) == 0 && len(children) == 0 {
		return
	}
	t.emit(t.agentID, subagentActivity{
		ToolUseID: toolUseID,
		Events:    events,
		Children:  children,
	})
}
