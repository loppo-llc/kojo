package backenddiag

import (
	"strings"
	"testing"
)

const banner = "API Error: Opus 5.5's safeguards flagged this message (https://www.anthropic.com/legal/aup).\n\nTry rephrasing the request in a new session or change your model.\n\nLearn more: https://support.claude.com/en/articles/8106465\n\nDetails: `[reasoning_extraction]`\n\nRequest ID: req_test123\n\nMessage ID: msg_test456"

func TestClaudeDiagnostic(t *testing.T) {
	d := Claude(banner, "")
	if d.Detail != banner || d.Source != "claude" || d.Category != "reasoning_extraction" || d.RequestID != "req_test123" || d.MessageID != "msg_test456" {
		t.Fatalf("diagnostic = %+v", d)
	}
	if Claude(banner, "explicit_category").Category != "explicit_category" {
		t.Fatal("explicit stream category lost")
	}
}

func TestHistoryText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"banner", banner, HistoryNotice},
		{"partial work retained", banner + "\n\n調査途中です。", HistoryNotice + "\n\n調査途中です。"},
		{"Slack wrapper", "⚠️ 処理に失敗しました。\n```\n" + strings.ReplaceAll(banner, "`", "'") + "\n```", HistoryNotice},
		{"Slack link formatting", strings.ReplaceAll(banner, "(https://www.anthropic.com/legal/aup)", "(anthropic.com/legal/aup (https://www.anthropic.com/legal/aup))"), HistoryNotice},
		{"CRLF", strings.ReplaceAll(banner, "\n", "\r\n"), HistoryNotice},
		{"normal explanation", "The error was:\n" + banner, "The error was:\n" + banner},
		{"code sample", "```text\n" + banner + "\n```", "```text\n" + banner + "\n```"},
		{"missing IDs", strings.Split(banner, "Request ID:")[0], strings.Split(banner, "Request ID:")[0]},
		{"unrelated API failure", "API Error: rate limited\nRequest ID: req_x\nMessage ID: msg_x", "API Error: rate limited\nRequest ID: req_x\nMessage ID: msg_x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HistoryText(tc.in); got != tc.want {
				t.Fatalf("HistoryText() = %q, want %q", got, tc.want)
			}
		})
	}
}
