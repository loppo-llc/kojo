package agent

import (
	"log/slog"
	"testing"
	"time"
)

func stalledHandoffFixture(t *testing.T) (string, *Manager) {
	t.Helper()
	id, _ := setupCodexTransferTest(t)
	binding := &GoalBinding{
		Handoff:       &GoalHandoff{ID: handoffTestOp, SourcePeerID: "source", TargetPeerID: "target", Phase: "resume_pending", AcceptedAt: time.Now().Add(-10 * time.Minute).UnixMilli()},
		DesiredPaused: true,
		Generation:    4,
		State:         &CodexGoal{ThreadID: handoffTestID, Status: "paused", Objective: "test"},
	}
	writeCodexThreadRef(id, "", codexThreadRef{ThreadID: handoffTestID, Goal: binding}, slog.Default())
	m := &Manager{agents: map[string]*Agent{id: {ID: id, Tool: ToolCodex}}}
	return id, m
}

func TestStalledGoalHandoffResumesFiltersIdentityAgeAndRuntime(t *testing.T) {
	id, m := stalledHandoffFixture(t)
	if got := m.StalledGoalHandoffResumes("target", time.Minute); len(got[id]) != 1 || got[id][0].Handoff.ID != handoffTestOp {
		t.Fatalf("stalled resume not enumerated: %+v", got)
	}
	if got := m.StalledGoalHandoffResumes("other", time.Minute); len(got) != 0 {
		t.Fatal("foreign target enumerated")
	}
	if got := m.StalledGoalHandoffResumes("", time.Minute); len(got) != 0 {
		t.Fatal("empty target enumerated")
	}
	if got := m.StalledGoalHandoffResumes("target", time.Hour); len(got) != 0 {
		t.Fatal("recent dispatch enumerated before grace elapsed")
	}
	r := &codexGoalRuntime{agentID: id, threadID: handoffTestID}
	codexGoalRuntimes.Store(codexThreadRefPath(id, ""), r)
	if got := m.StalledGoalHandoffResumes("target", time.Minute); len(got) != 0 {
		codexGoalRuntimes.Delete(codexThreadRefPath(id, ""))
		t.Fatal("live runtime enumerated")
	}
	codexGoalRuntimes.Delete(codexThreadRefPath(id, ""))
	for _, phase := range []string{"resuming", "resumed", "failed", "cancelled", "ready"} {
		_ = updateGoalBinding(id, "", func(b *GoalBinding) { b.Handoff.Phase = phase })
		if got := m.StalledGoalHandoffResumes("target", time.Minute); len(got) != 0 {
			t.Fatalf("phase %s enumerated", phase)
		}
	}
}

func TestClaimGoalHandoffResumeIsBoundedAndFailsClosed(t *testing.T) {
	id, m := stalledHandoffFixture(t)
	if _, ok := m.ClaimGoalHandoffResume(id, "", "wrong-op"); ok {
		t.Fatal("foreign identity claimed")
	}
	for i := 1; i <= maxGoalHandoffResumeAttempts; i++ {
		_ = updateGoalBinding(id, "", func(b *GoalBinding) { b.Handoff.AcceptedAt = 1 })
		if _, ok := m.ClaimGoalHandoffResume(id, "", handoffTestOp); !ok {
			t.Fatalf("attempt %d refused", i)
		}
		b, _ := goalBindingFor(id, "")
		if b.Handoff.ResumeAttempts != i || b.Handoff.Phase != "resume_pending" || b.Handoff.AcceptedAt == 1 {
			t.Fatalf("attempt %d not recorded: %+v", i, b.Handoff)
		}
		gen := b.Generation
		ref, _ := readCodexThreadRef(id, "")
		if !goalHandoffResumeAllowed(ref, &GoalRequest{Action: "resume", ExpectedThreadID: handoffTestID, ExpectedGeneration: &gen, ExpectedHandoffID: handoffTestOp}) {
			t.Fatalf("attempt %d broke explicit resume-if admission", i)
		}
		if got := m.StalledGoalHandoffResumes("target", time.Minute); len(got) != 0 {
			t.Fatalf("attempt %d left the handoff enumerable inside the grace window", i)
		}
	}
	if _, ok := m.ClaimGoalHandoffResume(id, "", handoffTestOp); ok {
		t.Fatal("claim beyond bound")
	}
	b, _ := goalBindingFor(id, "")
	if b.Handoff.Phase != "failed" || !b.DesiredPaused || b.Generation != 5 || b.Handoff.Error == "" {
		t.Fatalf("exhausted handoff did not fail closed: %+v gen=%d", b.Handoff, b.Generation)
	}
	if _, ok := m.ClaimGoalHandoffResume(id, "", handoffTestOp); ok {
		t.Fatal("failed handoff claimed again")
	}
}

func TestClaimGoalHandoffResumeRearmsCrashedActivation(t *testing.T) {
	id, m := stalledHandoffFixture(t)
	_ = updateGoalBinding(id, "", func(b *GoalBinding) {
		b.Handoff.Phase = "resuming"
		b.ActivationPending = true
		b.DesiredPaused = false
		b.RunID = "run-1"
	})
	before, _ := goalBindingFor(id, "")
	if got := m.StalledGoalHandoffResumes("target", time.Minute); len(got[id]) != 1 {
		t.Fatalf("crashed activation not enumerated: %+v", got)
	}
	claimed, ok := m.ClaimGoalHandoffResume(id, "", handoffTestOp)
	if !ok || claimed == nil || claimed.Handoff == nil {
		t.Fatal("crashed activation not claimed")
	}
	if claimed.Handoff.Phase != "resume_pending" || claimed.ActivationPending || !claimed.DesiredPaused || claimed.RunID != "run-1" || claimed.Generation != before.Generation || claimed.Handoff.ResumeAttempts != 1 {
		t.Fatalf("rearmed binding = %+v", claimed)
	}
	gen := claimed.Generation
	ref, _ := readCodexThreadRef(id, "")
	if !goalHandoffResumeAllowed(ref, &GoalRequest{Action: "resume", ExpectedThreadID: handoffTestID, ExpectedGeneration: &gen, ExpectedHandoffID: handoffTestOp}) {
		t.Fatal("rearmed identity is not admissible")
	}
}

func TestResolvedGoalHandoffsFiltersExactDestinationAndPhase(t *testing.T) {
	id, m := stalledHandoffFixture(t)
	_ = updateGoalBinding(id, "", func(b *GoalBinding) { b.Handoff.Phase = "resuming" })
	if got := m.ResolvedGoalHandoffs("target"); len(got[id]) != 1 {
		t.Fatalf("resuming handoff not resolved: %+v", got)
	}
	if got := m.ResolvedGoalHandoffs("other"); len(got) != 0 {
		t.Fatal("foreign destination resolved")
	}
	for _, phase := range []string{"resumed", "failed", "cancelled"} {
		_ = updateGoalBinding(id, "", func(b *GoalBinding) { b.Handoff.Phase = phase })
		if got := m.ResolvedGoalHandoffs("target"); len(got[id]) != 1 {
			t.Fatalf("phase %s not resolved: %+v", phase, got)
		}
	}
	_ = updateGoalBinding(id, "", func(b *GoalBinding) { b.Handoff.Phase = "resume_pending" })
	if got := m.ResolvedGoalHandoffs("target"); len(got) != 0 {
		t.Fatal("resume_pending treated as resolved")
	}
}
