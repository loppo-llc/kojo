package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestChecker_CheckNow_available(t *testing.T) {
	t.Parallel()
	srv := releaseAPIServer(t, "v0.2.0")
	t.Cleanup(srv.Close)

	c := NewClient("v0.1.0")
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()

	h := &captureHandler{}
	ch := NewChecker(c, "v0.1.0", slog.New(h))

	st, err := ch.CheckNow(context.Background())
	if err != nil {
		t.Fatalf("CheckNow: %v", err)
	}
	if !st.UpdateAvailable || st.Latest != "v0.2.0" || st.Current != "v0.1.0" {
		t.Fatalf("status = %+v", st)
	}
	if ch.Status().Latest != "v0.2.0" {
		t.Fatalf("stored status = %+v", ch.Status())
	}
	if !h.hasInfo("kojo update available") {
		t.Fatalf("expected Info log on first sighting; records = %v", h.records)
	}

	// Second check of the same tag: still available, but Debug not Info.
	h.reset()
	st2, err := ch.CheckNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st2.UpdateAvailable {
		t.Fatal("expected still available")
	}
	if h.hasInfo("kojo update available") {
		t.Fatal("second sighting must not Info again")
	}
	if !h.hasDebug("kojo update available") {
		t.Fatalf("expected Debug on repeat; records = %v", h.records)
	}
}

func TestChecker_CheckNow_notAvailable(t *testing.T) {
	t.Parallel()
	srv := releaseAPIServer(t, "v0.1.0")
	t.Cleanup(srv.Close)

	c := NewClient("v0.1.0")
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()
	ch := NewChecker(c, "v0.1.0", slog.New(slog.DiscardHandler))

	st, err := ch.CheckNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.UpdateAvailable {
		t.Fatalf("UpdateAvailable = true, want false: %+v", st)
	}
}

func TestChecker_CheckNow_fetchFailureKeepsLast(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		shouldFail := fail
		mu.Unlock()
		if shouldFail {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(Release{
			TagName: "v0.3.0",
			HTMLURL: "https://example.com/v0.3.0",
		})
	}))
	t.Cleanup(srv.Close)

	c := NewClient("v0.1.0")
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()
	ch := NewChecker(c, "v0.1.0", slog.New(slog.DiscardHandler))

	if _, err := ch.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	good := ch.Status()

	mu.Lock()
	fail = true
	mu.Unlock()
	if _, err := ch.CheckNow(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if got := ch.Status(); got.Latest != good.Latest || got.CheckedAt != good.CheckedAt {
		t.Fatalf("last status clobbered: got %+v want %+v", got, good)
	}
}

func TestChecker_Apply_upToDate(t *testing.T) {
	t.Parallel()
	srv := releaseAPIServer(t, "v0.1.0")
	t.Cleanup(srv.Close)

	c := NewClient("v0.1.0")
	c.BaseURL = srv.URL
	c.HTTPClient = srv.Client()
	ch := NewChecker(c, "v0.1.0", slog.New(slog.DiscardHandler))

	_, err := ch.Apply(context.Background())
	if !errors.Is(err, ErrUpToDate) {
		t.Fatalf("Apply err = %v, want ErrUpToDate", err)
	}
}

func releaseAPIServer(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/loppo-llc/kojo/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Release{
			TagName: tag,
			HTMLURL: "https://github.com/loppo-llc/kojo/releases/tag/" + tag,
		})
	}))
}

// captureHandler records slog records for notify-once assertions.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = nil
}

func (h *captureHandler) hasInfo(msg string) bool {
	return h.hasLevel(slog.LevelInfo, msg)
}

func (h *captureHandler) hasDebug(msg string) bool {
	return h.hasLevel(slog.LevelDebug, msg)
}

func (h *captureHandler) hasLevel(level slog.Level, msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == level && r.Message == msg {
			return true
		}
	}
	return false
}
