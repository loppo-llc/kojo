package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/session"
)

// WebSocket message types
type WSMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

type WSOutputMsg struct {
	Type string `json:"type"`
	Data string `json:"data"` // base64
}

type WSExitMsg struct {
	Type     string `json:"type"`
	ExitCode int    `json:"exitCode"`
	Live     bool   `json:"live"`
}

type WSScrollbackMsg struct {
	Type string `json:"type"`
	Data string `json:"data"` // base64
}

type WSInputMsg struct {
	Type string `json:"type"`
	Data string `json:"data"` // base64
}

type WSResizeMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

type WSYoloDebugMsg struct {
	Type string `json:"type"`
	Tail string `json:"tail"`
}

type WSAttachmentMsg struct {
	Type        string                `json:"type"`
	Attachments []*session.Attachment `json:"attachments"`
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing session parameter")
		return
	}

	// Peer-routed attach: the UI carries the session's home peer
	// in `?peer=` so the Hub knows where to forward the WS.
	// Self / empty falls through to the local handler. Loop
	// prevention: a RolePeer-signed inbound WS upgrade must NOT
	// re-proxy.
	if pid := r.URL.Query().Get("peer"); pid != "" && s.peerID != nil && pid != s.peerID.DeviceID {
		if !isPeerWS(r) {
			s.proxySessionWebSocket(w, r, sessionID, pid)
			return
		}
	}

	sess, ok := s.sessions.Get(sessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found: "+sessionID)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: wsOriginPatterns,
	})
	if err != nil {
		s.logger.Error("websocket accept failed", "err", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 * 1024) // 64KB max for terminal input

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	s.logger.Info("websocket connected", "session", sessionID)

	// subscribe to session output
	ch, scrollback := sess.Subscribe()
	defer sess.Unsubscribe(ch)

	var yoloCh chan string
	if s.devMode {
		yoloCh = sess.SubscribeYoloDebug()
		defer sess.UnsubscribeYoloDebug(yoloCh)
	}

	attachCh := sess.SubscribeAttachments()
	defer sess.UnsubscribeAttachments(attachCh)

	// send scrollback
	if len(scrollback) > 0 {
		msg := WSScrollbackMsg{
			Type: "scrollback",
			Data: base64.StdEncoding.EncodeToString(scrollback),
		}
		if err := writeJSON(ctx, conn, msg); err != nil {
			return
		}
	}

	// send existing attachments
	if atts := sess.Attachments(); len(atts) > 0 {
		msg := WSAttachmentMsg{
			Type:        "attachment",
			Attachments: atts,
		}
		if err := writeJSON(ctx, conn, msg); err != nil {
			return
		}
	}

	// if session already exited, send non-live exit and return
	select {
	case <-sess.Done():
		info := sess.Info()
		exitCode := 0
		if info.ExitCode != nil {
			exitCode = *info.ExitCode
		}
		_ = writeJSON(ctx, conn, WSExitMsg{
			Type:     "exit",
			ExitCode: exitCode,
			Live:     false,
		})
		return
	default:
	}

	// read from client
	go s.wsReadLoop(ctx, cancel, conn, sess)

	// keepalive: ping every 30s to detect dead connections on mobile
	go s.wsPingLoop(ctx, cancel, conn)

	// write to client
	s.wsWriteLoop(ctx, conn, sess, ch, yoloCh, attachCh)
}

func (s *Server) wsPingLoop(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn) {
	defer cancel()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				s.logger.Debug("websocket ping failed", "err", err)
				return
			}
		}
	}
}

func (s *Server) wsReadLoop(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, sess *session.Session) {
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		var msg WSMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			s.logger.Debug("invalid ws message", "err", err)
			continue
		}

		switch msg.Type {
		case "input":
			var input WSInputMsg
			if err := json.Unmarshal(data, &input); err != nil {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(input.Data)
			if err != nil {
				continue
			}
			if _, err := sess.Write(decoded); err != nil {
				s.logger.Debug("pty write error", "err", err)
			}

		case "resize":
			var resize WSResizeMsg
			if err := json.Unmarshal(data, &resize); err != nil {
				continue
			}
			if err := sess.Resize(uint16(resize.Cols), uint16(resize.Rows)); err != nil {
				s.logger.Debug("pty resize error", "err", err)
			}

		default:
			s.logger.Debug("unknown ws message type", "type", msg.Type)
		}
	}
}

func (s *Server) wsWriteLoop(ctx context.Context, conn *websocket.Conn, sess *session.Session, ch chan []byte, yoloCh chan string, attachCh chan []*session.Attachment) {
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			// Coalesce: drain pending chunks into a single message to avoid
			// splitting ANSI escape sequences across WebSocket frames.
			// Cap at 256KB to prevent unbounded memory growth.
			const maxCoalesceBytes = 256 * 1024
		drain:
			for len(data) < maxCoalesceBytes {
				select {
				case more, ok := <-ch:
					if !ok {
						break drain
					}
					data = append(data, more...)
				default:
					break drain
				}
			}
			msg := WSOutputMsg{
				Type: "output",
				Data: base64.StdEncoding.EncodeToString(data),
			}
			if err := writeJSON(ctx, conn, msg); err != nil {
				return
			}
		case tail := <-yoloCh:
			msg := WSYoloDebugMsg{
				Type: "yolo_debug",
				Tail: tail,
			}
			if err := writeJSON(ctx, conn, msg); err != nil {
				return
			}
		case attachments := <-attachCh:
			msg := WSAttachmentMsg{
				Type:        "attachment",
				Attachments: attachments,
			}
			if err := writeJSON(ctx, conn, msg); err != nil {
				return
			}
		case <-sess.Done():
			info := sess.Info()
			exitCode := 0
			if info.ExitCode != nil {
				exitCode = *info.ExitCode
			}
			msg := WSExitMsg{
				Type:     "exit",
				ExitCode: exitCode,
				Live:     true,
			}
			_ = writeJSON(ctx, conn, msg)
			return
		}
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

// isPeerWS sniffs the inbound WS upgrade for a peer-auth signature
// header so the proxy router doesn't re-proxy a peer-signed
// request. We can't use auth.FromContext for the principal because
// AuthMiddleware may not have run on the WS upgrade path on every
// listener configuration; checking the header directly is the
// minimal, correct loop guard.
func isPeerWS(r *http.Request) bool {
	return r.Header.Get("X-Kojo-Peer-Sig") != ""
}

// proxySessionWebSocket dials the target peer's
// `/api/v1/ws?session=<id>` with peer-auth headers and pipes
// frames between the inbound (UI) and outbound (peer) sockets
// until either side closes. Mirrors proxyAgentWebSocket — same
// trust model, same nonce-replay defence.
func (s *Server) proxySessionWebSocket(w http.ResponseWriter, r *http.Request, sessionID, targetDeviceID string) {
	if s.agents == nil || s.agents.Store() == nil || s.peerID == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"peer routing not available on this host")
		return
	}
	targetRec, err := s.agents.Store().GetPeer(r.Context(), targetDeviceID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"target peer not in registry: "+err.Error())
		return
	}
	addr, err := peer.NormalizeAddress(targetRec.URL)
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
	targetURL.Path = "/api/v1/ws"
	q := targetURL.Query()
	q.Set("session", sessionID)
	targetURL.RawQuery = q.Encode()

	upgrade, err := http.NewRequestWithContext(r.Context(),
		http.MethodGet, targetURL.String(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	targetConn, _, err := websocket.Dial(r.Context(), targetURL.String(),
		&websocket.DialOptions{
			HTTPHeader: upgrade.Header,
			HTTPClient: peer.NoKeepAliveHTTPClient(10 * time.Second),
		})
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_gateway",
			"connect to peer: "+err.Error())
		return
	}
	defer targetConn.CloseNow()

	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: wsOriginPatterns,
	})
	if err != nil {
		s.logger.Error("session ws proxy: accept inbound failed", "session", sessionID, "err", err)
		return
	}
	defer clientConn.CloseNow()
	clientConn.SetReadLimit(256 * 1024)
	targetConn.SetReadLimit(256 * 1024)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); defer cancel(); copyWS(ctx, clientConn, targetConn) }()
	go func() { defer wg.Done(); defer cancel(); copyWS(ctx, targetConn, clientConn) }()
	wg.Wait()
}
