package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// TestBuiltinToolModelsMatchWeb keeps the Go allowlist in step with the
// model list the WebUI offers.
func TestBuiltinToolModelsMatchWeb(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "toolModels.ts"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	id := regexp.MustCompile(`"([^"]+)"`)
	for tool, want := range builtinToolModels {
		block := regexp.MustCompile(`(?s)"?` + regexp.QuoteMeta(tool) + `"?:\s*\{.*?models:\s*\[(.*?)\]`).FindStringSubmatch(src)
		if block == nil {
			t.Errorf("%s: not found in toolModels.ts", tool)
			continue
		}
		var got []string
		for _, m := range id.FindAllStringSubmatch(block[1], -1) {
			got = append(got, m[1])
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s drifted:\n web: %v\n  go: %v", tool, got, want)
		}
	}
}

func TestResolveModelSwitchTarget(t *testing.T) {
	str := func(s string) *string { return &s }
	cur := ModelSwitchTarget{Tool: ToolClaude, Model: "sonnet"}
	if _, _, err := resolveModelSwitchTarget(cur, ModelSwitchRequest{Model: "gpt-5.5"}); !errors.Is(err, ErrModelSwitchUnknownModel) {
		t.Errorf("model of another backend: %v", err)
	}
	if _, _, err := resolveModelSwitchTarget(cur, ModelSwitchRequest{Model: "x", Tool: str("nope")}); !errors.Is(err, ErrModelSwitchUnknownTool) {
		t.Errorf("unknown tool: %v", err)
	}
	if _, _, err := resolveModelSwitchTarget(cur, ModelSwitchRequest{Model: "opus", Effort: str("bogus")}); !errors.Is(err, ErrModelSwitchInvalidEffort) {
		t.Errorf("bad effort: %v", err)
	}
	to, _, err := resolveModelSwitchTarget(cur, ModelSwitchRequest{Model: "gpt-5.5", Tool: str(ToolCodex)})
	if err != nil || to.Tool != ToolCodex || to.Model != "gpt-5.5" {
		t.Errorf("cross-backend: %+v %v", to, err)
	}
	custom, _, err := resolveModelSwitchTarget(ModelSwitchTarget{Tool: ToolCustomClaude, Model: "a"}, ModelSwitchRequest{Model: "anything"})
	if err != nil || custom.Model != "anything" {
		t.Errorf("custom backend: %+v %v", custom, err)
	}
}

type modelSwitchTurn struct {
	model, msg string
}

// modelSwitchBackend records the model each turn was spawned with. Queue a
// script per turn: "hold" runs until released or cancelled, "fail" ends in an
// error event, anything else completes.
type modelSwitchBackend struct {
	turns   chan modelSwitchTurn
	script  chan string
	release chan struct{}
}

func (b *modelSwitchBackend) Name() string    { return ToolClaude }
func (b *modelSwitchBackend) Available() bool { return true }
func (b *modelSwitchBackend) Chat(ctx context.Context, a *Agent, msg, _ string, _ ChatOptions) (<-chan ChatEvent, error) {
	action := ""
	select {
	case action = <-b.script:
	default:
	}
	b.turns <- modelSwitchTurn{model: a.Model, msg: msg}
	out := make(chan ChatEvent, 1)
	go func() {
		defer close(out)
		switch action {
		case "hold":
			select {
			case <-b.release:
			case <-ctx.Done():
				return
			}
		case "fail":
			out <- ChatEvent{Type: "error", ErrorMessage: "model not available"}
			return
		}
		reply := newAssistantMessage()
		reply.Content = "ok"
		out <- ChatEvent{Type: "done", Message: reply}
	}()
	return out, nil
}

func newModelSwitchManager(t *testing.T) (*Manager, *modelSwitchBackend, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	b := &modelSwitchBackend{
		turns:   make(chan modelSwitchTurn, 16),
		script:  make(chan string, 16),
		release: make(chan struct{}),
	}
	m.backends[ToolClaude] = b
	a, err := m.Create(AgentConfig{Name: "switcher", Tool: ToolClaude, Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	return m, b, a.ID
}

func (b *modelSwitchBackend) nextTurn(t *testing.T) modelSwitchTurn {
	t.Helper()
	select {
	case turn := <-b.turns:
		return turn
	case <-time.After(10 * time.Second):
		t.Fatal("no turn started")
		return modelSwitchTurn{}
	}
}

func (b *modelSwitchBackend) assertNoTurn(t *testing.T) {
	t.Helper()
	select {
	case turn := <-b.turns:
		t.Fatalf("unexpected turn: %+v", turn)
	case <-time.After(500 * time.Millisecond):
	}
}

func startHeldTurn(t *testing.T, m *Manager, b *modelSwitchBackend, id string) <-chan ChatEvent {
	t.Helper()
	b.script <- "hold"
	events, err := m.Chat(context.Background(), id, "hello", "user", nil, BusySourceUser)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.nextTurn(t); got.model != "sonnet" {
		t.Fatalf("first turn model = %q", got.model)
	}
	return events
}

func waitModelSwitchIdle(t *testing.T, m *Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m.modelSwitchMu.Lock()
		_, pending := m.modelSwitches[id]
		m.modelSwitchMu.Unlock()
		if !pending && !m.IsBusy(id) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("model switch still pending")
}

func currentModel(t *testing.T, m *Manager, id string) string {
	t.Helper()
	a, ok := m.Get(id)
	if !ok {
		t.Fatal("agent gone")
	}
	return a.Model
}

func TestSwitchModel_NextTurnRunsOnNewModel(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	events := startHeldTurn(t, m, b, id)

	res, err := m.SwitchModel(id, ModelSwitchRequest{Model: "opus", Note: "finish the refactor"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != ModelSwitchQueued || res.From.Model != "sonnet" || res.To.Model != "opus" {
		t.Fatalf("result = %+v", res)
	}
	b.assertNoTurn(t) // nothing starts while the calling turn runs

	close(b.release)
	drainEvents(events)
	next := b.nextTurn(t)
	if next.model != "opus" {
		t.Errorf("next turn model = %q", next.model)
	}
	if !strings.Contains(next.msg, "sonnet") || !strings.Contains(next.msg, "opus") || !strings.Contains(next.msg, "finish the refactor") {
		t.Errorf("prompt = %q", next.msg)
	}
	waitModelSwitchIdle(t, m, id)
	if got := currentModel(t, m, id); got != "opus" {
		t.Errorf("model after success = %q", got)
	}
	if _, err := m.Store().GetKV(context.Background(), modelSwitchKVNamespace, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("marker left behind: %v", err)
	}
}

func TestSwitchModel_IdleStartsImmediately(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	if _, err := m.SwitchModel(id, ModelSwitchRequest{Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	if got := b.nextTurn(t); got.model != "opus" {
		t.Errorf("model = %q", got.model)
	}
	waitModelSwitchIdle(t, m, id)
}

func TestSwitchModel_RejectsWithoutChanging(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	if _, err := m.SwitchModel(id, ModelSwitchRequest{Model: "claude-typo"}); !errors.Is(err, ErrModelSwitchUnknownModel) {
		t.Fatalf("err = %v", err)
	}
	if _, err := m.SwitchModel(id, ModelSwitchRequest{Model: "opus", SessionKey: "groupdm:gone"}); !errors.Is(err, ErrModelSwitchCallerInactive) {
		t.Fatalf("inactive caller err = %v", err)
	}
	if got := currentModel(t, m, id); got != "sonnet" {
		t.Errorf("model = %q", got)
	}
	b.assertNoTurn(t)
}

func TestSwitchModel_Unchanged(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	res, err := m.SwitchModel(id, ModelSwitchRequest{Model: "sonnet"})
	if err != nil || res.Status != ModelSwitchUnchanged {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	b.assertNoTurn(t)
}

func TestSwitchModel_SwitchBackCancels(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	events := startHeldTurn(t, m, b, id)
	if _, err := m.SwitchModel(id, ModelSwitchRequest{Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	res, err := m.SwitchModel(id, ModelSwitchRequest{Model: "sonnet"})
	if err != nil || res.Status != ModelSwitchUnchanged {
		t.Fatalf("switch back: res=%+v err=%v", res, err)
	}
	close(b.release)
	drainEvents(events)
	b.assertNoTurn(t)
	if got := currentModel(t, m, id); got != "sonnet" {
		t.Errorf("model = %q", got)
	}
}

func TestSwitchModel_UserStopKeepsModelWithoutTurn(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	events := startHeldTurn(t, m, b, id)
	if _, err := m.SwitchModel(id, ModelSwitchRequest{Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	m.AbortByUser(id)
	drainEvents(events)
	waitModelSwitchIdle(t, m, id)
	b.assertNoTurn(t)
	if got := currentModel(t, m, id); got != "opus" {
		t.Errorf("model = %q", got)
	}
}

func TestSwitchModel_FailedTurnRollsBack(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	events := startHeldTurn(t, m, b, id)
	if _, err := m.SwitchModel(id, ModelSwitchRequest{Model: "opus", Note: "carry on"}); err != nil {
		t.Fatal(err)
	}
	b.script <- "fail"
	close(b.release)
	drainEvents(events)

	if got := b.nextTurn(t); got.model != "opus" {
		t.Fatalf("switched turn model = %q", got.model)
	}
	notice := b.nextTurn(t)
	if notice.model != "sonnet" {
		t.Errorf("recovery turn model = %q", notice.model)
	}
	if !strings.Contains(notice.msg, "model not available") || !strings.Contains(notice.msg, "carry on") {
		t.Errorf("recovery prompt = %q", notice.msg)
	}
	if got := currentModel(t, m, id); got != "sonnet" {
		t.Errorf("model after rollback = %q", got)
	}
	b.assertNoTurn(t) // the recovery turn is not verified: no loop
}

type recordingReservation struct {
	prompts chan string
}

func (r *recordingReservation) Activate(_ context.Context, prompt, _ string) error {
	r.prompts <- prompt
	return nil
}
func (r *recordingReservation) Release() {}

func TestSwitchModel_ThreadContinuesThroughReservation(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	key := id + ":ext.test:k1"
	res := &recordingReservation{prompts: make(chan string, 1)}
	b.script <- "hold"
	events, err := m.ChatOneShot(context.Background(), id, "external", OneShotOpts{SessionKey: key, HandoffArrivalReservation: res})
	if err != nil {
		t.Fatal(err)
	}
	b.nextTurn(t)
	if _, err := m.SwitchModel(id, ModelSwitchRequest{Model: "opus", SessionKey: key, Note: "thread note"}); err != nil {
		t.Fatal(err)
	}
	close(b.release)
	drainEvents(events)
	select {
	case prompt := <-res.prompts:
		if !strings.Contains(prompt, "thread note") {
			t.Errorf("prompt = %q", prompt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reservation not activated")
	}
	b.assertNoTurn(t) // the adapter runs the arrival; main must not also wake
}

func TestSwitchModel_GoneThreadFallsBackToMain(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	key := "groupdm:gd_missing"
	b.script <- "hold"
	events, err := m.ChatOneShot(context.Background(), id, "room", OneShotOpts{SessionKey: key})
	if err != nil {
		t.Fatal(err)
	}
	b.nextTurn(t)
	if _, err := m.SwitchModel(id, ModelSwitchRequest{Model: "opus", SessionKey: key}); err != nil {
		t.Fatal(err)
	}
	close(b.release)
	drainEvents(events)
	got := b.nextTurn(t)
	if got.model != "opus" || !strings.Contains(got.msg, key) {
		t.Errorf("fallback turn = %+v", got)
	}
	waitModelSwitchIdle(t, m, id)
}

func TestModelSwitchStopOf(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		want  modelSwitchStop
	}{
		{"normal end", nil, modelSwitchProceed},
		{"user stop", UserStop(), modelSwitchDrop},
		{"reset", KojoStop(stopReasonReset), modelSwitchDrop},
		{"device switch", KojoStop(stopReasonSwitch), modelSwitchDrop},
		{"shutdown", KojoStop(stopReasonShutdown), modelSwitchDefer},
		{"timeout", KojoStop(stopReasonTimeout), modelSwitchProceed},
	}
	for _, c := range cases {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(c.cause)
		if got := modelSwitchStopOf(ctx); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestConsumeModelSwitchWakes(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	if _, err := m.Update(id, modelSwitchUpdate(
		ModelSwitchTarget{Tool: ToolClaude, Model: "sonnet"},
		ModelSwitchTarget{Tool: ToolClaude, Model: "opus"})); err != nil {
		t.Fatal(err)
	}
	val, _ := json.Marshal(modelSwitchMarker{
		AgentID: id,
		From:    ModelSwitchTarget{Tool: ToolClaude, Model: "sonnet"},
		To:      ModelSwitchTarget{Tool: ToolClaude, Model: "opus"},
		Note:    "resume after restart",
	})
	if _, err := m.Store().PutKV(context.Background(), &store.KVRecord{
		Namespace: modelSwitchKVNamespace, Key: id, Value: string(val),
		Type: store.KVTypeString, Scope: store.KVScopeMachine,
	}, store.KVPutOptions{}); err != nil {
		t.Fatal(err)
	}

	// A marker newer than this boot belongs to the next process.
	m.ConsumeModelSwitchWakes(time.Now().Add(-time.Hour))
	b.assertNoTurn(t)

	m.ConsumeModelSwitchWakes(time.Now().Add(time.Minute))
	got := b.nextTurn(t)
	if got.model != "opus" || !strings.Contains(got.msg, "resume after restart") {
		t.Errorf("turn = %+v", got)
	}
	waitModelSwitchIdle(t, m, id)
	if _, err := m.Store().GetKV(context.Background(), modelSwitchKVNamespace, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("marker left behind: %v", err)
	}
}

func TestConsumeModelSwitchWakes_DropsMovedOnMarker(t *testing.T) {
	m, b, id := newModelSwitchManager(t) // current model: sonnet
	val, _ := json.Marshal(modelSwitchMarker{
		AgentID: id,
		From:    ModelSwitchTarget{Tool: ToolClaude, Model: "haiku"},
		To:      ModelSwitchTarget{Tool: ToolClaude, Model: "opus"},
	})
	if _, err := m.Store().PutKV(context.Background(), &store.KVRecord{
		Namespace: modelSwitchKVNamespace, Key: id, Value: string(val),
		Type: store.KVTypeString, Scope: store.KVScopeMachine,
	}, store.KVPutOptions{}); err != nil {
		t.Fatal(err)
	}
	m.ConsumeModelSwitchWakes(time.Now().Add(time.Minute))
	b.assertNoTurn(t)
	if _, err := m.Store().GetKV(context.Background(), modelSwitchKVNamespace, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("stale marker kept: %v", err)
	}
}

// A turn other than the auto-started one must not settle the switch.
func TestSwitchModel_ForeignTurnDoesNotSettle(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	events := startHeldTurn(t, m, b, id)
	if _, err := m.SwitchModel(id, ModelSwitchRequest{Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	m.noteModelSwitchTurnEnd(id, "cron:x", 0, context.Background(), turnEnd{terminal: true, errMsg: "boom"}, nil)
	m.modelSwitchMu.Lock()
	_, pending := m.modelSwitches[id]
	m.modelSwitchMu.Unlock()
	if !pending {
		t.Fatal("foreign turn settled the switch")
	}
	close(b.release)
	drainEvents(events)
	if got := b.nextTurn(t); got.model != "opus" || !strings.Contains(got.msg, "(switch ") {
		t.Errorf("next turn = %+v", got)
	}
	waitModelSwitchIdle(t, m, id)
	if got := currentModel(t, m, id); got != "opus" {
		t.Errorf("model = %q", got)
	}
}

func modelSwitchMarkerExists(t *testing.T, m *Manager, id string) bool {
	t.Helper()
	_, err := m.Store().GetKV(context.Background(), modelSwitchKVNamespace, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// The marker must outlive everything up to the turn reaching its backend.
func TestSwitchModel_MarkerConsumedWhenTurnStarts(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	events := startHeldTurn(t, m, b, id)
	if _, err := m.SwitchModel(id, ModelSwitchRequest{Model: "opus"}); err != nil {
		t.Fatal(err)
	}
	if !modelSwitchMarkerExists(t, m, id) {
		t.Fatal("marker missing while the source turn runs")
	}
	close(b.release)
	drainEvents(events)
	b.nextTurn(t)
	if modelSwitchMarkerExists(t, m, id) {
		t.Error("marker survived the turn start")
	}
	waitModelSwitchIdle(t, m, id)
}

func TestNoteModelSwitchStartFailure(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		keepMarker bool
		wantModel  string
	}{
		{"restart keeps the marker", ErrQuiescing, true, "opus"},
		{"busy drops the wake", ErrAgentBusy, false, "opus"},
		{"real failure reverts", errors.New("cli missing"), false, "sonnet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, b, id := newModelSwitchManager(t)
			events := startHeldTurn(t, m, b, id)
			if _, err := m.SwitchModel(id, ModelSwitchRequest{Model: "opus"}); err != nil {
				t.Fatal(err)
			}
			m.modelSwitchMu.Lock()
			p := m.modelSwitches[id]
			token := modelSwitchTokenText(p.token)
			m.modelSwitchMu.Unlock()
			m.noteModelSwitchStartFailure(id, "unrelated message", tc.err)
			m.modelSwitchMu.Lock()
			still := m.modelSwitches[id] == p
			m.modelSwitchMu.Unlock()
			if !still {
				t.Fatal("a message without the token settled the switch")
			}
			if errors.Is(tc.err, ErrQuiescing) {
				m.SetQuiescing(true)
			}
			m.noteModelSwitchStartFailure(id, "x "+token, tc.err)
			if got := modelSwitchMarkerExists(t, m, id); got != tc.keepMarker {
				t.Errorf("marker exists = %v", got)
			}
			deadline := time.Now().Add(5 * time.Second)
			for currentModel(t, m, id) != tc.wantModel && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if got := currentModel(t, m, id); got != tc.wantModel {
				t.Errorf("model = %q", got)
			}
			close(b.release)
			drainEvents(events)
			waitModelSwitchIdle(t, m, id)
		})
	}
}

func TestConsumeModelSwitchWakes_RunsOwedRollback(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	if _, err := m.Update(id, modelSwitchUpdate(
		ModelSwitchTarget{Tool: ToolClaude, Model: "sonnet"},
		ModelSwitchTarget{Tool: ToolClaude, Model: "opus"})); err != nil {
		t.Fatal(err)
	}
	m.deferModelSwitchRollback(&pendingModelSwitch{
		agentID: id,
		from:    ModelSwitchTarget{Tool: ToolClaude, Model: "sonnet"},
		to:      ModelSwitchTarget{Tool: ToolClaude, Model: "opus"},
	}, "boom")
	m.ConsumeModelSwitchWakes(time.Now().Add(time.Minute))
	got := b.nextTurn(t)
	if got.model != "sonnet" || !strings.Contains(got.msg, "boom") {
		t.Errorf("revert turn = %+v", got)
	}
	waitModelSwitchIdle(t, m, id)
	if modelSwitchMarkerExists(t, m, id) {
		t.Error("marker kept after the revert")
	}
}

// A delayed revert of an older switch must not replace a newer switch's marker.
func TestDeferModelSwitchRollback_KeepsNewerMarker(t *testing.T) {
	m, _, id := newModelSwitchManager(t)
	sonnet := ModelSwitchTarget{Tool: ToolClaude, Model: "sonnet"}
	opus := ModelSwitchTarget{Tool: ToolClaude, Model: "opus"}
	haiku := ModelSwitchTarget{Tool: ToolClaude, Model: "haiku"}
	if _, err := m.Update(id, modelSwitchUpdate(sonnet, opus)); err != nil {
		t.Fatal(err)
	}
	m.SetQuiescing(true)
	defer m.SetQuiescing(false)
	m.putModelSwitchMarker(&pendingModelSwitch{agentID: id, from: haiku, to: opus, failure: "newer"})
	m.deferModelSwitchRollback(&pendingModelSwitch{agentID: id, from: sonnet, to: haiku}, "older")
	rec, err := m.Store().GetKV(context.Background(), modelSwitchKVNamespace, id)
	if err != nil {
		t.Fatal(err)
	}
	var got modelSwitchMarker
	if err := json.Unmarshal([]byte(rec.Value), &got); err != nil {
		t.Fatal(err)
	}
	if got.Failure != "newer" || got.To != opus {
		t.Errorf("marker = %+v", got)
	}
	m.deleteModelSwitchMarker(id)
}

func TestCloseKeyedSessionIfIdle_KeepsWorkingProcess(t *testing.T) {
	const agentID, key = "ag_k", "slack:t"
	for _, tc := range []struct {
		name    string
		state   sessionState
		pending int
	}{
		{"running a turn", sessIdle + 1, 0},
		{"background tasks", sessIdle, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &ClaudeBackend{sessions: map[string]*claudeSession{}}
			s := &claudeSession{state: tc.state, pendingTasks: tc.pending, keyed: true}
			b.sessions[keyedPoolKey(agentID, key)] = s
			if b.CloseKeyedSessionIfIdle(agentID, key, "x") {
				t.Error("reported the key as free")
			}
			if s.closing {
				t.Error("marked a working process closing")
			}
		})
	}
	if !(&ClaudeBackend{sessions: map[string]*claudeSession{}}).CloseKeyedSessionIfIdle(agentID, key, "x") {
		t.Error("no process should mean free")
	}
}

// A backend refusing a bound turn must not count as a failure of the model:
// the marker comes back and the switch is released without a revert.
func TestUnbindModelSwitchTurn_BusyDoesNotRevert(t *testing.T) {
	m, _, id := newModelSwitchManager(t)
	to := modelSwitchTargetOf(mustAgent(t, m, id))
	p := &pendingModelSwitch{agentID: id, token: "abc", bound: true, boundTurnID: 7,
		from: ModelSwitchTarget{Tool: ToolClaude, Model: "haiku"}, to: to}
	m.modelSwitches = map[string]*pendingModelSwitch{id: p}
	msg := modelSwitchTokenText("abc")
	m.unbindModelSwitchTurn(id, msg)
	if p.bound || !modelSwitchMarkerExists(t, m, id) {
		t.Fatalf("bound=%v, marker restored=%v", p.bound, modelSwitchMarkerExists(t, m, id))
	}
	m.noteModelSwitchStartFailure(id, msg, ErrAgentBusy)
	if _, pending := m.modelSwitches[id]; pending {
		t.Error("switch left pending")
	}
	time.Sleep(50 * time.Millisecond)
	if got := modelSwitchTargetOf(mustAgent(t, m, id)); got != to {
		t.Errorf("reverted to %v", got)
	}
}

func mustAgent(t *testing.T, m *Manager, id string) *Agent {
	t.Helper()
	a, ok := m.Get(id)
	if !ok {
		t.Fatal("agent missing")
	}
	return a
}

// An aborted restart must pick up what it deferred.
func TestQuiesceLiftResumesDeferredWake(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	to := modelSwitchTargetOf(mustAgent(t, m, id))
	m.SetQuiescing(true)
	m.putModelSwitchMarker(&pendingModelSwitch{agentID: id,
		from: ModelSwitchTarget{Tool: ToolClaude, Model: "haiku"}, to: to})
	time.Sleep(5 * time.Millisecond)
	m.SetQuiescing(false)
	if got := b.nextTurn(t); got.model != to.Model {
		t.Errorf("model = %q", got.model)
	}
	waitModelSwitchIdle(t, m, id)
}

// A listing taken before a wake ran must not start that wake again.
func TestRestoreModelSwitch_IgnoresConsumedRecord(t *testing.T) {
	m, _, id := newModelSwitchManager(t)
	to := modelSwitchTargetOf(mustAgent(t, m, id))
	p := &pendingModelSwitch{agentID: id, to: to, from: ModelSwitchTarget{Tool: ToolClaude, Model: "haiku"}}
	m.putModelSwitchMarker(p)
	rec, err := m.Store().GetKV(context.Background(), modelSwitchKVNamespace, id)
	if err != nil {
		t.Fatal(err)
	}
	m.deleteModelSwitchMarker(id)
	if got := m.restoreModelSwitch(context.Background(), rec); got != nil {
		t.Error("restored a consumed marker")
	}
}

// A wake deferred just after the quiesce was lifted must still run.
func TestDeferredWakeAfterQuiesceLift(t *testing.T) {
	m, b, id := newModelSwitchManager(t)
	to := modelSwitchTargetOf(mustAgent(t, m, id))
	p := &pendingModelSwitch{agentID: id, to: to, token: "abc", sourceEnded: true,
		from: ModelSwitchTarget{Tool: ToolClaude, Model: "haiku"}}
	m.modelSwitches = map[string]*pendingModelSwitch{id: p}
	m.noteModelSwitchStartFailure(id, modelSwitchTokenText("abc"), ErrQuiescing)
	if got := b.nextTurn(t); got.model != to.Model {
		t.Errorf("model = %q", got.model)
	}
	waitModelSwitchIdle(t, m, id)
}

// Switching back inside a thread turn must release the waiting goroutine.
func TestSwitchBackReleasesKeyedWaiter(t *testing.T) {
	m, _, id := newModelSwitchManager(t)
	cur := modelSwitchTargetOf(mustAgent(t, m, id))
	if _, err := m.Update(id, modelSwitchUpdate(cur, ModelSwitchTarget{Tool: cur.Tool, Model: "opus"})); err != nil {
		t.Fatal(err)
	}
	m.busyMu.Lock()
	osID := m.trackOneShotCause(id, func(error) {}, "slack:t", "", "")
	m.busyMu.Unlock()
	defer m.untrackOneShot(id, osID)
	p := &pendingModelSwitch{agentID: id, sessionKey: "slack:t", oneShotID: osID,
		from: cur, to: ModelSwitchTarget{Tool: cur.Tool, Model: "opus"}, sourceDone: make(chan struct{})}
	m.modelSwitches = map[string]*pendingModelSwitch{id: p}
	if _, err := m.SwitchModel(id, ModelSwitchRequest{Model: cur.Model, SessionKey: "slack:t"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.sourceDone:
	case <-time.After(time.Second):
		t.Error("waiter not released")
	}
}

// The expiry removes only the marker: a turn still queued is verified.
func TestExpireModelSwitchMarker_KeepsPending(t *testing.T) {
	m, _, id := newModelSwitchManager(t)
	p := &pendingModelSwitch{
		agentID: id, token: "abc", phase: modelSwitchVerifying,
		to: ModelSwitchTarget{Tool: ToolClaude, Model: "opus"},
	}
	m.modelSwitches = map[string]*pendingModelSwitch{id: p}
	m.putModelSwitchMarker(p)
	m.expireModelSwitchMarker(p)
	if modelSwitchMarkerExists(t, m, id) {
		t.Error("marker not expired")
	}
	if m.modelSwitches[id] != p {
		t.Fatal("expiry dropped the pending switch")
	}
	m.bindModelSwitchTurn(id, "", 0, modelSwitchTokenText("abc"))
	if !p.bound {
		t.Error("queued turn no longer binds")
	}
}

// A restored wake refused by another restart must survive to the next boot.
func TestRestoredWakeSurvivesAnotherRestart(t *testing.T) {
	m, _, id := newModelSwitchManager(t) // current model: sonnet
	to := ModelSwitchTarget{Tool: ToolClaude, Model: "sonnet"}
	if a, _ := m.Get(id); modelSwitchTargetOf(a) != to {
		to = modelSwitchTargetOf(a)
	}
	val, _ := json.Marshal(modelSwitchMarker{AgentID: id, From: ModelSwitchTarget{Tool: ToolClaude, Model: "haiku"}, To: to})
	rec, err := m.Store().PutKV(context.Background(), &store.KVRecord{
		Namespace: modelSwitchKVNamespace, Key: id, Value: string(val),
		Type: store.KVTypeString, Scope: store.KVScopeMachine,
	}, store.KVPutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := m.restoreModelSwitch(context.Background(), rec)
	if p == nil {
		t.Fatal("marker not restored")
	}
	if !modelSwitchMarkerExists(t, m, id) {
		t.Fatal("restore consumed the marker before the turn started")
	}
	m.SetQuiescing(true)
	m.noteModelSwitchStartFailure(id, modelSwitchTokenText(p.token), ErrQuiescing)
	if !modelSwitchMarkerExists(t, m, id) {
		t.Error("marker lost on the second restart")
	}
	if _, pending := m.modelSwitches[id]; pending {
		t.Error("deferred wake still pending in memory")
	}
}

func TestSwitchModel_RollbackDoesNotOverwriteNewerConfig(t *testing.T) {
	m, _, id := newModelSwitchManager(t)
	stale := modelSwitchUpdate(ModelSwitchTarget{Tool: ToolClaude, Model: "opus"}, ModelSwitchTarget{Tool: ToolClaude, Model: "haiku"})
	if _, err := m.Update(id, stale); !errors.Is(err, errModelSwitchSuperseded) {
		t.Fatalf("err = %v", err)
	}
	if got := currentModel(t, m, id); got != "sonnet" {
		t.Errorf("model = %q", got)
	}
}

func TestSyncModelSwitchSkill_FollowsBackend(t *testing.T) {
	dir := t.TempDir()
	logger := slog.Default()
	path := func(root string) string {
		return filepath.Join(dir, root, "skills", modelSwitchSkillDirName, "SKILL.md")
	}
	syncModelSwitchSkillAt(dir, extensionSkillRoot(ToolClaude), modelSwitchSkillBodyPOSIX, logger)
	if _, err := os.Stat(path(".claude")); err != nil {
		t.Fatal(err)
	}
	syncModelSwitchSkillAt(dir, extensionSkillRoot(ToolCodex), modelSwitchSkillBodyPOSIX, logger)
	if _, err := os.Stat(path(".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale copy kept: %v", err)
	}
	if _, err := os.Stat(path(".codex")); err != nil {
		t.Error(err)
	}
	syncModelSwitchSkillAt(dir, extensionSkillRoot(ToolCustomBare), modelSwitchSkillBodyPOSIX, logger)
	if _, err := os.Stat(path(".codex")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("skill kept for a backend without skills: %v", err)
	}
}
