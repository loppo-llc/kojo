package agent

import (
	"fmt"
	"sync"
	"time"
)

// codexSteerTurnWait bounds how long a steer call waits for the active
// turn id to become known. The turn/start RPC response normally arrives
// within milliseconds of the request, so this only trips when the
// app-server is wedged.
const codexSteerTurnWait = 15 * time.Second

// codexSteerRespWait bounds how long a steer call waits for the
// turn/steer RPC response after the request was written. The app-server
// validates expectedTurnId and answers immediately.
const codexSteerRespWait = 10 * time.Second

// codexSteerer injects additional user input into an in-flight codex
// turn via the app-server turn/steer RPC. Unlike claude (where steering
// is a bare fire-and-forget user-message line on stdin), codex requires
// the active turn id as a precondition (expectedTurnId) — and that id is
// only known once the turn/start RPC response (or the turn/started
// notification) has been read from the stream. steer() therefore blocks
// briefly on ready until parseCodexStream captures the id, and then
// waits for the turn/steer response so a rejected steer (turn already
// over) is reported as an error instead of being silently dropped.
type codexSteerer struct {
	threadID string
	// writeRPC allocates a request id and writes a JSON-RPC request to
	// the app-server stdin. Returns the id and any write error.
	writeRPC func(method string, params any) (int64, error)

	mu        sync.Mutex
	turnID    string
	closed    bool
	readyOnce sync.Once
	ready     chan struct{}        // closed once turnID is set or the steerer is closed
	pending   map[int64]chan error // in-flight turn/steer request id -> response slot
}

func newCodexSteerer(threadID string, writeRPC func(method string, params any) (int64, error)) *codexSteerer {
	return &codexSteerer{
		threadID: threadID,
		writeRPC: writeRPC,
		ready:    make(chan struct{}),
		pending:  make(map[int64]chan error),
	}
}

// setTurnID records the active turn id and unblocks pending steer calls.
func (s *codexSteerer) setTurnID(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	if s.turnID == "" && !s.closed {
		s.turnID = id
	}
	s.mu.Unlock()
	s.readyOnce.Do(func() { close(s.ready) })
}

// close marks the turn as over: subsequent steer calls fail with
// ErrAgentNotBusy and steers still waiting for a response are failed.
// Idempotent.
func (s *codexSteerer) close() {
	s.mu.Lock()
	s.closed = true
	pending := s.pending
	s.pending = make(map[int64]chan error)
	s.mu.Unlock()
	for _, ch := range pending {
		// The turn is over, so map to ErrAgentNotBusy — the HTTP handler
		// turns that into 409 not_busy instead of a 500.
		ch <- fmt.Errorf("codex: turn ended before turn/steer was acknowledged: %w", ErrAgentNotBusy)
	}
	s.readyOnce.Do(func() { close(s.ready) })
}

// resolve delivers the RPC response for an outstanding turn/steer
// request. rpcErr is nil on success. Reports whether id belonged to a
// steer request (so the caller can stop treating the response as
// unclaimed).
func (s *codexSteerer) resolve(id int64, rpcErr *rpcError) bool {
	s.mu.Lock()
	ch, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	// Buffered; never blocks.
	if rpcErr == nil {
		ch <- nil
	} else {
		// Any rejection (no active turn, expectedTurnId mismatch, turn not
		// steerable) means the input did not enter the turn — surface it
		// as ErrAgentNotBusy so the HTTP layer answers 409 not_busy and
		// the client can fall back to sending a normal message.
		ch <- fmt.Errorf("codex turn/steer rejected: %s: %w", rpcErr.Message, ErrAgentNotBusy)
	}
	return true
}

// steer implements SteerFunc for the codex backend.
func (s *codexSteerer) steer(text string) error {
	select {
	case <-s.ready:
	case <-time.After(codexSteerTurnWait):
		return fmt.Errorf("codex: turn did not start within %s", codexSteerTurnWait)
	}

	// Hold the lock across the closed-check and the write (mirroring
	// claudeStdinWriter) so a steer can't slip a request onto stdin after
	// close() has declared the turn over.
	s.mu.Lock()
	if s.closed || s.turnID == "" {
		s.mu.Unlock()
		return ErrAgentNotBusy
	}
	id, err := s.writeRPC("turn/steer", map[string]any{
		"threadId":       s.threadID,
		"expectedTurnId": s.turnID,
		"input": []map[string]any{
			{"type": "text", "text": text},
		},
	})
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("codex turn/steer write: %w", err)
	}
	respCh := make(chan error, 1)
	s.pending[id] = respCh
	s.mu.Unlock()

	select {
	case err := <-respCh:
		return err
	case <-time.After(codexSteerRespWait):
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return fmt.Errorf("codex: turn/steer was not acknowledged within %s", codexSteerRespWait)
	}
}
