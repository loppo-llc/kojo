package slackbot

import (
	"strings"
	"testing"
)

func TestSlackToPlain(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "link with text",
			input: `Check <https://example.com|this link>`,
			want:  `Check this link (https://example.com)`,
		},
		{
			name:  "bare link",
			input: `Visit <https://example.com>`,
			want:  `Visit https://example.com`,
		},
		{
			name:  "channel mention",
			input: `Go to <#C12345|general>`,
			want:  `Go to #general`,
		},
		{
			name:  "user mention",
			input: `Hello <@U12345>`,
			want:  `Hello @U12345`,
		},
		{
			name:  "mixed formatting",
			input: `<@U12345> posted <https://example.com|a link> in <#C12345|random>`,
			want:  `@U12345 posted a link (https://example.com) in #random`,
		},
		{
			name:  "plain text unchanged",
			input: `Hello world`,
			want:  `Hello world`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SlackToPlain(tt.input)
			if got != tt.want {
				t.Errorf("SlackToPlain(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStripBotMention(t *testing.T) {
	botID := "U99999"
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "mention at start",
			input: "<@U99999> help me",
			want:  "help me",
		},
		{
			name:  "mention in middle",
			input: "hey <@U99999> help me",
			want:  "hey  help me",
		},
		{
			name:  "mention at end",
			input: "help me <@U99999>",
			want:  "help me",
		},
		{
			name:  "multiple mentions",
			input: "<@U99999> do <@U99999> this",
			want:  "do  this",
		},
		{
			name:  "no mention",
			input: "hello there",
			want:  "hello there",
		},
		{
			name:  "only mention",
			input: "<@U99999>",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripBotMention(tt.input, botID)
			if got != tt.want {
				t.Errorf("StripBotMention(%q, %q) = %q, want %q", tt.input, botID, got, tt.want)
			}
		})
	}
}

func TestPlainToSlack(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "bold conversion",
			input: "This is **bold** text",
			want:  "This is *bold* text",
		},
		{
			name:  "strikethrough conversion",
			input: "This is ~~deleted~~ text",
			want:  "This is ~deleted~ text",
		},
		{
			name:  "markdown link conversion",
			input: "Click [here](https://example.com) now",
			want:  "Click <https://example.com|here> now",
		},
		{
			name:  "heading conversion",
			input: "# Main Title\n## Subtitle",
			want:  "*Main Title*\n*Subtitle*",
		},
		{
			name:  "code block preserved",
			input: "```go\n**bold inside code**\n```",
			want:  "```go\n**bold inside code**\n```",
		},
		{
			name:  "inline code preserved",
			input: "Use `**not bold**` here",
			want:  "Use `**not bold**` here",
		},
		{
			name:  "mixed with code blocks",
			input: "**bold** then ```\n**code**\n``` then **more bold**",
			want:  "*bold* then ```\n**code**\n``` then *more bold*",
		},
		{
			name:  "italic unchanged",
			input: "_italic text_",
			want:  "_italic text_",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlainToSlack(tt.input)
			if got != tt.want {
				t.Errorf("PlainToSlack(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSplitMessage(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		maxLen int
		want   int // expected number of chunks
	}{
		{
			name:   "short message",
			text:   "hello",
			maxLen: 100,
			want:   1,
		},
		{
			name:   "exact length",
			text:   "12345",
			maxLen: 5,
			want:   1,
		},
		{
			name:   "needs split",
			text:   strings.Repeat("a", 100),
			maxLen: 40,
			want:   3,
		},
		{
			name:   "split at paragraph",
			text:   strings.Repeat("a", 30) + "\n\n" + strings.Repeat("b", 30),
			maxLen: 50,
			want:   2,
		},
		{
			name:   "zero maxLen defaults to 3000",
			text:   "short",
			maxLen: 0,
			want:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := SplitMessage(tt.text, tt.maxLen)
			if len(chunks) != tt.want {
				t.Errorf("SplitMessage() returned %d chunks, want %d", len(chunks), tt.want)
			}
			// Verify all text is preserved
			joined := strings.Join(chunks, "")
			if joined != tt.text {
				t.Errorf("SplitMessage() lost content: joined length %d, original %d", len(joined), len(tt.text))
			}
			// Verify no chunk exceeds maxLen (use default if 0)
			limit := tt.maxLen
			if limit <= 0 {
				limit = 3000
			}
			for i, chunk := range chunks {
				if len(chunk) > limit {
					t.Errorf("chunk %d exceeds maxLen: %d > %d", i, len(chunk), limit)
				}
			}
		})
	}
}
