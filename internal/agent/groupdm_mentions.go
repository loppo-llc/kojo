package agent

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// mentionToken matches @name tokens in message text. Names may contain
// letters, digits, underscore, dot, and hyphen — matching both agent ids
// (ag_xxx) and typical display names. Multi-word display names are matched
// by their first word (see parseGroupMentions).
var mentionToken = regexp.MustCompile(`@([\p{L}\p{N}_.\-]+)`)

// parseGroupMentions extracts the set of mentioned member ids from content.
//
// A token @X mentions:
//   - the human operator when X folds to "user" (the reserved sentinel),
//   - a member whose agent id equals X (case-sensitive ids like "ag_..."),
//   - a member whose display name equals X case-insensitively, or whose
//     display name's first whitespace-separated word equals X (so
//     "@Alice" hits the member named "Alice Smith").
//
// Result is deduplicated, in first-occurrence order. names maps agent id →
// current display name (may be empty for unknown agents).
func parseGroupMentions(content string, memberIDs []string, names map[string]string) []string {
	if !strings.Contains(content, "@") {
		return nil
	}
	matches := mentionToken.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, m := range matches {
		// Boundary check: the @ must sit at the start of the text or after
		// a non-word rune, so "foo@user.com" (an email) does not read as a
		// mention of @user.com's prefix.
		if at := m[0]; at > 0 {
			r, _ := utf8.DecodeLastRuneInString(content[:at])
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '@' {
				continue
			}
		}
		tok := content[m[2]:m[3]]
		fold := strings.ToLower(tok)
		if fold == UserSenderID {
			add(UserSenderID)
			continue
		}
		for _, id := range memberIDs {
			if id == tok {
				add(id)
				continue
			}
			name := names[id]
			if name == "" {
				continue
			}
			nf := strings.ToLower(name)
			first, _, _ := strings.Cut(nf, " ")
			if nf == fold || first == fold {
				add(id)
			}
		}
	}
	return out
}

// containsMention reports whether id is in the parsed mention list.
func containsMention(mentions []string, id string) bool {
	for _, m := range mentions {
		if m == id {
			return true
		}
	}
	return false
}
