//go:build heavy_test

// Heavy because the test pulls in store.Open + agent.Manager via the
// Server graph. Run with `go test -tags heavy_test ./internal/server/...`.

package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/store"
)

// newChangesTestServer wires a Server with an agent.Manager whose
// store is real, so InsertAgent / UpdateAgent / SoftDeleteAgent paths
// actually emit events. The Manager opens its own store internally;
// we get a handle back via mgr.Store() so the test can inject domain
// rows directly.
func newChangesTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	// agent.NewManager calls newStore which uses configdir.Resolve,
	// which honors KOJO_CONFIG_DIR. Point it at the test's temp dir
	// so each test gets a clean DB and we don't pollute the user's
	// real config dir.
	t.Setenv("KOJO_CONFIG_DIR", t.TempDir())

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr, err := agent.NewManager(logger)
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}
	t.Cleanup(func() { mgr.Shutdown() })

	st := mgr.Store()
	if st == nil {
		t.Fatal("agent.Manager has no store")
	}

	srv := New(Config{
		Addr:         ":0",
		Logger:       logger,
		Version:      "test",
		AgentManager: mgr,
	})
	return srv, st
}

func doJSON(srv *Server, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func TestChangesEmptyStateReturnsZeros(t *testing.T) {
	srv, _ := newChangesTestServer(t)
	w := doJSON(srv, http.MethodGet, "/api/v1/changes")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Events    []map[string]any `json:"events"`
		NextSince int64            `json:"next_since"`
		Watermark int64            `json:"watermark"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if len(body.Events) != 0 || body.NextSince != 0 || body.Watermark != 0 || body.Truncated {
		t.Errorf("expected zeros, got %+v", body)
	}
}

func TestChangesReturnsAgentInsertEvent(t *testing.T) {
	srv, st := newChangesTestServer(t)
	ctx := context.Background()
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{
		ID: "ag_x", Name: "Bob",
	}, store.AgentInsertOptions{}); err != nil {
		t.Fatalf("InsertAgent: %v", err)
	}

	w := doJSON(srv, http.MethodGet, "/api/v1/changes?table=agents")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Events []struct {
			Seq   int64  `json:"seq"`
			Table string `json:"table"`
			ID    string `json:"id"`
			ETag  string `json:"etag"`
			Op    string `json:"op"`
			TS    int64  `json:"ts"`
		} `json:"events"`
		NextSince int64 `json:"next_since"`
		Watermark int64 `json:"watermark"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if len(body.Events) != 1 {
		t.Fatalf("events len = %d, want 1; body=%s", len(body.Events), w.Body.String())
	}
	e := body.Events[0]
	if e.Table != "agents" || e.ID != "ag_x" || e.Op != "insert" || e.ETag == "" {
		t.Errorf("unexpected event: %+v", e)
	}
	if body.NextSince != e.Seq {
		t.Errorf("NextSince = %d, want %d", body.NextSince, e.Seq)
	}
	if body.Watermark <= 0 {
		t.Errorf("Watermark = %d, want >0", body.Watermark)
	}
}

func TestChangesRejectsBadParams(t *testing.T) {
	srv, _ := newChangesTestServer(t)
	for _, target := range []string{
		"/api/v1/changes?since=-1",
		"/api/v1/changes?since=abc",
		"/api/v1/changes?limit=0",
		"/api/v1/changes?limit=99999",
		"/api/v1/changes?limit=foo",
	} {
		w := doJSON(srv, http.MethodGet, target)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400; body=%s", target, w.Code, w.Body.String())
		}
	}
}

func TestChangesSinceNarrows(t *testing.T) {
	srv, st := newChangesTestServer(t)
	ctx := context.Background()
	a, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag1", Name: "A"}, store.AgentInsertOptions{})
	if err != nil {
		t.Fatalf("InsertAgent: %v", err)
	}
	if _, err := st.InsertAgent(ctx, &store.AgentRecord{ID: "ag2", Name: "B"}, store.AgentInsertOptions{}); err != nil {
		t.Fatalf("InsertAgent: %v", err)
	}
	// Find the seq of ag1's event so we can use it as ?since.
	res, err := st.ListEventsSince(ctx, 0, store.ListEventsSinceOptions{Table: "agents"})
	if err != nil {
		t.Fatalf("ListEventsSince: %v", err)
	}
	if len(res.Events) < 2 {
		t.Fatalf("setup: events=%d want >=2", len(res.Events))
	}
	var sinceSeq int64
	for _, e := range res.Events {
		if e.ID == a.ID {
			sinceSeq = e.Seq
			break
		}
	}
	if sinceSeq == 0 {
		t.Fatalf("could not find ag1 event")
	}

	w := doJSON(srv, http.MethodGet, "/api/v1/changes?table=agents&since="+itoa(sinceSeq))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Events []struct {
			ID string `json:"id"`
		} `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Events) != 1 || body.Events[0].ID != "ag2" {
		t.Errorf("expected only ag2 after sinceSeq, got %+v", body)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
