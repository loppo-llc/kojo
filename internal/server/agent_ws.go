package server

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"github.com/loppo-llc/kojo/internal/agent"
)

// Agent WebSocket message types
type agentWSClientMsg struct {
	Type        string                  `json:"type"`                  // "message", "abort"
	Content     string                  `json:"content"`               // for "message" type
	Attachments []agent.MessageAttachment `json:"attachments,omitempty"` // file attachments
}

func (s *Server) handleAgentWebSocket(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing agent id")
		return
	}

	if _, ok := s.agents.Get(agentID); !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent not found: "+agentID)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: wsOriginPatterns,
	})
	if err != nil {
		s.logger.Error("agent websocket accept failed", "err", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(256 * 1024) // 256KB max for chat messages

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	s.logger.Info("agent websocket connected", "agent", agentID)

	bgEvents, bgUnsub := s.resumeBackgroundChat(ctx, conn, agentID)
	defer func() {
		if bgUnsub != nil {
			bgUnsub()
		}
	}()

	// Watch for new chats started by cron/notify while this WS is idle.
	// (Slack-driven chats use ChatOneShot and intentionally do not trigger
	// this signal — they have their own platform transcript.)
	chatStarted, unwatchChat := s.agents.WatchChatStart(agentID)
	defer unwatchChat()

	// Channel for client messages (read goroutine → main loop)
	clientMsgs := make(chan agentWSClientMsg, 8)

	// Keepalive ping
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
				if err := conn.Ping(pingCtx); err != nil {
					pingCancel()
					cancel()
					return
				}
				pingCancel()
			}
		}
	}()

	// Read goroutine: continuously reads from client, decoupled from write
	go func() {
		defer cancel()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}

			var msg agentWSClientMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				s.logger.Debug("invalid agent ws message", "err", err)
				continue
			}

			select {
			case clientMsgs <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Main loop: process client messages and stream events
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-bgEvents:
			if !ok {
				bgEvents = nil
				_ = writeJSON(ctx, conn, s.synthesizeTerminal(agentID))
				continue
			}
			_ = writeJSON(ctx, conn, event)
			if event.Type == "done" || event.Type == "error" {
				bgEvents = nil // terminal event, stop reading
			}
		case <-chatStarted:
			// A new chat was started (e.g. by cron or notification) while idle.
			if bgEvents != nil {
				continue // already streaming a chat
			}
			newBgEvents, newBgUnsub := s.resumeBackgroundChat(ctx, conn, agentID)
			if newBgEvents != nil {
				bgEvents = newBgEvents
				if bgUnsub != nil {
					bgUnsub()
				}
				bgUnsub = newBgUnsub
			}
		case msg := <-clientMsgs:
			switch msg.Type {
			case "message":
				if msg.Content == "" && len(msg.Attachments) == 0 {
					continue
				}

				// Validate attachments: paths must be inside uploadDir and exist on disk.
				validatedAtts := validateAttachments(msg.Attachments)

				// Reject empty messages after validation
				if msg.Content == "" && len(validatedAtts) == 0 {
					continue
				}

				// §3.7 per-frame fencing: the WS upgrade is a
				// GET so AgentFencingMiddleware lets it pass,
				// but every "message" frame here triggers a
				// Chat that writes to the agent's tables.
				// Re-check the lock holder so a handoff that
				// landed mid-conversation refuses follow-up
				// turns on this peer instead of silently
				// writing under the new holder's nose.
				if !s.fencingAllowsAgentWrite(ctx, agentID) {
					_ = writeJSON(ctx, conn, map[string]string{
						"type":         "error",
						"errorMessage": "agent_lock is held by another peer; reload to reconnect to the holder",
					})
					continue
				}

				// Check if agent is busy
				if s.agents.IsBusy(agentID) {
					_ = writeJSON(ctx, conn, map[string]string{
						"type":         "error",
						"errorMessage": "agent is busy",
					})
					continue
				}

				// Send "thinking" status
				_ = writeJSON(ctx, conn, map[string]string{
					"type":   "status",
					"status": "thinking",
				})

				// Use background context for chat so it survives WebSocket disconnects.
				// The response is saved to transcript even if the client navigates away.
				events, err := s.agents.Chat(context.Background(), agentID, msg.Content, "user", validatedAtts)
				if err != nil {
					_ = writeJSON(ctx, conn, map[string]string{
						"type":         "error",
						"errorMessage": err.Error(),
					})
					continue
				}

				// Stream events to client, while also listening for abort
				s.streamAgentEvents(ctx, conn, events, agentID, clientMsgs)

			case "abort":
				s.agents.Abort(agentID)
				if bgEvents != nil {
					terminal := drainAfterAbort(ctx, bgEvents)
					bgEvents = nil
					if bgUnsub != nil {
						bgUnsub()
						bgUnsub = nil
					}
					if terminal != nil {
						_ = writeJSON(ctx, conn, *terminal)
					} else {
						_ = writeJSON(ctx, conn, agent.ChatEvent{Type: "done"})
					}
				}
			}
		}
	}
}

// streamAgentEvents streams chat events to the WebSocket while allowing
// abort messages to be processed concurrently.
func (s *Server) streamAgentEvents(
	ctx context.Context,
	conn *websocket.Conn,
	events <-chan agent.ChatEvent,
	agentID string,
	clientMsgs <-chan agentWSClientMsg,
) {
	for {
		select {
		case <-ctx.Done():
			// WebSocket disconnected — let chat continue in background.
			// Don't abort: the response will be saved to transcript.
			return
		case event, ok := <-events:
			if !ok {
				_ = writeJSON(ctx, conn, s.synthesizeTerminal(agentID))
				return
			}
			if err := writeJSON(ctx, conn, event); err != nil {
				// Write failed (WS disconnected) — let chat continue.
				return
			}
			if event.Type == "done" || event.Type == "error" {
				return
			}
		case msg := <-clientMsgs:
			if msg.Type == "abort" {
				s.agents.Abort(agentID)
				terminal := drainAfterAbort(ctx, events)
				if terminal != nil {
					_ = writeJSON(ctx, conn, *terminal)
				} else {
					_ = writeJSON(ctx, conn, agent.ChatEvent{Type: "done"})
				}
				return
			}
			// Ignore other messages while streaming
		}
	}
}

// synthesizeTerminal creates a terminal event from the transcript when the
// broadcaster's channel closed without delivering one. It searches recent
// messages for the latest assistant message (skipping trailing system error
// messages that may have been appended after the response). If no assistant
// message is found, it falls back to the most recent persisted system error.
func (s *Server) synthesizeTerminal(agentID string) agent.ChatEvent {
	msgs, _ := s.agents.Messages(agentID, 5)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return agent.ChatEvent{Type: "done", Message: msgs[i]}
		}
	}
	// No assistant message found — look for a persisted system error
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "system" && strings.HasPrefix(msgs[i].Content, "⚠️ Error: ") {
			errText := strings.TrimPrefix(msgs[i].Content, "⚠️ Error: ")
			return agent.ChatEvent{Type: "error", ErrorMessage: errText}
		}
	}
	return agent.ChatEvent{Type: "error", ErrorMessage: "chat ended unexpectedly"}
}

// validateAttachments checks that each attachment path is inside the upload
// directory and exists on disk, then rebuilds metadata from the file system.
// Any attachment that fails validation is silently dropped.
func validateAttachments(atts []agent.MessageAttachment) []agent.MessageAttachment {
	if len(atts) == 0 {
		return nil
	}

	// Resolve uploadDir once (handles /tmp → /private/tmp on macOS)
	canonicalUploadDir, err := filepath.EvalSymlinks(uploadDir)
	if err != nil {
		canonicalUploadDir = uploadDir
	}

	result := make([]agent.MessageAttachment, 0, len(atts))
	for _, a := range atts {
		// Resolve to absolute, canonical path
		resolved, err := filepath.Abs(a.Path)
		if err != nil {
			continue
		}
		resolved, err = filepath.EvalSymlinks(resolved)
		if err != nil {
			continue
		}

		// Must be inside the upload directory
		if !strings.HasPrefix(resolved, canonicalUploadDir+string(filepath.Separator)) && resolved != canonicalUploadDir {
			continue
		}

		// Must exist
		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() {
			continue
		}

		// Derive metadata from disk, not client. Strip control characters
		// from filenames to prevent prompt injection via crafted names.
		name := sanitizeFilename(filepath.Base(resolved))
		mimeType := mime.TypeByExtension(filepath.Ext(name))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}

		result = append(result, agent.MessageAttachment{
			Path: resolved,
			Name: name,
			Size: info.Size(),
			Mime: mimeType,
		})
	}
	return result
}

// resumeBackgroundChat checks for an active chat and sets up live event
// streaming if one is running. Returns the event channel and unsubscribe func.
func (s *Server) resumeBackgroundChat(
	ctx context.Context,
	conn *websocket.Conn,
	agentID string,
) (<-chan agent.ChatEvent, func()) {
	since, past, live, unsub, busy := s.agents.Subscribe(agentID)
	if !busy {
		return nil, nil
	}

	_ = writeJSON(ctx, conn, map[string]any{
		"type":      "status",
		"status":    "thinking",
		"startedAt": since.Format(time.RFC3339),
	})

	// Replay past events so the client catches up.
	sentTerminal := false
	for _, ev := range past {
		_ = writeJSON(ctx, conn, ev)
		if ev.Type == "done" || ev.Type == "error" {
			sentTerminal = true
			break
		}
	}

	if sentTerminal {
		unsub()
		return nil, nil
	}
	if live != nil {
		return live, unsub
	}

	// Fallback: no broadcaster available, poll for completion.
	unsub()
	ch := make(chan agent.ChatEvent, 1)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !s.agents.IsBusy(agentID) {
					ev := s.synthesizeTerminal(agentID)
					select {
					case ch <- ev:
					case <-ctx.Done():
					}
					return
				}
			}
		}
	}()
	return ch, nil
}

// drainAfterAbort reads from an event channel after abort, waiting up to 10s
// for a terminal event. Returns the terminal event if one was received.
func drainAfterAbort(ctx context.Context, events <-chan agent.ChatEvent) *agent.ChatEvent {
	drainTimer := time.NewTimer(10 * time.Second)
	defer drainTimer.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if ev.Type == "done" || ev.Type == "error" {
				return &ev
			}
		case <-drainTimer.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// sanitizeFilename removes control characters and newlines from a filename.
func sanitizeFilename(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
}
