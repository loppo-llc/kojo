package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------
// sessionCursorStart
// ---------------------------------------------------------------------

func TestSessionCursorStart(t *testing.T) {
	stripped := []*Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
		{Role: "user", Content: "three"},
		{Role: "assistant", Content: "four"},
	}

	t.Run("zero marker returns 0", func(t *testing.T) {
		got := sessionCursorStart(stripped, autoSummaryMarker{})
		if got != 0 {
			t.Errorf("expected 0, got %d", got)
		}
	})

	t.Run("valid cursor returns marker cursor", func(t *testing.T) {
		n := 2
		marker := autoSummaryMarker{
			Source:     markerSourceSession,
			Cursor:     n,
			CursorHash: messagesFingerprint(stripped[:n]),
		}
		got := sessionCursorStart(stripped, marker)
		if got != n {
			t.Errorf("expected %d, got %d", n, got)
		}
	})

	t.Run("cursor past end of stripped returns 0", func(t *testing.T) {
		marker := autoSummaryMarker{
			Source:     markerSourceSession,
			Cursor:     len(stripped) + 1,
			CursorHash: "irrelevant",
		}
		got := sessionCursorStart(stripped, marker)
		if got != 0 {
			t.Errorf("expected 0, got %d", got)
		}
	})

	t.Run("cursor hash mismatch returns 0", func(t *testing.T) {
		n := 2
		marker := autoSummaryMarker{
			Source:     markerSourceSession,
			Cursor:     n,
			CursorHash: "not-the-real-hash",
		}
		got := sessionCursorStart(stripped, marker)
		if got != 0 {
			t.Errorf("expected 0, got %d", got)
		}
	})

	t.Run("transcript source never honoured", func(t *testing.T) {
		n := 2
		marker := autoSummaryMarker{
			Source:     markerSourceTranscript,
			Cursor:     n,
			CursorHash: messagesFingerprint(stripped[:n]),
		}
		got := sessionCursorStart(stripped, marker)
		if got != 0 {
			t.Errorf("expected 0, got %d", got)
		}
	})
}

// ---------------------------------------------------------------------
// splitMessagesForSummary
// ---------------------------------------------------------------------

func TestSplitMessagesForSummary(t *testing.T) {
	t.Run("small messages fit in one chunk", func(t *testing.T) {
		var msgs []*Message
		for i := 0; i < 5; i++ {
			msgs = append(msgs, &Message{Role: "user", Content: fmt.Sprintf("short message %d", i)})
		}
		chunks := splitMessagesForSummary(msgs, preCompactMaxPromptBytes)
		if len(chunks) != 1 {
			t.Fatalf("expected 1 chunk, got %d", len(chunks))
		}
		if len(chunks[0]) != len(msgs) {
			t.Errorf("expected all %d messages in the single chunk, got %d", len(msgs), len(chunks[0]))
		}
	})

	t.Run("many large messages split into multiple chunks, order preserved", func(t *testing.T) {
		const n = 40
		var msgs []*Message
		big := strings.Repeat("x", 2500)
		for i := 0; i < n; i++ {
			msgs = append(msgs, &Message{Role: "user", Content: fmt.Sprintf("CHUNKMSG-%d-%s", i, big)})
		}
		budget := 64 * 1024
		chunks := splitMessagesForSummary(msgs, budget)
		if len(chunks) < 2 {
			t.Fatalf("expected multiple chunks, got %d", len(chunks))
		}

		// Concatenation of chunks must equal the input, in order, with
		// nothing lost or duplicated.
		var flat []*Message
		for _, c := range chunks {
			flat = append(flat, c...)
		}
		if len(flat) != len(msgs) {
			t.Fatalf("expected %d total messages across chunks, got %d", len(msgs), len(flat))
		}
		for i := range msgs {
			if flat[i] != msgs[i] {
				t.Errorf("message %d out of order or lost: want %v got %v", i, msgs[i], flat[i])
			}
		}

		// Every chunk must actually respect the budget when rendered
		// through the real production prompt builder (not a duplicated
		// size formula) — a splitter that grouped messages arbitrarily
		// (e.g. fixed-size batches unrelated to byte size) would still
		// pass the order/completeness checks above, so verify the
		// black-box output size directly.
		for ci, c := range chunks {
			if len(c) <= 1 {
				continue // a lone oversized message may legitimately exceed budget
			}
			if got := len(buildSummaryPrompt(c, "")); got > budget {
				t.Errorf("chunk %d rendered prompt exceeds budget: %d > %d", ci, got, budget)
			}
		}

		// Tightness: for every non-terminal, multi-message chunk, the
		// split must be as-late-as-possible — folding in the next
		// chunk's first message would have exceeded the usable portion
		// of the budget (budget minus the headroom splitMessagesForSummary
		// reserves for the prompt preamble / carried summary). This rules
		// out an implementation that splits into needlessly small
		// fixed-size batches while still passing the budget check above.
		// usable mirrors splitMessagesForSummary's own headroom
		// reservation (documented on the function) — some duplication is
		// unavoidable to test the split boundary itself, but the actual
		// per-chunk byte count below still comes from the real
		// buildSummaryPrompt output, not a reimplemented size formula.
		usable := budget - summaryChunkHeadroomBytes
		if usable < 4096 {
			usable = 4096
		}
		for ci := 0; ci < len(chunks)-1; ci++ {
			if len(chunks[ci]) <= 1 {
				continue
			}
			withNext := append(append([]*Message{}, chunks[ci]...), chunks[ci+1][0])
			if got := len(buildSummaryPrompt(withNext, "")); got <= usable {
				t.Errorf("chunk %d split too early: appending the next chunk's first message still fits the usable budget (%d <= %d)", ci, got, usable)
			}
		}
	})

	t.Run("single oversized message still yields a chunk containing it", func(t *testing.T) {
		huge := &Message{Role: "user", Content: strings.Repeat("あ", 2500)}
		chunks := splitMessagesForSummary([]*Message{huge}, 1024)
		if len(chunks) != 1 {
			t.Fatalf("expected 1 chunk, got %d", len(chunks))
		}
		if len(chunks[0]) != 1 || chunks[0][0] != huge {
			t.Fatalf("expected chunk to contain the single oversized message, got %v", chunks[0])
		}
	})
}

// ---------------------------------------------------------------------
// PreCompactSummarize end-to-end incremental behaviour
// ---------------------------------------------------------------------

// fakeSummaryLLM records every generateSummary invocation and returns a
// deterministic, distinguishable "SUMMARY vN" string so tests can assert
// exactly which chunk/call produced which carried-forward summary.
type fakeSummaryLLM struct {
	Calls   int
	Prompts []string
}

func (f *fakeSummaryLLM) generate(tool, prompt string) (string, error) {
	f.Calls++
	f.Prompts = append(f.Prompts, prompt)
	return fmt.Sprintf("SUMMARY v%d", f.Calls), nil
}

// stubGenerateSummary installs a deterministic fake in place of the real
// LLM call, registers its own restoration via t.Cleanup, and returns the
// fake so the test can inspect Calls / Prompts. Not safe for parallel
// tests (generateSummary is a package var) — callers must not use
// t.Parallel().
func stubGenerateSummary(t *testing.T) *fakeSummaryLLM {
	t.Helper()
	old := generateSummary
	fake := &fakeSummaryLLM{}
	generateSummary = fake.generate
	t.Cleanup(func() { generateSummary = old })
	return fake
}

// writeSessionLine appends a single claude-format JSONL line to path,
// creating the file if needed.
func writeSessionLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open session file: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("write session line: %v", err)
	}
}

func userLine(text string) string {
	return fmt.Sprintf(`{"type":"user","message":{"content":%q}}`, text)
}

func assistantLine(text string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}`, text)
}

// setupIncrementalAgent creates the HOME/CLAUDE_CONFIG_DIR sandbox, the
// claude project dir, and the agent's memory dir for agentID, and returns
// the path to the session file (not yet created).
func setupIncrementalAgent(t *testing.T, agentID string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	absDir, err := filepath.Abs(agentDir(agentID))
	if err != nil {
		t.Fatalf("abs agent dir: %v", err)
	}
	projectDir := claudeProjectDir(absDir)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(agentDir(agentID), "memory"), 0o755); err != nil {
		t.Fatalf("mkdir memory dir: %v", err)
	}
	return filepath.Join(projectDir, "sess.jsonl")
}

func recentMdContent(t *testing.T, agentID string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(agentDir(agentID), "memory", recentSummaryFile))
	if err != nil {
		t.Fatalf("read recent.md: %v", err)
	}
	return string(data)
}

func TestPreCompactSummarize_IncrementalCursor(t *testing.T) {
	agentID := "ag_incr"
	sessionFile := setupIncrementalAgent(t, agentID)

	fake := stubGenerateSummary(t)

	// Each message gets a unique canary so an assertion that checks for
	// e.g. "NEWMSG" can't pass by accident if only one of the two new
	// messages actually made it into the prompt (or if a stale message
	// leaked through the cursor slice).
	const oldUser = "OLDMSG-U-canary-1"
	const oldAssistant = "OLDMSG-A-canary-2"
	const newUser = "NEWMSG-U-canary-3"
	const newAssistant = "NEWMSG-A-canary-4"

	// --- Call 1: initial content -----------------------------------
	writeSessionLine(t, sessionFile, userLine(oldUser))
	writeSessionLine(t, sessionFile, assistantLine(oldAssistant))

	if err := PreCompactSummarize(agentID, "claude", sessionFile, testLogger()); err != nil {
		t.Fatalf("PreCompactSummarize (call 1): %v", err)
	}
	if fake.Calls != 1 {
		t.Fatalf("expected 1 generateSummary call, got %d", fake.Calls)
	}
	if strings.Count(fake.Prompts[0], oldUser) != 1 || strings.Count(fake.Prompts[0], oldAssistant) != 1 {
		t.Errorf("expected first prompt to contain both initial messages exactly once each, got: %q", fake.Prompts[0])
	}
	recent := recentMdContent(t, agentID)
	if !strings.Contains(recent, "SUMMARY") {
		t.Errorf("expected recent.md to contain the summary, got: %q", recent)
	}

	// --- Call 2: two new messages appended --------------------------
	writeSessionLine(t, sessionFile, userLine(newUser))
	writeSessionLine(t, sessionFile, assistantLine(newAssistant))

	if err := PreCompactSummarize(agentID, "claude", sessionFile, testLogger()); err != nil {
		t.Fatalf("PreCompactSummarize (call 2): %v", err)
	}
	if fake.Calls != 2 {
		t.Fatalf("expected 2 generateSummary calls, got %d", fake.Calls)
	}
	secondPrompt := fake.Prompts[1]
	if strings.Count(secondPrompt, newUser) != 1 || strings.Count(secondPrompt, newAssistant) != 1 {
		t.Errorf("expected second prompt to contain both new messages exactly once each, got: %q", secondPrompt)
	}
	convoSection := secondPrompt
	if idx := strings.Index(secondPrompt, "## 会話"); idx >= 0 {
		convoSection = secondPrompt[idx:]
	}
	if strings.Contains(convoSection, oldUser) || strings.Contains(convoSection, oldAssistant) {
		t.Errorf("expected old message text to be absent from the ## 会話 section, got: %q", convoSection)
	}
	if !strings.Contains(secondPrompt, "## これまでの要約") {
		t.Errorf("expected second prompt to carry forward the previous summary, got: %q", secondPrompt)
	}
	if !strings.Contains(secondPrompt, "SUMMARY v1") {
		t.Errorf("expected second prompt to carry SUMMARY v1, got: %q", secondPrompt)
	}

	// --- Call 3: no new messages -------------------------------------
	if err := PreCompactSummarize(agentID, "claude", sessionFile, testLogger()); err != nil {
		t.Fatalf("PreCompactSummarize (call 3): %v", err)
	}
	if fake.Calls != 2 {
		t.Errorf("expected no additional generateSummary call with no new messages, got %d calls", fake.Calls)
	}
}

// TestPreCompactSummarize_MultiChunkCarriesForwardWithinOneCall forces a
// single PreCompactSummarize invocation to split its backlog into more
// than one chunk (via splitMessagesForSummary) and verifies the summary
// produced by the first chunk is carried into the second chunk's prompt
// — the same carry-forward mechanism that TestPreCompactSummarize_
// IncrementalCursor only exercises across separate top-level calls.
func TestPreCompactSummarize_MultiChunkCarriesForwardWithinOneCall(t *testing.T) {
	agentID := "ag_incr_multichunk"
	sessionFile := setupIncrementalAgent(t, agentID)

	fake := stubGenerateSummary(t)

	// Enough large messages in a single call to exceed one chunk's
	// budget (preCompactMaxPromptBytes, 64KiB, minus headroom).
	big := strings.Repeat("y", 2500)
	for i := 0; i < 30; i++ {
		writeSessionLine(t, sessionFile, userLine(fmt.Sprintf("BULK-%d-%s", i, big)))
	}

	if err := PreCompactSummarize(agentID, "claude", sessionFile, testLogger()); err != nil {
		t.Fatalf("PreCompactSummarize: %v", err)
	}
	if fake.Calls < 2 {
		t.Fatalf("expected the backlog to force multiple chunk-level generateSummary calls, got %d", fake.Calls)
	}
	// The second chunk's prompt must carry the first chunk's output
	// forward, and must not itself repeat the full first-chunk prompt
	// content (i.e. it's an incremental carry, not a re-summarization
	// of everything).
	secondPrompt := fake.Prompts[1]
	if !strings.Contains(secondPrompt, "## これまでの要約") {
		t.Errorf("expected second chunk's prompt to carry the first chunk's summary, got: %q", secondPrompt)
	}
	if !strings.Contains(secondPrompt, "SUMMARY v1") {
		t.Errorf("expected second chunk's prompt to carry SUMMARY v1 specifically, got: %q", secondPrompt)
	}

	// Completeness + no-duplication across ALL chunk prompts: every
	// BULK-i message must appear in exactly one prompt. This rules out
	// an implementation that just re-feeds the same chunk into every
	// generateSummary call (which would trivially satisfy the two
	// carry-forward checks above without actually partitioning the
	// backlog).
	for i := 0; i < 30; i++ {
		tag := fmt.Sprintf("BULK-%d-", i)
		seenIn := -1
		for pi, p := range fake.Prompts {
			if strings.Contains(p, tag) {
				if seenIn != -1 {
					t.Errorf("message %d appears in both prompt %d and prompt %d — expected exactly one", i, seenIn, pi)
				}
				seenIn = pi
			}
		}
		if seenIn == -1 {
			t.Errorf("message %d missing from all chunk prompts", i)
		}
	}
}

func TestPreCompactSummarize_CursorMismatchResummarizesAll(t *testing.T) {
	agentID := "ag_incr_mismatch"
	sessionFile := setupIncrementalAgent(t, agentID)

	fake := stubGenerateSummary(t)

	// --- Call 1: initial content -----------------------------------
	writeSessionLine(t, sessionFile, userLine("FIRSTGEN-U-canary-1"))
	writeSessionLine(t, sessionFile, assistantLine("FIRSTGEN-A-canary-2"))

	if err := PreCompactSummarize(agentID, "claude", sessionFile, testLogger()); err != nil {
		t.Fatalf("PreCompactSummarize (call 1): %v", err)
	}
	if fake.Calls != 1 {
		t.Fatalf("expected 1 generateSummary call, got %d", fake.Calls)
	}

	// --- Rewrite the session file with the SAME message count but
	// entirely different content. This specifically exercises the
	// content-hash mismatch branch of sessionCursorStart (Cursor <=
	// len(stripped) but chain fingerprint mismatch),
	// as opposed to the simpler "cursor past end of file" branch — a
	// session reset that reuses the same deterministic file path could
	// plausibly rewrite with an equal or greater message count too.
	rewritten := userLine("SECONDGEN-U-canary-3") + "\n" + assistantLine("SECONDGEN-A-canary-4") + "\n"
	if err := os.WriteFile(sessionFile, []byte(rewritten), 0o644); err != nil {
		t.Fatalf("rewrite session file: %v", err)
	}

	if err := PreCompactSummarize(agentID, "claude", sessionFile, testLogger()); err != nil {
		t.Fatalf("PreCompactSummarize (call 2): %v", err)
	}
	if fake.Calls != 2 {
		t.Fatalf("expected a second generateSummary call after cursor mismatch, got %d calls", fake.Calls)
	}
	secondPrompt := fake.Prompts[1]
	if !strings.Contains(secondPrompt, "SECONDGEN-U-canary-3") || !strings.Contains(secondPrompt, "SECONDGEN-A-canary-4") {
		t.Errorf("expected prompt to contain both of the new file's messages, got: %q", secondPrompt)
	}
	if strings.Contains(secondPrompt, "FIRSTGEN-U-canary-1") || strings.Contains(secondPrompt, "FIRSTGEN-A-canary-2") {
		t.Errorf("expected old (pre-rewrite) message text to be absent, got: %q", secondPrompt)
	}
	if strings.Contains(secondPrompt, "## これまでの要約") {
		t.Errorf("expected no carried-forward summary section after cursor mismatch, got: %q", secondPrompt)
	}
}
