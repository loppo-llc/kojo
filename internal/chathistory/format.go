package chathistory

import (
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultMaxMessages is the default message count limit for injection.
	DefaultMaxMessages = 100
	// DefaultMaxChars is the default character limit for injection.
	DefaultMaxChars = 50000
)

// FormatForInjection formats history messages into a human-readable transcript
// for injection into the agent's user message.
//
// Messages are trimmed to fit within maxMessages and maxChars limits, keeping
// the most recent messages and dropping older ones first.
// botUserID identifies the agent's own messages for labeling.
func FormatForInjection(msgs []HistoryMessage, botUserID string, maxMessages, maxChars int) string {
	if len(msgs) == 0 {
		return ""
	}
	if maxMessages <= 0 {
		maxMessages = DefaultMaxMessages
	}
	if maxChars <= 0 {
		maxChars = DefaultMaxChars
	}

	// Apply message count limit (keep most recent)
	if len(msgs) > maxMessages {
		msgs = msgs[len(msgs)-maxMessages:]
	}

	// Format all messages
	var lines []string
	for _, m := range msgs {
		line := formatMessage(m, botUserID)
		lines = append(lines, line)
	}

	// Apply character limit (drop oldest lines first)
	header := "[Chat conversation history]\n\n"
	footer := "\n[End of history]"
	overhead := len(header) + len(footer)
	totalChars := overhead
	for _, l := range lines {
		totalChars += len(l) + 1 // +1 for newline
	}

	if totalChars > maxChars {
		// Drop from the front until we fit
		budget := maxChars - overhead - len("...(older messages omitted)\n")
		for len(lines) > 0 && sumLen(lines)+len(lines) > budget {
			lines = lines[1:]
		}
		lines = append([]string{"...(older messages omitted)"}, lines...)
	}

	var sb strings.Builder
	sb.WriteString(header)
	for i, l := range lines {
		sb.WriteString(l)
		if i < len(lines)-1 {
			sb.WriteByte('\n')
		}
	}
	sb.WriteString(footer)
	return sb.String()
}

func formatMessage(m HistoryMessage, botUserID string) string {
	ts := ""
	if t, err := time.Parse(time.RFC3339, m.Timestamp); err == nil {
		ts = t.Format("15:04")
	}

	name := m.UserName
	if name == "" {
		name = m.UserID
	}

	label := ""
	if m.IsBot && m.UserID == botUserID {
		label = fmt.Sprintf("%s [you] (%s)", name, ts)
	} else if m.IsBot {
		label = fmt.Sprintf("%s [bot] (%s)", name, ts)
	} else {
		label = fmt.Sprintf("%s (%s)", name, ts)
	}

	return label + ": " + m.Text
}

func sumLen(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l)
	}
	return n
}
