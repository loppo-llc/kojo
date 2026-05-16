package gmail

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	gmailScope     = "https://www.googleapis.com/auth/gmail.readonly"
)

// PendingAuth tracks an in-progress OAuth2 authorization flow.
type PendingAuth struct {
	AgentID      string
	SourceID     string
	State        string
	CodeVerifier string
	RedirectURI  string
	CreatedAt    time.Time
}

// OAuth2Manager manages OAuth2 authorization flows for Gmail.
type OAuth2Manager struct {
	mu      sync.Mutex
	pending map[string]*PendingAuth // state → PendingAuth
}

// NewOAuth2Manager creates a new OAuth2 manager.
func NewOAuth2Manager() *OAuth2Manager {
	return &OAuth2Manager{
		pending: make(map[string]*PendingAuth),
	}
}

// StartAuthFlow begins an OAuth2 authorization flow and returns the
// URL to redirect the user to AND the freshly-generated `state`
// parameter so the caller can echo it back to the client (the editor
// uses it to correlate the postMessage callback against a specific
// popup — necessary when the user opens two consecutive popups for
// the same source and we need to drop the older one's late callback).
func (m *OAuth2Manager) StartAuthFlow(clientID, agentID, sourceID, redirectURI string) (string, string, error) {
	state, err := randomString(32)
	if err != nil {
		return "", "", fmt.Errorf("generate state: %w", err)
	}
	codeVerifier, err := randomString(64)
	if err != nil {
		return "", "", fmt.Errorf("generate code verifier: %w", err)
	}

	// PKCE S256 challenge
	h := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(h[:])

	m.mu.Lock()
	// Clean up stale entries (older than 10 minutes)
	for k, p := range m.pending {
		if time.Since(p.CreatedAt) > 10*time.Minute {
			delete(m.pending, k)
		}
	}
	m.pending[state] = &PendingAuth{
		AgentID:      agentID,
		SourceID:     sourceID,
		State:        state,
		CodeVerifier: codeVerifier,
		RedirectURI:  redirectURI,
		CreatedAt:    time.Now(),
	}
	m.mu.Unlock()

	params := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {gmailScope},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
	}

	return googleAuthURL + "?" + params.Encode(), state, nil
}

// PeekPending returns the pending auth for a state without consuming it.
func (m *OAuth2Manager) PeekPending(state string) *PendingAuth {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending[state]
}

// CompleteAuthFlow exchanges the authorization code for tokens.
// Returns the PendingAuth (for agent/source binding) and the token response.
func (m *OAuth2Manager) CompleteAuthFlow(ctx context.Context, state, code, clientID, clientSecret string) (*PendingAuth, *TokenResponse, error) {
	m.mu.Lock()
	pending, ok := m.pending[state]
	if !ok {
		m.mu.Unlock()
		return nil, nil, fmt.Errorf("unknown or expired state parameter")
	}
	delete(m.pending, state)
	m.mu.Unlock()

	// Verify the flow isn't too old
	if time.Since(pending.CreatedAt) > 10*time.Minute {
		return nil, nil, fmt.Errorf("authorization flow expired")
	}

	// Exchange code for tokens
	data := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"code_verifier": {pending.CodeVerifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {pending.RedirectURI},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", googleTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, body)
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, nil, fmt.Errorf("parse token response: %w", err)
	}

	return pending, &tokenResp, nil
}

// TokenResponse represents the OAuth2 token exchange response.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// FindAvailablePort finds an available TCP port on localhost.
func FindAvailablePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}
