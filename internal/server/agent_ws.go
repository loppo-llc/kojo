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
		OriginPatterns: []string{"100.*.*.*", "*.ts.net", "localhost:*", "127.0.0.1:*"},
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

	// If the agent has a chat running in the background (e.g. user navigated
	// away and came back), notify the client and resume streaming live events.
	var bgEvents <-chan agent.ChatEvent
	if since, events, busy := s.agents.BusyState(agentID); busy {
		statusMsg := map[string]any{
			"type":      "status",
			"status":    "thinking",
			"startedAt": since.UTC().Format(time.RFC3339),
		}
		_ = writeJSON(ctx, conn, statusMsg)

		if events != nil {
			// Drain stale buffered events so the client only sees live
			// updates from this point forward. If "done" is already in
			// the buffer, deliver it immediately and skip live streaming.
			sentTerminal := false
		drained := false
		drainLoop:
			for {
				select {
				case ev, ok := <-events:
					if !ok {
						// Channel closed — agent already finished.
						drained = true
						break drainLoop
					}
					if ev.Type == "done" || ev.Type == "error" {
						_ = writeJSON(ctx, conn, ev)
						sentTerminal = true
						drained = true
						break drainLoop
					}
					// Skip stale streaming events (text deltas, tool_use, etc.)
				default:
					// Buffer empty — remaining events are live.
					break drainLoop
				}
			}
			if !drained {
				bgEvents = events
			} else if !sentTerminal {
				// Channel closed without terminal event — load final message.
				msgs, _ := s.agents.Messages(agentID, 1)
				if len(msgs) > 0 && msgs[len(msgs)-1].Role == "assistant" {
					_ = writeJSON(ctx, conn, agent.ChatEvent{
						Type:    "done",
						Message: msgs[len(msgs)-1],
					})
				} else {
					_ = writeJSON(ctx, conn, agent.ChatEvent{Type: "done"})
				}
			}
		} else {
			// Fallback: no event channel available, poll for completion.
			ch := make(chan agent.ChatEvent, 1)
			bgEvents = ch
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
							var ev agent.ChatEvent
							msgs, _ := s.agents.Messages(agentID, 1)
							if len(msgs) > 0 && msgs[len(msgs)-1].Role == "assistant" {
								ev = agent.ChatEvent{
									Type:    "done",
									Message: msgs[len(msgs)-1],
								}
							} else {
								ev = agent.ChatEvent{Type: "done"}
							}
							select {
							case ch <- ev:
							case <-ctx.Done():
							}
							return
						}
					}
				}
			}()
		}
	}

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
				// Channel closed — agent finished before we could read "done".
				// Load the latest assistant message as a fallback.
				bgEvents = nil
				msgs, _ := s.agents.Messages(agentID, 1)
				if len(msgs) > 0 && msgs[len(msgs)-1].Role == "assistant" {
					_ = writeJSON(ctx, conn, agent.ChatEvent{
						Type:    "done",
						Message: msgs[len(msgs)-1],
					})
				} else {
					_ = writeJSON(ctx, conn, agent.ChatEvent{Type: "done"})
				}
				continue
			}
			_ = writeJSON(ctx, conn, event)
			if event.Type == "done" || event.Type == "error" {
				bgEvents = nil // terminal event, stop reading
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
				return // channel closed
			}
			if err := writeJSON(ctx, conn, event); err != nil {
				// Write failed (WS disconnected) — let chat continue.
				return
			}
		case msg := <-clientMsgs:
			if msg.Type == "abort" {
				s.agents.Abort(agentID)
				// Drain remaining events with timeout
				drainTimer := time.NewTimer(10 * time.Second)
				defer drainTimer.Stop()
			drainLoop:
				for {
					select {
					case _, ok := <-events:
						if !ok {
							break drainLoop
						}
					case <-drainTimer.C:
						break drainLoop
					}
				}
				return
			}
			// Ignore other messages while streaming
		}
	}
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

// sanitizeFilename removes control characters and newlines from a filename.
func sanitizeFilename(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
}
