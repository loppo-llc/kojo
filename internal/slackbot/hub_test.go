package slackbot

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/slack-go/slack"
)

// --- shared test mocks ---

type mockTokens struct {
	tokens map[string]string
}

func (m *mockTokens) GetToken(_, _, _, key string) (string, error) {
	return m.tokens[key], nil
}
func (m *mockTokens) SetToken(_, _, _, _, _ string, _ time.Time) error { return nil }
func (m *mockTokens) DeleteToken(_, _, _, _ string) error              { return nil }
func (m *mockTokens) DeleteTokensBySource(_, _, _ string) error        { return nil }

type mockMgr struct{}

func (m *mockMgr) Chat(_ context.Context, _, _, _ string, _ []agent.MessageAttachment, _ ...agent.BusySource) (<-chan agent.ChatEvent, error) {
	ch := make(chan agent.ChatEvent, 1)
	ch <- agent.ChatEvent{Type: "done"}
	close(ch)
	return ch, nil
}

func (m *mockMgr) ChatOneShot(_ context.Context, _, _ string, _ ...string) (<-chan agent.ChatEvent, error) {
	ch := make(chan agent.ChatEvent, 1)
	ch <- agent.ChatEvent{Type: "done"}
	close(ch)
	return ch, nil
}

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// mockSlackServer returns an httptest.Server that handles common Slack API calls.
func mockSlackServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			fmt.Fprintf(w, `{"ok":true,"user_id":"UBOTTEST","user":"testbot","team":"TestTeam","team_id":"T1"}`)
		case "/users.info":
			_ = r.ParseForm()
			uid := r.FormValue("user")
			switch uid {
			case "U123":
				fmt.Fprintf(w, `{"ok":true,"user":{"id":"U123","name":"alice","profile":{"display_name":"Alice"}}}`)
			case "U456":
				fmt.Fprintf(w, `{"ok":true,"user":{"id":"U456","name":"bob","profile":{},"real_name":"Bob Real"}}`)
			default:
				fmt.Fprintf(w, `{"ok":false,"error":"user_not_found"}`)
			}
		default:
			fmt.Fprintf(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestHub(t *testing.T, tokens map[string]string) *Hub {
	t.Helper()
	srv := mockSlackServer(t)
	var tp TokenProvider
	if tokens != nil {
		tp = &mockTokens{tokens: tokens}
	}
	h := NewHub(&mockMgr{}, tp, func(id string) string { return t.TempDir() }, testLogger)
	h.slackOpts = []slack.Option{slack.OptionAPIURL(srv.URL + "/")}
	return h
}

func validTokens() map[string]string {
	return map[string]string{
		"app_token": "xapp-test-123",
		"bot_token": "xoxb-test-456",
	}
}

// --- Hub tests ---

func TestHubStartStopBot(t *testing.T) {
	h := newTestHub(t, validTokens())
	defer h.Stop()

	h.StartBot("a1", agent.SlackBotConfig{Enabled: true})

	if !h.IsRunning("a1") {
		t.Fatal("expected a1 to be running after StartBot")
	}

	h.StopBot("a1")

	if h.IsRunning("a1") {
		t.Fatal("expected a1 to not be running after StopBot")
	}
}

func TestHubIsRunningNonexistent(t *testing.T) {
	h := newTestHub(t, validTokens())
	defer h.Stop()

	if h.IsRunning("nobody") {
		t.Fatal("non-existent bot should not be running")
	}
}

func TestHubStopBotNonexistent(t *testing.T) {
	h := newTestHub(t, validTokens())
	defer h.Stop()

	// Should not panic.
	h.StopBot("nobody")
}

func TestHubMissingTokens(t *testing.T) {
	h := newTestHub(t, map[string]string{})
	defer h.Stop()

	h.StartBot("a1", agent.SlackBotConfig{Enabled: true})

	if h.IsRunning("a1") {
		t.Fatal("bot should not be running without tokens")
	}
}

func TestHubNilTokenProvider(t *testing.T) {
	h := NewHub(&mockMgr{}, nil, func(id string) string { return "/tmp" }, testLogger)
	defer h.Stop()

	h.StartBot("a1", agent.SlackBotConfig{Enabled: true})

	if h.IsRunning("a1") {
		t.Fatal("bot should not be running without token provider")
	}
}

func TestHubReconfigure(t *testing.T) {
	h := newTestHub(t, validTokens())
	defer h.Stop()

	// Reconfigure with enabled → starts bot.
	h.Reconfigure("a1", agent.SlackBotConfig{Enabled: true})
	if !h.IsRunning("a1") {
		t.Fatal("bot should be running after Reconfigure(enabled=true)")
	}

	// Reconfigure with disabled → stops bot.
	h.Reconfigure("a1", agent.SlackBotConfig{Enabled: false})
	if h.IsRunning("a1") {
		t.Fatal("bot should not be running after Reconfigure(enabled=false)")
	}
}

func TestHubRestartOnDuplicateStart(t *testing.T) {
	h := newTestHub(t, validTokens())
	defer h.Stop()

	h.StartBot("a1", agent.SlackBotConfig{Enabled: true})
	if !h.IsRunning("a1") {
		t.Fatal("bot should be running")
	}

	// Starting again should stop old and start new — no panic.
	h.StartBot("a1", agent.SlackBotConfig{Enabled: true})
	if !h.IsRunning("a1") {
		t.Fatal("bot should still be running after restart")
	}
}

func TestHubStopAll(t *testing.T) {
	h := newTestHub(t, validTokens())

	h.StartBot("a1", agent.SlackBotConfig{Enabled: true})
	h.StartBot("a2", agent.SlackBotConfig{Enabled: true})

	if !h.IsRunning("a1") || !h.IsRunning("a2") {
		t.Fatal("both bots should be running")
	}

	h.Stop()

	// After Stop, IsRunning should return false (hub is shut down).
	if h.IsRunning("a1") || h.IsRunning("a2") {
		t.Fatal("bots should not be running after Hub.Stop()")
	}
}

func TestHubSendAfterStop(t *testing.T) {
	h := newTestHub(t, validTokens())
	h.Stop()

	// All operations after Stop should be safe — no panic, no deadlock.
	h.StartBot("a1", agent.SlackBotConfig{Enabled: true})
	h.StopBot("a1")
	h.Reconfigure("a1", agent.SlackBotConfig{Enabled: true})

	if h.IsRunning("a1") {
		t.Fatal("should not be running after hub stopped")
	}
}
