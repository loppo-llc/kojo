package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestDrainBackgroundChatOutcome(t *testing.T) {
	tests := []struct {
		name   string
		events []ChatEvent
		ctxErr string
		want   string
		detail string
	}{
		{"success", []ChatEvent{{Type: "text", Delta: "done"}, {Type: "done"}}, "", "completed", ""},
		{"done with failure", []ChatEvent{{Type: "done", ErrorMessage: "capacity", ErrorCode: "serverOverloaded"}}, "", "failed", "serverOverloaded"},
		{"error before clean done", []ChatEvent{{Type: "error", ErrorMessage: "broken"}, {Type: "done"}}, "", "failed", "broken"},
		{"bare error", []ChatEvent{{Type: "error"}}, "", "failed", "without details"},
		{"missing terminal", []ChatEvent{{Type: "text", Delta: "partial"}}, "", "failed", "without a terminal result"},
		{"empty stream", nil, "", "failed", "without a terminal result"},
		{"timeout event", []ChatEvent{{Type: "done", ErrorMessage: ErrMsgTimeout}}, "", "timed out", ""},
		{"cancel event", []ChatEvent{{Type: "done", ErrorMessage: ErrMsgCancelled}}, "", "cancelled", ""},
		{"interrupted event", []ChatEvent{{Type: "done", ErrorMessage: "codex turn interrupted"}}, "", "cancelled", ""},
		{"timeout context", nil, "timeout", "timed out", ""},
		{"cancel context", nil, "cancel", "cancelled", ""},
	}
	for _, label := range []string{"cron job", "manual checkin", "wake chat"} {
		for _, tt := range tests {
			t.Run(label+"/"+tt.name, func(t *testing.T) {
				ctx := context.Background()
				if tt.ctxErr == "timeout" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer cancel()
				} else if tt.ctxErr == "cancel" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				events := make(chan ChatEvent)
				producerDone := make(chan struct{})
				go func() {
					defer close(producerDone)
					defer close(events)
					for _, e := range tt.events {
						events <- e
					}
				}()
				var log bytes.Buffer
				drainBackgroundChat(ctx, events, slog.New(slog.NewTextHandler(&log, nil)), label, "ag_test", time.Minute)
				<-producerDone
				if !strings.Contains(log.String(), label+" "+tt.want) || !strings.Contains(log.String(), tt.detail) {
					t.Fatalf("log = %s", log.String())
				}
				if tt.want != "completed" && strings.Contains(log.String(), label+" completed") {
					t.Fatalf("false success: %s", log.String())
				}
			})
		}
	}
}

func TestProcessChatEvents_OverloadRetainsErrorCode(t *testing.T) {
	m := newTestManager(t)
	a := seedPreviewTestAgent(t, m, "ag_overload")
	backendCh := make(chan ChatEvent, 1)
	backendCh <- ChatEvent{Type: "done", ErrorMessage: "model overloaded", ErrorCode: "serverOverloaded"}
	close(backendCh)
	outCh := make(chan ChatEvent, 8)
	m.processChatEvents(context.Background(), a.ID, backendCh, outCh)
	close(outCh)
	var log bytes.Buffer
	drainBackgroundChat(context.Background(), outCh, slog.New(slog.NewTextHandler(&log, nil)), "cron job", a.ID, time.Minute)
	if !strings.Contains(log.String(), "cron job failed") || !strings.Contains(log.String(), "serverOverloaded") {
		t.Fatalf("log = %s", log.String())
	}
	msgs, err := loadMessages(a.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range msgs {
		if strings.Contains(msg.Content, "model overloaded") {
			found = true
		}
	}
	if !found {
		t.Fatal("error missing from persisted transcript")
	}
}

type checkinOptionsBackend struct{ opts chan ChatOptions }

func (b *checkinOptionsBackend) Name() string    { return "checkin-test" }
func (b *checkinOptionsBackend) Available() bool { return true }
func (b *checkinOptionsBackend) Chat(ctx context.Context, _ *Agent, _, _ string, opts ChatOptions) (<-chan ChatEvent, error) {
	b.opts <- opts
	out := make(chan ChatEvent)
	go func() {
		defer close(out)
		<-ctx.Done()
	}()
	return out, nil
}

func TestCheckinManagerRetryScopeAndAbort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		role   string
		source BusySource
		goal   bool
		retry  bool
	}{
		{"checkin", "system", BusySourceCron, false, true},
		{"human", "user", BusySourceUser, false, false},
		{"notification", "system", BusySourceNotification, false, false},
		{"native goal", "system", BusySourceCron, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			a := seedPreviewTestAgent(t, m, "ag_scope")
			a.Tool = "checkin-test"
			a.Effort = "medium"
			b := &checkinOptionsBackend{opts: make(chan ChatOptions, 1)}
			m.backends[a.Tool] = b
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if tc.goal {
				ctx = context.WithValue(ctx, goalRequestContextKey{}, &GoalRequest{Action: "resume"})
			}
			events, err := m.Chat(ctx, a.ID, "test", tc.role, nil, tc.source)
			if err != nil {
				t.Fatal(err)
			}
			opts := <-b.opts
			if opts.RetryOverload != tc.retry {
				t.Fatalf("RetryOverload = %v, want %v", opts.RetryOverload, tc.retry)
			}
			m.busyMu.Lock()
			_, busy := m.busy[a.ID]
			m.busyMu.Unlock()
			if !busy {
				t.Fatal("busy lock not held during backend wait")
			}
			m.Abort(a.ID, nil)
			var log bytes.Buffer
			drainBackgroundChat(ctx, events, slog.New(slog.NewTextHandler(&log, nil)), "cron job", a.ID, time.Minute)
			if tc.source == BusySourceCron && !strings.Contains(log.String(), "cron job cancelled") {
				t.Fatalf("Abort lost child-context cancellation: %s", log.String())
			}
			if ctx.Err() != nil {
				t.Fatalf("test relied on parent cancellation: %v", ctx.Err())
			}
		})
	}
}
