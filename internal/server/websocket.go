package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
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

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing session parameter")
		return
	}

	sess, ok := s.sessions.Get(sessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found: "+sessionID)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"100.*.*.*", "*.ts.net", "localhost:*", "127.0.0.1:*"},
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
	s.wsWriteLoop(ctx, conn, sess, ch, yoloCh)
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

func (s *Server) wsWriteLoop(ctx context.Context, conn *websocket.Conn, sess *session.Session, ch chan []byte, yoloCh chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-ch:
			if !ok {
				return
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

