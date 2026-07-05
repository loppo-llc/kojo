package agent

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/loppo-llc/kojo/internal/store"
)

// kojoCtx wraps body in the exact `<context>...</context>` envelope
// BuildVolatileContext emits, carrying volatileContextSentinel so the
// sentinel-gated strip recognises it.
func kojoCtx(inner string) string {
	return "<context>\n" + volatileContextSentinel + "\n" + inner + "\n</context>"
}

// TestStripArrivalContextBlock covers the sentinel-gated strip. Without the
// sentinel guard, a user who legitimately opens their message with
// `<context>` would have their actual content silently deleted — which is
// the original symptom stripVolatileContext was designed to avoid. We
// inherit the same posture here.
func TestStripArrivalContextBlock(t *testing.T) {
	instr := "もう一度 LOPPO-STUDIO-1 に転移して、デバイスのセキュリティーチェックを行って"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "kojo-injected block (with sentinel) stripped",
			in:   kojoCtx("diary stuff") + "\n\n" + instr,
			want: instr,
		},
		{
			name: "user-authored leading context (no sentinel) preserved",
			in:   "<context>my own note</context>\n\nactual body",
			want: "<context>my own note</context>\n\nactual body",
		},
		{
			name: "no context block: passes through",
			in:   "just an instruction",
			want: "just an instruction",
		},
		{
			name: "inline context tag (not leading) survives",
			in:   "do X.\n\nsee <context>foo</context> for ref",
			want: "do X.\n\nsee <context>foo</context> for ref",
		},
		{
			name: "kojo block then user block — only kojo stripped",
			in:   kojoCtx("diary") + "\n<context>user</context>\n\nbody",
			want: "<context>user</context>\n\nbody",
		},
		{
			name: "unclosed context tag passes through",
			in:   "<context>" + volatileContextSentinel + " unclosed",
			want: "<context>" + volatileContextSentinel + " unclosed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripArrivalContextBlock(tc.in)
			if got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestLatestUnaddressedUserInstruction_NilManager / NilStore exercises the
// defensive bail-outs so a buggy caller can't panic the arrival goroutine.
func TestLatestUnaddressedUserInstruction_NilManager(t *testing.T) {
	if got := collectArrivalContext(context.Background(), nil, "ag").UserInstruction; got != "" {
		t.Errorf("nil manager: got %q; want empty", got)
	}
	if got := collectArrivalContext(context.Background(), &Manager{}, "ag").UserInstruction; got != "" {
		t.Errorf("nil store: got %q; want empty", got)
	}
}

// TestLatestUnaddressedUserInstruction_PicksNewestUnaddressed is the
// regression case for ag_f71bf5… on 2026-05-28: transcript ends with a
// user instruction followed by an EMPTY assistant snapshot (the in-flight
// row the switch ships). The empty assistant does NOT count as an
// addresser; the user msg must surface.
func TestLatestUnaddressedUserInstruction_PicksNewestUnaddressed(t *testing.T) {
	mgr := newTestManager(t)
	ctx := context.Background()
	st := mgr.Store()
	if st == nil {
		t.Fatal("nil store")
	}

	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_x", Name: "x"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert agent: %v", err)
	}

	now := time.Now().UnixMilli()
	mustAppend := func(id, role, content string, deltaMs int64) {
		t.Helper()
		_, err := st.AppendMessage(ctx, &store.MessageRecord{
			ID:      id,
			AgentID: "ag_x",
			Role:    role,
			Content: content,
		}, store.MessageInsertOptions{
			Now: now + deltaMs,
		})
		if err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	instr := "もう一度 STUDIO-1 に転移して、セキュリティチェック実施"
	mustAppend("m1", "user", "first instruction", 0)
	mustAppend("m2", "assistant", "OK done", 1)
	mustAppend("m3", "system", "[system] checkin", 2)
	mustAppend("m4", "user", kojoCtx("diary-notes\nfoo")+"\n\n"+instr, 3)
	mustAppend("m5", "assistant", "", 4) // empty in-flight snapshot

	got := collectArrivalContext(ctx, mgr, "ag_x").UserInstruction
	if got != instr {
		t.Errorf("got %q\nwant %q", got, instr)
	}
}

// TestLatestUnaddressedUserInstruction_AddressedReturnsEmpty: if the
// latest user msg already has a non-empty assistant reply, the arrival
// prompt should NOT quote it — re-quoting would shake completed work
// loose on every cron-triggered switch.
func TestLatestUnaddressedUserInstruction_AddressedReturnsEmpty(t *testing.T) {
	mgr := newTestManager(t)
	ctx := context.Background()
	st := mgr.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_a", Name: "a"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	now := time.Now().UnixMilli()
	mustAppend := func(id, role, content string, deltaMs int64) {
		t.Helper()
		if _, err := st.AppendMessage(ctx, &store.MessageRecord{
			ID:      id,
			AgentID: "ag_a",
			Role:    role,
			Content: content,
		}, store.MessageInsertOptions{Now: now + deltaMs}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	mustAppend("u1", "user", "do the thing", 0)
	mustAppend("a1", "assistant", "Done — here is the result.", 1)

	if got := collectArrivalContext(ctx, mgr, "ag_a").UserInstruction; got != "" {
		t.Errorf("addressed instruction should NOT quote; got %q", got)
	}
}

// TestLatestUnaddressedUserInstruction_SkipsEmptyAfterStrip falls through
// to the next older user row when the latest one's body is entirely
// kojo-injected context.
func TestLatestUnaddressedUserInstruction_SkipsEmptyAfterStrip(t *testing.T) {
	mgr := newTestManager(t)
	ctx := context.Background()
	st := mgr.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_y", Name: "y"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	now := time.Now().UnixMilli()
	mustAppend := func(id, role, content string, deltaMs int64) {
		t.Helper()
		if _, err := st.AppendMessage(ctx, &store.MessageRecord{
			ID:      id,
			AgentID: "ag_y",
			Role:    role,
			Content: content,
		}, store.MessageInsertOptions{Now: now + deltaMs}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	mustAppend("u1", "user", "real instruction A", 0)
	mustAppend("u2", "user", kojoCtx("only context"), 1) // empty after strip

	got := collectArrivalContext(ctx, mgr, "ag_y").UserInstruction
	want := "real instruction A"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// TestTruncatePromptByRune_MultibyteBoundary guards the UTF-8 boundary:
// truncation must never produce invalid UTF-8 or split a codepoint
// mid-byte. Byte-slicing a Japanese paragraph at 4000 bytes is the
// previous-implementation foot-gun this test pins down.
func TestTruncatePromptByRune_MultibyteBoundary(t *testing.T) {
	body := strings.Repeat("あ", arrivalPromptUserInstructionPreviewLimit+200) // 3 bytes/rune in UTF-8
	got := truncatePromptByRune(body, arrivalPromptUserInstructionPreviewLimit)

	if !utf8.ValidString(got) {
		t.Fatalf("truncation produced invalid UTF-8")
	}
	if !strings.HasSuffix(got, "…（以下省略、完全な内容はトランスクリプト参照）") {
		t.Errorf("expected ellipsis suffix; got tail: %q", tailN(got, 60))
	}
	// Strip the suffix and count runes.
	suffix := "…（以下省略、完全な内容はトランスクリプト参照）"
	head := strings.TrimSuffix(got, suffix)
	if rc := utf8.RuneCountInString(head); rc != arrivalPromptUserInstructionPreviewLimit {
		t.Errorf("head rune count = %d; want %d", rc, arrivalPromptUserInstructionPreviewLimit)
	}
}

// TestTruncatePromptByRune_ShortPassthrough leaves bodies under the cap
// alone (no spurious ellipsis added).
func TestTruncatePromptByRune_ShortPassthrough(t *testing.T) {
	in := "短いメッセージ"
	if got := truncatePromptByRune(in, arrivalPromptUserInstructionPreviewLimit); got != in {
		t.Errorf("short body should pass through; got %q", got)
	}
}

// TestLatestUnaddressedUserInstruction_TruncatesLongBody confirms the
// helper itself wires the truncate path — the body comes back trimmed
// when it exceeds the preview limit.
func TestLatestUnaddressedUserInstruction_TruncatesLongBody(t *testing.T) {
	mgr := newTestManager(t)
	ctx := context.Background()
	st := mgr.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_z", Name: "z"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	big := strings.Repeat("x", arrivalPromptUserInstructionPreviewLimit+1000)
	if _, err := st.AppendMessage(ctx, &store.MessageRecord{
		ID: "m1", AgentID: "ag_z", Role: "user", Content: big,
	}, store.MessageInsertOptions{}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got := collectArrivalContext(ctx, mgr, "ag_z").UserInstruction
	if !strings.HasSuffix(got, "…（以下省略、完全な内容はトランスクリプト参照）") {
		t.Errorf("expected truncation suffix; got tail: %q", tailN(got, 80))
	}
}

// TestBuildArrivalPrompt_NoUserInstructionUsesFallback exercises the
// fallback tail when ListMessages returns nothing user-flavoured.
func TestBuildArrivalPrompt_NoUserInstructionUsesFallback(t *testing.T) {
	mgr := newTestManager(t)
	ctx := context.Background()
	st := mgr.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_f", Name: "f"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.AppendMessage(ctx, &store.MessageRecord{
		ID: "a1", AgentID: "ag_f", Role: "assistant", Content: "hi",
	}, store.MessageInsertOptions{}); err != nil {
		t.Fatalf("append a1: %v", err)
	}
	if _, err := st.AppendMessage(ctx, &store.MessageRecord{
		ID: "s1", AgentID: "ag_f", Role: "system", Content: "checkin",
	}, store.MessageInsertOptions{}); err != nil {
		t.Fatalf("append s1: %v", err)
	}
	got := buildArrivalPrompt(ctx, mgr, "ag_f", "SRC-PEER", ArrivalNotes{})
	if !strings.Contains(got, "SRC-PEER") {
		t.Errorf("missing source peer in prompt: %q", got)
	}
	if !strings.Contains(got, arrivalPromptFallbackTail) {
		t.Errorf("expected fallback tail; got %q", got)
	}
}

// TestBuildArrivalPrompt_QuotesUnaddressedUserInstruction: regression for
// ag_f71bf5… on 2026-05-28. When the last user msg has no non-empty
// assistant reply, arrival prompt MUST quote it so the LLM can't miss it
// under the auto-context block.
func TestBuildArrivalPrompt_QuotesUnaddressedUserInstruction(t *testing.T) {
	mgr := newTestManager(t)
	ctx := context.Background()
	st := mgr.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_q", Name: "q"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	instr := "もう一度 STUDIO-1 に転移して、セキュリティーチェックを行って"
	if _, err := st.AppendMessage(ctx, &store.MessageRecord{
		ID: "u1", AgentID: "ag_q", Role: "user",
		Content: kojoCtx("diary stuff") + "\n\n" + instr,
	}, store.MessageInsertOptions{}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got := buildArrivalPrompt(ctx, mgr, "ag_q", "STUDIO-2", ArrivalNotes{})
	if !strings.Contains(got, instr) {
		t.Errorf("prompt missing user instruction; got %q", got)
	}
	if !strings.Contains(got, "STUDIO-2") {
		t.Errorf("prompt missing source peer; got %q", got)
	}
	if strings.Contains(got, arrivalPromptFallbackTail) {
		t.Errorf("fallback tail leaked into instruction-quoted prompt: %q", got)
	}
	// The kojo-injected `<context>` block must NOT appear inside the
	// quoted instruction; stripping was the whole point.
	if strings.Contains(got, volatileContextSentinel) {
		t.Errorf("kojo context sentinel leaked into arrival prompt: %q", got)
	}
}

// TestBuildArrivalPrompt_AddressedFallsBackToGeneric: if the user msg is
// already answered by a non-empty assistant, the prompt must NOT re-quote
// it. Cron-fired switches benefit from this — the agent just got a "check
// in" prompt and an arrival landing 30s later should not re-issue the
// last hour-old instruction.
func TestBuildArrivalPrompt_AddressedFallsBackToGeneric(t *testing.T) {
	mgr := newTestManager(t)
	ctx := context.Background()
	st := mgr.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_r", Name: "r"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := st.AppendMessage(ctx, &store.MessageRecord{
		ID: "u1", AgentID: "ag_r", Role: "user", Content: "do thing X",
	}, store.MessageInsertOptions{Now: now}); err != nil {
		t.Fatalf("append u1: %v", err)
	}
	if _, err := st.AppendMessage(ctx, &store.MessageRecord{
		ID: "a1", AgentID: "ag_r", Role: "assistant", Content: "X is done.",
	}, store.MessageInsertOptions{Now: now + 1}); err != nil {
		t.Fatalf("append a1: %v", err)
	}
	got := buildArrivalPrompt(ctx, mgr, "ag_r", "PEER", ArrivalNotes{})
	if strings.Contains(got, "do thing X") {
		t.Errorf("addressed instruction should not surface; got %q", got)
	}
	if !strings.Contains(got, arrivalPromptFallbackTail) {
		t.Errorf("expected fallback tail; got %q", got)
	}
}

// TestUserMessageAddressed_SkipsKojoSwitchSnapshot is the precise regression
// gate for the second codex finding: a SnapshotAccumulatedMessageRecord row
// landed during a kojo-switch-device call MAY have a short pre-amble in
// Content (the LLM streamed "I'll transfer now" before tool_use), but it is
// NOT a substantive reply to the user's ask. Detect via the
// kojo-switch-device skill name in the ToolUses JSON.
func TestUserMessageAddressed_SkipsKojoSwitchSnapshot(t *testing.T) {
	toolUses := []byte(`[{"id":"toolu_X","name":"Skill","input":{"name":"kojo-switch-device"}}]`)
	rows := []*store.MessageRecord{
		// newer (higher seq) side first — desc order
		{Role: "assistant", Content: "I'll transfer now…", ToolUses: toolUses},
	}
	if userMessageAddressed(rows) {
		t.Error("kojo-switch-device snapshot must not count as an addresser")
	}
}

// TestUserMessageAddressed_PlanATailDoesNotAddressInSwitchTurn:
// regression gate for the Plan A side-effect — after the deferred
// finalize lands the tail row at seq=snapshot+1, the newer-set has
// BOTH the snapshot AND the tail. A naive "skip only kojo-switch
// rows" walk would let the tail (non-empty content, no kojo-switch
// ToolUses) count as an addresser and suppress
// buildArrivalPrompt's user-instruction quoting. The fix detects
// the snapshot anywhere in `newer` and treats the entire run as
// in-flight → user instruction stays un-addressed.
func TestUserMessageAddressed_PlanATailDoesNotAddressInSwitchTurn(t *testing.T) {
	switchToolUses := []byte(`[{"id":"toolu_X","name":"Skill","input":{"name":"kojo-switch-device"}}]`)
	rows := []*store.MessageRecord{
		// Plan A tail (newest, no kojo-switch ToolUses)
		{Role: "assistant", Content: "到着したらセキュリティチェックを実施する。"},
		// In-flight snapshot (older, with kojo-switch ToolUses)
		{Role: "assistant", Content: "", ToolUses: switchToolUses},
	}
	if userMessageAddressed(rows) {
		t.Error("tail row following a kojo-switch snapshot must NOT count as an addresser")
	}
}

// TestUserMessageAddressed_AcceptsRegularAssistant: a non-empty assistant
// with no kojo-switch ToolUses is a real addresser.
func TestUserMessageAddressed_AcceptsRegularAssistant(t *testing.T) {
	rows := []*store.MessageRecord{
		{Role: "assistant", Content: "Done. Result is X."},
	}
	if !userMessageAddressed(rows) {
		t.Error("non-empty regular assistant must count as an addresser")
	}
}

// TestUserMessageAddressed_SkipsEmpty: fully empty assistant rows don't
// count (the in-flight snapshot in the ag_f71bf5 case had empty fields).
func TestUserMessageAddressed_SkipsEmpty(t *testing.T) {
	rows := []*store.MessageRecord{
		{Role: "assistant", Content: "", Thinking: ""},
	}
	if userMessageAddressed(rows) {
		t.Error("empty assistant must not count as an addresser")
	}
}

// TestBuildArrivalPrompt_QuotesEvenWhenSwitchSnapshotPresent: the full
// chain — when the only assistant rows post-user are the kojo-switch
// snapshot (with a short pre-amble), the arrival prompt MUST still quote
// the user's instruction. This is the exact ag_f71bf5… failure mode.
func TestBuildArrivalPrompt_QuotesEvenWhenSwitchSnapshotPresent(t *testing.T) {
	mgr := newTestManager(t)
	ctx := context.Background()
	st := mgr.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_s", Name: "s"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	instr := "もう一度 STUDIO-1 に転移して、セキュリティーチェックを行って"
	now := time.Now().UnixMilli()
	if _, err := st.AppendMessage(ctx, &store.MessageRecord{
		ID: "u1", AgentID: "ag_s", Role: "user",
		Content: kojoCtx("diary") + "\n\n" + instr,
	}, store.MessageInsertOptions{Now: now}); err != nil {
		t.Fatalf("u1: %v", err)
	}
	if _, err := st.AppendMessage(ctx, &store.MessageRecord{
		ID: "a_snap", AgentID: "ag_s", Role: "assistant",
		Content:  "I'll transfer now…",
		ToolUses: []byte(`[{"id":"toolu_X","name":"Skill","input":{"name":"kojo-switch-device"}}]`),
	}, store.MessageInsertOptions{Now: now + 1}); err != nil {
		t.Fatalf("a_snap: %v", err)
	}

	got := buildArrivalPrompt(ctx, mgr, "ag_s", "STUDIO-2", ArrivalNotes{})
	if !strings.Contains(got, instr) {
		t.Errorf("arrival prompt must quote user instruction even when only the kojo-switch snapshot follows; got %q",
			got)
	}
}

// TestUserMessageAddressed_PostArrivalWorkAddresses: regression for
// the codex finding on "snapshot 永続効果" — after a Plan A switch
// completes (snapshot + tail + real post-arrival assistant work),
// a subsequent cron switch must consider the original user
// instruction ADDRESSED so it doesn't shake stale work loose.
func TestUserMessageAddressed_PostArrivalWorkAddresses(t *testing.T) {
	switchToolUses := []byte(`[{"id":"toolu_X","name":"Skill","input":{"name":"kojo-switch-device"}}]`)
	rows := []*store.MessageRecord{
		// Most recent (post-arrival completion)
		{Role: "assistant", Content: "Security check complete. Findings: ..."},
		// Arrival prompt response
		{Role: "system", Content: "デバイス転移完了..."},
		// Plan A tail (commitment)
		{Role: "assistant", Content: "到着したらセキュリティチェックを実施する。"},
		// In-flight snapshot (oldest in newer)
		{Role: "assistant", Content: "", ToolUses: switchToolUses},
	}
	if !userMessageAddressed(rows) {
		t.Error("post-arrival assistant work newer than snapshot+tail must count as addresser")
	}
}

// TestBuildArrivalPrompt_QuotesTailWhenPresent: regression for the
// "tail not in LLM context" codex finding. When a tail row exists
// at snapshot+1, the arrival prompt must quote its content so the
// LLM on target (which can't see the tail via JSONL) gets the
// agent's commitment text.
func TestBuildArrivalPrompt_QuotesTailWhenPresent(t *testing.T) {
	mgr := newTestManager(t)
	ctx := context.Background()
	st := mgr.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_tail", Name: "tail"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	now := time.Now().UnixMilli()
	mustAppend := func(id, role, content string, toolUses []byte, deltaMs int64) {
		t.Helper()
		if _, err := st.AppendMessage(ctx, &store.MessageRecord{
			ID: id, AgentID: "ag_tail", Role: role,
			Content: content, ToolUses: toolUses,
		}, store.MessageInsertOptions{Now: now + deltaMs}); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
	}
	instr := "もう一度 STUDIO-1 に転移して、セキュリティーチェックを行って"
	tailContent := "到着したらセキュリティチェックを実施する。"
	switchToolUses := []byte(`[{"id":"X","name":"Skill","input":{"name":"kojo-switch-device"}}]`)
	mustAppend("u1", "user", kojoCtx("diary")+"\n\n"+instr, nil, 0)
	mustAppend("a_snap", "assistant", "", switchToolUses, 1)
	mustAppend("a_tail", "assistant", tailContent, nil, 2)

	got := buildArrivalPrompt(ctx, mgr, "ag_tail", "STUDIO-2", ArrivalNotes{})
	if !strings.Contains(got, instr) {
		t.Errorf("prompt missing user instruction; got %q", got)
	}
	if !strings.Contains(got, tailContent) {
		t.Errorf("prompt missing tail commitment; got %q", got)
	}
}

// TestUserMessageAddressed_MultiSwitchPeelsAllUnits: two stacked
// snapshot+tail pairs (e.g. self-call switch then cron switch
// before the user msg got any non-switch reply). EVERY unit must
// be peeled; otherwise the second tail leaks through as an
// addresser and suppresses arrival-prompt quoting on the third
// switch.
func TestUserMessageAddressed_MultiSwitchPeelsAllUnits(t *testing.T) {
	switchToolUses := []byte(`[{"id":"X","name":"Skill","input":{"name":"kojo-switch-device"}}]`)
	rows := []*store.MessageRecord{
		// Newest: 2nd switch tail
		{Role: "assistant", Content: "2nd-tail: I will retry."},
		// 2nd switch snapshot
		{Role: "assistant", Content: "", ToolUses: switchToolUses},
		// 1st switch tail
		{Role: "assistant", Content: "1st-tail: I will do X."},
		// Oldest: 1st switch snapshot
		{Role: "assistant", Content: "", ToolUses: switchToolUses},
	}
	if userMessageAddressed(rows) {
		t.Error("multi-switch transcript with only snapshot+tail units must NOT be addressed")
	}
}

// TestPlanATailContent_PicksMostRecentTail: with multiple snapshots
// stacked on the user instruction, the arrival prompt should quote
// the MOST RECENT tail (the one belonging to THIS arrival's
// switch), not an older one.
func TestPlanATailContent_PicksMostRecentTail(t *testing.T) {
	switchToolUses := []byte(`[{"id":"X","name":"Skill","input":{"name":"kojo-switch-device"}}]`)
	rows := []*store.MessageRecord{
		{Role: "assistant", Content: "newest tail"},
		{Role: "assistant", Content: "", ToolUses: switchToolUses},
		{Role: "assistant", Content: "older tail"},
		{Role: "assistant", Content: "", ToolUses: switchToolUses},
	}
	got := planATailContent(rows)
	if got != "newest tail" {
		t.Errorf("got %q; want %q (most recent)", got, "newest tail")
	}
}

// TestPlanATailContent_NoSnapshotReturnsEmpty: when there's no
// snapshot in `newer`, there's no tail to quote.
func TestPlanATailContent_NoSnapshotReturnsEmpty(t *testing.T) {
	rows := []*store.MessageRecord{
		{Role: "assistant", Content: "regular reply"},
		{Role: "user", Content: "regular ask"},
	}
	if got := planATailContent(rows); got != "" {
		t.Errorf("got %q; want empty", got)
	}
}

// TestBuildArrivalPrompt_NoTailWhenSnapshotOnly: only the snapshot
// (no tail) — the arrival prompt quotes the user instruction but
// no tail section since there's nothing to quote.
func TestBuildArrivalPrompt_NoTailWhenSnapshotOnly(t *testing.T) {
	mgr := newTestManager(t)
	ctx := context.Background()
	st := mgr.Store()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag_no_tail", Name: "x"},
		store.AgentInsertOptions{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	now := time.Now().UnixMilli()
	instr := "transfer + check"
	switchToolUses := []byte(`[{"id":"X","name":"Skill","input":{"name":"kojo-switch-device"}}]`)
	if _, err := st.AppendMessage(ctx, &store.MessageRecord{
		ID: "u1", AgentID: "ag_no_tail", Role: "user", Content: instr,
	}, store.MessageInsertOptions{Now: now}); err != nil {
		t.Fatalf("u1: %v", err)
	}
	if _, err := st.AppendMessage(ctx, &store.MessageRecord{
		ID: "a_snap", AgentID: "ag_no_tail", Role: "assistant", Content: "", ToolUses: switchToolUses,
	}, store.MessageInsertOptions{Now: now + 1}); err != nil {
		t.Fatalf("a_snap: %v", err)
	}

	got := buildArrivalPrompt(ctx, mgr, "ag_no_tail", "STUDIO-2", ArrivalNotes{})
	if !strings.Contains(got, instr) {
		t.Errorf("prompt missing user instruction; got %q", got)
	}
	if strings.Contains(got, "あなた自身が宣言した作業") {
		t.Errorf("prompt should not have tail section when no tail exists; got %q", got)
	}
}

// TestTruncatePromptByRune_BoundaryAtLimit: a body exactly at runeLimit
// must NOT be truncated (no ellipsis tail).
func TestTruncatePromptByRune_BoundaryAtLimit(t *testing.T) {
	body := strings.Repeat("あ", arrivalPromptUserInstructionPreviewLimit)
	got := truncatePromptByRune(body, arrivalPromptUserInstructionPreviewLimit)
	if got != body {
		t.Errorf("body at limit must pass through unchanged")
	}
}

func tailN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
