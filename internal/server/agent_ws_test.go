package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/loppo-llc/kojo/internal/agent"
)

// TestAgentWebSocket_HeartbeatEmitted verifies the server pushes the
// application-level heartbeat frame ({"type":"ping",...}) on an open,
// idle agent chat WS. This is the frame the client watchdog times out
// on to recover from a proxy-black-holed zombie socket.
func TestAgentWebSocket_HeartbeatEmitted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")

	// Shrink the heartbeat so the test isn't gated on the 20s prod
	// cadence. Restored after the server is torn down (Cleanup is LIFO,
	// so this runs after ts.Close waits for the handler to exit).
	orig := agentHeartbeatInterval
	t.Cleanup(func() { agentHeartbeatInterval = orig })
	agentHeartbeatInterval = 50 * time.Millisecond

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr, err := agent.NewManager(logger)
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	disableCron := ""
	a, err := mgr.Create(agent.AgentConfig{Name: "Alice", Tool: "claude", CronExpr: &disableCron})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	srv := &Server{agents: mgr, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/{id}/ws", srv.handleAgentWebSocket)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/agents/" + a.ID + "/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// The agent is idle (not busy), so resumeBackgroundChat sends
	// nothing and the only frames on the wire are heartbeats. Read one
	// and assert it's the application-level ping.
	readCtx, cancelRead := context.WithTimeout(ctx, 3*time.Second)
	defer cancelRead()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read heartbeat: %v", err)
	}

	var msg struct {
		Type string `json:"type"`
		T    int64  `json:"t"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal %q: %v", data, err)
	}
	if msg.Type != "ping" {
		t.Fatalf("frame type = %q, want %q", msg.Type, "ping")
	}
	if msg.T == 0 {
		t.Fatalf("heartbeat timestamp t not set")
	}

	conn.Close(websocket.StatusNormalClosure, "done")
}
