package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// PreserveNativeGoalsOnShutdown distinguishes daemon shutdown from user stop.
// It must run before cancellation of any active response surface.
func (m *Manager) PreserveNativeGoalsOnShutdown() { m.goalShutdown.Store(true) }

// RecoverableGoals only enumerates already-authorized active bindings. It does
// not read commands out of checkpoints or infer goals from ordinary chats.
func (m *Manager) RecoverableGoals() map[string][]GoalBinding {
	out := map[string][]GoalBinding{}
	for _, a := range m.List() {
		if a.Archived || a.Tool != ToolCodex {
			continue
		}
		entries, err := os.ReadDir(codexThreadRefDir(a.ID))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !validCodexThreadRefName(entry.Name()) {
				continue
			}
			ref, err := readCodexThreadRefFile(filepath.Join(codexThreadRefDir(a.ID), entry.Name()))
			if err != nil || ref.Goal == nil || ref.Goal.DesiredPaused || ref.Goal.Handoff.Pending() || ref.Goal.State == nil || ref.Goal.State.Status != "active" {
				continue
			}
			if codexThreadRefName(ref.Goal.SessionKey) != entry.Name() {
				continue
			}
			if _, running := codexGoalRuntimes.Load(codexThreadRefPath(a.ID, ref.Goal.SessionKey)); running {
				continue
			}
			out[a.ID] = append(out[a.ID], *ref.Goal)
		}
	}
	return out
}

// maxGoalHandoffResumeAttempts bounds destination-side re-dispatch of a
// handoff resume whose asynchronous surface delivery never reached admission.
const maxGoalHandoffResumeAttempts = 3

// StalledGoalHandoffResumes enumerates accepted handoffs on this destination
// (target == local peer) whose resume has not completed for at least minAge
// since the last dispatch and that have no live runtime. A resuming binding
// with ActivationPending survived a crash after admission but before the
// native activation ACK; ClaimGoalHandoffResume safely rearms it before the
// caller re-dispatches `!goal resume-if`.
func (m *Manager) StalledGoalHandoffResumes(target string, minAge time.Duration) map[string][]GoalBinding {
	out := map[string][]GoalBinding{}
	if target == "" {
		return out
	}
	cutoff := time.Now().Add(-minAge).UnixMilli()
	for _, a := range m.List() {
		if a.Archived || a.Tool != ToolCodex {
			continue
		}
		entries, err := os.ReadDir(codexThreadRefDir(a.ID))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !validCodexThreadRefName(entry.Name()) {
				continue
			}
			ref, err := readCodexThreadRefFile(filepath.Join(codexThreadRefDir(a.ID), entry.Name()))
			if err != nil || ref.Goal == nil || ref.Goal.State == nil || ref.Goal.State.Status != "paused" {
				continue
			}
			h := ref.Goal.Handoff
			stalled := h != nil && (h.Phase == "resume_pending" || h.Phase == "resuming" && ref.Goal.ActivationPending)
			if !stalled || h.TargetPeerID != target || h.AcceptedAt > cutoff {
				continue
			}
			if codexThreadRefName(ref.Goal.SessionKey) != entry.Name() {
				continue
			}
			if _, running := codexGoalRuntimes.Load(codexThreadRefPath(a.ID, ref.Goal.SessionKey)); running {
				continue
			}
			out[a.ID] = append(out[a.ID], *ref.Goal)
		}
	}
	return out
}

// ClaimGoalHandoffResume consumes one automatic resume attempt for the exact
// handoff identity. Once the bound is exhausted the handoff fails closed so the
// operator sees the stall instead of a silently parked goal; explicit
// `!goal pause` + `!goal resume` remains available.
// The returned binding is the post-claim snapshot and must be used for the
// fenced resume request: rearming a crashed `resuming` activation preserves its
// generation, but callers must not dispatch an older enumeration snapshot.
func (m *Manager) ClaimGoalHandoffResume(id, key, op string) (*GoalBinding, bool) {
	unlock := goalAdmissions.Lock(codexThreadRefPath(id, key))
	defer unlock()
	if _, running := codexGoalRuntimes.Load(codexThreadRefPath(id, key)); running {
		return nil, false
	}
	claimed := false
	var snapshot *GoalBinding
	err := updateGoalBinding(id, key, func(b *GoalBinding) {
		h := b.Handoff
		if h == nil || h.ID != op || b.State == nil || b.State.Status != "paused" {
			return
		}
		if h.Phase == "resuming" && b.ActivationPending {
			// The prior runner persisted admission but vanished before its ACK.
			// Re-arm the same identity. The backend reads native state first and
			// normalizes either an active or paused native goal before resuming.
			h.Phase = "resume_pending"
			b.DesiredPaused = true
			b.ActivationPending = false
			b.RecoveryPending = false
		}
		if h.Phase != "resume_pending" {
			return
		}
		if h.ResumeAttempts >= maxGoalHandoffResumeAttempts {
			h.Phase = "failed"
			h.Error = "automatic resume was not admitted after device transfer; use !goal pause then !goal resume"
			b.DesiredPaused = true
			b.RecoveryPending = false
			b.Generation++
			return
		}
		h.ResumeAttempts++
		h.AcceptedAt = time.Now().UnixMilli()
		claimed = true
		copyBinding := *b
		copyHandoff := *b.Handoff
		copyBinding.Handoff = &copyHandoff
		copyState := *b.State
		copyBinding.State = &copyState
		snapshot = &copyBinding
	})
	if err != nil || !claimed {
		return nil, false
	}
	return snapshot, snapshot != nil
}

// ResolvedGoalHandoffs enumerates destination handoffs whose exact resume
// decision is durably known. `resuming` is included because backend admission
// is persisted before the native activation RPC; it is enough to resolve the
// transport uncertainty even if the native ACK has not arrived yet.
func (m *Manager) ResolvedGoalHandoffs(target string) map[string][]GoalBinding {
	out := map[string][]GoalBinding{}
	if target == "" {
		return out
	}
	for _, a := range m.List() {
		if a.Archived || a.Tool != ToolCodex {
			continue
		}
		entries, err := os.ReadDir(codexThreadRefDir(a.ID))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !validCodexThreadRefName(entry.Name()) {
				continue
			}
			ref, err := readCodexThreadRefFile(filepath.Join(codexThreadRefDir(a.ID), entry.Name()))
			if err != nil || ref.Goal == nil || codexThreadRefName(ref.Goal.SessionKey) != entry.Name() {
				continue
			}
			h := ref.Goal.Handoff
			if h == nil || h.TargetPeerID != target {
				continue
			}
			switch h.Phase {
			case "resuming", "resumed", "failed", "cancelled":
				out[a.ID] = append(out[a.ID], *ref.Goal)
			}
		}
	}
	return out
}

func (m *Manager) SetGoalRecoveryPaused(id, key string) error {
	b, err := goalBindingFor(id, key)
	if err != nil {
		return err
	}
	if b == nil {
		return errors.New("goal binding missing")
	}
	return updateGoalBinding(id, key, func(b *GoalBinding) { b.DesiredPaused = true })
}

// Explicit transport metadata for trusted arrival paths, not parsed prose.
type goalRequestContextKey struct{}

func (m *Manager) SetNativeGoalLifecycle(ctx context.Context) { m.goalLifecycle.Store(&ctx) }
func (m *Manager) NativeGoalsShuttingDown() bool {
	ctx := m.goalLifecycle.Load()
	return m.goalShutdown.Load() || (ctx != nil && (*ctx).Err() != nil)
}

func (m *Manager) FenceGoalRun(id, key, runID, origin string) error {
	if runID == "" {
		return errors.New("goal run id required")
	}
	if raw, ok := codexGoalRuntimes.Load(codexThreadRefPath(id, key)); ok {
		r := raw.(*codexGoalRuntime)
		r.mu.Lock()
		if r.runID != runID || r.origin != origin {
			r.mu.Unlock()
			return errors.New("goal run changed or origin mismatch")
		}
		r.stopRequested = true
		r.mu.Unlock()
	}
	b, err := goalBindingFor(id, key)
	if err != nil {
		return err
	}
	if b == nil {
		return nil
	} // setup checks the runtime fence before activation
	if b.RunID != runID || b.OriginPeerID != origin {
		return errors.New("goal run changed or origin mismatch")
	}
	return updateGoalBinding(id, key, func(b *GoalBinding) {
		if b.RunID == runID {
			b.DesiredPaused = true
			cancelGoalHandoff(b, "goal explicitly stopped")
			b.RecoveryPending = false
			b.Generation++
		}
	})
}

func (m *Manager) ClaimGoalRecovery(id, key string, generation int64) bool {
	claimed := false
	err := updateGoalBinding(id, key, func(b *GoalBinding) {
		if b.Generation != generation || b.DesiredPaused {
			return
		}
		if b.RecoveryAttempts >= 3 {
			b.DesiredPaused = true
			cancelGoalHandoff(b, "goal explicitly stopped")
			b.RecoveryPending = false
			return
		}
		b.RecoveryAttempts++
		b.RecoveryPending = true
		claimed = true
	})
	return claimed && err == nil
}

func goalArrivalContext(ctx context.Context, id string) context.Context {
	b, err := goalBindingFor(id, "")
	if err != nil || b == nil || b.DesiredPaused || b.State == nil || b.State.Status != "active" {
		return ctx
	}
	generation := b.Generation
	return context.WithValue(ctx, goalRequestContextKey{}, &GoalRequest{Action: "resume", ExpectedThreadID: b.State.ThreadID, ExpectedGeneration: &generation})
}
