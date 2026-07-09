package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/selfupdate"
)

// handleSystemUpdateStatus GET /api/v1/system/update
//
// Reports the last (or freshly-fetched) GitHub Releases check. Owner
// or privileged agent only (same gate as restart). When no checker is
// wired the response is 200 {"supported":false} so the UI can hide the
// control without treating it as an error. ?refresh=1 forces CheckNow
// (20s timeout); a fetch failure becomes 502 upstream_error.
func (s *Server) handleSystemUpdateStatus(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.CanRestartServer() {
		writeError(w, http.StatusForbidden, "forbidden",
			"update status requires Owner or a privileged agent")
		return
	}
	if s.updateChecker == nil {
		writeJSONResponse(w, http.StatusOK, map[string]any{"supported": false})
		return
	}

	st := s.updateChecker.Status()
	if r.URL.Query().Get("refresh") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		fresh, err := s.updateChecker.CheckNow(ctx)
		if err != nil {
			writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
			return
		}
		st = fresh
	}

	s.restartMu.Lock()
	restartOK := s.restartTrigger != nil
	s.restartMu.Unlock()

	resp := map[string]any{
		"supported":       restartOK,
		"current":         s.version,
		"latest":          st.Latest,
		"updateAvailable": st.UpdateAvailable,
		"notesUrl":        st.NotesURL,
	}
	if !st.CheckedAt.IsZero() {
		resp["checkedAt"] = st.CheckedAt.UTC().Format(time.RFC3339)
	}
	writeJSONResponse(w, http.StatusOK, resp)
}

// handleSystemUpdate POST /api/v1/system/update
//
// Downloads the latest platform binary, swaps it in place, then starts
// the same graceful restart drain as POST /system/restart. Owner or
// privileged agent only. Self-update without a restart path is refused
// (501): leaving a swapped binary for a later manual restart is too
// easy to forget mid-deploy.
//
// Optional JSON body {"wake":bool,"agentId":string} mirrors restart.
//
// The restart slot is claimed BEFORE Apply so a concurrent
// POST /system/restart cannot arm (or finish) a drain mid-download.
// On Apply failure the claim is released; on success startRestartDrain
// runs without re-claiming.
//
// Apply runs under a 5-minute context derived from
// context.WithoutCancel(r.Context()) so a dropped client connection
// cannot abort a half-applied download or mid-flight swap.
func (s *Server) handleSystemUpdate(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if !p.CanRestartServer() {
		writeError(w, http.StatusForbidden, "forbidden",
			"update requires Owner or a privileged agent")
		return
	}
	if s.updateChecker == nil {
		writeError(w, http.StatusNotImplemented, "unsupported",
			"self-update is not configured in this run mode")
		return
	}

	var body struct {
		Wake    bool   `json:"wake"`
		AgentID string `json:"agentId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	wakeID, ok := s.validateRestartWake(w, p, body.Wake, body.AgentID)
	if !ok {
		return
	}
	wakeSessionKey := wakeThreadForRestart(p, wakeID, s.agents.InFlightOneShotSessionKey)

	// Claim before Apply: mutual exclusion with POST /system/restart for
	// the whole download/swap window, not just the post-Apply drain.
	claimed, unsupported := s.tryClaimRestart()
	if unsupported {
		writeError(w, http.StatusNotImplemented, "unsupported",
			"self-update requires an in-place restart path (not supported in this run mode)")
		return
	}
	if !claimed {
		// Same shape as restart's already_pending path (202 + status).
		writeJSONResponse(w, http.StatusAccepted, map[string]any{"status": "already_pending"})
		return
	}

	// WithoutCancel: a cancelled request context must not kill the
	// download/swap mid-flight; the timeout still bounds hung mirrors.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()

	st, err := s.updateChecker.Apply(ctx)
	if err != nil {
		// Release the restart slot so a subsequent restart/update can run.
		s.restartPending.Store(false)
		switch {
		case errors.Is(err, selfupdate.ErrUpToDate):
			writeJSONResponse(w, http.StatusOK, map[string]any{"status": "up_to_date"})
		case errors.Is(err, selfupdate.ErrApplyInFlight):
			writeError(w, http.StatusConflict, "already_updating",
				"an update is already in progress")
		case errors.Is(err, selfupdate.ErrAssetNotFound):
			writeError(w, http.StatusBadGateway, "assets_not_ready",
				"release has no binary for this platform yet (CI may still be uploading)")
		default:
			writeError(w, http.StatusBadGateway, "update_failed", err.Error())
		}
		return
	}

	// Claim already held — enter the drain without re-claiming.
	// Re-check trigger: a test (or future hot-unplug) can race
	// SetRestartTrigger(nil) while Apply ran; do not report pending
	// if no drain can start.
	s.restartMu.Lock()
	triggerOK := s.restartTrigger != nil
	s.restartMu.Unlock()
	if !triggerOK {
		s.restartPending.Store(false)
		writeError(w, http.StatusNotImplemented, "unsupported",
			"binary updated but restart is not supported in this run mode")
		return
	}
	s.startRestartDrain(wakeID, wakeSessionKey)
	writeJSONResponse(w, http.StatusAccepted, map[string]any{
		"status": "pending",
		"from":   st.Current,
		"to":     st.Latest,
	})
}
