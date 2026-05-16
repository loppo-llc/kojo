package gmail

import (
	"net/url"
	"strings"
	"testing"
)

// TestStartAuthFlow_ReturnsStateMatchingURL verifies the new
// (authURL, state, error) contract: the explicit state return value
// equals the `state` query param embedded in authURL, so callers can
// thread either to the postMessage correlation path without parsing.
func TestStartAuthFlow_ReturnsStateMatchingURL(t *testing.T) {
	mgr := NewOAuth2Manager()
	authURL, state, err := mgr.StartAuthFlow("client-id", "ag_1", "src_1", "https://example/cb")
	if err != nil {
		t.Fatalf("StartAuthFlow: %v", err)
	}
	if state == "" {
		t.Fatal("returned state is empty")
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authURL: %v", err)
	}
	if got := parsed.Query().Get("state"); got != state {
		t.Errorf("authURL state=%q, want %q", got, state)
	}
	// Sanity: the URL is the expected provider endpoint with the
	// fields we passed in.
	if !strings.HasPrefix(authURL, googleAuthURL+"?") {
		t.Errorf("authURL prefix unexpected: %q", authURL)
	}
	if parsed.Query().Get("client_id") != "client-id" {
		t.Errorf("client_id missing from authURL")
	}
	if parsed.Query().Get("redirect_uri") != "https://example/cb" {
		t.Errorf("redirect_uri missing from authURL")
	}
}

// TestPeekPending_DoesNotConsume confirms the flow that the OAuth
// callback relies on: the callback path looks up the pending entry
// twice (once on errParam to fill sourceID for postMessage, once
// after CompleteAuthFlow consumes it). PeekPending must NOT delete
// the entry, otherwise the second lookup would 412 spuriously.
func TestPeekPending_DoesNotConsume(t *testing.T) {
	mgr := NewOAuth2Manager()
	_, state, err := mgr.StartAuthFlow("c", "ag_1", "src_1", "https://example/cb")
	if err != nil {
		t.Fatalf("StartAuthFlow: %v", err)
	}
	first := mgr.PeekPending(state)
	if first == nil {
		t.Fatalf("first PeekPending: nil")
	}
	second := mgr.PeekPending(state)
	if second == nil {
		t.Fatalf("second PeekPending: nil — Peek consumed the entry")
	}
	if first.SourceID != "src_1" || second.SourceID != "src_1" {
		t.Errorf("SourceID mismatch: %q / %q", first.SourceID, second.SourceID)
	}
	if first.State != state || second.State != state {
		t.Errorf("State mismatch: %q / %q (want %q)", first.State, second.State, state)
	}
}

// TestStartAuthFlow_DistinctStateAcrossCalls each call mints a fresh
// state — without that, the postMessage correlation logic in
// NotifySourcesEditor would mis-attribute messages across consecutive
// popups for the same source.
func TestStartAuthFlow_DistinctStateAcrossCalls(t *testing.T) {
	mgr := NewOAuth2Manager()
	_, s1, err := mgr.StartAuthFlow("c", "ag_1", "src_1", "https://example/cb")
	if err != nil {
		t.Fatalf("first StartAuthFlow: %v", err)
	}
	_, s2, err := mgr.StartAuthFlow("c", "ag_1", "src_1", "https://example/cb")
	if err != nil {
		t.Fatalf("second StartAuthFlow: %v", err)
	}
	if s1 == s2 {
		t.Fatalf("two StartAuthFlow calls returned identical state %q — same-source double-click would be indistinguishable", s1)
	}
	// Both pending entries should be retrievable independently.
	if mgr.PeekPending(s1) == nil {
		t.Errorf("pending entry for first state evicted")
	}
	if mgr.PeekPending(s2) == nil {
		t.Errorf("pending entry for second state evicted")
	}
}
