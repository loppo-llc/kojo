package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// waitThreadIdle waits until no thread turn is running or queued for groupID.
func waitThreadIdle(t *testing.T, gdm *GroupDMManager, groupID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := gdm.threadTurns.TryReserve(groupID); ok {
			r.Release()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("thread %s never went idle", groupID)
}

func threadContents(t *testing.T, gdm *GroupDMManager, groupID string) []string {
	t.Helper()
	msgs, _, _, err := gdm.Messages(groupID, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Content
	}
	return out
}

func TestRewindThread_TruncatesAndFreshensNextTurnOnly(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "pong"}
	gdm.oneShot = stub.fn
	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := gdm.PostUserMessage(ctx, g.ID, "one", nil, true); err != nil {
		t.Fatal(err)
	}
	waitThreadIdle(t, gdm, g.ID)
	second, err := gdm.PostUserMessage(ctx, g.ID, "two", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	waitThreadIdle(t, gdm, g.ID)
	if got := threadContents(t, gdm, g.ID); len(got) != 4 {
		t.Fatalf("before rewind = %q, want 4 messages", got)
	}
	if stub.lastOpts().ForceFreshSession {
		t.Fatal("ordinary turn ran with ForceFreshSession")
	}

	res, err := gdm.RewindThread(g.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.MessagesRemoved != 2 {
		t.Errorf("MessagesRemoved = %d, want 2", res.MessagesRemoved)
	}
	if got := threadContents(t, gdm, g.ID); len(got) != 2 || got[0] != "one" || got[1] != "pong" {
		t.Fatalf("after rewind = %q, want [one pong]", got)
	}
	if head := gdm.LatestMessageID(g.ID); head == "" || head == second.ID {
		t.Errorf("latestMsgID cache = %q, want the surviving head", head)
	}

	// Simulate a daemon restart: only the persisted marker survives.
	gdm.threadFresh.mu.Lock()
	gdm.threadFresh.pending = nil
	gdm.threadFresh.mu.Unlock()

	if _, err := gdm.PostUserMessage(ctx, g.ID, "three", nil, true); err != nil {
		t.Fatal(err)
	}
	waitThreadIdle(t, gdm, g.ID)
	opts := stub.lastOpts()
	if !opts.ForceFreshSession {
		t.Fatal("first turn after rewind did not force a fresh native session")
	}
	for _, h := range opts.History {
		if h.Text == "two" {
			t.Fatalf("rewound message leaked into history: %+v", opts.History)
		}
	}
	if len(opts.History) != 2 {
		t.Errorf("history len = %d, want 2 (one, pong)", len(opts.History))
	}

	if _, err := gdm.PostUserMessage(ctx, g.ID, "four", nil, true); err != nil {
		t.Fatal(err)
	}
	waitThreadIdle(t, gdm, g.ID)
	if stub.lastOpts().ForceFreshSession {
		t.Fatal("marker not consumed: second turn after rewind still forced fresh")
	}
}

func TestRewindThread_KeepsMarkerWhenTurnFailsToStart(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "pong"}
	gdm.oneShot = stub.fn
	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := gdm.PostUserMessage(ctx, g.ID, "one", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	waitThreadIdle(t, gdm, g.ID)
	if _, err := gdm.RewindThread(g.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if got := threadContents(t, gdm, g.ID); len(got) != 0 {
		t.Fatalf("after rewind = %q, want empty", got)
	}

	var calls []bool
	gdm.oneShot = func(ctx context.Context, agentID, msg string, opts OneShotOpts) (<-chan ChatEvent, error) {
		calls = append(calls, opts.ForceFreshSession)
		if len(calls) == 1 {
			return nil, errors.New("holder unreachable")
		}
		return stub.fn(ctx, agentID, msg, opts)
	}
	for _, text := range []string{"retry1", "retry2", "retry3"} {
		if _, err := gdm.PostUserMessage(ctx, g.ID, text, nil, true); err != nil {
			t.Fatal(err)
		}
		waitThreadIdle(t, gdm, g.ID)
	}
	if len(calls) != 3 || !calls[0] || !calls[1] || calls[2] {
		t.Fatalf("ForceFreshSession per turn = %v, want [true true false]", calls)
	}
}

func TestRewindThread_RejectsBusyAndNonThread(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{blockUntilCtx: true}
	gdm.oneShot = stub.fn
	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := gdm.PostUserMessage(context.Background(), g.ID, "ping", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.RewindThread(g.ID, msg.ID); !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("rewind during a turn: err = %v, want ErrAgentBusy", err)
	}
	for deadline := time.Now().Add(3 * time.Second); stub.callCount() == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := gdm.RewindThread(g.ID, msg.ID); !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("rewind while the backend runs: err = %v, want ErrAgentBusy", err)
	}
	for deadline := time.Now().Add(3 * time.Second); !gdm.StopThreadTurn(g.ID) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	waitThreadIdle(t, gdm, g.ID)
	if got := threadContents(t, gdm, g.ID); len(got) == 0 || got[0] != "ping" {
		t.Fatalf("busy rejection touched the transcript: %q", got)
	}

	if _, err := gdm.RewindThread(g.ID, "gm_missing"); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("unknown message: err = %v, want ErrMessageNotFound", err)
	}
	if got := threadContents(t, gdm, g.ID); len(got) == 0 || got[0] != "ping" {
		t.Fatalf("unknown-id rewind touched the transcript: %q", got)
	}

	grp, err := gdm.Create("team", []string{"ag_alice", "ag_bob"}, 0, GroupDMStyleEfficient, defaultGroupDMVenue)
	if err != nil {
		t.Fatal(err)
	}
	gm, err := gdm.PostUserMessage(context.Background(), grp.ID, "hi", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.RewindThread(grp.ID, gm.ID); !errors.Is(err, ErrNotThreadRoom) {
		t.Fatalf("group room: err = %v, want ErrNotThreadRoom", err)
	}
}

func TestRewindThread_RefusesWhileCodexGoalBound(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	gdm.oneShot = (&threadStub{reply: "pong"}).fn
	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := gdm.PostUserMessage(context.Background(), g.ID, "one", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	waitThreadIdle(t, gdm, g.ID)
	logger := gdm.logger
	writeCodexThreadRef("ag_alice", webUIThreadKeyPrefix+g.ID, codexThreadRef{
		ThreadID: "019e7cc9-dd5e-7971-b654-7840c683879e",
		Goal:     &GoalBinding{State: &CodexGoal{Status: "active"}},
	}, logger)
	if _, err := gdm.RewindThread(g.ID, msg.ID); !errors.Is(err, ErrThreadGoalBound) {
		t.Fatalf("err = %v, want ErrThreadGoalBound", err)
	}
	if got := threadContents(t, gdm, g.ID); len(got) != 2 {
		t.Fatalf("refused rewind touched the transcript: %q", got)
	}
	if gdm.threadFreshSessionPending(g.ID) {
		t.Fatal("refused rewind left a fresh-session marker")
	}
}

func TestRewindThread_ResetsAutoTitleOnlyWhenTranscriptEmptied(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	gdm.oneShot = (&threadStub{reply: "pong"}).fn
	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := gdm.PostUserMessage(ctx, g.ID, "plan the trip", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	waitThreadIdle(t, gdm, g.ID)
	second, err := gdm.PostUserMessage(ctx, g.ID, "more", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	waitThreadIdle(t, gdm, g.ID)
	name := func() string {
		got, ok := gdm.Get(g.ID)
		if !ok {
			t.Fatal("room vanished")
		}
		return got.Name
	}
	if name() != "plan the trip" {
		t.Fatalf("auto-title = %q", name())
	}
	if _, err := gdm.RewindThread(g.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if name() != "plan the trip" {
		t.Fatalf("partial rewind changed the title to %q", name())
	}
	if _, err := gdm.RewindThread(g.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if name() != DefaultThreadName {
		t.Fatalf("emptying rewind kept title %q, want default", name())
	}
	if _, err := gdm.PostUserMessage(ctx, g.ID, "new topic", nil, true); err != nil {
		t.Fatal(err)
	}
	waitThreadIdle(t, gdm, g.ID)
	if name() != "new topic" {
		t.Fatalf("re-title = %q, want %q", name(), "new topic")
	}
}

func TestRewindThread_DropsStaleBackgroundTurnUntilFreshTurn(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	gdm.oneShot = (&threadStub{reply: "pong"}).fn
	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	msg, err := gdm.PostUserMessage(ctx, g.ID, "one", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	waitThreadIdle(t, gdm, g.ID)
	if _, err := gdm.RewindThread(g.ID, msg.ID); err != nil {
		t.Fatal(err)
	}

	background := func(text string) (aborted bool) {
		events := make(chan ChatEvent, 1)
		events <- ChatEvent{Type: "done", Message: &Message{Content: text}}
		close(events)
		gdm.runKeyedBackgroundTurn(context.Background(), "ag_alice", g.ID, events, func(error) { aborted = true }, generateGroupMessageID(), false)
		return aborted
	}
	if !background("stale result") {
		t.Fatal("pre-fresh background turn was not aborted")
	}
	if got := threadContents(t, gdm, g.ID); len(got) != 0 {
		t.Fatalf("stale background result posted: %q", got)
	}

	if _, err := gdm.PostUserMessage(ctx, g.ID, "two", nil, true); err != nil {
		t.Fatal(err)
	}
	waitThreadIdle(t, gdm, g.ID)
	background("fresh result")
	got := threadContents(t, gdm, g.ID)
	if len(got) != 3 || got[2] != "fresh result" {
		t.Fatalf("post-fresh background result = %q, want it posted", got)
	}
}
