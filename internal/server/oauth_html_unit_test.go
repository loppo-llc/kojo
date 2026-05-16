package server

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// Pure-function tests for writeOAuthErrorHTML. The handler integration
// path (callback hits the OAuth provider, agent state changes, etc.) is
// gated behind heavy_test and exercised separately; this file just
// pins down the response shape so the postMessage contract with
// NotifySourcesEditor stays stable.

// extractPostMessagePayload pulls the JSON object out of the
// `window.opener.postMessage({...},"*")` script the helper emits.
// The popup HTML is small enough that a regex is the lightest tool;
// no need to parse the whole document. The helper marshals payload
// as a single JSON object (quoted keys) so json.Unmarshal works
// without translating JS object-literal syntax.
func extractPostMessagePayload(t *testing.T, body string) map[string]any {
	t.Helper()
	// Match `postMessage(<json>,` — capture the JSON object. Use
	// a non-greedy match for the body so embedded escape sequences
	// in detail (e.g. <script>) don't shift the boundary.
	re := regexp.MustCompile(`(?s)postMessage\((\{.*?\}),"\*"\)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no postMessage call found in body:\n%s", body)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(m[1]), &payload); err != nil {
		t.Fatalf("postMessage payload not JSON: %v\nraw: %s", err, m[1])
	}
	return payload
}

func TestWriteOAuthErrorHTML_Shape(t *testing.T) {
	w := httptest.NewRecorder()
	writeOAuthErrorHTML(w, 410, "agent_gone", "Agent no longer exists.", "src_42", "state-abc")

	if w.Code != 410 {
		t.Errorf("status = %d, want 410", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := w.Body.String()

	// Visible body uses the human detail, not the JSON-escaped form.
	if !strings.Contains(body, "Agent no longer exists.") {
		t.Errorf("body missing detail text:\n%s", body)
	}
	// window.close() must run unconditionally so the popup never
	// hangs.
	if !strings.Contains(body, "window.close()") {
		t.Errorf("body missing window.close():\n%s", body)
	}

	// postMessage payload carries every correlation field the
	// editor relies on.
	payload := extractPostMessagePayload(t, body)
	if got := payload["type"]; got != "oauth_error" {
		t.Errorf("type = %v, want oauth_error", got)
	}
	if got := payload["reason"]; got != "agent_gone" {
		t.Errorf("reason = %v, want agent_gone", got)
	}
	if got := payload["detail"]; got != "Agent no longer exists." {
		t.Errorf("detail = %v, want Agent no longer exists.", got)
	}
	if got := payload["sourceId"]; got != "src_42" {
		t.Errorf("sourceId = %v, want src_42", got)
	}
	if got := payload["state"]; got != "state-abc" {
		t.Errorf("state = %v, want state-abc", got)
	}
}

// TestWriteOAuthErrorHTML_EmptyStateAndSource verifies the truly
// state-less failure path (missing query param / unknown state) emits
// "" for both sourceId and state — NotifySourcesEditor accepts those
// unconditionally so the user still sees feedback when nothing else
// is in flight.
func TestWriteOAuthErrorHTML_EmptyStateAndSource(t *testing.T) {
	w := httptest.NewRecorder()
	writeOAuthErrorHTML(w, 400, "bad_request", "Missing code or state", "", "")
	payload := extractPostMessagePayload(t, w.Body.String())
	if got := payload["sourceId"]; got != "" {
		t.Errorf("sourceId = %v, want empty string", got)
	}
	if got := payload["state"]; got != "" {
		t.Errorf("state = %v, want empty string", got)
	}
}

// TestWriteOAuthErrorHTML_DetailEscaped pins down the only
// untrusted-content path: detail is server-generated today but if a
// future caller passes a string containing HTML or </script>, the
// response must not break out of the visible body or the JS string
// literal.
func TestWriteOAuthErrorHTML_DetailEscaped(t *testing.T) {
	w := httptest.NewRecorder()
	writeOAuthErrorHTML(w, 500, "token_failed",
		`<script>alert(1)</script>`, "src_1", "state-1")
	body := w.Body.String()

	// Visible <p> body must escape the angle brackets.
	if strings.Contains(body, "<p>Authorization failed: <script>alert(1)</script></p>") {
		t.Errorf("HTML body did not escape detail:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("HTML body missing escaped detail:\n%s", body)
	}

	// JSON-encoded payload must keep the raw string literal intact
	// (json.Marshal handles the </script> escape via <).
	payload := extractPostMessagePayload(t, body)
	if got := payload["detail"]; got != `<script>alert(1)</script>` {
		t.Errorf("detail in payload = %v, want raw script literal", got)
	}
}
