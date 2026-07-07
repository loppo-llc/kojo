package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// bufWriteCloser adapts a bytes.Buffer to io.WriteCloser for stdin capture.
type bufWriteCloser struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *bufWriteCloser) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *bufWriteCloser) Close() error { return nil }
func (b *bufWriteCloser) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestStdinWriter() (*claudeStdinWriter, *bufWriteCloser) {
	w := &bufWriteCloser{}
	return &claudeStdinWriter{w: w}, w
}

const askQuestionInput = `{"questions":[{"question":"色は?","header":"色","options":[{"label":"青"},{"label":"赤"}],"multiSelect":false}]}`

func askUserQuestionCR() *controlRequestMsg {
	cr := &controlRequestMsg{Type: "control_request", RequestID: "req-1"}
	cr.Request.Subtype = "can_use_tool"
	cr.Request.ToolName = "AskUserQuestion"
	cr.Request.ToolUseID = "tu-1"
	cr.Request.Input = json.RawMessage(askQuestionInput)
	return cr
}

// TestHandleControlRequest_AutoAllowNonQuestion verifies a non-AskUserQuestion
// control_request is answered inline with behavior=allow and never surfaces an
// event.
func TestHandleControlRequest_AutoAllowNonQuestion(t *testing.T) {
	sw, buf := newTestStdinWriter()
	qs := newClaudeQuestionState(sw)
	cr := &controlRequestMsg{Type: "control_request", RequestID: "req-x"}
	cr.Request.Subtype = "can_use_tool"
	cr.Request.ToolName = "Bash"
	cr.Request.Input = json.RawMessage(`{"command":"ls"}`)

	emitted := false
	handleControlRequest(cr, sw, qs, 0, testLogger(), func(ChatEvent) bool { emitted = true; return true })

	if emitted {
		t.Error("non-question control_request must not emit an event")
	}
	out := buf.String()
	if !strings.Contains(out, `"behavior":"allow"`) || !strings.Contains(out, "req-x") {
		t.Errorf("expected allow control_response, got %q", out)
	}
}

// TestHandleControlRequest_NoChannelDenies verifies AskUserQuestion is denied
// inline with no event when there is no answer channel (qstate nil) — a turn
// that can never surface or answer a question.
func TestHandleControlRequest_NoChannelDenies(t *testing.T) {
	sw, buf := newTestStdinWriter()

	emitted := false
	handleControlRequest(askUserQuestionCR(), sw, nil, 0, testLogger(), func(ChatEvent) bool { emitted = true; return true })

	if emitted {
		t.Error("AskUserQuestion with no answer channel must not emit an event")
	}
	if out := buf.String(); !strings.Contains(out, `"behavior":"deny"`) {
		t.Errorf("expected deny control_response, got %q", out)
	}
}

// TestHandleControlRequest_AutomatedSurfacesAndTimesOut verifies an automated
// turn (positive timeout) surfaces the question as a card, holds it, and then
// auto-denies once the timeout elapses.
func TestHandleControlRequest_AutomatedSurfacesAndTimesOut(t *testing.T) {
	sw, buf := newTestStdinWriter()
	qs := newClaudeQuestionState(sw)

	var got ChatEvent
	handleControlRequest(askUserQuestionCR(), sw, qs, 30*time.Millisecond, testLogger(),
		func(e ChatEvent) bool { got = e; return true })

	if got.Type != "user_question" || got.RequestID != "req-1" {
		t.Fatalf("automated turn must surface a user_question card, got %+v", got)
	}
	qs.mu.Lock()
	_, registered := qs.pending["req-1"]
	qs.mu.Unlock()
	if !registered {
		t.Fatal("automated question must be registered as pending")
	}
	// Wait for the auto-deny timer to fire and write the deny control_response.
	deadline := time.Now().Add(2 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out = buf.String()
		if strings.Contains(out, `"behavior":"deny"`) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(out, `"behavior":"deny"`) || !strings.Contains(out, "time limit") {
		t.Errorf("expected timeout deny control_response, got %q", out)
	}
	qs.mu.Lock()
	_, stillPending := qs.pending["req-1"]
	qs.mu.Unlock()
	if stillPending {
		t.Error("timeout should have cleared the pending question")
	}
}

// TestHandleControlRequest_AutomatedAnsweredInTime verifies an answer that
// arrives before the timeout works like a user turn: an allow control_response
// is written, the timer is stopped, and no deny is emitted.
func TestHandleControlRequest_AutomatedAnsweredInTime(t *testing.T) {
	sw, buf := newTestStdinWriter()
	qs := newClaudeQuestionState(sw)

	handleControlRequest(askUserQuestionCR(), sw, qs, time.Minute, testLogger(),
		func(ChatEvent) bool { return true })

	if err := qs.answer("req-1", map[string]any{"色は?": "青"}, false, ""); err != nil {
		t.Fatalf("answer: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"behavior":"allow"`) || !strings.Contains(out, "青") {
		t.Errorf("expected allow control_response, got %q", out)
	}
	if strings.Contains(out, `"behavior":"deny"`) {
		t.Error("answered-in-time question must not be denied")
	}
	// The auto-deny timer must have been stopped/cleared.
	qs.mu.Lock()
	_, hasTimer := qs.timers["req-1"]
	_, stillPending := qs.pending["req-1"]
	qs.mu.Unlock()
	if hasTimer || stillPending {
		t.Error("answer must clear the pending entry and stop the timer")
	}
	// Give any (incorrectly-not-stopped) timer a chance to fire; nothing new
	// should be written.
	time.Sleep(20 * time.Millisecond)
	if buf.String() != out {
		t.Errorf("no further control_response expected after answer, got %q", buf.String())
	}
}

// TestHandleControlRequest_UserTurnRegistersAndEmits verifies a watched turn
// registers the pending question and emits a user_question event.
func TestHandleControlRequest_UserTurnRegistersAndEmits(t *testing.T) {
	sw, buf := newTestStdinWriter()
	qs := newClaudeQuestionState(sw)

	var got ChatEvent
	handleControlRequest(askUserQuestionCR(), sw, qs, 0, testLogger(), func(e ChatEvent) bool { got = e; return true })

	if got.Type != "user_question" || got.RequestID != "req-1" || got.ToolUseID != "tu-1" {
		t.Errorf("unexpected event: %+v", got)
	}
	if len(got.Questions) == 0 || !strings.Contains(string(got.Questions), "色は?") {
		t.Errorf("event missing questions payload: %s", got.Questions)
	}
	if _, ok := qs.pending["req-1"]; !ok {
		t.Error("question not registered as pending")
	}
	// Nothing written to stdin yet — the answer comes later.
	if buf.String() != "" {
		t.Errorf("no control_response should be written before answering, got %q", buf.String())
	}
}

// TestQuestionState_AnswerAllow verifies answer() writes an allow
// control_response echoing the questions array and clears the pending entry.
func TestQuestionState_AnswerAllow(t *testing.T) {
	sw, buf := newTestStdinWriter()
	qs := newClaudeQuestionState(sw)
	qs.register("req-1", json.RawMessage(askQuestionInput), 0, "")

	if err := qs.answer("req-1", map[string]any{"色は?": "青"}, false, ""); err != nil {
		t.Fatalf("answer: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"behavior":"allow"`) || !strings.Contains(out, "青") || !strings.Contains(out, "色は?") {
		t.Errorf("unexpected control_response: %q", out)
	}
	if _, ok := qs.pending["req-1"]; ok {
		t.Error("answered question should be removed from pending")
	}
}

// TestQuestionState_AnswerUnknown verifies answering an unknown requestID
// returns ErrQuestionNotFound.
func TestQuestionState_AnswerUnknown(t *testing.T) {
	sw, _ := newTestStdinWriter()
	qs := newClaudeQuestionState(sw)
	if err := qs.answer("nope", map[string]any{"a": "b"}, false, ""); !errors.Is(err, ErrQuestionNotFound) {
		t.Errorf("err = %v, want ErrQuestionNotFound", err)
	}
}

// TestQuestionState_DenyAllPending verifies pending questions are flushed with
// a deny on turn teardown.
func TestQuestionState_DenyAllPending(t *testing.T) {
	sw, buf := newTestStdinWriter()
	qs := newClaudeQuestionState(sw)
	qs.register("req-1", json.RawMessage(askQuestionInput), 0, "")
	qs.denyAllPending("turn ended")
	if len(qs.pending) != 0 {
		t.Error("denyAllPending must clear pending map")
	}
	if !strings.Contains(buf.String(), `"behavior":"deny"`) {
		t.Errorf("expected deny write, got %q", buf.String())
	}
}

// --- Manager.AnswerQuestion ---

// TestManager_AnswerQuestion_NotBusy verifies a 409-mapping ErrAgentNotBusy
// when no turn is running.
func TestManager_AnswerQuestion_NotBusy(t *testing.T) {
	m := newTestManager(t)
	err := m.AnswerQuestion(context.Background(), "ag_none", "req-1", map[string]any{"a": "b"}, false, "")
	if !errors.Is(err, ErrAgentNotBusy) {
		t.Errorf("err = %v, want ErrAgentNotBusy", err)
	}
}

// TestManager_AnswerQuestion_NoHandle verifies a busy turn whose backend never
// registered an answer handle surfaces ErrAgentNotBusy.
func TestManager_AnswerQuestion_NoHandle(t *testing.T) {
	m := newTestManager(t)
	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{startedAt: time.Now(), cancel: func() {}}
	m.busyMu.Unlock()
	err := m.AnswerQuestion(context.Background(), "ag_test", "req-1", map[string]any{"a": "b"}, false, "")
	if !errors.Is(err, ErrAgentNotBusy) {
		t.Errorf("err = %v, want ErrAgentNotBusy", err)
	}
}

// TestManager_AnswerQuestion_Allow verifies the happy path: the answer handle
// is called, an answered user message is persisted, and a live event is pushed.
func TestManager_AnswerQuestion_Allow(t *testing.T) {
	m := newTestManager(t)
	m.agents["ag_test"] = &Agent{ID: "ag_test", Name: "Test", Tool: "claude"}
	if err := m.store.Upsert(m.agents["ag_test"]); err != nil {
		t.Fatal(err)
	}

	var gotReq string
	var gotAns map[string]any
	outCh := make(chan ChatEvent, 4)
	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{
		startedAt: time.Now(),
		cancel:    func() {},
		outCh:     outCh,
		answer: func(requestID string, answers map[string]any, deny bool, denyMessage string) error {
			gotReq = requestID
			gotAns = answers
			return nil
		},
	}
	m.busyMu.Unlock()

	if err := m.AnswerQuestion(context.Background(), "ag_test", "req-1", map[string]any{"色選択": "青"}, false, ""); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if gotReq != "req-1" || gotAns["色選択"] != "青" {
		t.Errorf("answer handle got req=%q ans=%v", gotReq, gotAns)
	}

	msgs, err := m.Messages("ag_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mm := range msgs {
		if mm.Role == "user" && strings.Contains(mm.Content, "色選択 → 青") {
			found = true
		}
	}
	if !found {
		t.Error("answered message not persisted to transcript")
	}

	select {
	case e := <-outCh:
		if e.Type != "message" || e.Message == nil || !strings.Contains(e.Message.Content, "青") {
			t.Errorf("unexpected outCh event: %+v", e)
		}
	default:
		t.Error("expected a live event pushed to outCh for the answered message")
	}
}

// TestManager_AnswerQuestion_Unknown verifies ErrQuestionNotFound from the
// answer handle propagates (→ 404 at the HTTP layer).
func TestManager_AnswerQuestion_Unknown(t *testing.T) {
	m := newTestManager(t)
	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{
		startedAt: time.Now(),
		cancel:    func() {},
		answer: func(string, map[string]any, bool, string) error {
			return ErrQuestionNotFound
		},
	}
	m.busyMu.Unlock()
	err := m.AnswerQuestion(context.Background(), "ag_test", "req-x", map[string]any{"a": "b"}, false, "")
	if !errors.Is(err, ErrQuestionNotFound) {
		t.Errorf("err = %v, want ErrQuestionNotFound", err)
	}
}

// TestManager_AnswerQuestion_Deny verifies a deny does not persist a transcript
// message.
func TestManager_AnswerQuestion_Deny(t *testing.T) {
	m := newTestManager(t)
	m.agents["ag_test"] = &Agent{ID: "ag_test", Name: "Test", Tool: "claude"}
	if err := m.store.Upsert(m.agents["ag_test"]); err != nil {
		t.Fatal(err)
	}
	denied := false
	m.busyMu.Lock()
	m.busy["ag_test"] = busyEntry{
		startedAt: time.Now(),
		cancel:    func() {},
		answer: func(_ string, _ map[string]any, deny bool, _ string) error {
			denied = deny
			return nil
		},
	}
	m.busyMu.Unlock()
	if err := m.AnswerQuestion(context.Background(), "ag_test", "req-1", nil, true, "declined"); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}
	if !denied {
		t.Error("expected deny=true forwarded to answer handle")
	}
	msgs, _ := m.Messages("ag_test", 10)
	for _, mm := range msgs {
		if mm.Role == "user" {
			t.Errorf("deny must not persist a user message, got %q", mm.Content)
		}
	}
}
