package agent

import "strings"

const (
	replyOpenTag  = "<reply>"
	replyCloseTag = "</reply>"
)

// ReplyTagFilter extracts text inside <reply>...</reply> tags from a stream
// of text deltas. Text outside the tags is treated as internal reasoning and
// discarded. If the stream ends without any <reply> tag, Flush returns all
// accumulated text as a graceful degradation (so a misbehaving agent still
// produces output).
type ReplyTagFilter struct {
	buf       strings.Builder // buffered text not yet classified
	emitted   strings.Builder // all text emitted so far (for graceful degradation tracking)
	allText   strings.Builder // all text received (for graceful degradation)
	inReply   bool            // currently inside <reply> tags
	sawReply  bool            // saw at least one <reply> tag
	done      bool            // </reply> seen, ignore rest
}

// Feed processes a text delta and returns the portion that should be forwarded
// (text inside <reply> tags). Returns empty string for text outside tags or
// when buffering incomplete tags.
func (f *ReplyTagFilter) Feed(delta string) string {
	if f.done {
		return ""
	}

	f.allText.WriteString(delta)
	f.buf.WriteString(delta)

	var out strings.Builder
	f.process(&out)
	if out.Len() > 0 {
		f.emitted.WriteString(out.String())
	}
	return out.String()
}

// Flush returns any remaining buffered content. Called when the stream ends.
// If no <reply> tag was ever seen, returns ALL accumulated text (graceful
// degradation so the agent's response is not silently swallowed).
func (f *ReplyTagFilter) Flush() string {
	if f.done {
		return ""
	}

	if !f.sawReply {
		// Agent never used <reply> tags — return everything
		return f.allText.String()
	}

	// If we're still inside <reply> (agent forgot </reply>), flush the buffer
	if f.inReply {
		remaining := f.buf.String()
		f.buf.Reset()
		f.done = true
		return remaining
	}

	// Outside <reply> with no pending tag — nothing to emit
	return ""
}

// process scans the buffer and emits reply-tagged content to out.
func (f *ReplyTagFilter) process(out *strings.Builder) {
	for {
		s := f.buf.String()
		if len(s) == 0 {
			return
		}

		if f.inReply {
			// Look for </reply>
			idx := strings.Index(s, replyCloseTag)
			if idx >= 0 {
				// Emit text up to </reply>
				out.WriteString(s[:idx])
				f.buf.Reset()
				f.buf.WriteString(s[idx+len(replyCloseTag):])
				f.inReply = false
				f.done = true
				return
			}
			// Check if buffer ends with a partial </reply> tag
			if hold := partialSuffix(s, replyCloseTag); hold > 0 {
				// Emit everything except the potential partial tag
				safe := s[:len(s)-hold]
				if len(safe) > 0 {
					out.WriteString(safe)
				}
				partial := s[len(s)-hold:]
				f.buf.Reset()
				f.buf.WriteString(partial)
				return
			}
			// No close tag or partial — emit everything
			out.WriteString(s)
			f.buf.Reset()
			return
		}

		// Not in reply — look for <reply>
		idx := strings.Index(s, replyOpenTag)
		if idx >= 0 {
			f.sawReply = true
			f.inReply = true
			f.buf.Reset()
			f.buf.WriteString(s[idx+len(replyOpenTag):])
			continue // re-process remaining buffer in inReply mode
		}
		// Check if buffer ends with a partial <reply> tag
		if hold := partialSuffix(s, replyOpenTag); hold > 0 {
			// Keep the partial tag in buffer, discard the rest
			partial := s[len(s)-hold:]
			f.buf.Reset()
			f.buf.WriteString(partial)
			return
		}
		// No open tag or partial — discard everything (outside reply)
		f.buf.Reset()
		return
	}
}

// partialSuffix returns the length of the longest suffix of s that is a
// prefix of tag. This detects cases like s ending with "<rep" when tag is
// "<reply>".
func partialSuffix(s, tag string) int {
	maxLen := len(tag) - 1
	if maxLen > len(s) {
		maxLen = len(s)
	}
	for n := maxLen; n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}
