package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/blob"
)

// threadStub records one-shot invocations and drives the reply stream. It
// stands in for Manager.ChatOneShot via GroupDMManager.oneShot so thread-turn
// behaviour can be tested without the full chat pipeline.
type threadStub struct {
	mu            sync.Mutex
	calls         []OneShotOpts
	messages      []string
	reply         string
	active        int32
	maxConcurrent int32
	blockUntilCtx bool
	turnDelay     time.Duration
}

func (s *threadStub) callCount() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.calls) }
func (s *threadStub) lastOpts() OneShotOpts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[len(s.calls)-1]
}
func (s *threadStub) lastMessage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messages[len(s.messages)-1]
}

func (s *threadStub) fn(ctx context.Context, agentID, userMessage string, opts OneShotOpts) (<-chan ChatEvent, error) {
	n := atomic.AddInt32(&s.active, 1)
	for {
		cur := atomic.LoadInt32(&s.maxConcurrent)
		if n <= cur || atomic.CompareAndSwapInt32(&s.maxConcurrent, cur, n) {
			break
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, opts)
	s.messages = append(s.messages, userMessage)
	reply := s.reply
	s.mu.Unlock()

	if s.blockUntilCtx {
		<-ctx.Done()
	} else if s.turnDelay > 0 {
		time.Sleep(s.turnDelay)
	}
	atomic.AddInt32(&s.active, -1)

	ch := make(chan ChatEvent, 2)
	if reply != "" && ctx.Err() == nil {
		ch <- ChatEvent{Type: "text", Delta: reply}
	}
	close(ch)
	return ch, nil
}

func waitForMessage(t *testing.T, gdm *GroupDMManager, groupID, want string) *GroupMessage {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _, _, err := gdm.Messages(groupID, 50, "")
		if err == nil {
			for _, msg := range msgs {
				if msg.Content == want {
					return msg
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("message %q never appeared in room %s", want, groupID)
	return nil
}

func TestThreadPost_RunsOneShotNotNotify(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "pong"}
	gdm.oneShot = stub.fn

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	prior := newGroupMessage("ag_alice", "Alice", "prior answer", nil)
	if err := appendGroupMessage(g.ID, prior, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "ping", nil, true); err != nil {
		t.Fatal(err)
	}

	reply := waitForMessage(t, gdm, g.ID, "pong")
	if reply.AgentID != "ag_alice" {
		t.Errorf("reply author = %q, want ag_alice", reply.AgentID)
	}
	// Thread turn used ChatOneShot with the per-thread SessionKey.
	if got := stub.lastOpts().SessionKey; got != "groupdm:"+g.ID {
		t.Errorf("SessionKey = %q, want %q", got, "groupdm:"+g.ID)
	}
	if got := stub.lastOpts().History; len(got) != 1 || got[0].Text != "prior answer" || !got[0].IsBot {
		t.Errorf("History = %+v, want prior agent answer only", got)
	}
	// notify path must not have fired for the thread room.
	gdm.notifyMu.Lock()
	_, exists := gdm.notify[g.ID+":ag_alice"]
	gdm.notifyMu.Unlock()
	if exists {
		t.Errorf("thread post should not create notify state")
	}
}

func TestThreadPost_ForwardsInputAttachmentsToOneShot(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "received"}
	gdm.oneShot = stub.fn

	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(sourcePath, []byte("report contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	attachments := []MessageAttachment{{
		Path: sourcePath, Name: "report.pdf", Mime: "application/pdf", Size: 15, PeerID: "hub",
	}}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "inspect this", attachments, true); err != nil {
		t.Fatal(err)
	}
	waitForMessage(t, gdm, g.ID, "received")

	got := stub.lastOpts().Attachments
	if len(got) != 1 || got[0] != attachments[0] {
		t.Fatalf("one-shot attachments = %+v, want %+v", got, attachments)
	}
	if strings.Contains(stub.lastMessage(), sourcePath) {
		t.Fatalf("live payload leaked Hub-local attachment path: %q", stub.lastMessage())
	}
}

func TestThreadConversationHistory_KeepsAttachmentOnlyMessage(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	msg := newGroupMessage(UserSenderID, "User", "", []MessageAttachment{{
		Path: "kojo://global/groupdms/" + g.ID + "/input/design.png",
		Name: "design.png", Mime: "image/png", Size: 123,
	}})
	if err := appendGroupMessage(g.ID, msg, 0, false); err != nil {
		t.Fatal(err)
	}
	gdm.latestMsgID[g.ID] = msg.ID

	history, err := gdm.threadConversationSnapshot(context.Background(), g.ID, msg.ID, "ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].MessageID != msg.ID ||
		!strings.Contains(history[0].Text, "design.png") || !strings.Contains(history[0].Text, "image/png") {
		t.Fatalf("attachment-only history = %+v", history)
	}
}

func TestThreadConversationHistory_PreservesContentWhitespace(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	content := "    indented code  \nnext line  "
	msg := newGroupMessage(UserSenderID, "User", content, nil)
	if err := appendGroupMessage(g.ID, msg, 0, false); err != nil {
		t.Fatal(err)
	}
	gdm.latestMsgID[g.ID] = msg.ID
	history, err := gdm.threadConversationSnapshot(context.Background(), g.ID, msg.ID, "ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Text != content {
		t.Fatalf("history text = %q, want exact %q", history[0].Text, content)
	}
}

func TestThreadConversationHistory_StopsBeforeTriggeringMessage(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	prior := newGroupMessage("ag_alice", "Alice", "prior", nil)
	current := newGroupMessage(UserSenderID, "User", "current", nil)
	future := newGroupMessage(UserSenderID, "User", "future", nil)
	for _, msg := range []*GroupMessage{prior, current, future} {
		if err := appendGroupMessage(g.ID, msg, 0, false); err != nil {
			t.Fatal(err)
		}
	}
	gdm.latestMsgID[g.ID] = future.ID
	gdm.registerThreadQueued(g.ID, current.ID)
	gdm.registerThreadQueued(g.ID, future.ID)

	history, err := gdm.threadConversationSnapshot(context.Background(), g.ID, current.ID, "ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	filtered := history[:0]
	for _, msg := range history {
		if msg.MessageID != current.ID {
			filtered = append(filtered, msg)
		}
	}
	history = filtered
	if len(history) != 1 || history[0].MessageID != prior.ID || history[0].Text != "prior" {
		t.Fatalf("history = %+v, want only the message before the trigger", history)
	}
}

func TestThreadConversationHistory_StopsThroughArrivalTrigger(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	prior := newGroupMessage("ag_alice", "Alice", "prior", nil)
	current := newGroupMessage(UserSenderID, "User", "current", nil)
	future := newGroupMessage(UserSenderID, "User", "future", nil)
	steer := newGroupMessage(UserSenderID, "User", "steer current turn", nil)
	latePriorReply := newGroupMessage("ag_alice", "Alice", "reply to prior", nil)
	for _, msg := range []*GroupMessage{prior, current, future, steer, latePriorReply} {
		if err := appendGroupMessage(g.ID, msg, 0, false); err != nil {
			t.Fatal(err)
		}
	}
	gdm.latestMsgID[g.ID] = latePriorReply.ID
	gdm.registerThreadQueued(g.ID, current.ID)
	gdm.registerThreadQueued(g.ID, future.ID)

	history, err := gdm.threadConversationSnapshot(context.Background(), g.ID, current.ID, "ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 || history[0].MessageID != prior.ID || history[1].MessageID != current.ID || history[2].MessageID != steer.ID || history[3].MessageID != latePriorReply.ID {
		t.Fatalf("history = %+v, want prior, trigger, steer, and preceding-turn reply but not future queued post", history)
	}
}

func TestThreadPost_AttachesStagedFile(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	mgr.blobStore = newWiredBlob(t)

	const body = "thread attachment body"
	stub := &threadStub{reply: "attached"}
	stubFn := stub.fn

	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	stageDir := threadAttachmentStageDir("ag_alice", g.ID)
	gdm.oneShot = func(ctx context.Context, agentID, userMessage string, opts OneShotOpts) (<-chan ChatEvent, error) {
		if err := os.MkdirAll(stageDir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(stageDir, "report.xlsx"), []byte(body), 0o644); err != nil {
			return nil, err
		}
		return stubFn(ctx, agentID, userMessage, opts)
	}

	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "make a report", nil, true); err != nil {
		t.Fatal(err)
	}

	reply := waitForMessage(t, gdm, g.ID, "attached")
	if len(reply.Attachments) != 1 {
		t.Fatalf("reply attachments = %d, want 1", len(reply.Attachments))
	}
	att := reply.Attachments[0]
	if att.Name != "report.xlsx" {
		t.Errorf("attachment name = %q, want report.xlsx", att.Name)
	}
	wantPath := "kojo://global/agents/ag_alice/attach/" + reply.ID + "/report.xlsx"
	if att.Path != wantPath {
		t.Errorf("attachment path = %q, want %q", att.Path, wantPath)
	}

	r, _, err := mgr.blobStore.Open(blob.ScopeGlobal,
		"agents/ag_alice/attach/"+reply.ID+"/report.xlsx")
	if err != nil {
		t.Fatalf("open attachment blob: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read attachment blob: %v", err)
	}
	if string(got) != body {
		t.Errorf("attachment body = %q, want %q", got, body)
	}
	if _, err := os.Stat(filepath.Join(stageDir, "report.xlsx")); !os.IsNotExist(err) {
		t.Errorf("staged file still exists after reply: %v", err)
	}
	opts := stub.lastOpts()
	if !opts.DisableKojoAttachmentInstructions {
		t.Error("thread one-shot must hide the shared .kojo/attach instructions")
	}
	if !strings.Contains(opts.SystemPromptExtra, stageDir) {
		t.Errorf("thread prompt does not contain isolated stage dir %q", stageDir)
	}
}

func TestThreadPost_NoAttachmentRemovesStageDir(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	mgr.blobStore = newWiredBlob(t)
	stub := &threadStub{reply: "no file"}
	gdm.oneShot = stub.fn

	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "plain reply", nil, true); err != nil {
		t.Fatal(err)
	}
	waitForMessage(t, gdm, g.ID, "no file")
	stageDir := threadAttachmentStageDir("ag_alice", g.ID)
	if _, err := os.Stat(stageDir); !os.IsNotExist(err) {
		t.Errorf("empty thread stage dir still exists: %v", err)
	}
}

func TestParallelThreadPosts_IsolateStagedFiles(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	mgr.blobStore = newWiredBlob(t)

	g1, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	g2, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}

	var started atomic.Int32
	var releaseOnce sync.Once
	release := make(chan struct{})
	gdm.oneShot = func(ctx context.Context, agentID, userMessage string, opts OneShotOpts) (<-chan ChatEvent, error) {
		groupID := strings.TrimPrefix(opts.SessionKey, "groupdm:")
		stageDir := threadAttachmentStageDir(agentID, groupID)
		if err := os.MkdirAll(stageDir, 0o755); err != nil {
			return nil, err
		}
		name := groupID + ".txt"
		if err := os.WriteFile(filepath.Join(stageDir, name), []byte(groupID), 0o644); err != nil {
			return nil, err
		}
		if started.Add(1) == 2 {
			releaseOnce.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		ch := make(chan ChatEvent, 1)
		ch <- ChatEvent{Type: "text", Delta: groupID}
		close(ch)
		return ch, nil
	}

	if _, err := gdm.PostUserMessage(context.Background(), g1.ID, "first", nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g2.ID, "second", nil, true); err != nil {
		t.Fatal(err)
	}

	for _, g := range []*GroupDM{g1, g2} {
		reply := waitForMessage(t, gdm, g.ID, g.ID)
		if len(reply.Attachments) != 1 {
			t.Fatalf("thread %s attachments = %d, want 1", g.ID, len(reply.Attachments))
		}
		wantName := g.ID + ".txt"
		if reply.Attachments[0].Name != wantName {
			t.Errorf("thread %s attachment = %q, want %q", g.ID, reply.Attachments[0].Name, wantName)
		}
		if !strings.Contains(reply.Attachments[0].Path, "/attach/"+reply.ID+"/"+wantName) {
			t.Errorf("thread %s attachment path mismatched reply ID: %q", g.ID, reply.Attachments[0].Path)
		}
	}
}

func TestThreadArchive_CancelsAttachmentFinalization(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	mgr.blobStore = newWiredBlob(t)

	g, err := gdm.CreateThread("ag_alice")
	if err != nil {
		t.Fatal(err)
	}
	stageDir := threadAttachmentStageDir("ag_alice", g.ID)
	gdm.oneShot = func(ctx context.Context, agentID, userMessage string, opts OneShotOpts) (<-chan ChatEvent, error) {
		if err := os.MkdirAll(stageDir, 0o755); err != nil {
			return nil, err
		}
		path := filepath.Join(stageDir, "large.bin")
		if err := os.WriteFile(path, []byte("body"), 0o644); err != nil {
			return nil, err
		}
		// Keep the live watcher's mtime-stability check re-debouncing until
		// the event stream closes. The final scan then owns the forward below,
		// making the archive-cancel assertion deterministic.
		future := time.Now().Add(time.Hour)
		if err := os.Chtimes(path, future, future); err != nil {
			return nil, err
		}
		ch := make(chan ChatEvent, 1)
		ch <- ChatEvent{Type: "text", Delta: "done"}
		close(ch)
		return ch, nil
	}

	forwardStarted := make(chan string, 1)
	mgr.SetAttachmentForwarder(func(ctx context.Context, scope blob.Scope, path, sha string, r io.Reader, size int64) error {
		forwardStarted <- path
		<-ctx.Done()
		return ctx.Err()
	})

	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "attach", nil, true); err != nil {
		t.Fatal(err)
	}
	var blobPath string
	select {
	case blobPath = <-forwardStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("attachment finalization did not start")
	}
	if err := gdm.Delete(g.ID, false); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, err := mgr.blobStore.Head(blob.ScopeGlobal, blobPath)
		if errors.Is(err, blob.ErrNotFound) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("orphaned attachment blob still exists after archive: %s", blobPath)
}

func TestThreadPost_EmptyReplyPostsNothing(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: ""}
	gdm.oneShot = stub.fn

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "ping", nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && stub.callCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	msgs, _, _, err := gdm.Messages(g.ID, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Errorf("expected only the user message, got %d messages", len(msgs))
	}
}

func TestGroupPost_UsesNotifyNotOneShot(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "hi"}
	gdm.oneShot = stub.fn

	g, err := gdm.Create("Team", []string{"ag_alice", "ag_bob"}, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "hello team", nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		gdm.notifyMu.Lock()
		_, a := gdm.notify[g.ID+":ag_alice"]
		_, b := gdm.notify[g.ID+":ag_bob"]
		gdm.notifyMu.Unlock()
		found = a || b
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Errorf("group post should use notify path (notify state expected)")
	}
	if stub.callCount() != 0 {
		t.Errorf("group post must not run a thread one-shot, got %d calls", stub.callCount())
	}
}

func TestTwoMemberDM_UsesNotifyNotThread(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "hi"}
	gdm.oneShot = stub.fn

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice", "ag_bob"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "hey", nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		gdm.notifyMu.Lock()
		_, a := gdm.notify[g.ID+":ag_alice"]
		_, b := gdm.notify[g.ID+":ag_bob"]
		gdm.notifyMu.Unlock()
		found = a || b
		if !found {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !found {
		t.Errorf("2-member dm should use notify path, not thread turn")
	}
	if stub.callCount() != 0 {
		t.Errorf("2-member dm must not run a thread one-shot, got %d calls", stub.callCount())
	}
}

func TestThreadTurn_ProceedsWhileMainChatBusy(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	stub := &threadStub{reply: "still-here"}
	gdm.oneShot = stub.fn

	// Mark the agent's main chat busy — the thread turn must not depend on it.
	mgr.busyMu.Lock()
	mgr.busy["ag_alice"] = busyEntry{startedAt: time.Now()}
	mgr.busyMu.Unlock()

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "ping", nil, true); err != nil {
		t.Fatal(err)
	}
	waitForMessage(t, gdm, g.ID, "still-here")
}

func TestThreadTurn_HasNoDeadline(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}

	hasDeadline := true
	gdm.oneShot = func(ctx context.Context, agentID, userMessage string, opts OneShotOpts) (<-chan ChatEvent, error) {
		_, hasDeadline = ctx.Deadline()
		ch := make(chan ChatEvent)
		close(ch)
		return ch, nil
	}

	gdm.runThreadTurn("ag_alice", g.ID, "G", newGroupMessage(UserSenderID, UserSenderName, "ping", nil))
	if hasDeadline {
		t.Fatal("thread turn context has a deadline, want none")
	}
}

func TestThreadTurn_SerializesConcurrentPosts(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "ok", turnDelay: 80 * time.Millisecond}
	gdm.oneShot = stub.fn

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "one", nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "two", nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && stub.callCount() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if stub.callCount() < 2 {
		t.Fatalf("expected 2 thread turns, got %d", stub.callCount())
	}
	if mc := atomic.LoadInt32(&stub.maxConcurrent); mc != 1 {
		t.Errorf("max concurrent thread turns = %d, want 1 (serialized)", mc)
	}
}

// TestArchiveThread_QueuedTurnBails verifies that a thread turn queued behind
// the per-room mutex during an archive does not run: no oneShot call for it,
// and no session JSONL re-created after archive removed it.
func TestArchiveThread_QueuedTurnBails(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{blockUntilCtx: true}
	gdm.oneShot = stub.fn

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}

	sessionID := expectedClaudeSessionID("ag_alice", "groupdm:"+g.ID, false)
	absDir, err := filepath.Abs(agentDir("ag_alice"))
	if err != nil {
		t.Fatal(err)
	}
	projectDir := claudeProjectDir(absDir)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(projectDir, sessionID+".jsonl")
	if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First post starts a blocking turn (holds threadMus until cancelled).
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "one", nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && stub.callCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if stub.callCount() != 1 {
		t.Fatalf("first turn never started, calls=%d", stub.callCount())
	}

	// Second post queues a turn behind the mutex.
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "two", nil, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// Archive: cancels the first turn and removes the session. The queued
	// second turn then acquires the mutex, sees the room is gone, and bails.
	if err := gdm.Delete(g.ID, false); err != nil {
		t.Fatal(err)
	}

	// Give the queued goroutine time to run (or, correctly, to bail).
	time.Sleep(200 * time.Millisecond)
	if got := stub.callCount(); got != 1 {
		t.Errorf("queued turn ran after archive: oneShot calls = %d, want 1", got)
	}
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Errorf("session file re-created (or not removed) after archive, stat err = %v", err)
	}
}

func TestArchiveThread_CancelsAndCleansSession(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{blockUntilCtx: true}
	gdm.oneShot = stub.fn

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}

	// Seed a fake session JSONL at the deterministic path so archive removes it.
	sessionID := expectedClaudeSessionID("ag_alice", "groupdm:"+g.ID, false)
	absDir, err := filepath.Abs(agentDir("ag_alice"))
	if err != nil {
		t.Fatal(err)
	}
	projectDir := claudeProjectDir(absDir)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(projectDir, sessionID+".jsonl")
	if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start a turn that blocks until its context is cancelled.
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "ping", nil, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gdm.threadCancelMu.Lock()
		_, running := gdm.threadCancels[g.ID]
		gdm.threadCancelMu.Unlock()
		if running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := gdm.Delete(g.ID, false); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Errorf("expected session file removed, stat err = %v", err)
	}
	drained := false
	for i := 0; i < 200; i++ {
		gdm.threadCancelMu.Lock()
		_, running := gdm.threadCancels[g.ID]
		gdm.threadCancelMu.Unlock()
		if !running {
			drained = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !drained {
		t.Errorf("thread turn cancel entry did not drain after archive")
	}
}

func TestThreadTurn_FiresOnThreadReply(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	stub := &threadStub{reply: "pong"}
	gdm.oneShot = stub.fn
	type call struct {
		agentID, groupID, content string
	}
	got := make(chan call, 4)
	gdm.OnThreadReply = func(agentID, groupID string, msg *GroupMessage) {
		got <- call{agentID, groupID, msg.Content}
	}

	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "ping", nil, true); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-got:
		if c.agentID != "ag_alice" || c.groupID != g.ID || c.content != "pong" {
			t.Errorf("OnThreadReply = %+v, want ag_alice/%s/pong", c, g.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnThreadReply never fired")
	}
}

func TestThreadTurn_StopDoesNotFireOnThreadReply(t *testing.T) {
	gdm, _ := setupGroupDMTest(t)
	g, _, err := gdm.FindOrCreateDM([]string{"ag_alice"})
	if err != nil {
		t.Fatal(err)
	}
	var fired atomic.Int32
	gdm.OnThreadReply = func(string, string, *GroupMessage) { fired.Add(1) }
	started := make(chan struct{})
	gdm.SetOneShotForTesting(func(ctx context.Context, agentID, userMessage string, opts OneShotOpts) (<-chan ChatEvent, error) {
		ch := make(chan ChatEvent)
		go func() {
			defer close(ch)
			ch <- ChatEvent{Type: "text", Delta: "partial"}
			close(started)
			<-ctx.Done()
		}()
		return ch, nil
	})
	done := make(chan struct{})
	go func() {
		gdm.runThreadTurn("ag_alice", g.ID, "G", newGroupMessage(UserSenderID, UserSenderName, "q", nil))
		close(done)
	}()
	<-started
	for i := 0; i < 500 && !gdm.StopThreadTurn(g.ID); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	<-done
	waitForMessage(t, gdm, g.ID, "partial")
	time.Sleep(100 * time.Millisecond)
	if n := fired.Load(); n != 0 {
		t.Errorf("OnThreadReply fired %d times on operator stop, want 0", n)
	}
}
