// Package backenddiag keeps provider failures out of assistant prose while
// retaining their original detail and correlation IDs for diagnostics.
package backenddiag

import (
	"regexp"
	"strings"
)

const HistoryNotice = "[Previous assistant turn ended with a backend error.]"

type Diagnostic struct {
	Source    string `json:"source"`
	Detail    string `json:"detail"`
	Code      string `json:"code,omitempty"`
	Category  string `json:"category,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	MessageID string `json:"messageId,omitempty"`
}

var (
	categoryRE = regexp.MustCompile("(?m)^Details: [`'\"]*\\[([a-z_]+)\\]")
	requestRE  = regexp.MustCompile(`(?m)^Request ID: (req_[A-Za-z0-9]+)[ \t\r]*$`)
	messageRE  = regexp.MustCompile(`(?m)^Message ID: (msg_[A-Za-z0-9]+)[ \t\r]*(?:\n|$)`)
)

// Claude extracts metadata from a CLI-marked failure (isApiErrorMessage or
// a terminal refusal/API-error result), never from ordinary model prose.
func Claude(detail, category string) *Diagnostic {
	d := &Diagnostic{Source: "claude", Detail: detail, Category: category}
	if d.Category == "" {
		if m := categoryRE.FindStringSubmatch(detail); m != nil {
			d.Category = m[1]
		}
	}
	if m := requestRE.FindStringSubmatch(detail); m != nil {
		d.RequestID = m[1]
	}
	if m := messageRE.FindStringSubmatch(detail); m != nil {
		d.MessageID = m[1]
	}
	return d
}

// HistoryText normalizes old bot-authored failure banners when constructing
// prompts, without changing the stored message. Require the complete known
// leading banner and correlation IDs: a discussion mentioning an error, an
// arbitrary code sample, or a user's own request must not be rewritten.
// Callers must restrict this to assistant/bot messages, never user messages.
func HistoryText(text string) string {
	s := strings.TrimSpace(text)
	wrapped := false
	if strings.HasPrefix(s, "⚠️ 処理に失敗しました。\n```\n") {
		s = strings.TrimPrefix(s, "⚠️ 処理に失敗しました。\n```\n")
		wrapped = true
	}
	if !strings.HasPrefix(s, "API Error: ") {
		return text
	}
	header, _, _ := strings.Cut(s, "\n")
	if !strings.Contains(header, "safeguards flagged this message") &&
		!strings.Contains(header, "safeguards flagged this session") {
		return text
	}
	end := messageRE.FindStringIndex(s)
	if end == nil || !requestRE.MatchString(s[:end[0]]) {
		return text
	}
	rest := s[end[1]:]
	if wrapped {
		rest = strings.TrimPrefix(strings.TrimLeft(rest, "\r\n"), "```")
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return HistoryNotice
	}
	return HistoryNotice + "\n\n" + rest
}
