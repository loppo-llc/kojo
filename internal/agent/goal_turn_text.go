package agent

import "strings"

// SlackNoReplyToken is a complete assistant response requesting no Slack post.
// A mention inside ordinary prose is not a control response.
const SlackNoReplyToken = "[[NO_REPLY]]"

// isSlackConversationKey reports whether a one-shot session key belongs to a
// Slack conversation. Only those turns interpret SlackNoReplyToken.
func isSlackConversationKey(agentID, key string) bool {
	return agentID != "" && strings.HasPrefix(key, agentID+":slack:")
}

// noReplySegments filters Slack no-reply control tokens out of one assistant
// text stream. The unit is a text segment: a contiguous run of main-turn text
// delimited by tool/thinking activity or an explicit backend segment start (a
// Claude text content block, a Codex agentMessage item). A segment whose
// trimmed body is exactly SlackNoReplyToken is a control signal and is
// dropped; every other segment, including prose that merely mentions the
// token, is emitted verbatim. Only a possible token prefix is ever buffered, so
// ordinary text still streams immediately.
type noReplySegments struct {
	emit func(ChatEvent) bool

	pending    strings.Builder // current segment while it may still be the token
	inSegment  bool            // current segment already diverged and streams through
	pendingEvt ChatEvent       // template (metadata) for the buffered text

	raw  strings.Builder // every main-turn text delta, as received
	kept strings.Builder // every main-turn text delta actually emitted

	tokenSegments int  // segments dropped as exact control tokens
	droppedAny    bool // any text (token or failed partial) was withheld
}

func newNoReplySegments(emit func(ChatEvent) bool) *noReplySegments {
	return &noReplySegments{emit: emit}
}

func isNoReplySegmentBoundary(evt ChatEvent) bool {
	if evt.ParentToolUseID != "" {
		return false
	}
	switch evt.Type {
	case "tool_use", "tool_result", "thinking":
		return true
	}
	return false
}

// send filters one event. It returns false when a downstream emit refused an
// event (the caller's cancellation signal).
func (f *noReplySegments) send(evt ChatEvent) bool {
	if evt.Type != "text" || evt.ParentToolUseID != "" {
		ok := true
		if isNoReplySegmentBoundary(evt) {
			ok = f.endSegment(true)
		}
		return f.emit(evt) && ok
	}
	if evt.Delta == "" {
		return f.emit(evt)
	}
	ok := true
	if evt.textSegmentStart {
		ok = f.endSegment(true)
	}
	f.raw.WriteString(evt.Delta)
	if f.inSegment {
		return f.emitText(evt) && ok
	}
	if f.pending.Len() == 0 {
		f.pendingEvt = evt
	}
	f.pending.WriteString(evt.Delta)
	if strings.HasPrefix(SlackNoReplyToken, strings.TrimSpace(f.pending.String())) {
		return ok
	}
	// Diverged from the token: flush the buffered prefix and stream the rest.
	f.inSegment = true
	out := f.pendingEvt
	out.Delta = f.pending.String()
	f.pending.Reset()
	return f.emitText(out) && ok
}

func (f *noReplySegments) emitText(evt ChatEvent) bool {
	evt.textSegmentStart = false
	f.kept.WriteString(evt.Delta)
	return f.emit(evt)
}

// endSegment resolves the buffered segment at a boundary. complete reports
// whether the segment really ended (boundary or clean terminal) rather than
// being cut off by a failure.
func (f *noReplySegments) endSegment(complete bool) bool {
	f.inSegment = false
	if f.pending.Len() == 0 {
		return true
	}
	body := strings.TrimSpace(f.pending.String())
	switch {
	case body == SlackNoReplyToken:
		f.tokenSegments++
		f.droppedAny = true
		f.pending.Reset()
		return true
	case body == "":
		// Whitespace only: keep it as a separator for the next segment. It is
		// discarded if nothing else is ever emitted.
		return true
	case !complete:
		// A failed/cancelled turn's partial control token is never content.
		f.droppedAny = true
		f.pending.Reset()
		return true
	}
	out := f.pendingEvt
	out.Delta = f.pending.String()
	f.pending.Reset()
	return f.emitText(out)
}

// finish resolves the final segment. clean reports a successful terminal.
func (f *noReplySegments) finish(clean bool) bool {
	ok := f.endSegment(clean)
	if f.pending.Len() > 0 {
		// Trailing whitespace only.
		if f.kept.Len() > 0 {
			out := f.pendingEvt
			out.Delta = f.pending.String()
			ok = f.emitText(out) && ok
		} else {
			f.droppedAny = true
		}
		f.pending.Reset()
	}
	return ok
}

// silent reports that the model's only output was control tokens.
func (f *noReplySegments) silent() bool {
	return f.tokenSegments > 0 && strings.TrimSpace(f.kept.String()) == ""
}

// rewrite maps an authoritative terminal body onto the filtered stream.
// Backends normally assemble it from the same deltas; when it wraps them
// (e.g. Claude's merged earlier assistant text) the streamed portion is
// replaced in place. An unrelated body is returned unchanged.
func (f *noReplySegments) rewrite(content string) string {
	if !f.droppedAny {
		return content
	}
	raw := f.raw.String()
	switch {
	case content == raw:
		return f.kept.String()
	case raw != "" && strings.Contains(content, raw):
		// Wrappers prepend earlier text (finalStreamText appends the
		// streamed fullText last), and that earlier text may itself mention
		// the token. Replace the last occurrence, which is the streamed one.
		i := strings.LastIndex(content, raw)
		return content[:i] + f.kept.String() + content[i+len(raw):]
	case strings.TrimSpace(content) == strings.TrimSpace(raw):
		return f.kept.String()
	}
	return content
}

// filterSlackNoReplyEvents applies noReplySegments to a Slack one-shot event
// stream before it leaves the holder, so live deltas, the terminal Message
// (persisted by the response adapter), and peer relays see the same body. A
// clean turn consisting only of control tokens terminates with the canonical
// SlackNoReplyToken body so the Slack transport suppresses the post.
func filterSlackNoReplyEvents(in <-chan ChatEvent) <-chan ChatEvent {
	out := make(chan ChatEvent, 64)
	go func() {
		defer close(out)
		forward := func(e ChatEvent) bool {
			out <- e
			return true
		}
		f := newNoReplySegments(forward)
		failed := false
		terminal := false
		for ev := range in {
			if terminal {
				out <- ev
				continue
			}
			switch ev.Type {
			case "error":
				failed = true
				f.send(ev)
			case "done":
				terminal = true
				clean := !failed && ev.ErrorMessage == ""
				f.finish(clean)
				if ev.Message == nil && clean && f.silent() {
					// Bare done: the withheld deltas were the only body, so
					// carry the suppression signal explicitly.
					ev.Message = &Message{Role: "assistant", Content: SlackNoReplyToken}
				} else if ev.Message != nil {
					msg := *ev.Message
					// Decide silence from the rewritten terminal body, not
					// the stream alone: the authoritative body may carry
					// text that never streamed (e.g. Claude prepending an
					// earlier assistant turn), which must not be discarded.
					body := f.rewrite(msg.Content)
					if clean && f.silent() && (strings.TrimSpace(body) == "" || IsNoReplyOnly(body)) {
						msg.Content = SlackNoReplyToken
					} else {
						msg.Content = body
						if !clean && couldBeNoReplyOnly(msg.Content) {
							msg.Content = ""
						}
					}
					ev.Message = &msg
				}
				out <- ev
			default:
				f.send(ev)
			}
		}
		if !terminal {
			f.finish(false)
		}
	}()
	return out
}

// IsNoReplyOnly reports whether text consists solely of one or more
// SlackNoReplyToken occurrences and whitespace. It is the transport-side
// equivalent of "every segment was a control token" for streams that lost
// segment boundaries (for example an older holder).
func IsNoReplyOnly(text string) bool {
	rest := strings.TrimSpace(text)
	if rest == "" {
		return false
	}
	for rest != "" {
		if !strings.HasPrefix(rest, SlackNoReplyToken) {
			return false
		}
		rest = strings.TrimSpace(rest[len(SlackNoReplyToken):])
	}
	return true
}

// CouldBeNoReplyOnly reports whether streaming text may still become an
// IsNoReplyOnly response (including empty/whitespace text).
func CouldBeNoReplyOnly(text string) bool { return couldBeNoReplyOnly(text) }

func couldBeNoReplyOnly(text string) bool {
	rest := strings.TrimSpace(text)
	for strings.HasPrefix(rest, SlackNoReplyToken) {
		rest = strings.TrimSpace(rest[len(SlackNoReplyToken):])
	}
	return strings.HasPrefix(SlackNoReplyToken, rest)
}

// goalTurnText applies the same segment filter to one native Codex goal turn.
// The same decision is applied to the terminal transcript before aggregation,
// so live delivery, persistence, and peer forwarding cannot disagree.
type goalTurnText struct {
	filter    bool
	separator bool
	started   bool
	segments  *noReplySegments
	emit      func(ChatEvent) bool
}

func newGoalTurnText(filter, separator bool, emit func(ChatEvent) bool) *goalTurnText {
	t := &goalTurnText{filter: filter, separator: separator, emit: emit}
	t.segments = newNoReplySegments(t.sendText)
	return t
}

func (t *goalTurnText) send(evt ChatEvent) bool {
	if !t.filter {
		if evt.Type == "text" && evt.Delta != "" {
			return t.sendText(evt)
		}
		return t.emit(evt)
	}
	return t.segments.send(evt)
}

func (t *goalTurnText) sendText(evt ChatEvent) bool {
	if evt.Type != "text" || evt.Delta == "" || evt.ParentToolUseID != "" {
		return t.emit(evt)
	}
	if !t.started && t.separator {
		evt.Delta = "\n\n" + evt.Delta
	}
	t.started = true
	return t.emit(evt)
}

func (t *goalTurnText) finish(res *codexStreamResult) {
	if t.filter {
		clean := res.turnCompleted && res.turnStatus == "completed" && res.processError == "" && !res.cancelled
		if !t.segments.finish(clean) {
			res.cancelled = true
		}
		if t.segments.droppedAny {
			// Failed/cancelled partial tokens and control segments are never
			// user content. Keep failure flags, usage, questions, and tools.
			body := t.segments.rewrite(res.fullText.String())
			if t.segments.silent() || (!clean && couldBeNoReplyOnly(body)) {
				body = ""
			}
			res.fullText.Reset()
			res.fullText.WriteString(body)
		}
	}
	if res.fullText.Len() > 0 && t.separator {
		body := res.fullText.String()
		res.fullText.Reset()
		res.fullText.WriteString("\n\n")
		res.fullText.WriteString(body)
	}
}
