package server

import (
	"context"
	"net/http"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
)

// restartDrainTimeout bounds the wait for all agent chats to drain
// before the re-exec trigger fires. Generous: a long autonomous turn
// can run many minutes. On timeout the restart is ABORTED (quiesce
// lifted, pending flag cleared) rather than forced — killing a turn
// mid-flight would lose its transcript tail and the caller can simply
// re-POST.
const restartDrainTimeout = 15 * time.Minute

// SetRestartTrigger wires the shutdown-for-restart callback. cmd/kojo
// passes a closure that marks the restart intent and cancels the
// signal context, funneling into the same ordered graceful-shutdown
// path as SIGINT; after that drain main re-execs the binary in place.
// When never set (tests, unsupported platforms), the handler returns
// 501.
func (s *Server) SetRestartTrigger(fn func()) {
	s.restartMu.Lock()
	s.restartTrigger = fn
	s.restartMu.Unlock()
}

// handleSystemRestart POST /api/v1/system/restart
//
// Gracefully restarts the daemon: quiesce new chats → wait for every
// in-flight turn (and post-turn summarizer) to drain → graceful
// shutdown → re-exec the binary at os.Executable(). Because the
// re-exec picks up whatever is on disk, `make build` + this endpoint
// is a full deploy.
//
// Allowed for Owner and privileged agents (auth.CanRestartServer;
// mirrored in policy.AllowNonOwner). An agent calling this mid-turn is
// itself busy — the drain waits for that turn to finish, so the
// caller's final response is delivered before the process swaps.
//
// Responds 202 immediately: {"status":"pending"} on the first call,
// {"status":"already_pending"} on duplicates while a drain is in
// progress.
func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	if p := auth.FromContext(r.Context()); !p.CanRestartServer() {
		writeError(w, http.StatusForbidden, "forbidden",
			"restart requires Owner or a privileged agent")
		return
	}
	s.restartMu.Lock()
	trigger := s.restartTrigger
	s.restartMu.Unlock()
	if trigger == nil {
		writeError(w, http.StatusNotImplemented, "unsupported",
			"restart is not supported in this run mode")
		return
	}
	if !s.restartPending.CompareAndSwap(false, true) {
		writeJSONResponse(w, http.StatusAccepted, map[string]any{"status": "already_pending"})
		return
	}
	go func() {
		if s.agents != nil {
			s.agents.SetQuiescing(true)
			ctx, cancel := context.WithTimeout(context.Background(), restartDrainTimeout)
			defer cancel()
			if err := s.agents.WaitAllChatsIdle(ctx); err != nil {
				s.logger.Error("restart aborted: chats did not drain; quiesce lifted", "err", err)
				s.agents.SetQuiescing(false)
				s.restartPending.Store(false)
				return
			}
		}
		s.logger.Info("restart: chats drained; shutting down for re-exec")
		trigger()
	}()
	writeJSONResponse(w, http.StatusAccepted, map[string]any{
		"status":  "pending",
		"version": s.version,
	})
}
