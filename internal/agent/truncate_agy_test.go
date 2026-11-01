package agent

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Minimal protobuf writers for building agy fixtures.
func pbVarint(b []byte, field, v uint64) []byte {
	b = binary.AppendUvarint(b, field<<3)
	return binary.AppendUvarint(b, v)
}

func pbBytes(b []byte, field uint64, v []byte) []byte {
	b = binary.AppendUvarint(b, field<<3|2)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

func pbTimestamp(b []byte, field uint64, t time.Time) []byte {
	var ts []byte
	ts = pbVarint(ts, 1, uint64(t.Unix()))
	ts = pbVarint(ts, 2, uint64(t.Nanosecond()))
	return pbBytes(b, field, ts)
}

// agyFixtureStep is one step of a fixture conversation.
type agyFixtureStep struct {
	exec    string
	created time.Time
	done    time.Time // zero: no completion timestamp
}

func agyStepMetadata(s agyFixtureStep) []byte {
	var md []byte
	if !s.created.IsZero() {
		md = pbTimestamp(md, 1, s.created)
	}
	md = pbVarint(md, 3, 2)
	if !s.done.IsZero() {
		md = pbTimestamp(md, 8, s.done)
	}
	if s.exec != "" {
		md = pbBytes(md, 12, []byte(s.exec))
	}
	return md
}

const agyFixtureSchema = "" +
	"CREATE TABLE `steps` (`idx` integer,`step_type` integer NOT NULL DEFAULT 0,`status` integer NOT NULL DEFAULT 0,`metadata` blob,`step_payload` blob,PRIMARY KEY (`idx`));" +
	"CREATE TABLE `gen_metadata` (`idx` integer,`data` blob,`size` integer NOT NULL DEFAULT 0,PRIMARY KEY (`idx`));" +
	"CREATE TABLE `executor_metadata` (`idx` integer,`data` blob,PRIMARY KEY (`idx`));"

// writeAgyFixture builds conversation convID under $HOME: three turns of
// two steps (user input, response), one executor row per turn, one gen
// row per turn, brain transcripts (the chunked copy split across two
// files) and per-step output dirs. It returns the turn end times.
func writeAgyFixture(t *testing.T, convID string, base time.Time) (ends []time.Time) {
	t.Helper()
	convDir := agyConversationsDir()
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(convDir, convID+".db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(agyFixtureSchema); err != nil {
		t.Fatal(err)
	}
	logs := filepath.Join(agyBrainDir(convID), ".system_generated", "logs")
	for _, d := range []string{logs, filepath.Join(logs, "chunks", "transcript")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var transcript, chunk0, chunk1 strings.Builder
	idx := 0
	for turn := 0; turn < 3; turn++ {
		exec := fmt.Sprintf("exec-%d", turn)
		start := base.Add(time.Duration(turn) * time.Minute)
		end := start.Add(20 * time.Second)
		ends = append(ends, end)
		for k, s := range []agyFixtureStep{
			{exec: exec, created: start},
			{exec: exec, created: start, done: end},
		} {
			if _, err := db.Exec(`INSERT INTO steps(idx, step_type, metadata) VALUES (?, ?, ?)`, idx, 14+k, agyStepMetadata(s)); err != nil {
				t.Fatal(err)
			}
			line, _ := json.Marshal(map[string]any{"step_index": idx, "content": fmt.Sprintf("turn%d-step%d", turn, k)})
			transcript.Write(append(line, '\n'))
			if idx < 4 {
				chunk0.Write(append(line, '\n'))
			} else {
				chunk1.Write(append(line, '\n'))
			}
			out := filepath.Join(agyBrainDir(convID), ".system_generated", "steps", fmt.Sprint(idx))
			if err := os.MkdirAll(out, 0o755); err != nil {
				t.Fatal(err)
			}
			idx++
		}
		var ex []byte
		ex = pbVarint(ex, 1, 4)
		ex = pbVarint(ex, 3, uint64(idx-1))
		ex = pbBytes(ex, 9, []byte(exec))
		if _, err := db.Exec(`INSERT INTO executor_metadata(idx, data) VALUES (?, ?)`, turn, ex); err != nil {
			t.Fatal(err)
		}
		var gen []byte
		gen = pbBytes(gen, 4, []byte(exec))
		if _, err := db.Exec(`INSERT INTO gen_metadata(idx, data) VALUES (?, ?)`, turn, gen); err != nil {
			t.Fatal(err)
		}
	}
	transcript.WriteString("not json\n")
	for path, body := range map[string]string{
		filepath.Join(logs, "transcript.jsonl"):                       transcript.String(),
		filepath.Join(logs, "chunks", "transcript", "00000000.jsonl"): chunk0.String(),
		filepath.Join(logs, "chunks", "transcript", "00000001.jsonl"): chunk1.String(),
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ends
}

func agyFixtureCounts(t *testing.T, convID string) (steps, execs, gens int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(agyConversationsDir(), convID+".db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []struct {
		table string
		n     *int
	}{{"steps", &steps}, {"executor_metadata", &execs}, {"gen_metadata", &gens}} {
		if err := db.QueryRow(`SELECT count(*) FROM ` + q.table).Scan(q.n); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func agyFixtureStepIndexes(t *testing.T, path string) []int {
	t.Helper()
	lines, err := readJSONLLines(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, l := range lines {
		var rec struct {
			StepIndex *int `json:"step_index"`
		}
		if json.Unmarshal(l, &rec) == nil && rec.StepIndex != nil {
			out = append(out, *rec.StepIndex)
		}
	}
	return out
}

func setupAgyTruncate(t *testing.T) (agentID string, ends []time.Time) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	agentID = "ag_agytrunc"
	if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgyConversationID(agentDir(agentID), "", agyIDA, silentLogger())
	writeAgyConversationID(agentDir(agentID), "thread-key", agyIDB, silentLogger())
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	return agentID, writeAgyFixture(t, agyIDA, base)
}

// The last surviving message is turn 1's reply (stored after the turn
// ended): turns 0 and 1 stay, turn 2 is cut everywhere.
func TestTruncateAgyMainConversation_CutsAtTurnBoundary(t *testing.T) {
	agentID, ends := setupAgyTruncate(t)
	res, err := truncateAgyMainConversation(agentID, ends[1].Add(time.Second), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.stepsRemoved != 2 || res.cleared {
		t.Fatalf("res = %+v, want 2 steps removed, not cleared", res)
	}
	if s, e, g := agyFixtureCounts(t, agyIDA); s != 4 || e != 2 || g != 2 {
		t.Errorf("rows = steps %d exec %d gen %d, want 4/2/2", s, e, g)
	}
	logs := filepath.Join(agyBrainDir(agyIDA), ".system_generated", "logs")
	if got := agyFixtureStepIndexes(t, filepath.Join(logs, "transcript.jsonl")); fmt.Sprint(got) != "[0 1 2 3]" {
		t.Errorf("transcript steps = %v", got)
	}
	if raw, _ := os.ReadFile(filepath.Join(logs, "transcript.jsonl")); !strings.Contains(string(raw), "not json") {
		t.Error("unparseable transcript line was dropped")
	}
	if got := agyFixtureStepIndexes(t, filepath.Join(logs, "chunks", "transcript", "00000000.jsonl")); fmt.Sprint(got) != "[0 1 2 3]" {
		t.Errorf("chunk 0 steps = %v", got)
	}
	if _, err := os.Stat(filepath.Join(logs, "chunks", "transcript", "00000001.jsonl")); !os.IsNotExist(err) {
		t.Errorf("emptied chunk 1 still present: %v", err)
	}
	stepsDir := filepath.Join(agyBrainDir(agyIDA), ".system_generated", "steps")
	for idx, want := range []bool{true, true, true, true, false, false} {
		_, err := os.Stat(filepath.Join(stepsDir, fmt.Sprint(idx)))
		if (err == nil) != want {
			t.Errorf("steps/%d present=%v, want %v", idx, err == nil, want)
		}
	}
	if readAgyConversationID(agentDir(agentID), "") != agyIDA {
		t.Error("main ref dropped for a trimmed conversation")
	}
}

// A survivor stored while turn 1 was still running (e.g. a steer) means
// turn 1's reply is gone: turn 1 is cut too.
func TestTruncateAgyMainConversation_CutoffInsideTurn(t *testing.T) {
	agentID, ends := setupAgyTruncate(t)
	res, err := truncateAgyMainConversation(agentID, ends[1].Add(-5*time.Second), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.stepsRemoved != 4 || res.cleared {
		t.Fatalf("res = %+v, want 4 steps removed", res)
	}
	if s, e, g := agyFixtureCounts(t, agyIDA); s != 2 || e != 1 || g != 1 {
		t.Errorf("rows = steps %d exec %d gen %d, want 2/1/1", s, e, g)
	}
}

func TestTruncateAgyMainConversation_ClearsWhenFirstTurnCut(t *testing.T) {
	agentID, ends := setupAgyTruncate(t)
	res, err := truncateAgyMainConversation(agentID, ends[0].Add(-time.Second), true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.cleared || res.stepsRemoved != 0 {
		t.Fatalf("res = %+v, want cleared", res)
	}
	if readAgyConversationID(agentDir(agentID), "") != "" {
		t.Error("main ref kept")
	}
	if readAgyConversationID(agentDir(agentID), "thread-key") != agyIDB {
		t.Error("thread ref touched")
	}
	if s, _, _ := agyFixtureCounts(t, agyIDA); s != 6 {
		t.Errorf("db rewritten on clear: %d steps", s)
	}
}

func TestTruncateAgyMainConversation_EmptyTranscriptClears(t *testing.T) {
	agentID, _ := setupAgyTruncate(t)
	res, err := truncateAgyMainConversation(agentID, time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.cleared || readAgyConversationID(agentDir(agentID), "") != "" {
		t.Fatalf("res = %+v, main ref not cleared", res)
	}
}

func TestTruncateAgyMainConversation_NoOpWhenAllOlder(t *testing.T) {
	agentID, ends := setupAgyTruncate(t)
	res, err := truncateAgyMainConversation(agentID, ends[2].Add(time.Second), true)
	if err != nil {
		t.Fatal(err)
	}
	if res.stepsRemoved != 0 || res.cleared {
		t.Fatalf("res = %+v, want no-op", res)
	}
	if s, e, g := agyFixtureCounts(t, agyIDA); s != 6 || e != 3 || g != 3 {
		t.Errorf("rows = steps %d exec %d gen %d, want 6/3/3", s, e, g)
	}
}

func TestTruncateAgyMainConversation_NoConversation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	res, err := truncateAgyMainConversation("ag_none", time.Now(), true)
	if err != nil || res != (agyTruncateResult{}) {
		t.Fatalf("res = %+v err = %v, want no-op", res, err)
	}
}

func TestFindAgyCut(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	steps := []agyStepInfo{
		{idx: 0, execID: "a", latest: at(1)},
		{idx: 1, execID: "a", latest: at(3)},
		{idx: 2, execID: "b", latest: at(10)},
		{idx: 3, execID: "", latest: at(14)}, // no exec id: stays with b
		{idx: 4, execID: "c", latest: at(20)},
	}
	for _, tc := range []struct {
		cutoff int
		cut    int
		found  bool
	}{
		{cutoff: 4, cut: 2, found: true},
		{cutoff: 12, cut: 2, found: true}, // b ends at 14 (its exec-less step)
		{cutoff: 15, cut: 4, found: true},
		{cutoff: 3, cut: 2, found: true}, // ending at cutoff precedes it
		{cutoff: 2, cut: 0, found: true},
		{cutoff: 21, found: false},
	} {
		cut, found := findAgyCut(steps, at(tc.cutoff))
		if cut != tc.cut || found != tc.found {
			t.Errorf("cutoff %d: got (%d,%v), want (%d,%v)", tc.cutoff, cut, found, tc.cut, tc.found)
		}
	}
	// Same millisecond as cutoff (kojo stores ms): still before it.
	sameMS := []agyStepInfo{{idx: 0, execID: "a", latest: at(3).Add(500 * time.Microsecond)}, {idx: 1, execID: "b", latest: at(9)}}
	if cut, found := findAgyCut(sameMS, at(3)); !found || cut != 1 {
		t.Errorf("same-ms end: got (%d,%v), want (1,true)", cut, found)
	}
	if cut, found := findAgyCut(sameMS, at(3).Add(-time.Millisecond)); !found || cut != 0 {
		t.Errorf("next-ms end: got (%d,%v), want (0,true)", cut, found)
	}
	// A turn without any timestamp cannot be placed: cut there.
	noTime := []agyStepInfo{{idx: 0, execID: "a", latest: at(1)}, {idx: 1, execID: "b"}}
	if cut, found := findAgyCut(noTime, at(100)); !found || cut != 1 {
		t.Errorf("untimed turn: got (%d,%v), want (1,true)", cut, found)
	}
}

func TestParseAgyStepMetadata(t *testing.T) {
	created := time.Date(2026, 11, 1, 15, 49, 44, 123456789, time.UTC)
	done := created.Add(3 * time.Second)
	s := parseAgyStepMetadata(7, agyStepMetadata(agyFixtureStep{exec: "e1", created: created, done: done}))
	if s.idx != 7 || s.execID != "e1" || !s.latest.Equal(done) {
		t.Errorf("got %+v", s)
	}
	if s := parseAgyStepMetadata(0, []byte{0xff}); s.execID != "" || !s.latest.IsZero() {
		t.Errorf("malformed metadata parsed as %+v", s)
	}
}

// RewindToMessage derives the agy cutoff from the surviving transcript:
// rewinding to a reply drops the turn that produced it, rewinding to a
// user message keeps the turn before it.
func TestRewindToMessage_TrimsAgyConversation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pivot     int // index into the 6 transcript messages
		wantSteps int
	}{
		{"reply of turn 1", 3, 2},
		{"user message of turn 2", 4, 4},
		{"first user message", 0, 6}, // conversation cleared, db kept
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", "")
			mgr, err := NewManager(silentLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = mgr.Close() })
			a, err := mgr.Create(AgentConfig{Name: "agyrewind", Tool: ToolGeminiAgy})
			if err != nil {
				t.Fatal(err)
			}
			writeAgyConversationID(agentDir(a.ID), "", agyIDA, silentLogger())
			base := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
			ends := writeAgyFixture(t, agyIDA, base)
			var ids []string
			for turn := 0; turn < 3; turn++ {
				start := base.Add(time.Duration(turn) * time.Minute)
				u := newUserMessage(fmt.Sprintf("u%d", turn), nil)
				u.Timestamp = start.Add(-time.Second).Format(time.RFC3339)
				r := newAssistantMessage()
				r.Content = fmt.Sprintf("a%d", turn)
				r.Timestamp = ends[turn].Add(time.Second).Format(time.RFC3339)
				for _, msg := range []*Message{u, r} {
					if err := appendMessage(a.ID, msg); err != nil {
						t.Fatal(err)
					}
					ids = append(ids, msg.ID)
				}
			}
			res, err := mgr.RewindToMessage(a.ID, ids[tc.pivot])
			if err != nil {
				t.Fatal(err)
			}
			if s, _, _ := agyFixtureCounts(t, agyIDA); s != tc.wantSteps {
				t.Errorf("steps = %d, want %d (res %+v)", s, tc.wantSteps, res)
			}
			wantCleared := tc.pivot == 0
			if res.AgyConversationCleared != wantCleared {
				t.Errorf("cleared = %v, want %v", res.AgyConversationCleared, wantCleared)
			}
			if got := readAgyConversationID(agentDir(a.ID), ""); (got == "") != wantCleared {
				t.Errorf("main ref = %q, cleared=%v", got, wantCleared)
			}
		})
	}
}
