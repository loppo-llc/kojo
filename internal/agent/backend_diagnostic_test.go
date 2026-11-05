package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/backenddiag"
	"github.com/loppo-llc/kojo/internal/chathistory"
)

const diagnosticBanner = "API Error: Opus 5.5's safeguards flagged this message.\n\nDetails: `[reasoning_extraction]`\n\nRequest ID: req_test123\n\nMessage ID: msg_test456"

func diagnosticJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func diagnosticAssistant(t *testing.T, parent string) string {
	return diagnosticJSON(t, map[string]any{
		"type": "assistant", "isApiErrorMessage": true, "parent_tool_use_id": parent,
		"message": map[string]any{"content": []map[string]string{{"type": "text", "text": diagnosticBanner}}},
	})
}

func diagnosticResult(t *testing.T) string {
	return diagnosticJSON(t, map[string]any{"type": "result", "subtype": "success", "is_error": true, "stop_reason": "refusal", "result": diagnosticBanner})
}

func TestClaudeProviderFailureIsNotAssistantText(t *testing.T) {
	for _, onlyResult := range []bool{false, true} {
		lines := []string{}
		if !onlyResult {
			lines = append(lines, diagnosticAssistant(t, ""))
		}
		lines = append(lines, diagnosticResult(t))
		events, res := collectEvents(t, lines...)
		if finalStreamText(res, true) != "" || res.resultError != diagnosticBanner || res.backendDiagnostic == nil {
			t.Fatalf("provider failure became prose or lost detail: %+v", res)
		}
		if res.backendDiagnostic.RequestID != "req_test123" || res.backendDiagnostic.Category != "reasoning_extraction" {
			t.Fatalf("missing correlation metadata: %+v", res.backendDiagnostic)
		}
		for _, e := range events {
			if e.Type == "text" {
				t.Fatalf("diagnostic streamed as text: %+v", e)
			}
		}
	}
}

func TestClaudeStreamSnakeCaseAPIError(t *testing.T) {
	// Captured CLI 2.1.285 stream schema differs from the JSONL transcript:
	// the stream uses is_api_error_message, not isApiErrorMessage.
	detail := "Failed to authenticate: OAuth session expired and could not be refreshed"
	line := diagnosticJSON(t, map[string]any{"type": "assistant", "is_api_error_message": true, "error": "authentication_failed", "message": map[string]any{"content": []map[string]string{{"type": "text", "text": detail}}}})
	result := diagnosticJSON(t, map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": detail})
	events, r := collectEvents(t, line, result)
	if r.backendDiagnostic == nil || r.backendDiagnostic.Detail != detail || r.backendDiagnostic.Code != "authentication_failed" || r.resultError != detail || finalStreamText(r, true) != "" {
		t.Fatalf("native API error leaked as prose: %+v", r)
	}
	for _, e := range events {
		if e.Type == "text" {
			t.Fatal("native API error streamed as text")
		}
	}
}

func TestClaudeProviderFailureRetainsPartialAndDoesNotFailParent(t *testing.T) {
	_, r := collectEvents(t, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial work"}}`, diagnosticAssistant(t, ""), diagnosticResult(t))
	if finalStreamText(r, true) != "partial work" {
		t.Fatalf("partial work lost: %+v", r)
	}
	events, r := collectEvents(t, diagnosticAssistant(t, "task-1"), `{"type":"result","subtype":"success","result":"parent answer"}`)
	if r.resultError != "" || r.backendDiagnostic != nil || finalStreamText(r, false) != "parent answer" {
		t.Fatalf("subagent failure poisoned parent: %+v", r)
	}
	for _, e := range events {
		if e.ParentToolUseID != "" && e.Delta != "" {
			t.Fatal("subagent error leaked as prose")
		}
	}
}

func TestClaudeProviderFailureRecoveryAndOrdinaryQuotes(t *testing.T) {
	_, r := collectEvents(t, diagnosticAssistant(t, ""), `{"type":"result","subtype":"success","is_error":false,"result":"recovered answer"}`)
	if r.resultError != "" || r.backendDiagnostic != nil || finalStreamText(r, false) != "recovered answer" {
		t.Fatalf("successful retry lost: %+v", r)
	}
	_, r = collectEvents(t, diagnosticJSON(t, map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]string{{"type": "text", "text": diagnosticBanner}}}}))
	if finalStreamText(r, false) != diagnosticBanner || r.backendDiagnostic != nil {
		t.Fatal("unmarked model text was treated as API error")
	}
	_, r = collectEvents(t, `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"legitimate partial result"}`)
	if finalStreamText(r, true) != "legitimate partial result" || r.resultError == "" {
		t.Fatalf("ordinary terminal error regressed: %+v", r)
	}
}

func TestClaudeFailureRemainsDiagnosticUntilTerminalSuccess(t *testing.T) {
	partial := `{"type":"assistant","message":{"content":[{"type":"text","text":"retry partial"}]}}`
	_, r := collectEvents(t, diagnosticAssistant(t, ""), partial,
		`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"retry partial"}`)
	if r.backendDiagnostic == nil || r.resultError != diagnosticBanner || finalStreamText(r, true) != "retry partial" {
		t.Fatalf("failed retry lost diagnostic or partial answer: %+v", r)
	}
	_, r = collectEvents(t, diagnosticAssistant(t, ""), partial,
		`{"type":"result","subtype":"success","is_error":false}`)
	if r.backendDiagnostic != nil || r.resultError != "" || finalStreamText(r, false) != "retry partial" {
		t.Fatalf("successful retry without result prose failed: %+v", r)
	}
	_, r = collectEvents(t, diagnosticAssistant(t, ""), partial, `{"type":"result","subtype":"success"}`)
	if r.backendDiagnostic != nil || r.resultError != "" || finalStreamText(r, false) != "retry partial" {
		t.Fatalf("legacy successful retry with omitted is_error failed: %+v", r)
	}
	_, r = collectEvents(t, diagnosticAssistant(t, ""), `{"type":"result","subtype":"success"}`)
	if r.backendDiagnostic == nil || r.resultError == "" {
		t.Fatal("ambiguous legacy success subtype erased a known failure without any recovery")
	}
}

func TestChildDiagnosticDoesNotInheritParentCategory(t *testing.T) {
	var logs bytes.Buffer
	acc := newTurnAccumulator(slog.New(slog.NewTextHandler(&logs, nil)), func(ChatEvent) bool { return true })
	for _, line := range []string{`{"type":"system","apiRefusalCategory":"bio"}`, diagnosticAssistant(t, "child")} {
		var e claudeStreamEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		acc.feed(e, e.ParentToolUseID)
	}
	if !strings.Contains(logs.String(), "category=reasoning_extraction") || strings.Contains(logs.String(), "category=bio") {
		t.Fatalf("child log has parent category: %s", logs.String())
	}
}

func TestSessionProviderFailureMetadata(t *testing.T) {
	s, pw := newTestSession(t, nil)
	defer pw.Close()
	sink, err := s.startTurn(context.Background(), &Agent{ID: "test-agent"}, "hi", false, false)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(pw, diagnosticAssistant(t, "")+"\n"+diagnosticResult(t)+"\n")
	var done bool
	for _, e := range collect(t, sink, 3*time.Second) {
		if e.Type == "text" {
			t.Fatal("provider diagnostic was streamed as assistant text")
		}
		if e.Type == "done" {
			done = true
			if e.ErrorMessage != diagnosticBanner || e.BackendDiagnostic == nil || e.Message == nil || e.Message.Content != "" {
				t.Fatalf("bad failure event: %+v", e)
			}
			var wire ChatEvent
			if err := json.Unmarshal([]byte(diagnosticJSON(t, e)), &wire); err != nil || wire.BackendDiagnostic.RequestID != "req_test123" {
				t.Fatalf("metadata lost in transport: %+v, %v", wire, err)
			}
		}
	}
	if !done {
		t.Fatal("missing terminal event")
	}
}

func TestHistoryAndSummaryKeepFailureNoticeButNotBanner(t *testing.T) {
	history := []chathistory.HistoryMessage{{IsBot: true, UserID: "bot", Text: diagnosticBanner + "\n\nUseful partial work."}, {UserID: "user", Text: "please continue"}}
	fresh, resume := FormatOneShotHistoryContexts(history, "bot")
	msgs := []*Message{{Role: "assistant", Content: diagnosticBanner + "\n\nUseful partial work."}, {Role: "user", Content: "please continue"}}
	for name, text := range map[string]string{"fresh": fresh, "resume": resume, "recent": formatRecentMessagesContext(msgs), "summary": buildSummaryPrompt(msgs, "")} {
		if strings.Contains(text, diagnosticBanner) || strings.Contains(text, "req_test123") || !strings.Contains(text, backenddiag.HistoryNotice) || !strings.Contains(text, "Useful partial work.") {
			t.Fatalf("%s still reinjects diagnostic or loses normal text: %s", name, text)
		}
	}
	// Human-authored error reports remain literal user data.
	for _, text := range []string{chathistory.FormatForInjection([]chathistory.HistoryMessage{{Text: diagnosticBanner}}, "bot", 10, 10000), formatRecentMessagesContext([]*Message{{Role: "user", Content: diagnosticBanner}}), buildSummaryPrompt([]*Message{{Role: "user", Content: diagnosticBanner}}, "")} {
		if !strings.Contains(text, diagnosticBanner) {
			t.Fatal("human error report was rewritten")
		}
	}
	if history[0].Text != diagnosticBanner+"\n\nUseful partial work." || msgs[0].Content != history[0].Text {
		t.Fatal("stored transcript was mutated")
	}
}

func TestRecoverDoesNotResurrectSyntheticAPIError(t *testing.T) {
	aid, write := setupRecoverTest(t)
	write([]map[string]any{{"type": "user", "message": map[string]any{"content": "hi"}}, {"type": "assistant", "isApiErrorMessage": true, "message": map[string]any{"content": []map[string]string{{"type": "text", "text": diagnosticBanner}}}}})
	if got := recoverFromSession(aid, "", slog.New(slog.NewTextHandler(io.Discard, nil))); got != "" {
		t.Fatalf("recovered diagnostic as answer: %q", got)
	}
	msgs := loadSessionMessages(aid, "claude", "", 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(msgs) != 2 || msgs[1].Content != backenddiag.HistoryNotice {
		t.Fatalf("summary input resurrected synthetic error: %+v", msgs)
	}
}

func TestHeadTailHistoryNormalizesBothEnds(t *testing.T) {
	history := make([]chathistory.HistoryMessage, 20)
	for i := range history {
		history[i] = chathistory.HistoryMessage{IsBot: true, UserID: "bot", Text: "normal answer"}
	}
	history[0].Text, history[19].Text = diagnosticBanner, diagnosticBanner
	text := chathistory.FormatForInjectionHeadTail(history, "bot", 3, 5, 10000)
	if strings.Count(text, backenddiag.HistoryNotice) != 2 || strings.Contains(text, "req_test123") {
		t.Fatalf("head/tail still injects diagnostics: %s", text)
	}
}

func TestOrdinaryRefusalRemainsAssistantAnswer(t *testing.T) {
	answer := "I cannot help with that request."
	_, r := collectEvents(t, diagnosticJSON(t, map[string]any{"type": "result", "subtype": "success", "is_error": false, "stop_reason": "refusal", "result": answer}))
	if r.resultError != "" || r.backendDiagnostic != nil || finalStreamText(r, false) != answer {
		t.Fatalf("ordinary refusal converted into API error: %+v", r)
	}
}

func TestChildTranscriptAPIErrorIsDiagnosticNotProse(t *testing.T) {
	for _, flag := range []string{"isApiErrorMessage", "is_api_error_message"} {
		var logs bytes.Buffer
		line := strings.Replace(diagnosticAssistant(t, ""), "isApiErrorMessage", flag, 1)
		events, children := subagentEventsFromLine([]byte(line), "task-child", slog.New(slog.NewTextHandler(&logs, nil)))
		if len(events) != 0 || len(children) != 0 || !strings.Contains(logs.String(), "req_test123") || !strings.Contains(logs.String(), "task-child") {
			t.Fatalf("child diagnostic leaked or correlation lost: %v %v %s", events, children, logs.String())
		}
	}
}

func TestCancelledTerminalRetainsBackendDiagnostic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan ChatEvent, 1)
	d := backenddiag.Claude(diagnosticBanner, "")
	emitCancelDone(ctx, ch, "partial work", "", nil, nil, d)
	e := <-ch
	if e.Type != "done" || e.ErrorMessage != ErrMsgCancelled || e.BackendDiagnostic != d || e.Message.Content != "partial work" {
		t.Fatalf("cancel lost diagnostic or partial work: %+v", e)
	}
}
