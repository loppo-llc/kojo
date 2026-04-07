package chathistory

import (
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

func TestFormatForInjection_OtherBot(t *testing.T) {
	msgs := []HistoryMessage{
		{UserName: "other-bot", UserID: "B2", Text: "hello", Timestamp: "2026-04-08T10:00:00+09:00", IsBot: true},
	}

	result := FormatForInjection(msgs, "B1", 100, 50000)
	if !strings.Contains(result, "other-bot [bot] (10:00): hello") {
		t.Errorf("expected [bot] label for other bot, got: %s", result)
	}
}
