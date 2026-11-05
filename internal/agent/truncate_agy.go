package agent

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// agy (Antigravity CLI) keeps a conversation in two places:
//
//	~/.gemini/antigravity-cli/conversations/<id>.db
//	  steps(idx, metadata, step_payload, ...)  one row per step; this is
//	                       what --conversation <id> replays into the
//	                       model's context.
//	  executor_metadata    one row per turn; top-level field 3 is the
//	                       turn's last step idx, field 9 its execution id.
//	  gen_metadata         one row per model call; field 4 is the
//	                       execution id of the turn that made it.
//	~/.gemini/antigravity-cli/brain/<id>/.system_generated/
//	  logs/transcript.jsonl, logs/transcript_full.jsonl and their
//	  logs/chunks/<name>/NNNNNNNN.jsonl copies — one JSON record per
//	  step keyed by "step_index"; steps/<idx>/ — per-step outputs.
//
// Each step's metadata (protobuf) carries the turn's execution id at
// top-level field 12 and google.protobuf.Timestamp values at fields 1
// (created), 6, 7, 8 and 32 (progress / completion). Grouping consecutive
// steps by execution id gives agy's own turn boundaries, so a rewind can
// cut the conversation between turns instead of dropping it.
//
// Verified against agy: deleting the steps at-or-after a turn's first idx
// (plus the turn's executor/gen rows and its transcript lines) and then
// resuming with --conversation makes the model forget the dropped turns,
// and the next turn's steps continue at the cut idx.

// agyCLIDir is agy's per-user state root.
func agyCLIDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli")
}

// agyBrainDir is the per-conversation artifact directory.
func agyBrainDir(convID string) string {
	root := agyCLIDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "brain", convID)
}

// protoVarintField returns the first varint field num in msg.
func protoVarintField(msg []byte, num uint64) (uint64, bool) {
	for len(msg) > 0 {
		key, n := binary.Uvarint(msg)
		if n <= 0 {
			return 0, false
		}
		msg = msg[n:]
		switch key & 7 {
		case 0:
			v, n := binary.Uvarint(msg)
			if n <= 0 {
				return 0, false
			}
			msg = msg[n:]
			if key>>3 == num {
				return v, true
			}
		case 1:
			if len(msg) < 8 {
				return 0, false
			}
			msg = msg[8:]
		case 5:
			if len(msg) < 4 {
				return 0, false
			}
			msg = msg[4:]
		case 2:
			l, n := binary.Uvarint(msg)
			if n <= 0 || l > uint64(len(msg)-n) {
				return 0, false
			}
			msg = msg[n+int(l):]
		default:
			return 0, false
		}
	}
	return 0, false
}

// protoTimestampField decodes a google.protobuf.Timestamp stored at
// length-delimited field num.
func protoTimestampField(msg []byte, num uint64) (time.Time, bool) {
	inner := protoLenField(msg, num)
	if inner == nil {
		return time.Time{}, false
	}
	sec, ok := protoVarintField(inner, 1)
	if !ok || sec == 0 || sec > 1<<40 {
		return time.Time{}, false
	}
	nanos, _ := protoVarintField(inner, 2)
	if nanos >= 1e9 {
		nanos = 0
	}
	return time.Unix(int64(sec), int64(nanos)).UTC(), true
}

// agyStepTimeFields are the step-metadata fields holding a Timestamp.
var agyStepTimeFields = []uint64{1, 6, 7, 8, 32}

type agyStepInfo struct {
	idx    int
	execID string
	// latest is the newest timestamp found in the step's metadata;
	// zero when it carried none.
	latest time.Time
}

func parseAgyStepMetadata(idx int, md []byte) agyStepInfo {
	s := agyStepInfo{idx: idx}
	if id := protoLenField(md, 12); id != nil {
		s.execID = string(id)
	}
	for _, f := range agyStepTimeFields {
		if t, ok := protoTimestampField(md, f); ok && t.After(s.latest) {
			s.latest = t
		}
	}
	return s
}

// findAgyCut returns the idx of the first step of the first turn that
// finished after cutoff, or found=false when none did. A turn is a run of consecutive steps sharing an execution id (a
// step without one stays with the turn before it); it finished at the
// newest timestamp among its steps. A turn with no timestamp at all
// cannot be placed and is treated as not finished, so a rewind never
// leaves unplaceable context behind.
//
// cutoff is the creation time of the last kojo message that survives the
// truncate. kojo stores a turn's reply only after agy has finished it,
// and starts the next turn only after storing the message that triggers
// it, so the turns that produced surviving messages end before cutoff
// and the first removed turn ends after it — even when the removed pivot
// is the reply itself. kojo's created_at has millisecond resolution, so a
// turn counts as finished after cutoff only when it ended in a later
// millisecond: one ending within cutoff's millisecond precedes the
// message stored then.
func findAgyCut(steps []agyStepInfo, cutoff time.Time) (cut int, found bool) {
	i := 0
	for i < len(steps) {
		start := i
		end := steps[i].latest
		id := steps[i].execID
		i++
		for i < len(steps) && (steps[i].execID == id || steps[i].execID == "") {
			if steps[i].latest.After(end) {
				end = steps[i].latest
			}
			i++
		}
		if end.IsZero() || end.Truncate(time.Millisecond).After(cutoff) {
			return steps[start].idx, true
		}
	}
	return 0, false
}

// agyTruncateResult reports what truncateAgyMainConversation changed.
type agyTruncateResult struct {
	stepsRemoved int
	// cleared is true when the whole conversation fell past the cut and
	// the main conversation ref was dropped instead, so the next turn
	// starts a fresh conversation bootstrapped from the transcript.
	cleared bool
}

// truncateAgyMainConversation trims the agent's main agy conversation
// (SessionKey "") to the turns that finished before cutoff. hasSurvivor
// is false when the kojo transcript is now empty, which drops the whole
// conversation. Thread / keyed conversations are left alone: they back
// their own transcripts, which a main-chat rewind does not touch.
//
// The caller must guarantee no agy turn of the agent is running on the
// main conversation (truncateMemory waits busy and one-shot chats out).
func truncateAgyMainConversation(agentID string, cutoff time.Time, hasSurvivor bool) (agyTruncateResult, error) {
	var res agyTruncateResult
	dir := agentDir(agentID)
	convID := readAgyConversationID(dir, "")
	if convID == "" {
		return res, nil
	}
	clear := func() (agyTruncateResult, error) {
		if err := os.Remove(agyConversationRefPath(dir, "")); err != nil && !os.IsNotExist(err) {
			return res, err
		}
		res.cleared = true
		return res, nil
	}
	if !hasSurvivor {
		return clear()
	}
	convDir := agyConversationsDir()
	if convDir == "" {
		return res, errors.New("agy conversation store not found")
	}
	dbPath := filepath.Join(convDir, convID+".db")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			// agy lost it: drop the ref so the caller clears whatever
			// else of it is left rather than keeping it as trimmed.
			return clear()
		}
		return res, err
	}

	db, err := sql.Open("sqlite", codexSQLiteDSN(dbPath, false))
	if err != nil {
		return res, fmt.Errorf("open agy conversation: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Read and delete under one write transaction (_txlock=immediate) so
	// the cut is computed from exactly the rows it removes.
	tx, err := db.Begin()
	if err != nil {
		return res, fmt.Errorf("begin agy truncate: %w", err)
	}
	defer tx.Rollback()
	steps, err := readAgySteps(tx)
	if err != nil {
		return res, err
	}
	cut, found := findAgyCut(steps, cutoff)
	if !found {
		return res, nil
	}
	if cut <= steps[0].idx {
		// Nothing conversational survives: start over rather than
		// resume an empty trajectory.
		_ = tx.Rollback()
		return clear()
	}

	dropped := map[string]bool{}
	removed := 0
	for _, s := range steps {
		if s.idx >= cut {
			removed++
			if s.execID != "" {
				dropped[s.execID] = true
			}
		}
	}
	if err := deleteAgyStepsFrom(tx, cut, dropped); err != nil {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit agy truncate: %w", err)
	}
	res.stepsRemoved = removed
	// The database is what agy replays; the brain logs only mirror it,
	// so a failure there is reported but does not undo the cut.
	if err := trimAgyBrain(agyBrainDir(convID), cut); err != nil {
		return res, fmt.Errorf("trim agy brain logs: %w", err)
	}
	return res, nil
}

func readAgySteps(tx *sql.Tx) ([]agyStepInfo, error) {
	rows, err := tx.Query(`SELECT idx, metadata FROM steps ORDER BY idx`)
	if err != nil {
		return nil, fmt.Errorf("read agy steps: %w", err)
	}
	defer rows.Close()
	var out []agyStepInfo
	for rows.Next() {
		var idx int
		var md []byte
		if err := rows.Scan(&idx, &md); err != nil {
			return nil, fmt.Errorf("scan agy step: %w", err)
		}
		out = append(out, parseAgyStepMetadata(idx, md))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read agy steps: %w", err)
	}
	return out, nil
}

// deleteAgyStepsFrom removes the steps at-or-after cut together with the
// executor (per-turn) and gen (per-model-call) rows of the dropped turns,
// inside tx.
func deleteAgyStepsFrom(tx *sql.Tx, cut int, dropped map[string]bool) error {
	if _, err := tx.Exec(`DELETE FROM steps WHERE idx >= ?`, cut); err != nil {
		return fmt.Errorf("delete agy steps: %w", err)
	}
	execDrop, err := collectAgyMetaRows(tx, "executor_metadata", func(data []byte) bool {
		if last, ok := protoVarintField(data, 3); ok && last >= uint64(cut) {
			return true
		}
		id := protoLenField(data, 9)
		return id != nil && dropped[string(id)]
	})
	if err != nil {
		return err
	}
	genDrop, err := collectAgyMetaRows(tx, "gen_metadata", func(data []byte) bool {
		id := protoLenField(data, 4)
		return id != nil && dropped[string(id)]
	})
	if err != nil {
		return err
	}
	for _, d := range []struct {
		table string
		idxs  []int
	}{{"executor_metadata", execDrop}, {"gen_metadata", genDrop}} {
		for _, idx := range d.idxs {
			if _, err := tx.Exec(`DELETE FROM `+d.table+` WHERE idx = ?`, idx); err != nil {
				return fmt.Errorf("delete agy %s row: %w", d.table, err)
			}
		}
	}
	return nil
}

// collectAgyMetaRows returns the idx of every row of table whose data
// blob matches drop. A missing table (older agy) matches nothing.
func collectAgyMetaRows(tx *sql.Tx, table string, drop func([]byte) bool) ([]int, error) {
	rows, err := tx.Query(`SELECT idx, data FROM ` + table)
	if err != nil {
		var exists int
		if qerr := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&exists); qerr == nil && exists == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("read agy %s: %w", table, err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var idx int
		var data []byte
		if err := rows.Scan(&idx, &data); err != nil {
			return nil, fmt.Errorf("scan agy %s: %w", table, err)
		}
		if drop(data) {
			out = append(out, idx)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read agy %s: %w", table, err)
	}
	return out, nil
}

// trimAgyBrain drops the step records at-or-after cut from every
// transcript log of the conversation and removes the matching per-step
// output directories. Lines that do not parse are kept.
func trimAgyBrain(brainDir string, cut int) error {
	if brainDir == "" {
		return nil
	}
	gen := filepath.Join(brainDir, ".system_generated")
	if _, err := os.Stat(gen); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	keep := func(raw []byte) bool {
		var rec struct {
			StepIndex *int `json:"step_index"`
		}
		if json.Unmarshal(bytes.TrimSpace(raw), &rec) != nil || rec.StepIndex == nil {
			return true
		}
		return *rec.StepIndex < cut
	}
	var errs []error
	// listJSONL returns the sorted *.jsonl files in dir with the given
	// name prefix; a missing dir lists nothing.
	listJSONL := func(dir, prefix string) []string {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			return nil
		}
		var out []string
		for _, e := range entries {
			if n := e.Name(); !e.IsDir() && strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".jsonl") {
				out = append(out, filepath.Join(dir, n))
			}
		}
		sort.Strings(out)
		return out
	}
	logs := filepath.Join(gen, "logs")
	for _, f := range listJSONL(logs, "transcript") {
		if _, err := filterJSONLLines(f, keep); err != nil {
			errs = append(errs, err)
		}
	}
	chunks := filepath.Join(logs, "chunks")
	chunkDirs, err := os.ReadDir(chunks)
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	for _, cd := range chunkDirs {
		if !cd.IsDir() || !strings.HasPrefix(cd.Name(), "transcript") {
			continue
		}
		for i, f := range listJSONL(filepath.Join(chunks, cd.Name()), "") {
			kept, err := filterJSONLLines(f, keep)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			// A chunk emptied by the cut is removed, except the first:
			// agy appends to the newest chunk it finds.
			if kept == 0 && i > 0 {
				if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
					errs = append(errs, err)
				}
			}
		}
	}
	stepDirs, err := os.ReadDir(filepath.Join(gen, "steps"))
	if err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	for _, e := range stepDirs {
		n, err := strconv.Atoi(e.Name())
		if err != nil || n < cut {
			continue
		}
		if err := os.RemoveAll(filepath.Join(gen, "steps", e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
