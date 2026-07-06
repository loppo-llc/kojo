package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestSteerer(t *testing.T) (*codexSteerer, *[]string) {
	t.Helper()
	var sent []string
	s := newCodexSteerer("thread-1", func(method string, params any) (int64, error) {
		data, _ := json.Marshal(map[string]any{"method": method, "params": params})
		sent = append(sent, string(data))
		return 42, nil
	})
	return s, &sent
}

// steerAsync runs steer in a goroutine and returns its result channel.
func steerAsync(s *codexSteerer, text string) chan error {
	done := make(chan error, 1)
	go func() { done <- s.steer(text) }()
	return done
}

// waitPending blocks until the steerer has an in-flight request for id.
func waitPending(t *testing.T, s *codexSteerer, id int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, ok := s.pending[id]
		s.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("steer request never became pending")
}

func TestCodexSteerer_SteerSuccess(t *testing.T) {
	s, sent := newTestSteerer(t)
	s.setTurnID("turn-9")

	done := steerAsync(s, "more input")
	waitPending(t, s, 42)
	if !s.resolve(42, nil) {
		t.Fatal("resolve should claim the steer request id")
	}
	if err := <-done; err != nil {
		t.Fatalf("steer: %v", err)
	}
	if len(*sent) != 1 {
		t.Fatalf("expected 1 RPC, got %d", len(*sent))
	}
	line := (*sent)[0]
	for _, want := range []string{"turn/steer", "turn-9", "thread-1", "more input"} {
		if !strings.Contains(line, want) {
			t.Errorf("steer RPC missing %q: %s", want, line)
		}
	}
	if s.resolve(42, nil) {
		t.Error("resolve should consume the id")
	}
}

func TestCodexSteerer_SteerRejected(t *testing.T) {
	s, _ := newTestSteerer(t)
	s.setTurnID("turn-9")

	done := steerAsync(s, "late input")
	waitPending(t, s, 42)
	s.resolve(42, &rpcError{Message: "turn mismatch"})
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "turn mismatch") {
		t.Fatalf("expected rejection error, got %v", err)
	}
	if !errors.Is(err, ErrAgentNotBusy) {
		t.Fatalf("rejection should wrap ErrAgentNotBusy, got %v", err)
	}
}

func TestCodexSteerer_SteerBlocksUntilTurnID(t *testing.T) {
	s, sent := newTestSteerer(t)

	done := steerAsync(s, "queued")
	select {
	case err := <-done:
		t.Fatalf("steer returned before turn id was known: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	s.setTurnID("turn-1")
	waitPending(t, s, 42)
	s.resolve(42, nil)
	if err := <-done; err != nil {
		t.Fatalf("steer: %v", err)
	}
	if len(*sent) != 1 {
		t.Fatalf("expected 1 RPC, got %d", len(*sent))
	}
}

func TestCodexSteerer_SteerAfterClose(t *testing.T) {
	s, sent := newTestSteerer(t)
	s.setTurnID("turn-1")
	s.close()

	if err := s.steer("late"); !errors.Is(err, ErrAgentNotBusy) {
		t.Fatalf("expected ErrAgentNotBusy, got %v", err)
	}
	if len(*sent) != 0 {
		t.Fatalf("no RPC should be sent after close, got %d", len(*sent))
	}
}

func TestCodexSteerer_CloseWithoutTurnIDUnblocks(t *testing.T) {
	s, _ := newTestSteerer(t)

	done := steerAsync(s, "never")
	s.close()

	if err := <-done; !errors.Is(err, ErrAgentNotBusy) {
		t.Fatalf("expected ErrAgentNotBusy, got %v", err)
	}
}

func TestCodexSteerer_CloseFailsPendingWaiter(t *testing.T) {
	s, _ := newTestSteerer(t)
	s.setTurnID("turn-1")

	done := steerAsync(s, "in flight")
	waitPending(t, s, 42)
	s.close()

	err := <-done
	if !errors.Is(err, ErrAgentNotBusy) {
		t.Fatalf("expected ErrAgentNotBusy-wrapped error, got %v", err)
	}
}

func TestCodexSteerer_WriteErrorPropagates(t *testing.T) {
	s := newCodexSteerer("thread-1", func(method string, params any) (int64, error) {
		return 7, errors.New("pipe broken")
	})
	s.setTurnID("turn-1")
	if err := s.steer("x"); err == nil || !strings.Contains(err.Error(), "pipe broken") {
		t.Fatalf("expected write error, got %v", err)
	}
	if s.resolve(7, nil) {
		t.Error("failed steer must not track its request id")
	}
}

func TestParseCodexStream_CapturesTurnIDFromResponse(t *testing.T) {
	s, _ := newTestSteerer(t)
	lines := []string{
		`{"id":3,"result":{"turn":{"id":"turn-abc","status":"inProgress"}}}`,
		`{"method":"turn/completed","params":{"threadId":"t","turn":{"id":"turn-abc","status":"completed"}}}`,
	}
	scanner := newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	result := parseCodexStream(scanner, 3, s, testLogger(), func(ChatEvent) bool { return true })
	if !result.turnCompleted {
		t.Fatal("turn should complete")
	}
	s.mu.Lock()
	got := s.turnID
	s.mu.Unlock()
	if got != "turn-abc" {
		t.Fatalf("turn id not captured, got %q", got)
	}
}

func TestParseCodexStream_CapturesTurnIDFromTurnStarted(t *testing.T) {
	s, _ := newTestSteerer(t)
	lines := []string{
		`{"method":"turn/started","params":{"threadId":"t","turn":{"id":"turn-def","status":"inProgress"}}}`,
		`{"method":"turn/completed","params":{"threadId":"t","turn":{"id":"turn-def","status":"completed"}}}`,
	}
	scanner := newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	parseCodexStream(scanner, 3, s, testLogger(), func(ChatEvent) bool { return true })
	s.mu.Lock()
	got := s.turnID
	s.mu.Unlock()
	if got != "turn-def" {
		t.Fatalf("turn id not captured from turn/started, got %q", got)
	}
}

func TestDecodeCodexTurnID_BothShapes(t *testing.T) {
	nested := json.RawMessage(`{"turn":{"id":"turn-1"}}`)
	if got := decodeCodexTurnID(&nested); got != "turn-1" {
		t.Errorf("nested shape: got %q", got)
	}
	flat := json.RawMessage(`{"turnId":"turn-2"}`)
	if got := decodeCodexTurnID(&flat); got != "turn-2" {
		t.Errorf("flat shape: got %q", got)
	}
	if got := decodeCodexTurnID(nil); got != "" {
		t.Errorf("nil result: got %q", got)
	}
}

func TestParseCodexStream_SteerErrorResponseNotTurnError(t *testing.T) {
	// A rejected turn/steer response must fail the steer call, not the turn.
	s, _ := newTestSteerer(t)
	s.setTurnID("turn-1")

	done := steerAsync(s, "x")
	waitPending(t, s, 42)

	lines := []string{
		`{"id":42,"error":{"code":-32600,"message":"turn mismatch"}}`,
		`{"method":"turn/completed","params":{"threadId":"t","turn":{"id":"turn-1","status":"completed"}}}`,
	}
	scanner := newCodexLineScanner(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	var errEvents int
	result := parseCodexStream(scanner, 3, s, testLogger(), func(e ChatEvent) bool {
		if e.Type == "error" {
			errEvents++
		}
		return true
	})
	if errEvents != 0 {
		t.Fatalf("steer rejection leaked %d error events", errEvents)
	}
	if !result.turnCompleted {
		t.Fatal("turn should still complete")
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "turn mismatch") {
		t.Fatalf("steer should surface the rejection, got %v", err)
	}
}
