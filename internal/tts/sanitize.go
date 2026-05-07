package tts

import (
	"regexp"
	"strings"
)

var (
	codeBlockRe  = regexp.MustCompile("(?s)```[a-zA-Z0-9_-]*\n.*?```")
	inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")
	// urlRe matches an http/https URL but stops at trailing sentence
	// punctuation and brackets so the surrounding sentence stays
	// readable when the URL is replaced. Excluded characters: whitespace,
	// `,;:!?` and closing brackets/quotes plus a trailing dot.
	urlRe         = regexp.MustCompile(`https?://[^\s,;:!?<>\[\]\(\){}"'` + "`" + `]+[^\s,;:!?<>\[\]\(\){}"'` + "`" + `.]`)
	multiSpaceRe  = regexp.MustCompile(`[ \t]+`)
	multiNewLines = regexp.MustCompile(`\n{3,}`)
)

// Sanitize prepares text for TTS by removing markdown noise that wastes
// audio token budget and degrades narration quality. The transformation
// is intentionally conservative — TTS narrators handle punctuation and
// natural language fine; we only strip what is genuinely unhelpful when
// spoken (long code blocks, URLs, inline backticks).
//
// The result is also length-clipped to MaxChars so a runaway agent reply
// can't blow up the audio token bill.
func Sanitize(s string) string {
	s = codeBlockRe.ReplaceAllString(s, "（コード省略）")
	s = inlineCodeRe.ReplaceAllString(s, "$1")
	s = urlRe.ReplaceAllString(s, "（リンク）")
	s = multiSpaceRe.ReplaceAllString(s, " ")
	s = multiNewLines.ReplaceAllString(s, "\n\n")
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > MaxChars {
		s = string(r[:MaxChars]) + "…"
	}
	return s
}
