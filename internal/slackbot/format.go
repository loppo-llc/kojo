package slackbot

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Slack mrkdwn uses different formatting from standard Markdown.
// These helpers convert between the two.

var (
	// Slack link format: <URL|text> or <URL>
	reLinkWithText = regexp.MustCompile(`<(https?://[^|>]+)\|([^>]+)>`)
	reLinkBare     = regexp.MustCompile(`<(https?://[^>]+)>`)

	// Slack user/channel mentions: <@U12345> or <#C12345|channel-name>
	reUserMention    = regexp.MustCompile(`<@([A-Z0-9]+)>`)
	reChannelMention = regexp.MustCompile(`<#[A-Z0-9]+\|([^>]+)>`)

	// Markdown bold **text** → Slack bold *text*
	reMdBold = regexp.MustCompile(`\*\*(.+?)\*\*`)
	// Markdown strikethrough ~~text~~ → Slack ~text~
	reMdStrike = regexp.MustCompile(`~~(.+?)~~`)
	// Markdown link [text](url) → Slack <url|text>
	reMdLink = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)]+)\)`)
	// Markdown heading: lines starting with # (1-6 levels)
	reMdHeading = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)

	// Code blocks (fenced with ```)
	reCodeBlock = regexp.MustCompile("(?s)```[^\n]*\n(.*?)```")
	// Inline code
	reInlineCode = regexp.MustCompile("`[^`]+`")
)

// UserResolver resolves a Slack user ID to a display name.
// Return the original ID if resolution fails.
type UserResolver func(userID string) string

// SlackToPlain converts Slack mrkdwn to plain text suitable for the agent.
// It resolves links and strips mention formatting.
// If resolve is non-nil, user mentions <@U12345> are resolved to display names.
func SlackToPlain(text string, resolve UserResolver) string {
	// Replace links: <url|text> → text (url)
	text = reLinkWithText.ReplaceAllString(text, "$2 ($1)")
	// Replace bare links: <url> → url
	text = reLinkBare.ReplaceAllString(text, "$1")
	// Replace channel mentions: <#C123|general> → #general
	text = reChannelMention.ReplaceAllString(text, "#$1")
	// Resolve user mentions: <@U12345> → @DisplayName
	text = reUserMention.ReplaceAllStringFunc(text, func(match string) string {
		id := reUserMention.FindStringSubmatch(match)[1]
		if resolve != nil {
			return "@" + resolve(id)
		}
		return "@" + id
	})
	return text
}

// StripBotMention removes all @bot mentions from the message text.
func StripBotMention(text, botUserID string) string {
	mention := "<@" + botUserID + ">"
	text = strings.ReplaceAll(text, mention, "")
	text = strings.TrimSpace(text)
	return text
}

// PlainToSlack converts standard Markdown from agent output to Slack mrkdwn.
// It preserves code blocks and inline code from being transformed.
func PlainToSlack(text string) string {
	// Extract code blocks and inline code, replace with placeholders
	type placeholder struct {
		key     string
		content string
	}
	var placeholders []placeholder
	idx := 0

	// Replace fenced code blocks first
	text = reCodeBlock.ReplaceAllStringFunc(text, func(match string) string {
		key := fmt.Sprintf("\x00CODEBLOCK%d\x00", idx)
		placeholders = append(placeholders, placeholder{key, match})
		idx++
		return key
	})

	// Replace inline code
	text = reInlineCode.ReplaceAllStringFunc(text, func(match string) string {
		key := fmt.Sprintf("\x00INLINECODE%d\x00", idx)
		placeholders = append(placeholders, placeholder{key, match})
		idx++
		return key
	})

	// Convert **bold** to *bold*
	text = reMdBold.ReplaceAllString(text, "*$1*")
	// Convert ~~strike~~ to ~strike~
	text = reMdStrike.ReplaceAllString(text, "~$1~")
	// Convert [text](url) to <url|text>
	text = reMdLink.ReplaceAllString(text, "<$2|$1>")
	// Convert headings to bold text
	text = reMdHeading.ReplaceAllString(text, "*$1*")

	// Restore code blocks and inline code
	for _, p := range placeholders {
		text = strings.Replace(text, p.key, p.content, 1)
	}

	return text
}

// SplitMessage splits a long message into chunks that fit within Slack's
// message length limit (approximately 3000 chars per chunk for safety).
func SplitMessage(text string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = 3000
	}
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		// Try to split at a paragraph boundary
		cut := maxLen
		if idx := strings.LastIndex(text[:maxLen], "\n\n"); idx > maxLen/2 {
			cut = idx + 2
		} else if idx := strings.LastIndex(text[:maxLen], "\n"); idx > maxLen/2 {
			cut = idx + 1
		}
		// Ensure cut falls on a UTF-8 rune boundary to avoid splitting
		// multi-byte characters (e.g. Japanese text at 3 bytes per char).
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	return chunks
}
