package agent

import (
	"strings"
	"testing"
)

func TestGoalTurnText(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		failed, web      bool
	}{
		{name: "silence", body: SlackNoReplyToken},
		{name: "whitespace silence", body: " \n" + SlackNoReplyToken + "\t\n"},
		{name: "ordinary", body: "hello", want: "hello"},
		{name: "discussion suffix", body: SlackNoReplyToken + " means silence", want: SlackNoReplyToken + " means silence"},
		{name: "embedded", body: "Use " + SlackNoReplyToken, want: "Use " + SlackNoReplyToken},
		{name: "quoted", body: "`" + SlackNoReplyToken + "`", want: "`" + SlackNoReplyToken + "`"},
		{name: "code block", body: "```\n" + SlackNoReplyToken + "\n```", want: "```\n" + SlackNoReplyToken + "\n```"},
		{name: "multiline prose", body: "before\n\n" + SlackNoReplyToken + "\n\nafter", want: "before\n\n" + SlackNoReplyToken + "\n\nafter"},
		{name: "clean partial", body: "[[NO_", want: "[[NO_"},
		{name: "failed partial", body: "[[NO_", failed: true},
		{name: "failed silence", body: SlackNoReplyToken, failed: true},
		{name: "failed prose", body: "partial reply", want: "partial reply", failed: true},
		{name: "web literal", body: SlackNoReplyToken, want: SlackNoReplyToken, web: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Every byte boundary, including a fully completed-item fallback.
			for split := 0; split <= len(tc.body); split++ {
				for _, separator := range []bool{false, true} {
					var live strings.Builder
					f := newGoalTurnText(!tc.web, separator, func(e ChatEvent) bool {
						if e.Type == "text" {
							live.WriteString(e.Delta)
						}
						return true
					})
					res := &codexStreamResult{turnCompleted: true, turnStatus: "completed"}
					if tc.failed {
						res.processError = "failure"
						res.turnStatus = "failed"
					}
					for _, delta := range []string{tc.body[:split], tc.body[split:]} {
						res.fullText.WriteString(delta)
						f.send(ChatEvent{Type: "text", Delta: delta})
						if tc.want == "" && live.Len() != 0 {
							t.Fatalf("token leaked before finish: %q", live.String())
						}
					}
					f.finish(res)
					want := tc.want
					if separator && want != "" {
						want = "\n\n" + want
					}
					if live.String() != want || res.fullText.String() != want {
						t.Fatalf("split=%d separator=%v live=%q terminal=%q want=%q", split, separator, live.String(), res.fullText.String(), want)
					}
					if tc.failed && res.processError != "failure" {
						t.Fatal("failure lost")
					}
				}
			}
		})
	}
}

func TestGoalTurnTextPreservesNonTextAndCancellation(t *testing.T) {
	var events []ChatEvent
	f := newGoalTurnText(true, false, func(e ChatEvent) bool {
		events = append(events, e)
		return e.Type != "text"
	})
	f.send(ChatEvent{Type: "text", Delta: "[["})
	// Non-boundary events pass while a possible token prefix is buffered.
	for _, kind := range []string{"status", "rate_limit", "user_question", "attachment", "error"} {
		if !f.send(ChatEvent{Type: kind}) {
			t.Fatal("non-text event was not forwarded")
		}
	}
	res := &codexStreamResult{turnCompleted: true, turnStatus: "completed"}
	res.fullText.WriteString("[[")
	f.finish(res)
	if !res.cancelled || len(events) != 6 {
		t.Fatalf("cancelled=%v events=%+v", res.cancelled, events)
	}
}

type segStep struct {
	text  string
	kind  string // non-text event type
	start bool   // explicit backend segment start
}

func runSegments(t *testing.T, steps []segStep, clean bool) (live string, events []ChatEvent, f *noReplySegments) {
	t.Helper()
	var b strings.Builder
	f = newNoReplySegments(func(e ChatEvent) bool {
		events = append(events, e)
		if e.Type == "text" {
			b.WriteString(e.Delta)
		}
		return true
	})
	for _, s := range steps {
		if s.kind != "" {
			f.send(ChatEvent{Type: s.kind})
			continue
		}
		f.send(ChatEvent{Type: "text", Delta: s.text, textSegmentStart: s.start})
		if strings.Contains(b.String(), SlackNoReplyToken) && !strings.Contains(s.text, "了解") && !strings.Contains(s.text, "means") {
			t.Fatalf("token leaked while streaming: %q", b.String())
		}
	}
	f.finish(clean)
	return b.String(), events, f
}

func TestNoReplySegments(t *testing.T) {
	tok := SlackNoReplyToken
	for _, tc := range []struct {
		name   string
		steps  []segStep
		clean  bool
		want   string
		silent bool
	}{
		{name: "three token segments around tools", clean: true, silent: true, steps: []segStep{
			{text: tok}, {kind: "tool_use"}, {kind: "tool_result"}, {text: tok}, {kind: "tool_use"}, {kind: "tool_result"}, {text: tok}}},
		{name: "split tokens across deltas", clean: true, silent: true, steps: []segStep{
			{text: "[[NO_"}, {text: "REPLY]]"}, {kind: "tool_use"}, {text: "\n[[NO"}, {text: "_REPLY]]\n"}}},
		{name: "consecutive blocks via segment start", clean: true, silent: true, steps: []segStep{
			{text: tok, start: true}, {text: tok, start: true}}},
		{name: "token then prose", clean: true, want: "了解しました", steps: []segStep{
			{text: tok}, {kind: "tool_use"}, {kind: "tool_result"}, {text: "了解しました"}}},
		{name: "prose then token", clean: true, want: "done", steps: []segStep{
			{text: "done"}, {kind: "tool_use"}, {text: tok}}},
		{name: "mixed segment verbatim", clean: true, want: tok + "\n了解です", steps: []segStep{
			{text: tok}, {text: "\n了解です"}}},
		{name: "prose mention", clean: true, want: tok + " means silence", steps: []segStep{
			{text: tok + " means silence"}}},
		{name: "failed partial prefix", clean: false, silent: true, steps: []segStep{
			{text: tok}, {kind: "tool_use"}, {text: "[[NO_"}}},
		{name: "clean partial prefix is prose", clean: true, want: "[[NO_", steps: []segStep{
			{text: "[[NO_"}}},
		{name: "strict prefix segment before tool is prose", clean: true, want: "[[NO_x", steps: []segStep{
			{text: "[[NO_"}, {kind: "tool_use"}, {text: "x"}}},
		{name: "single token", clean: true, silent: true, steps: []segStep{
			{text: tok}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, _, f := runSegments(t, tc.steps, tc.clean)
			if live != tc.want {
				t.Fatalf("live=%q want=%q", live, tc.want)
			}
			if f.silent() != tc.silent {
				t.Fatalf("silent=%v want %v", f.silent(), tc.silent)
			}
			var raw strings.Builder
			for _, s := range tc.steps {
				raw.WriteString(s.text)
			}
			if got := f.rewrite(raw.String()); got != tc.want {
				t.Fatalf("rewrite=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestNoReplySegmentsOrderAndMergedTerminal(t *testing.T) {
	_, events, f := runSegments(t, []segStep{{text: "[[NO_"}, {kind: "tool_use"}, {text: SlackNoReplyToken}}, true)
	if len(events) != 2 || events[0].Type != "text" || events[1].Type != "tool_use" {
		t.Fatalf("buffered prose must precede its boundary event: %+v", events)
	}
	// Claude may prepend earlier assistant text to the streamed body.
	if got := f.rewrite("earlier\n\n[[NO_" + SlackNoReplyToken); got != "earlier\n\n[[NO_" {
		t.Fatalf("merged rewrite=%q", got)
	}
}

func collectFiltered(t *testing.T, in []ChatEvent) []ChatEvent {
	t.Helper()
	ch := make(chan ChatEvent, len(in))
	for _, e := range in {
		ch <- e
	}
	close(ch)
	var out []ChatEvent
	for e := range filterSlackNoReplyEvents(ch) {
		out = append(out, e)
	}
	return out
}

func liveAndTerminal(events []ChatEvent) (string, *ChatEvent) {
	var b strings.Builder
	var done *ChatEvent
	for i := range events {
		if events[i].Type == "text" {
			b.WriteString(events[i].Delta)
		}
		if events[i].Type == "done" {
			done = &events[i]
		}
	}
	return b.String(), done
}

func TestFilterSlackNoReplyEvents(t *testing.T) {
	tok := SlackNoReplyToken
	msg := func(c string) *Message { return &Message{Role: "assistant", Content: c} }

	// Observed Claude case: three token text blocks separated by tools.
	live, done := liveAndTerminal(collectFiltered(t, []ChatEvent{
		{Type: "text", Delta: tok, textSegmentStart: true},
		{Type: "tool_use", ToolName: "Write"}, {Type: "tool_result"},
		{Type: "text", Delta: tok, textSegmentStart: true},
		{Type: "tool_use", ToolName: "Write"}, {Type: "tool_result"},
		{Type: "text", Delta: tok, textSegmentStart: true},
		{Type: "done", Message: msg(tok + tok + tok)},
	}))
	if live != "" || done == nil || done.Message.Content != tok {
		t.Fatalf("all-token: live=%q done=%+v", live, done)
	}

	// Token then prose: only prose is streamed and persisted.
	live, done = liveAndTerminal(collectFiltered(t, []ChatEvent{
		{Type: "text", Delta: tok},
		{Type: "tool_use"}, {Type: "tool_result"},
		{Type: "text", Delta: "メモしました"},
		{Type: "done", Message: msg(tok + "メモしました")},
	}))
	if live != "メモしました" || done.Message.Content != "メモしました" {
		t.Fatalf("token+prose: live=%q done=%q", live, done.Message.Content)
	}

	// Cancelled turn with only a token prefix publishes nothing.
	live, done = liveAndTerminal(collectFiltered(t, []ChatEvent{
		{Type: "text", Delta: tok},
		{Type: "tool_use"},
		{Type: "text", Delta: "[[NO"},
		{Type: "done", ErrorMessage: ErrMsgCancelled, Message: msg(tok + "[[NO")},
	}))
	if live != "" || done.Message.Content != "" {
		t.Fatalf("cancelled: live=%q done=%q", live, done.Message.Content)
	}

	// A failed exact token is not a silence request: body empty, error kept.
	live, done = liveAndTerminal(collectFiltered(t, []ChatEvent{
		{Type: "text", Delta: tok},
		{Type: "error", ErrorMessage: "boom"},
		{Type: "done", Message: msg(tok)},
	}))
	if live != "" || done.Message.Content != "" {
		t.Fatalf("failed: live=%q done=%q", live, done.Message.Content)
	}

	// Stream closed without a terminal: nothing leaks.
	live, _ = liveAndTerminal(collectFiltered(t, []ChatEvent{{Type: "text", Delta: "[[NO_"}}))
	if live != "" {
		t.Fatalf("unterminated partial leaked: %q", live)
	}

	// Terminal body carries text that never streamed (Claude merging an
	// earlier assistant turn): silence must not discard it.
	live, done = liveAndTerminal(collectFiltered(t, []ChatEvent{
		{Type: "text", Delta: tok, textSegmentStart: true},
		{Type: "done", Message: msg("先に調べた結果です\n\n" + tok)},
	}))
	if live != "" || done.Message.Content != "先に調べた結果です\n\n" {
		t.Fatalf("merged terminal: live=%q done=%q", live, done.Message.Content)
	}

	// Bare done after token-only segments still signals suppression.
	live, done = liveAndTerminal(collectFiltered(t, []ChatEvent{
		{Type: "text", Delta: tok, textSegmentStart: true},
		{Type: "tool_use"}, {Type: "tool_result"},
		{Type: "text", Delta: tok, textSegmentStart: true},
		{Type: "done"},
	}))
	if live != "" || done == nil || done.Message == nil || done.Message.Content != tok {
		t.Fatalf("bare done: live=%q done=%+v", live, done)
	}

	// Earlier merged prose mentioning the token keeps it; only the streamed
	// (last) control token is removed.
	live, done = liveAndTerminal(collectFiltered(t, []ChatEvent{
		{Type: "text", Delta: tok, textSegmentStart: true},
		{Type: "done", Message: msg("Use " + tok + " to stay silent\n\n" + tok)},
	}))
	if live != "" || done.Message.Content != "Use "+tok+" to stay silent\n\n" {
		t.Fatalf("last occurrence: live=%q done=%q", live, done.Message.Content)
	}

	// Ordinary prose is untouched, including the terminal body.
	live, done = liveAndTerminal(collectFiltered(t, []ChatEvent{
		{Type: "text", Delta: "Use " + tok},
		{Type: "done", Message: msg("Use " + tok)},
	}))
	if live != "Use "+tok || done.Message.Content != "Use "+tok {
		t.Fatalf("prose: live=%q done=%q", live, done.Message.Content)
	}
}

func TestIsNoReplyOnly(t *testing.T) {
	tok := SlackNoReplyToken
	for text, want := range map[string]bool{
		tok: true, tok + tok + tok: true, " " + tok + "\n\n" + tok + " ": true,
		"": false, tok + " ok": false, "`" + tok + "`": false,
	} {
		if IsNoReplyOnly(text) != want {
			t.Errorf("IsNoReplyOnly(%q) != %v", text, want)
		}
	}
	for text, want := range map[string]bool{
		"": true, "[[NO": true, tok + "\n[[NO_RE": true, tok + "x": false, "hi": false,
	} {
		if CouldBeNoReplyOnly(text) != want {
			t.Errorf("CouldBeNoReplyOnly(%q) != %v", text, want)
		}
	}
}

func TestGoalTurnTextMultiSegment(t *testing.T) {
	var live strings.Builder
	f := newGoalTurnText(true, true, func(e ChatEvent) bool {
		if e.Type == "text" {
			live.WriteString(e.Delta)
		}
		return true
	})
	res := &codexStreamResult{turnCompleted: true, turnStatus: "completed"}
	for _, e := range []ChatEvent{
		{Type: "text", Delta: SlackNoReplyToken, textSegmentStart: true},
		{Type: "tool_use"}, {Type: "tool_result"},
		{Type: "text", Delta: SlackNoReplyToken, textSegmentStart: true},
	} {
		if e.Type == "text" {
			res.fullText.WriteString(e.Delta)
		}
		f.send(e)
	}
	f.finish(res)
	if live.Len() != 0 || res.fullText.Len() != 0 {
		t.Fatalf("goal multi-token leaked: live=%q terminal=%q", live.String(), res.fullText.String())
	}
}

func segmentStarts(events []ChatEvent) []bool {
	var starts []bool
	for _, e := range events {
		if e.Type == "text" {
			starts = append(starts, e.textSegmentStart)
		}
	}
	return starts
}

func TestClaudeTextBlocksMarkSegmentStarts(t *testing.T) {
	events, _ := collectEvents(t,
		`{"type":"content_block_start","content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"[[NO_"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"REPLY]]"}}`,
		`{"type":"content_block_stop"}`,
		`{"type":"content_block_start","content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"[[NO_REPLY]]"}}`,
	)
	got := segmentStarts(events)
	if len(got) != 3 || !got[0] || got[1] || !got[2] {
		t.Fatalf("segment starts = %v", got)
	}
}

func TestCodexAgentMessagesMarkSegmentStarts(t *testing.T) {
	events, _ := collectCodexEvents(t, 1,
		rpcLine("item/agentMessage/delta", map[string]any{"itemId": "i1", "delta": "[[NO_"}),
		rpcLine("item/agentMessage/delta", map[string]any{"itemId": "i1", "delta": "REPLY]]"}),
		rpcLine("item/agentMessage/delta", map[string]any{"itemId": "i2", "delta": "[[NO_REPLY]]"}),
		rpcLine("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}}),
	)
	got := segmentStarts(events)
	if len(got) != 3 || !got[0] || got[1] || !got[2] {
		t.Fatalf("segment starts = %v", got)
	}
}
