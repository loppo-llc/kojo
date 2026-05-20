package chathistory

import (
	"fmt"
	"strings"
	"testing"
)

func TestFormatForInjection_Basic(t *testing.T) {
	msgs := []HistoryMessage{
		{UserName: "alice", Text: "hello", Timestamp: "2026-04-08T10:00:00+09:00", IsBot: false},
		{UserName: "bot", UserID: "B1", Text: "hi there", Timestamp: "2026-04-08T10:01:00+09:00", IsBot: true},
		{UserName: "alice", Text: "thanks", Timestamp: "2026-04-08T10:02:00+09:00", IsBot: false},
	}

	result := FormatForInjection(msgs, "B1", 100, 50000)

	if !strings.Contains(result, "[Chat conversation history]") {
		t.Error("missing header")
	}
	if !strings.Contains(result, "[End of history]") {
		t.Error("missing footer")
	}
	if !strings.Contains(result, "alice (10:00): hello") {
		t.Error("missing alice message")
	}
	if !strings.Contains(result, "bot [you] (10:01): hi there") {
		t.Error("missing bot [you] message")
	}
}

func TestFormatForInjection_Empty(t *testing.T) {
	result := FormatForInjection(nil, "B1", 100, 50000)
	if result != "" {
		t.Errorf("expected empty, got %q", result)
	}
}

func TestFormatForInjection_MaxMessages(t *testing.T) {
	var msgs []HistoryMessage
	for i := 0; i < 150; i++ {
		msgs = append(msgs, HistoryMessage{
			UserName:  "user",
			Text:      "msg",
			Timestamp: "2026-04-08T10:00:00+09:00",
		})
	}

	result := FormatForInjection(msgs, "B1", 100, 500000)
	// Should have at most 100 message lines
	lines := strings.Split(result, "\n")
	msgLines := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "user (") {
			msgLines++
		}
	}
	if msgLines > 100 {
		t.Errorf("expected at most 100 message lines, got %d", msgLines)
	}
}

func TestFormatForInjection_MaxChars(t *testing.T) {
	var msgs []HistoryMessage
	for i := 0; i < 50; i++ {
		msgs = append(msgs, HistoryMessage{
			UserName:  "user",
			Text:      strings.Repeat("x", 2000),
			Timestamp: "2026-04-08T10:00:00+09:00",
		})
	}

	result := FormatForInjection(msgs, "B1", 100, 5000)
	if len(result) > 5000 {
		t.Errorf("result exceeds maxChars: %d > 5000", len(result))
	}
	if !strings.Contains(result, "...(older messages omitted)") {
		t.Error("expected truncation marker")
	}
}

func TestFormatForInjectionHeadTail_BelowThreshold(t *testing.T) {
	// 6 messages, head=3 + tail=5 = 8 → all messages emitted, no marker.
	var msgs []HistoryMessage
	for i := 0; i < 6; i++ {
		msgs = append(msgs, HistoryMessage{
			UserName:  "user",
			Text:      fmt.Sprintf("m%d", i),
			Timestamp: "2026-04-08T10:00:00+09:00",
		})
	}
	result := FormatForInjectionHeadTail(msgs, "B1", 3, 5, 50000)
	if strings.Contains(result, "件のメッセージを省略") {
		t.Errorf("expected no omission marker for 6 messages, got: %s", result)
	}
	for i := 0; i < 6; i++ {
		if !strings.Contains(result, fmt.Sprintf(": m%d", i)) {
			t.Errorf("missing message m%d in output: %s", i, result)
		}
	}
}

func TestFormatForInjectionHeadTail_AboveThreshold(t *testing.T) {
	// 20 messages with head=3, tail=5 → first 3 + marker(12) + last 5.
	var msgs []HistoryMessage
	for i := 0; i < 20; i++ {
		msgs = append(msgs, HistoryMessage{
			UserName:  "user",
			Text:      fmt.Sprintf("m%02d", i),
			Timestamp: "2026-04-08T10:00:00+09:00",
		})
	}
	result := FormatForInjectionHeadTail(msgs, "B1", 3, 5, 50000)

	// Head (first 3) present, indices 0..2.
	for i := 0; i < 3; i++ {
		if !strings.Contains(result, fmt.Sprintf(": m%02d", i)) {
			t.Errorf("missing head message m%02d: %s", i, result)
		}
	}
	// Middle dropped, indices 3..14.
	for i := 3; i < 15; i++ {
		if strings.Contains(result, fmt.Sprintf(": m%02d", i)) {
			t.Errorf("middle message m%02d should be omitted: %s", i, result)
		}
	}
	// Tail (last 5) present, indices 15..19.
	for i := 15; i < 20; i++ {
		if !strings.Contains(result, fmt.Sprintf(": m%02d", i)) {
			t.Errorf("missing tail message m%02d: %s", i, result)
		}
	}
	// Marker with the correct omitted count.
	if !strings.Contains(result, "...(12件のメッセージを省略)...") {
		t.Errorf("expected '12件のメッセージを省略' marker, got: %s", result)
	}
}

func TestFormatForInjectionHeadTail_AtExactThreshold(t *testing.T) {
	// 8 messages with head=3, tail=5 → exact boundary, no omission.
	var msgs []HistoryMessage
	for i := 0; i < 8; i++ {
		msgs = append(msgs, HistoryMessage{
			UserName:  "user",
			Text:      fmt.Sprintf("m%d", i),
			Timestamp: "2026-04-08T10:00:00+09:00",
		})
	}
	result := FormatForInjectionHeadTail(msgs, "B1", 3, 5, 50000)
	if strings.Contains(result, "件のメッセージを省略") {
		t.Errorf("expected no omission marker at exact threshold, got: %s", result)
	}
	for i := 0; i < 8; i++ {
		if !strings.Contains(result, fmt.Sprintf(": m%d", i)) {
			t.Errorf("missing message m%d: %s", i, result)
		}
	}
}

func TestFormatForInjectionHeadTail_Empty(t *testing.T) {
	if got := FormatForInjectionHeadTail(nil, "B1", 3, 5, 50000); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestFormatForInjectionHeadTail_Defaults(t *testing.T) {
	// head<=0 / tail<=0 / maxChars<=0 fall back to defaults.
	var msgs []HistoryMessage
	for i := 0; i < 20; i++ {
		msgs = append(msgs, HistoryMessage{
			UserName:  "user",
			Text:      fmt.Sprintf("m%02d", i),
			Timestamp: "2026-04-08T10:00:00+09:00",
		})
	}
	result := FormatForInjectionHeadTail(msgs, "B1", 0, 0, 0)
	// Defaults are head=3, tail=5: omit 12 messages.
	if !strings.Contains(result, "...(12件のメッセージを省略)...") {
		t.Errorf("expected '12件のメッセージを省略' marker under defaults, got: %s", result)
	}
}

func TestFormatForInjectionHeadTail_MaxCharsTrimsHeadFirst(t *testing.T) {
	// Build messages whose every line is ~2010 chars (head/tail count = 3/5
	// → before trimming we'd render 8 lines * ~2010 = ~16K plus overhead).
	// Cap at 11000 chars; expect the head to vanish but the full tail to
	// survive (tail-priority rule).
	var msgs []HistoryMessage
	for i := 0; i < 20; i++ {
		msgs = append(msgs, HistoryMessage{
			UserName:  "u",
			Text:      strings.Repeat("x", 2000) + fmt.Sprintf("M%02d", i),
			Timestamp: "2026-04-08T10:00:00+09:00",
		})
	}
	// Budget = 11000: tail alone (5 * ~2010 ≈ 10050) fits with marker+overhead,
	// but head (+3 * ~2010 ≈ 6030) pushes us well past the limit. Expect
	// every head entry trimmed before any tail entry is touched.
	result := FormatForInjectionHeadTail(msgs, "B1", 3, 5, 11000)
	if len(result) > 11000 {
		t.Errorf("result exceeds maxChars: %d > 11000", len(result))
	}
	// Tail entries (15..19) must all survive.
	for i := 15; i < 20; i++ {
		if !strings.Contains(result, fmt.Sprintf("M%02d", i)) {
			t.Errorf("tail entry M%02d dropped under budget pressure: result len=%d", i, len(result))
		}
	}
	// Head entries (0..2) should be dropped before any tail entry.
	for i := 0; i < 3; i++ {
		if strings.Contains(result, fmt.Sprintf("M%02d", i)) {
			t.Errorf("head entry M%02d should have been dropped before tail under maxChars=8000", i)
		}
	}
	// Marker still present.
	if !strings.Contains(result, "件のメッセージを省略") {
		t.Errorf("expected omission marker, got: %s", result)
	}
}

func TestFormatForInjectionHeadTail_MarkerCountReflectsTrim(t *testing.T) {
	// 20 messages; head=3, tail=5 → omitted starts at 12. Tight budget
	// forces all head + 2 tail entries to drop, so omitted should become
	// 12 + 3 + 2 = 17 and the marker must reflect that.
	var msgs []HistoryMessage
	for i := 0; i < 20; i++ {
		msgs = append(msgs, HistoryMessage{
			UserName:  "u",
			Text:      strings.Repeat("x", 2000) + fmt.Sprintf("M%02d", i),
			Timestamp: "2026-04-08T10:00:00+09:00",
		})
	}
	// Budget = 7000: tail alone (5 * ~2010 ≈ 10050) overflows, head must
	// be fully trimmed and tail trimmed down until we fit.
	result := FormatForInjectionHeadTail(msgs, "B1", 3, 5, 7000)
	if len(result) > 7000 {
		t.Errorf("result exceeds maxChars: %d > 7000", len(result))
	}
	// Count how many of the original tail entries survive (M15..M19).
	surviving := 0
	for i := 15; i < 20; i++ {
		if strings.Contains(result, fmt.Sprintf("M%02d", i)) {
			surviving++
		}
	}
	expectedOmitted := 20 - surviving // 12 middle + (3 head + (5-surviving) tail)
	want := fmt.Sprintf("...(%d件のメッセージを省略)...", expectedOmitted)
	if !strings.Contains(result, want) {
		t.Errorf("expected marker %q (surviving tail=%d), got: %s", want, surviving, result)
	}
}

func TestFormatForInjectionHeadTail_TightBudgetDrainsTail(t *testing.T) {
	// Pathological budget — too small for even one tail line. The loop
	// must terminate (no infinite loop) and produce at most header,
	// marker, footer with omitted == len(msgs).
	var msgs []HistoryMessage
	for i := 0; i < 10; i++ {
		msgs = append(msgs, HistoryMessage{
			UserName:  "u",
			Text:      strings.Repeat("x", 2000) + fmt.Sprintf("M%02d", i),
			Timestamp: "2026-04-08T10:00:00+09:00",
		})
	}
	result := FormatForInjectionHeadTail(msgs, "B1", 3, 5, 200)
	// No M%02d entry should survive when the budget is below a single line.
	for i := 0; i < 10; i++ {
		if strings.Contains(result, fmt.Sprintf("M%02d", i)) {
			t.Errorf("entry M%02d should have been dropped under budget=200", i)
		}
	}
	// Marker should report all 10 entries omitted (2 middle + 3 head + 5 tail).
	if !strings.Contains(result, "...(10件のメッセージを省略)...") {
		t.Errorf("expected 10件 marker, got: %s", result)
	}
}

func TestFormatForInjection_OtherBot(t *testing.T) {
	msgs := []HistoryMessage{
		{UserName: "other-bot", UserID: "B2", Text: "hello", Timestamp: "2026-04-08T10:00:00+09:00", IsBot: true},
	}

	result := FormatForInjection(msgs, "B1", 100, 50000)
	if !strings.Contains(result, "other-bot [bot] (10:00): hello") {
		t.Errorf("expected [bot] label for other bot, got: %s", result)
	}
}
