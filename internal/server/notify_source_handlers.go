package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/notifysource"
	gmailpkg "github.com/loppo-llc/kojo/internal/notifysource/gmail"
)

// --- Notify Source CRUD ---

func (s *Server) handleListNotifySources(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.agents.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	sources := a.NotifySources
	if sources == nil {
		sources = []notifysource.Config{}
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"sources": sources})
}

func (s *Server) handleCreateNotifySource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Parse If-Match before any side-effect so a malformed precondition
	// (weak etag, comma-list, missing quotes) is rejected before the
	// agent lookup or body decode. Notify-source rows are stored as a
	// JSON slice on the agent row, so the parent agent's etag is the
	// resource etag — same scope as PATCH /agents/{id}.
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	if _, ok := s.agents.Get(id); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}

	var req struct {
		Type            string            `json:"type"`
		IntervalMinutes int               `json:"intervalMinutes"`
		Query           string            `json:"query"`
		Options         map[string]string `json:"options"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}

	if req.Type == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "type is required")
		return
	}
	if req.IntervalMinutes <= 0 {
		req.IntervalMinutes = 10
	}

	cfg := notifysource.Config{
		ID:              generateSourceID(),
		Type:            req.Type,
		Enabled:         false, // disabled until OAuth2 is set up
		IntervalMinutes: req.IntervalMinutes,
		Query:           req.Query,
		Options:         req.Options,
	}

	// Per-agent serialization across precheck → UpdateNotifySources →
	// ETag echo. Without this, two concurrent mutations carrying the
	// same If-Match would both observe the same store etag, both pass
	// the precheck, and both succeed. Re-snapshot the agent's source
	// list under the lock so a concurrent unrelated mutation that
	// landed between the initial Get() and LockPatch() (e.g. another
	// notify-source edit) is not clobbered when we append.
	release := s.agents.LockPatch(id)
	defer release()
	if !s.agentIfMatchPrecheck(w, r, id, ifMatch, ifMatchPresent) {
		return
	}
	current, ok := s.agents.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+id)
		return
	}
	sources := append(current.NotifySources, cfg)
	if err := s.agents.UpdateNotifySources(id, sources); err != nil {
		if errors.Is(err, agent.ErrAgentBusy) {
			writeError(w, http.StatusConflict, "agent_busy", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if newETag := s.readAgentETag(r, id); newETag != "" {
		w.Header().Set("ETag", quoteETag(newETag))
	}
	writeJSONResponse(w, http.StatusCreated, map[string]any{"source": cfg})
}

func (s *Server) handleUpdateNotifySource(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	sourceID := r.PathValue("sourceId")

	// Parse If-Match before any side-effect. The notify-source row is a
	// JSON entry on the parent agent's row, so the precondition gates
	// on the parent agent's etag — same scope as PATCH /agents/{id}.
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	if _, ok := s.agents.Get(agentID); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+agentID)
		return
	}

	var req struct {
		Enabled         *bool             `json:"enabled"`
		IntervalMinutes *int              `json:"intervalMinutes"`
		Query           *string           `json:"query"`
		Options         map[string]string `json:"options"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}

	// Per-agent serialization across precheck → UpdateNotifySources →
	// ETag echo (see handleCreateNotifySource for rationale).
	release := s.agents.LockPatch(agentID)
	defer release()
	if !s.agentIfMatchPrecheck(w, r, agentID, ifMatch, ifMatchPresent) {
		return
	}

	// Re-snapshot under the lock so concurrent unrelated edits to the
	// agent (or to a sibling source) are not clobbered.
	a, ok := s.agents.Get(agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+agentID)
		return
	}

	// If enabling, verify OAuth tokens exist
	if req.Enabled != nil && *req.Enabled {
		if !s.agents.HasCredentials() {
			writeError(w, http.StatusBadRequest, "bad_request", "credential store not available")
			return
		}
		var srcType string
		for _, cfg := range a.NotifySources {
			if cfg.ID == sourceID {
				srcType = cfg.Type
				break
			}
		}
		if srcType != "" {
			creds := s.agents.Credentials()
			if _, err := creds.GetToken(srcType, agentID, sourceID, "access_token"); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "OAuth not configured — authorize first")
				return
			}
		}
	}

	found := false
	sources := make([]notifysource.Config, len(a.NotifySources))
	copy(sources, a.NotifySources)
	for i := range sources {
		if sources[i].ID != sourceID {
			continue
		}
		found = true
		if req.Enabled != nil {
			sources[i].Enabled = *req.Enabled
		}
		if req.IntervalMinutes != nil {
			sources[i].IntervalMinutes = *req.IntervalMinutes
		}
		if req.Query != nil {
			sources[i].Query = *req.Query
		}
		if req.Options != nil {
			sources[i].Options = req.Options
		}
	}

	if !found {
		writeError(w, http.StatusNotFound, "not_found", "source not found: "+sourceID)
		return
	}

	if err := s.agents.UpdateNotifySources(agentID, sources); err != nil {
		if errors.Is(err, agent.ErrAgentBusy) {
			writeError(w, http.StatusConflict, "agent_busy", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	if newETag := s.readAgentETag(r, agentID); newETag != "" {
		w.Header().Set("ETag", quoteETag(newETag))
	}
	for _, cfg := range sources {
		if cfg.ID == sourceID {
			writeJSONResponse(w, http.StatusOK, map[string]any{"source": cfg})
			return
		}
	}
}

func (s *Server) handleDeleteNotifySource(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	sourceID := r.PathValue("sourceId")

	// Parse If-Match before any side-effect. Gates on the parent
	// agent's etag (see handleUpdateNotifySource).
	ifMatch, ifMatchPresent, err := extractDomainIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid If-Match header")
		return
	}
	if !s.enforceIfMatchPresence(w, r, ifMatchPresent) {
		return
	}
	// §3.7 device-switch gate: hold one AcquireMutation across
	// the config update AND the token cleanup so a switching
	// flip between them cannot escape quiesce — without it,
	// UpdateNotifySources' internal AcquireMutation would refuse
	// after a switch arrives, leaving NotifySources untouched
	// but the DeleteTokensBySource still firing on stale state.
	releaseMut, mutErr := s.agents.AcquireMutation(agentID)
	if mutErr != nil {
		writeError(w, http.StatusConflict, "agent_busy", mutErr.Error())
		return
	}
	defer releaseMut()
	if _, ok := s.agents.Get(agentID); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+agentID)
		return
	}

	release := s.agents.LockPatch(agentID)
	defer release()
	if !s.agentIfMatchPrecheck(w, r, agentID, ifMatch, ifMatchPresent) {
		return
	}

	// Re-snapshot under the lock so concurrent edits to a sibling
	// source are not clobbered when we filter.
	a, ok := s.agents.Get(agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+agentID)
		return
	}

	found := false
	var sourceType string
	var sources []notifysource.Config
	for _, cfg := range a.NotifySources {
		if cfg.ID == sourceID {
			found = true
			sourceType = cfg.Type
			continue
		}
		sources = append(sources, cfg)
	}

	if !found {
		writeError(w, http.StatusNotFound, "not_found", "source not found: "+sourceID)
		return
	}

	// No-guard variant: outer AcquireMutation is held above.
	if err := s.agents.UpdateNotifySourcesAlreadyGuarded(agentID, sources); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Clean up tokens for this source — still under the outer
	// mutation hold so a switching flip can't sneak in between
	// UpdateNotifySources and this delete.
	if s.agents.HasCredentials() && sourceType != "" {
		s.agents.Credentials().DeleteTokensBySource(sourceType, agentID, sourceID)
	}

	if newETag := s.readAgentETag(r, agentID); newETag != "" {
		w.Header().Set("ETag", quoteETag(newETag))
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- OAuth2 Flow ---

func (s *Server) handleNotifySourceAuth(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	sourceID := r.PathValue("sourceId")

	a, ok := s.agents.Get(agentID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+agentID)
		return
	}

	// Find the source config
	var srcCfg *notifysource.Config
	for _, cfg := range a.NotifySources {
		if cfg.ID == sourceID {
			c := cfg
			srcCfg = &c
			break
		}
	}
	if srcCfg == nil {
		writeError(w, http.StatusNotFound, "not_found", "source not found: "+sourceID)
		return
	}

	// Get OAuth2 client credentials (stored globally)
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}
	creds := s.agents.Credentials()
	clientID, err := creds.GetToken(srcCfg.Type, "", "", "client_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("OAuth2 client_id not configured for %s. Set it via POST /api/v1/oauth-clients/%s", srcCfg.Type, srcCfg.Type))
		return
	}

	// Build redirect URI based on the request's host.
	// Host header manipulation is mitigated by:
	// 1. Tailscale network controls access
	// 2. Google requires redirect_uri to be pre-registered in GCP Console
	scheme := "https"
	if s.devMode {
		scheme = "http"
	}
	redirectURI := fmt.Sprintf("%s://%s/oauth2/callback", scheme, r.Host)

	oauth2Mgr := s.getOAuth2Manager()
	authURL, state, err := oauth2Mgr.StartAuthFlow(clientID, agentID, sourceID, redirectURI)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Echo `state` so the editor can correlate the eventual
	// postMessage callback against this exact popup (rather than a
	// stale one from a previous double-click). The state is also
	// embedded in authURL itself; we surface it explicitly so the
	// client doesn't have to URL-parse to extract it.
	writeJSONResponse(w, http.StatusOK, map[string]any{"authUrl": authURL, "state": state})
}

func (s *Server) handleOAuth2Callback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	errParam := r.URL.Query().Get("error")

	oauth2Mgr := s.getOAuth2Manager()

	// Provider-side denial includes the original `state` so we can
	// resolve sourceID for postMessage correlation. The editor
	// matches incoming messages on activeAuthState (the per-popup
	// nonce) — denied-with-state messages need to carry both
	// sourceId AND state so the editor can tell which popup the
	// denial belongs to. Without this lookup, denied-at-provider
	// would arrive with empty sourceId and would only correlate via
	// state, which still works but loses one piece of context the
	// editor uses for its banner copy.
	deniedSourceID := ""
	if errParam != "" && state != "" {
		if pending := oauth2Mgr.PeekPending(state); pending != nil {
			deniedSourceID = pending.SourceID
		}
	}

	// Every error from this point on returns the OAuth-error HTML
	// (postMessage to opener + window.close()) — without it the
	// popup hangs with raw text and the editor never gets feedback.
	if errParam != "" {
		writeOAuthErrorHTML(w, http.StatusBadRequest, "denied",
			"Authorization denied: "+errParam, deniedSourceID, state)
		return
	}
	if code == "" || state == "" {
		writeOAuthErrorHTML(w, http.StatusBadRequest, "bad_request",
			"Missing code or state", "", state)
		return
	}

	// Look up the pending auth to determine the source type (peek only, don't consume)
	pending := oauth2Mgr.PeekPending(state)
	if pending == nil {
		writeOAuthErrorHTML(w, http.StatusBadRequest, "expired",
			"Unknown or expired state", "", state)
		return
	}

	// Pre-exchange agent / source / archived check. Concurrent
	// Delete/Archive (serialized via LockPatch on the lifecycle
	// methods) cannot land mid-callback once we acquire the patch
	// lock below — so the post-CompleteAuthFlow re-check under the
	// lock is what catches a transition that lands during the token
	// exchange round-trip. The pre-exchange check here is the fast
	// fail for the common case where the transition already
	// happened before we got the callback: refuse cleanly without
	// burning the OAuth code (CompleteAuthFlow consumes it from the
	// pending map AND can be replayed once at the provider side, so
	// we want to skip that step on a known-doomed callback).
	a, ok := s.agents.Get(pending.AgentID)
	if !ok {
		writeOAuthErrorHTML(w, http.StatusGone, "agent_gone",
			"Agent no longer exists.", pending.SourceID, state)
		return
	}
	if a.Archived {
		writeOAuthErrorHTML(w, http.StatusConflict, "agent_archived",
			"Agent was archived before authorization completed; unarchive before re-authorizing.", pending.SourceID, state)
		return
	}
	var provider string
	for _, cfg := range a.NotifySources {
		if cfg.ID == pending.SourceID {
			provider = cfg.Type
			break
		}
	}
	if provider == "" {
		writeOAuthErrorHTML(w, http.StatusGone, "source_gone",
			"Notification source was removed before authorization completed; please re-add it.", pending.SourceID, state)
		return
	}

	if !s.agents.HasCredentials() {
		writeOAuthErrorHTML(w, http.StatusInternalServerError, "no_credstore",
			"Credential store not available", pending.SourceID, state)
		return
	}
	creds := s.agents.Credentials()
	clientID, err := creds.GetToken(provider, "", "", "client_id")
	if err != nil {
		writeOAuthErrorHTML(w, http.StatusInternalServerError, "no_client_id",
			"OAuth client_id not found", pending.SourceID, state)
		return
	}
	clientSecret, err := creds.GetToken(provider, "", "", "client_secret")
	if err != nil {
		writeOAuthErrorHTML(w, http.StatusInternalServerError, "no_client_secret",
			"OAuth client_secret not found", pending.SourceID, state)
		return
	}

	// §3.7 device switch gate: take ONE mutation hold for the
	// entire callback — token exchange round-trip AND token
	// persistence + Enable flip. Holding through the round-trip
	// means a switch that starts during the OAuth exchange will
	// be blocked from quiescing until we're done, instead of
	// landing mid-callback and forcing us to 409 after the
	// auth code has already been consumed by the provider.
	// AcquireMutation is per-agent, not global, so this only
	// blocks switches of THIS agent — other agents proceed.
	releaseMut, mutErr := s.agents.AcquireMutation(pending.AgentID)
	if mutErr != nil {
		writeOAuthErrorHTML(w, http.StatusConflict, "agent_busy",
			mutErr.Error(), pending.SourceID, state)
		return
	}
	defer releaseMut()

	auth, tokenResp, err := oauth2Mgr.CompleteAuthFlow(r.Context(), state, code, clientID, clientSecret)
	if err != nil {
		writeOAuthErrorHTML(w, http.StatusInternalServerError, "token_failed",
			"Token exchange failed: "+err.Error(), pending.SourceID, state)
		return
	}
	// auth.AgentID should always match pending.AgentID — the
	// state cookie binds them. If they diverge the callback's
	// invariants are broken; refuse rather than write tokens
	// for a different agent than the mutation hold protects.
	if auth.AgentID != pending.AgentID {
		writeOAuthErrorHTML(w, http.StatusInternalServerError, "agent_mismatch",
			"OAuth callback resolved an agent different from the pending state",
			auth.SourceID, auth.State)
		return
	}

	// Take the per-agent patch lock for the rest of the callback —
	// covers token persistence AND the Enable flip, so a concurrent
	// DELETE notify-source cannot land between (a) the source-still-
	// exists check and the token writes (which would orphan tokens
	// on a vanished source) or (b) the token writes and the Enable
	// flip (which would silently no-op the for-loop and leave the
	// user looking at a "succeeded" page that did nothing). The lock
	// is the same one the CRUD handlers use, so the callback queues
	// behind any in-flight notify-source mutation.
	release := s.agents.LockPatch(auth.AgentID)
	defer release()

	// Re-verify the source still exists AND the agent is not archived
	// under the lock. A concurrent DELETE that landed during the OAuth
	// round-trip would otherwise produce orphan token rows the user
	// has no way to clean up. A concurrent Archive would leave the
	// source intact but inert — saving tokens + flipping Enabled on
	// an archived agent would (a) lie to the user via "success" page
	// and (b) burn the OAuth grant against an agent that won't poll.
	updatedA, ok := s.agents.Get(auth.AgentID)
	if !ok {
		writeOAuthErrorHTML(w, http.StatusGone, "agent_gone",
			"Agent no longer exists.", auth.SourceID, auth.State)
		return
	}
	if updatedA.Archived {
		writeOAuthErrorHTML(w, http.StatusConflict, "agent_archived",
			"Agent was archived during authorization; unarchive before re-authorizing.", auth.SourceID, auth.State)
		return
	}
	sourceExists := false
	for _, cfg := range updatedA.NotifySources {
		if cfg.ID == auth.SourceID {
			sourceExists = true
			break
		}
	}
	if !sourceExists {
		// Source was deleted while the OAuth flow was in flight.
		// Don't save tokens against a row that no longer exists.
		writeOAuthErrorHTML(w, http.StatusGone, "source_gone",
			"Notification source was removed during authorization; please re-add it.", auth.SourceID, auth.State)
		return
	}

	// Snapshot any existing tokens BEFORE we start overwriting. A
	// re-auth flow that fails mid-write would otherwise leave the
	// user with partial new tokens AND no working old ones —
	// blanket-deleting the orphans (the original cleanup) is just as
	// destructive (sweeps the still-valid old tokens away). Snapshot
	// + restore-on-failure preserves the previous working state when
	// possible, and only deletes when there was nothing to preserve.
	tokenKeys := []string{"client_id", "client_secret", "access_token", "refresh_token"}
	type tokenSnapshot struct {
		key   string
		value string
		exp   time.Time
		had   bool
	}
	snapshots := make([]tokenSnapshot, 0, len(tokenKeys))
	for _, k := range tokenKeys {
		v, exp, err := creds.GetTokenExpiry(provider, auth.AgentID, auth.SourceID, k)
		snap := tokenSnapshot{key: k}
		switch {
		case err == nil:
			snap.value = v
			snap.exp = exp
			snap.had = true
		case errors.Is(err, sql.ErrNoRows):
			// Genuinely missing — first-time auth for this source.
			// rollback (if it runs) will DeleteToken which is a no-op.
		default:
			// Transient DB / decrypt error. Treating this as
			// "missing" would let a later rollback delete a token
			// that actually exists. Bail before any write so the
			// existing state is preserved untouched.
			writeOAuthErrorHTML(w, http.StatusInternalServerError, "snapshot_failed",
				"Failed to read existing token for "+k+": "+err.Error(),
				auth.SourceID, auth.State)
			return
		}
		snapshots = append(snapshots, snap)
	}
	rollbackTokens := func() {
		// Best-effort: log the cleanup failure but keep going. If the
		// snapshot says we had a value, restore it (overwrites
		// whatever partial value may now be there). If we didn't,
		// delete so we don't leave a partial new token behind.
		for _, snap := range snapshots {
			if snap.had {
				_ = creds.SetToken(provider, auth.AgentID, auth.SourceID, snap.key, snap.value, snap.exp)
			} else {
				_ = creds.DeleteToken(provider, auth.AgentID, auth.SourceID, snap.key)
			}
		}
	}

	// Store tokens — check each operation for errors. Inside the
	// lock so concurrent DELETE / token cleanup can't race with us.
	// On any failure we restore from snapshot so a partial save
	// doesn't either leave orphan rows OR clobber a working prior
	// configuration.
	expiry := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	for _, kv := range []struct{ k, v string }{
		{"client_id", clientID},
		{"client_secret", clientSecret},
		{"access_token", tokenResp.AccessToken},
	} {
		exp := time.Time{}
		if kv.k == "access_token" {
			exp = expiry
		}
		if err := creds.SetToken(provider, auth.AgentID, auth.SourceID, kv.k, kv.v, exp); err != nil {
			rollbackTokens()
			writeOAuthErrorHTML(w, http.StatusInternalServerError, "token_save_failed",
				"Failed to save token: "+err.Error(), auth.SourceID, auth.State)
			return
		}
	}
	if tokenResp.RefreshToken != "" {
		if err := creds.SetToken(provider, auth.AgentID, auth.SourceID, "refresh_token", tokenResp.RefreshToken, time.Time{}); err != nil {
			rollbackTokens()
			writeOAuthErrorHTML(w, http.StatusInternalServerError, "token_save_failed",
				"Failed to save refresh token: "+err.Error(), auth.SourceID, auth.State)
			return
		}
	}

	// Enable the source. Re-snapshot under the lock so a concurrent
	// edit on a sibling source that landed before our LockPatch is
	// not clobbered.
	current, ok := s.agents.Get(auth.AgentID)
	if ok {
		sources := make([]notifysource.Config, len(current.NotifySources))
		copy(sources, current.NotifySources)
		for i := range sources {
			if sources[i].ID == auth.SourceID {
				sources[i].Enabled = true
				break
			}
		}
		// AlreadyGuarded: the outer AcquireMutation(pending.AgentID)
		// at the top of this callback (and the auth.AgentID ==
		// pending.AgentID invariant verified just below it) still
		// covers us here. A re-entrant AcquireMutation would fail-
		// closed if a switch flipped `switching` between the token
		// save above and this Enable flip — leaving freshly-written
		// tokens with Enabled=false until the operator re-runs OAuth.
		if err := s.agents.UpdateNotifySourcesAlreadyGuarded(auth.AgentID, sources); err != nil {
			rollbackTokens()
			writeOAuthErrorHTML(w, http.StatusInternalServerError, "enable_failed",
				"Failed to enable source: "+err.Error(), auth.SourceID, auth.State)
			return
		}
	}

	// Notify the opener window and close. Include sourceID AND state
	// so the editor can correlate this message with its active popup
	// — without that, a stale message from a previous popup (same
	// source, double-clicked Auth) could overwrite a fresh banner.
	// state is the per-popup nonce, so it discriminates same-source
	// retries that sourceId alone cannot. Marshal as one JSON object
	// (quoted keys) so the postMessage argument is also valid JSON
	// — see writeOAuthErrorHTML for the rationale.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	payload, _ := json.Marshal(map[string]string{
		"type":     "oauth_complete",
		"sourceId": auth.SourceID,
		"state":    auth.State,
	})
	fmt.Fprintf(w, `<!DOCTYPE html><html><body>
<p>Authorization successful.</p>
<script>
if(window.opener){window.opener.postMessage(%s,"*")}
window.close()
</script>
</body></html>`, payload)
}

// writeOAuthErrorHTML emits an HTML error page that mirrors the
// success-page contract: postMessage to the opener (so the editor can
// surface the failure) and window.close(). Without this, a plain
// http.Error response from inside the popup would (a) leave the
// popup open with raw error text the user has to manually dismiss,
// and (b) never reach the parent window — NotifySourcesEditor's
// "oauth_complete" listener would just never fire and the user would
// see no feedback at all.
//
// reason is a short machine-readable string ("agent_gone", "source_gone",
// "token_failed") so the listener can branch on cause if needed.
// detail is the human-readable string baked into the popup body.
// sourceID is the notify-source whose authorization failed; empty
// when the failure happened before we resolved a source (e.g. invalid
// state parameter).
// state is the OAuth2 `state` parameter the editor minted when it
// opened this popup; the editor uses it to correlate against the
// active popup so a stale message from a prior popup (same source,
// double-click) can't overwrite a fresh banner. Empty when the
// callback failed before we could resolve state (truly unknown popup).
func writeOAuthErrorHTML(w http.ResponseWriter, status int, reason, detail, sourceID, state string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	// Marshal the full payload as a single JSON object so keys are
	// quoted — keeps the postMessage argument valid JSON instead of
	// relying on JS's looser object-literal grammar. Lets unit tests
	// parse the body with encoding/json without resorting to a JS
	// runtime, and any future caller who pipes payload through
	// JSON.parse on the receiver side stays happy.
	payload, _ := json.Marshal(map[string]string{
		"type":     "oauth_error",
		"reason":   reason,
		"detail":   detail,
		"sourceId": sourceID,
		"state":    state,
	})
	fmt.Fprintf(w, `<!DOCTYPE html><html><body>
<p>Authorization failed: %s</p>
<script>
if(window.opener){window.opener.postMessage(%s,"*")}
window.close()
</script>
</body></html>`, htmlEscape(detail), payload)
}

// htmlEscape is a 4-byte minimal escaper for the visible <p> body.
// json.Marshal handles the postMessage payload separately so the JS
// strings are always well-formed even if detail contains a "</script>"
// sequence.
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

// --- OAuth Client Configuration ---

func (s *Server) handleListOAuthClients(w http.ResponseWriter, r *http.Request) {
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	providers := []string{"gmail"}
	type clientInfo struct {
		Provider   string `json:"provider"`
		Configured bool   `json:"configured"`
	}

	var clients []clientInfo
	creds := s.agents.Credentials()
	for _, p := range providers {
		_, err := creds.GetToken(p, "", "", "client_id")
		clients = append(clients, clientInfo{
			Provider:   p,
			Configured: err == nil,
		})
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{"clients": clients})
}

func (s *Server) handleSetOAuthClient(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	var req struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
		return
	}
	if req.ClientID == "" || req.ClientSecret == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "clientId and clientSecret are required")
		return
	}

	creds := s.agents.Credentials()
	if err := creds.SetToken(provider, "", "", "client_id", req.ClientID, time.Time{}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to save client_id: "+err.Error())
		return
	}
	if err := creds.SetToken(provider, "", "", "client_secret", req.ClientSecret, time.Time{}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to save client_secret: "+err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteOAuthClient(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if !s.agents.HasCredentials() {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "credential store not available")
		return
	}

	creds := s.agents.Credentials()
	if err := creds.DeleteToken(provider, "", "", "client_id"); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if err := creds.DeleteToken(provider, "", "", "client_secret"); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Source Type Registry ---

func (s *Server) handleListNotifySourceTypes(w http.ResponseWriter, r *http.Request) {
	types := []map[string]any{
		{
			"type":        "gmail",
			"name":        "Gmail",
			"description": "Google Gmail notifications",
			"authType":    "oauth2",
		},
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"types": types})
}

// --- OAuth2 Manager (lazy init) ---

func (s *Server) getOAuth2Manager() *gmailpkg.OAuth2Manager {
	s.oauth2Once.Do(func() {
		s.oauth2Mgr = gmailpkg.NewOAuth2Manager()
	})
	return s.oauth2Mgr
}

// helpers

func generateSourceID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "ns_" + hex.EncodeToString(b)
}
