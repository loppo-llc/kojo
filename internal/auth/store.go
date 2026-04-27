package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// agentIDPattern restricts the legal agentID character set to a tightly
// scoped alphabet that cannot contain "/" or ".." sequences. The auth
// store uses agentID directly as a filename, so a corrupted agents.json
// would otherwise expose a path-traversal foothold.
var agentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,128}$`)

// validateAgentID returns an error if the agent ID contains anything
// other than the safe alphabet. Empty IDs are also rejected.
func validateAgentID(id string) error {
	if !agentIDPattern.MatchString(id) {
		return fmt.Errorf("auth: invalid agent id %q", id)
	}
	return nil
}

// TokenStore persists owner / agent tokens on disk (mode 0600) and keeps
// an in-memory index for O(1) reverse lookup.
//
// Layout:
//
//	<base>/owner.token            — owner secret (single line, hex)
//	<base>/agent_tokens/<id>      — per-agent secret (single line, hex)
//
// The store does NOT depend on the agent package; agent.Manager is the
// caller that drives EnsureAgentToken / RemoveAgentToken on agent
// lifecycle events.
type TokenStore struct {
	base string

	mu      sync.RWMutex
	owner   string
	tokens  map[string]string // token -> agentID
	idIndex map[string]string // agentID -> token
}

// NewTokenStore initializes a store rooted at base. It creates the
// directory tree if missing and loads any existing tokens. The owner
// token is created on first use if absent unless overrideOwner is
// non-empty (in which case overrideOwner is treated as the canonical
// owner token and is *not* persisted to disk).
func NewTokenStore(base string, overrideOwner string) (*TokenStore, error) {
	if base == "" {
		return nil, errors.New("auth: token store base path is empty")
	}
	dir := filepath.Join(base, "agent_tokens")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("auth: mkdir %s: %w", dir, err)
	}
	st := &TokenStore{
		base:    base,
		tokens:  make(map[string]string),
		idIndex: make(map[string]string),
	}

	if overrideOwner != "" {
		st.owner = overrideOwner
	} else {
		owner, err := loadOrCreateOwner(filepath.Join(base, "owner.token"))
		if err != nil {
			return nil, err
		}
		st.owner = owner
	}

	// Load any existing per-agent tokens.
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("auth: read agent_tokens: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id := e.Name()
		// Skip files whose name does not match the strict agent ID
		// pattern. Anything else is suspicious — a stray backup file,
		// a manual edit, or a corrupted entry — and is not worth
		// surfacing as a valid principal.
		if validateAgentID(id) != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, id))
		if err != nil {
			continue // skip unreadable tokens; they will be regenerated on next agent load if applicable
		}
		tok := trimToken(data)
		if tok == "" {
			continue
		}
		st.tokens[tok] = id
		st.idIndex[id] = tok
	}
	return st, nil
}

// OwnerToken returns the current owner token (hex, 64 chars).
func (s *TokenStore) OwnerToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.owner
}

// LookupAgent returns the agent ID associated with the given token, if any.
func (s *TokenStore) LookupAgent(token string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.tokens[token]
	return id, ok
}

// AgentToken returns the token for the given agent ID, generating and
// persisting one if it does not already exist.
func (s *TokenStore) AgentToken(agentID string) (string, error) {
	if err := validateAgentID(agentID); err != nil {
		return "", err
	}
	s.mu.RLock()
	if t, ok := s.idIndex[agentID]; ok {
		s.mu.RUnlock()
		return t, nil
	}
	s.mu.RUnlock()

	// Generate + persist under write lock; double-check in case of race.
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.idIndex[agentID]; ok {
		return t, nil
	}
	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := s.persistAgentToken(agentID, tok); err != nil {
		return "", err
	}
	s.tokens[tok] = agentID
	s.idIndex[agentID] = tok
	return tok, nil
}

// EnsureAgentToken is a convenience wrapper used at agent-create time.
func (s *TokenStore) EnsureAgentToken(agentID string) error {
	_, err := s.AgentToken(agentID)
	return err
}

// RemoveAgentToken deletes the on-disk and in-memory token for an agent.
// Safe to call on an unknown ID. Invalid IDs are ignored.
func (s *TokenStore) RemoveAgentToken(agentID string) {
	if validateAgentID(agentID) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.idIndex[agentID]
	if ok {
		delete(s.tokens, tok)
		delete(s.idIndex, agentID)
	}
	_ = os.Remove(filepath.Join(s.base, "agent_tokens", agentID))
}

// --- internals -------------------------------------------------------

func (s *TokenStore) persistAgentToken(agentID, token string) error {
	path := filepath.Join(s.base, "agent_tokens", agentID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("auth: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("auth: rename %s: %w", path, err)
	}
	return nil
}

func loadOrCreateOwner(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		if t := trimToken(data); t != "" {
			return t, nil
		}
	}
	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("auth: mkdir owner.token parent: %w", err)
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("auth: write owner.token: %w", err)
	}
	return tok, nil
}

func generateToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

func trimToken(data []byte) string {
	s := string(data)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
