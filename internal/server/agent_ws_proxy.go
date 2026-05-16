package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

// docs/multi-device-storage.md §3.7 — cross-peer UI WebSocket
// proxy.
//
// The UI lives on Hub. The user connects to Hub's
// /api/v1/agents/{id}/ws which historically just spawned the
// chat loop locally. After the §3.7 device switch this can land
// on a host that is no longer the agent's home_peer — the agent
// runtime is on the target peer and only THAT peer's PTY can
// service chat events.
//
// The proxy:
//   1. Reads agent_locks.holder_peer.
//   2. If holder == local peer → fall through to the local
//      handler (existing handleAgentWebSocket behaviour).
//   3. Otherwise → upgrade the UI's WS on this side, dial the
//      target peer's /api/v1/agents/{id}/ws with Ed25519-signed
//      peer-auth headers, and pipe binary frames both ways
//      until either side closes.
//
// This keeps the UI bookmark stable: the user always points at
// Hub. When an agent moves to target, chat IO transparently
// flows through Hub to target's PTY.

// handleAgentWebSocketRouting is the top-level dispatcher. If
// agent_locks says the agent's holder is the local peer, defer
// to handleAgentWebSocket; otherwise proxy. Returns true when the
// router itself wrote a 4xx/5xx (so the caller knows the request
// is done); false means "delegate to local handler".
func (s *Server) handleAgentWebSocketRouting(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing agent id")
		return
	}
	// No peer identity wired (test fixture / single-host build):
	// always local.
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		s.handleAgentWebSocket(w, r)
		return
	}
	lock, err := s.agents.Store().GetAgentLock(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// No lock claimed yet. The local handler will
			// trigger AgentLockGuard.AddAgent → Acquire on
			// the first write; serve locally for now.
			s.handleAgentWebSocket(w, r)
			return
		}
		s.logger.Error("agent ws router: lock read failed",
			"agent", agentID, "err", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"agent lock read failed")
		return
	}
	if lock.HolderPeer == "" || lock.HolderPeer == s.peerID.DeviceID {
		s.handleAgentWebSocket(w, r)
		return
	}
	// RolePeer callers are TERMINAL — a request from another
	// peer's WS proxy MUST land on this peer's own handler if
	// we hold the lock; otherwise refuse with 409 wrong_holder
	// rather than re-proxy. Without this guard, a stale lock
	// view could cycle the WS upgrade between peers and create
	// an unbounded proxy chain.
	if p := auth.FromContext(r.Context()); p.IsPeer() {
		writeError(w, http.StatusConflict, "wrong_holder",
			"agent_lock holder is "+lock.HolderPeer+", not this peer; orchestrator must dial the holder directly")
		return
	}
	// Owner / agent caller — proxy to the holder peer.
	s.proxyAgentWebSocket(w, r, agentID, lock.HolderPeer)
}

// proxyAgentWebSocket dials targetPeer's /api/v1/agents/{id}/ws
// with peer-auth headers, accepts the inbound UI WebSocket, and
// pumps frames both ways. Closes both sides when either reads an
// error.
func (s *Server) proxyAgentWebSocket(w http.ResponseWriter, r *http.Request, agentID, targetDeviceID string) {
	targetRec, err := s.agents.Store().GetPeer(r.Context(), targetDeviceID)
	if err != nil {
		s.logger.Error("agent ws proxy: target peer not in registry",
			"agent", agentID, "holder", targetDeviceID, "err", err)
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"agent lock points at unknown peer "+targetDeviceID)
		return
	}
	addr, err := peer.NormalizeAddress(targetRec.Name)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"target peer has no usable dial address: "+err.Error())
		return
	}
	targetURL, err := url.Parse(addr)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"target address unparseable: "+err.Error())
		return
	}
	switch targetURL.Scheme {
	case "http":
		targetURL.Scheme = "ws"
	case "https":
		targetURL.Scheme = "wss"
	default:
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"target scheme not http(s): "+targetURL.Scheme)
		return
	}
	targetURL.Path = "/api/v1/agents/" + agentID + "/ws"

	// Build a signed upgrade request. SignRequest stamps the
	// peer-auth headers we'll hand to the WS dial via HTTPHeader.
	upgrade, err := http.NewRequestWithContext(r.Context(),
		http.MethodGet, targetURL.String(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"build upgrade request: "+err.Error())
		return
	}
	nonce, err := peer.MakeNonce()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"nonce: "+err.Error())
		return
	}
	if err := peer.SignRequest(upgrade,
		s.peerID.DeviceID, s.peerID.PrivateKey, nonce, targetDeviceID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"sign upgrade: "+err.Error())
		return
	}

	// Dial the target's WS FIRST so a target-side reject (lock
	// has rotated again, target down, etc.) surfaces as a clean
	// HTTP error before we've upgraded the inbound conn.
	//
	// HTTPClient with keep-alives disabled: the upgrade request
	// carries an Ed25519-signed Authorization header with a
	// single-use nonce. Go's default transport silently retries
	// idempotent GETs on stale idle connections, resending the
	// SAME nonce — the recipient's peer auth middleware rejects
	// the retry as a replay. Forcing a fresh TCP/TLS handshake
	// per upgrade is cheap (one extra round-trip) and keeps the
	// nonce semantics intact. 10 s ceiling on the upgrade leg
	// matches the subscriber dial budget.
	targetConn, _, err := websocket.Dial(r.Context(), targetURL.String(),
		&websocket.DialOptions{
			HTTPHeader: upgrade.Header,
			HTTPClient: peer.NoKeepAliveHTTPClient(10 * time.Second),
		})
	if err != nil {
		s.logger.Warn("agent ws proxy: dial target failed",
			"agent", agentID, "target", targetDeviceID, "err", err)
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"connect to holder peer: "+err.Error())
		return
	}
	defer targetConn.CloseNow()

	// Now upgrade the inbound (UI) connection. Origin patterns
	// match the local handler so a browser tab from the Hub UI
	// is accepted; the cross-peer proxy doesn't change that
	// gate.
	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: wsOriginPatterns,
	})
	if err != nil {
		s.logger.Error("agent ws proxy: accept inbound failed",
			"agent", agentID, "err", err)
		return
	}
	defer clientConn.CloseNow()
	// Mirror the local handler's read limit so a buggy / hostile
	// peer can't fan a multi-megabyte frame straight into the
	// browser.
	clientConn.SetReadLimit(256 * 1024)
	targetConn.SetReadLimit(256 * 1024)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel()
		copyWS(ctx, clientConn, targetConn)
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		copyWS(ctx, targetConn, clientConn)
	}()
	wg.Wait()

	s.logger.Info("agent ws proxy: closed",
		"agent", agentID, "target", targetDeviceID)
}

// fencingAllowsAgentWrite returns true when the local peer is
// still the lock holder for the agent. Mirrors the request-level
// gate in auth.AgentFencingMiddleware but checks per chat-frame —
// the WS upgrade is a GET (read), the frames are writes.
//
// Fail-closed: a transient lock read error returns false so an
// unknown-holder write doesn't slip past the gate (the
// middleware itself returns 503 on lock-read failure; the WS
// surface returns false here and the caller surfaces an "agent
// busy elsewhere" error frame which the user can react to by
// reloading the page). A missing lock row (ErrNotFound) passes
// because no one has claimed the agent yet — same semantic as
// the request-level middleware.
func (s *Server) fencingAllowsAgentWrite(ctx context.Context, agentID string) bool {
	if s.peerID == nil || s.agents == nil || s.agents.Store() == nil {
		return true
	}
	lock, err := s.agents.Store().GetAgentLock(ctx, agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return true
		}
		s.logger.Warn("agent ws: fencing check failed; refusing frame",
			"agent", agentID, "err", err)
		return false
	}
	return lock.HolderPeer == "" || lock.HolderPeer == s.peerID.DeviceID
}

// copyWS forwards frames src → dst until either side returns an
// error. Preserves the original frame type (binary vs text) so
// JSON frames the local handler emits round-trip identically.
func copyWS(ctx context.Context, src, dst *websocket.Conn) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			_ = dst.Close(websocket.StatusNormalClosure, "peer closed")
			return
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			_ = src.Close(websocket.StatusNormalClosure, "peer closed")
			return
		}
	}
}
