package slackbot

import (
	"regexp"
	"strings"
)

// Slack mrkdwn uses different formatting from standard Markdown.
// These helpers convert between the two.

var (
	// Slack link format: <URL|text> or <URL>
	reLinkWithText = regexp.MustCompile(`<(https?://[^|>]+)\|([^>]+)>`)
	reLinkBare     = regexp.MustCompile(`<(https?://[^>]+)>`)

	// Slack user/channel mentions: <@U12345> or <#C12345|channel-name>
	reUserMention   = regexp.MustCompile(`<@([A-Z0-9]+)>`)
	reChannelMention = regexp.MustCompile(`<#[A-Z0-9]+\|([^>]+)>`)

	// Markdown bold **text** → Slack bold *text*
	reMdBold = regexp.MustCompile(`\*\*(.+?)\*\*`)
	// Markdown strikethrough ~~text~~ → Slack ~text~
	reMdStrike = regexp.MustCompile(`~~(.+?)~~`)
)

// SlackToPlain converts Slack mrkdwn to plain text suitable for the agent.
// It resolves links and strips mention formatting.
func SlackToPlain(text string) string {
	// Replace links: <url|text> → text (url)
	text = reLinkWithText.ReplaceAllString(text, "$2 ($1)")
	// Replace bare links: <url> → url
	text = reLinkBare.ReplaceAllString(text, "$1")
	// Replace channel mentions: <#C123|general> → #general
	text = reChannelMention.ReplaceAllString(text, "#$1")
	// User mentions are kept as-is for now (resolved by caller if needed)
	return text
}

// StripBotMention removes the bot's own @mention from the beginning of a message.
func StripBotMention(text, botUserID string) string {
	mention := "<@" + botUserID + ">"
	text = strings.TrimPrefix(text, mention)
	text = strings.TrimLeft(text, " ")
	return text
}

// PlainToSlack converts standard Markdown from agent output to Slack mrkdwn.
func PlainToSlack(text string) string {
	// Convert **bold** to *bold*
	text = reMdBold.ReplaceAllString(text, "*$1*")
	// Convert ~~strike~~ to ~strike~
	text = reMdStrike.ReplaceAllString(text, "~$1~")
	// Code blocks and inline code use the same syntax — no conversion needed.
	// Italic (_text_) is the same — no conversion needed.
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
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	return chunks
}
