package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

// fakeLockStore is the minimal AgentFencingStore impl used by the
// middleware test. It returns a configurable lock for one agent and
// ErrNotFound for everything else, so each subtest can pin one
// holder relationship without standing up SQLite.
type fakeLockStore struct {
	holderByID map[string]string
	failErr    error // when set, GetAgentLock returns this regardless of agentID
}

func (f *fakeLockStore) GetAgentLock(_ context.Context, agentID string) (*store.AgentLockRecord, error) {
	if f.failErr != nil {
		return nil, f.failErr
	}
	holder, ok := f.holderByID[agentID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &store.AgentLockRecord{
		AgentID:    agentID,
		HolderPeer: holder,
	}, nil
}

func newFencingHandler(t *testing.T, st *fakeLockStore, selfPeerID string, p Principal) http.Handler {
	t.Helper()
	innerCalled := false
	t.Cleanup(func() { _ = innerCalled }) // silence unused if not flipped
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerCalled = true
		w.WriteHeader(http.StatusOK)
	})
	mw := AgentFencingMiddleware(st, selfPeerID, nil)
	// Inject principal into ctx so the middleware sees a real
	// agent — production wires this through AuthMiddleware.
	withCtx := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(WithPrincipal(r.Context(), p))
		mw(inner).ServeHTTP(w, r)
	})
	return withCtx
}

func TestAgentFencing_PassesWhenLockHolderIsSelf(t *testing.T) {
	st := &fakeLockStore{holderByID: map[string]string{"ag_x": "peer-self"}}
	h := newFencingHandler(t, st, "peer-self",
		Principal{Role: RoleAgent, AgentID: "ag_x"})

	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/ag_x/messages", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestAgentFencing_BlocksWhenHolderIsOtherPeer(t *testing.T) {
	st := &fakeLockStore{holderByID: map[string]string{"ag_x": "peer-tgt"}}
	h := newFencingHandler(t, st, "peer-self",
		Principal{Role: RoleAgent, AgentID: "ag_x"})

	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/ag_x/messages", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "wrong_holder") {
		t.Errorf("body should contain wrong_holder: %s", w.Body.String())
	}
}

func TestAgentFencing_PassesReadMethods(t *testing.T) {
	// Read methods must always pass — they observe transient state
	// without writing.
	st := &fakeLockStore{holderByID: map[string]string{"ag_x": "peer-tgt"}}
	h := newFencingHandler(t, st, "peer-self",
		Principal{Role: RoleAgent, AgentID: "ag_x"})

	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		r := httptest.NewRequest(m, "/api/v1/agents/ag_x/messages", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("%s should pass, got %d", m, w.Code)
		}
	}
}

func TestAgentFencing_PassesHandoffSwitch(t *testing.T) {
	// The agent-self orchestrator endpoint MUST pass even when
	// holder differs (in practice it doesn't yet, but the route
	// is the one that moves the lock away — exempting it avoids
	// a chicken-and-egg refusal during the migration call).
	st := &fakeLockStore{holderByID: map[string]string{"ag_x": "peer-tgt"}}
	h := newFencingHandler(t, st, "peer-self",
		Principal{Role: RoleAgent, AgentID: "ag_x"})

	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/ag_x/handoff/switch", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestAgentFencing_PassesWhenNoLockExists(t *testing.T) {
	// v1: ErrNotFound means AgentLockGuard hasn't Acquired yet.
	// Pass through rather than block a fresh agent.
	st := &fakeLockStore{} // empty map → ErrNotFound for everyone
	h := newFencingHandler(t, st, "peer-self",
		Principal{Role: RoleAgent, AgentID: "ag_new"})

	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/ag_new/messages", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestAgentFencing_ReturnsServiceUnavailableOnStoreError(t *testing.T) {
	st := &fakeLockStore{failErr: errors.New("db wedged")}
	h := newFencingHandler(t, st, "peer-self",
		Principal{Role: RoleAgent, AgentID: "ag_x"})

	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/ag_x/messages", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

func TestAgentFencing_PassesNonAgentPrincipals(t *testing.T) {
	// Owner must pass without the middleware consulting the lock
	// at all — they're admin. fakeLockStore with failErr ensures
	// a hypothetical GetAgentLock call would error, proving the
	// middleware short-circuited before reaching it.
	st := &fakeLockStore{failErr: errors.New("should not be called")}
	for _, p := range []Principal{
		{Role: RoleOwner},
		{Role: RolePeer, PeerID: "some-peer"},
		{Role: RoleWebDAV},
		{Role: RoleGuest},
	} {
		h := newFencingHandler(t, st, "peer-self", p)
		r := httptest.NewRequest(http.MethodPost,
			"/api/v1/agents/ag_x/messages", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("role=%v: status=%d, want 200 (body=%s)", p.Role, w.Code, w.Body.String())
		}
	}
}

func TestAgentFencing_FencesGroupDMRoutes(t *testing.T) {
	// agent-callable group DM routes mutate state tied to the
	// calling agent's lock. They live OUTSIDE /api/v1/agents/{id}
	// so the per-agent path-split check skips them; the helper
	// agentIDForFencing covers them explicitly.
	st := &fakeLockStore{holderByID: map[string]string{"ag_x": "peer-tgt"}}
	h := newFencingHandler(t, st, "peer-self",
		Principal{Role: RoleAgent, AgentID: "ag_x"})

	for _, path := range []string{
		"/api/v1/groupdms",
		"/api/v1/groupdms/grp_1/messages",
		"/api/v1/groupdms/grp_1/members",
	} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusConflict {
			t.Errorf("path=%s: status = %d, want 409", path, w.Code)
		}
	}
}

func TestAgentFencing_409StampsNoIdempotencyCacheHeader(t *testing.T) {
	// The idempotency middleware checks for this header to
	// decide whether to save the captured response. Without it,
	// a cached 409 wrong_holder would shadow a successful retry
	// after the lock returns to this peer.
	st := &fakeLockStore{holderByID: map[string]string{"ag_x": "peer-tgt"}}
	h := newFencingHandler(t, st, "peer-self",
		Principal{Role: RoleAgent, AgentID: "ag_x"})

	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/ag_x/messages", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if w.Header().Get(HeaderNoIdempotencyCache) == "" {
		t.Errorf("missing %s header on 409 wrong_holder response", HeaderNoIdempotencyCache)
	}
}

func TestAgentFencing_PassesCrossAgentPath(t *testing.T) {
	// EnforceMiddleware already 403s cross-agent paths for
	// non-owner principals; the fencing middleware just passes
	// them through and lets the policy layer have the final say.
	st := &fakeLockStore{holderByID: map[string]string{"ag_y": "peer-tgt"}}
	h := newFencingHandler(t, st, "peer-self",
		Principal{Role: RoleAgent, AgentID: "ag_x"})

	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/ag_y/messages", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("cross-agent path should pass through: status=%d", w.Code)
	}
}
