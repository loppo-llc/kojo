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

	// DefaultHeadCount is the default number of oldest messages preserved
	// by FormatForInjectionHeadTail. Anchors the conversation's framing
	// (initial request, ground rules) even after many turns.
	DefaultHeadCount = 3
	// DefaultTailCount is the default number of most-recent messages
	// preserved by FormatForInjectionHeadTail. Recovers the immediate
	// flow that a resumed backend session might have lost to a mid-thread
	// summary/reset.
	DefaultTailCount = 5
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

// FormatForInjectionHeadTail formats history as a head + omission marker +
// tail transcript. Use this when the backend already holds most of the
// conversation in its resumed session and only needs a small safety net to
// survive mid-thread session resets (and to surface user messages that
// landed between the last bot reply and the current turn).
//
// Behavior:
//   - head and tail default to DefaultHeadCount / DefaultTailCount when <= 0.
//   - When len(msgs) <= head+tail, the call delegates to
//     FormatForInjection so all messages are emitted in order with no
//     head/tail omission marker. FormatForInjection's own maxChars trim
//     still applies, so if maxChars is tight enough to drop entries it
//     will emit its own "(以下省略)" legacy footer instead.
//   - When len(msgs) >  head+tail, the output is head + "...(N件のメッセージ
//     を省略)..." + tail.
//   - maxChars caps the rendered length. The tail is treated as more
//     valuable (it carries the immediately preceding turns), so when the
//     budget is tight the head is trimmed from its oldest entry first;
//     if even the tail alone exceeds the budget, the tail is then trimmed
//     from its oldest entry. This mirrors FormatForInjection's "drop
//     oldest first" rule applied to each section. Every dropped entry
//     joins the omission count carried by the marker, so "N件" always
//     equals what is missing from the rendered output.
//
// maxMessages from FormatForInjection has no analogue here: callers
// requesting head+tail have already chosen a small bounded slice via head
// and tail, so an additional per-section cap would be redundant.
func FormatForInjectionHeadTail(msgs []HistoryMessage, botUserID string, head, tail, maxChars int) string {
	if len(msgs) == 0 {
		return ""
	}
	if head <= 0 {
		head = DefaultHeadCount
	}
	if tail <= 0 {
		tail = DefaultTailCount
	}
	if maxChars <= 0 {
		maxChars = DefaultMaxChars
	}

	// Collapse to a plain transcript when we have nothing to omit. Reuse
	// FormatForInjection so the small-thread output stays byte-identical
	// to the legacy injection format (no marker, no head/tail framing).
	// Pass len(msgs) as maxMessages so a caller that intentionally chose
	// head+tail >= 100 isn't silently clipped by DefaultMaxMessages.
	if len(msgs) <= head+tail {
		return FormatForInjection(msgs, botUserID, len(msgs), maxChars)
	}

	headLines := make([]string, 0, head)
	for _, m := range msgs[:head] {
		headLines = append(headLines, formatMessage(m, botUserID))
	}
	tailLines := make([]string, 0, tail)
	for _, m := range msgs[len(msgs)-tail:] {
		tailLines = append(tailLines, formatMessage(m, botUserID))
	}
	omitted := len(msgs) - head - tail

	header := "[Chat conversation history]\n\n"
	footer := "\n[End of history]"
	overhead := len(header) + len(footer)

	// Build the marker on demand so its character count reflects the
	// real number of omitted messages after every trim. If we shrank
	// `headLines` or `tailLines` to fit maxChars, those dropped entries
	// joined the omission, and the marker must say so.
	markerFor := func(n int) string {
		return fmt.Sprintf("...(%d件のメッセージを省略)...", n)
	}

	// Render with the current head/tail and check budget. If we exceed
	// maxChars, drop head's oldest line first; once head is empty and we
	// still overflow, drop tail's oldest line (tail-priority rule). Each
	// drop increments `omitted` so the marker stays accurate. If the
	// overhead+marker alone exceeds maxChars (only possible with an
	// absurdly small budget) we still return a usable shape — letting
	// the result exceed maxChars by a few dozen chars is strictly better
	// than emitting a truncated header that the caller will misparse.
	total := func() int {
		n := overhead + len(markerFor(omitted)) + 1 // +1 for newline after marker
		for _, l := range headLines {
			n += len(l) + 1
		}
		for _, l := range tailLines {
			n += len(l) + 1
		}
		return n
	}
	for total() > maxChars {
		if len(headLines) > 0 {
			headLines = headLines[1:]
			omitted++
			continue
		}
		if len(tailLines) > 0 {
			tailLines = tailLines[1:]
			omitted++
			continue
		}
		break
	}

	var sb strings.Builder
	sb.WriteString(header)
	for _, l := range headLines {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	sb.WriteString(markerFor(omitted))
	for _, l := range tailLines {
		sb.WriteByte('\n')
		sb.WriteString(l)
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
