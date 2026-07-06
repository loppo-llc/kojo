package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
)

// rebuildTimeout bounds `make build`. A cold build (npm + go) can take
// several minutes; 10 min leaves generous headroom.
const rebuildTimeout = 10 * time.Minute

// rebuildOutputTailBytes caps how much combined build output is echoed
// back to the caller — enough to see the failing tail without shipping
// megabytes of npm chatter.
const rebuildOutputTailBytes = 8 * 1024

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// handleSystemRebuild POST /api/v1/system/rebuild
//
// Runs `make build` synchronously in the configured source checkout
// (Config.RepoDir / $KOJO_REPO_DIR), then copies the freshly built
// binary over the running os.Executable() so a subsequent
// /api/v1/system/restart re-execs the new build in place. Does NOT
// restart on its own — the UI calls restart separately.
//
// Owner-only (auth.CanRestartServer, same as restart). One rebuild at
// a time (409 on overlap). 409 when no repo dir is configured.
func (s *Server) handleSystemRebuild(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.CanRestartServer() {
		writeError(w, http.StatusForbidden, "forbidden",
			"rebuild requires Owner or a privileged agent")
		return
	}
	if s.repoDir == "" {
		writeError(w, http.StatusConflict, "not_configured",
			"repo dir not configured (set KOJO_REPO_DIR)")
		return
	}
	if !s.rebuildRunning.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, "already_running",
			"a rebuild is already in progress")
		return
	}
	defer s.rebuildRunning.Store(false)

	// Parent on Background, not r.Context(): a client disconnect or a
	// proxy read-timeout must not abort a multi-minute build midway.
	ctx, cancel := context.WithTimeout(context.Background(), rebuildTimeout)
	defer cancel()

	s.logger.Info("rebuild: make build starting", "dir", s.repoDir)
	cmd := exec.CommandContext(ctx, "make", "build")
	cmd.Dir = s.repoDir
	// On timeout kill the whole process group, not just make, so that
	// go build / npm grandchildren don't get orphaned (unix only).
	setupBuildProcessGroup(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.logger.Error("rebuild: make build failed", "err", err)
		writeJSONResponse(w, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  err.Error(),
			"output": tail(out, rebuildOutputTailBytes),
		})
		return
	}

	if err := deployBuiltBinary(s.repoDir); err != nil {
		s.logger.Error("rebuild: deploy of built binary failed", "err", err)
		writeJSONResponse(w, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  "build succeeded but deploy failed: " + err.Error(),
			"output": tail(out, rebuildOutputTailBytes),
		})
		return
	}

	s.logger.Info("rebuild: build + deploy complete; restart to apply")
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"status": "ok",
		"output": tail(out, rebuildOutputTailBytes),
	})
}

// deployBuiltBinary copies <repoDir>/kojo over the running executable
// via a temp file + atomic rename in the destination directory. When
// the running binary already IS <repoDir>/kojo (make wrote it in
// place), the copy is skipped.
func deployBuiltBinary(repoDir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return deployBuiltBinaryTo(filepath.Join(repoDir, "kojo"), self)
}

// deployBuiltBinaryTo copies built over self via temp-file + atomic
// rename in self's directory, skipping when both resolve to the same
// path. Split out from deployBuiltBinary so tests can exercise the
// copy/skip logic without touching os.Executable().
func deployBuiltBinaryTo(built, self string) error {
	self, err := filepath.EvalSymlinks(self)
	if err != nil {
		return err
	}
	builtResolved, err := filepath.EvalSymlinks(built)
	if err != nil {
		return err
	}
	if builtResolved == self {
		return nil // make already wrote the running binary in place
	}
	src, err := os.Open(builtResolved)
	if err != nil {
		return err
	}
	defer src.Close()
	dstDir := filepath.Dir(self)
	tmp, err := os.CreateTemp(dstDir, ".kojo-deploy-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpName, self)
}

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
// The closure reports whether the intent was accepted (false = a
// signal shutdown already won the race). When never set (tests,
// unsupported platforms), the handler returns 501.
func (s *Server) SetRestartTrigger(fn func() bool) {
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
// Optional JSON body {"wake": true, "agentId": "ag_..."} arms a
// restart-wake marker: after the re-exec, boot auto-triggers ONE
// system-role chat turn for the agent so it can verify the deploy and
// continue without a human message. Agents may only wake themselves
// (agentId defaults to the caller); the Owner must name an agentId
// explicitly. The marker is armed only when the drain succeeds, right
// before the shutdown trigger. A duplicate request while a restart is
// pending gets already_pending and its wake payload is IGNORED.
//
// Responds 202 immediately: {"status":"pending"} on the first call,
// {"status":"already_pending"} on duplicates while a drain is in
// progress.
func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.CanRestartServer() {
		writeError(w, http.StatusForbidden, "forbidden",
			"restart requires Owner or a privileged agent")
		return
	}
	// Optional body. An empty body (the pre-wake curl) must keep
	// working, so io.EOF is tolerated; anything else malformed is 400.
	var body struct {
		Wake    bool   `json:"wake"`
		AgentID string `json:"agentId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	wakeID := ""
	if body.Wake {
		wakeID = body.AgentID
		if p.IsAgent() {
			// Agents may only wake themselves — waking someone else
			// would drop an unexpected system turn into that agent's
			// transcript.
			if wakeID == "" {
				wakeID = p.AgentID
			} else if wakeID != p.AgentID {
				writeError(w, http.StatusForbidden, "forbidden",
					"agents may only wake themselves")
				return
			}
		}
		if wakeID == "" {
			writeError(w, http.StatusBadRequest, "bad_request",
				"wake requires agentId when called by the Owner")
			return
		}
		if s.agents == nil {
			writeError(w, http.StatusNotImplemented, "unsupported",
				"wake is not supported in this run mode")
			return
		}
		if _, ok := s.agents.Get(wakeID); !ok {
			writeError(w, http.StatusNotFound, "not_found", "agent not found: "+wakeID)
			return
		}
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
			// Chats are idle: close any persistent claude processes so the
			// restart doesn't strand a live CLI holding the session file.
			s.agents.CloseAllClaudeSessions()
		}
		// Arm the wake marker BEFORE the trigger: the trigger cancels
		// the signal ctx and main's shutdown (store close, exec) starts
		// racing immediately, so a post-trigger write could be lost.
		// Armed only after the drain succeeded, so an aborted drain
		// never leaves a marker.
		armed := false
		if wakeID != "" {
			// Re-validate: the target may have been deleted or
			// archived while the drain waited.
			if a, ok := s.agents.Get(wakeID); !ok || a.Archived {
				s.logger.Warn("restart: wake target gone or archived; wake skipped", "agent", wakeID)
			} else if err := s.agents.ArmRestartWake(wakeID); err != nil {
				s.logger.Warn("restart: wake marker write failed; restarting without wake",
					"agent", wakeID, "err", err)
			} else {
				armed = true
			}
		}
		s.logger.Info("restart: chats drained; shutting down for re-exec")
		if !trigger() {
			// A signal-initiated shutdown won the race — this is a
			// stop, not a restart. Best-effort disarm; if the store
			// closes first the leftover marker fires one benign (and
			// factually accurate) "restarted" turn on the next boot.
			s.logger.Info("restart: trigger refused (shutdown already in flight)")
			if armed {
				s.agents.DisarmRestartWake()
			}
		}
	}()
	writeJSONResponse(w, http.StatusAccepted, map[string]any{
		"status":  "pending",
		"version": s.version,
	})
}
