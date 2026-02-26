package session

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	configDir    = ".config/kojo"
	sessionsFile = "sessions.json"
	maxAge       = 7 * 24 * time.Hour
)

// Store persists session metadata to disk.
type Store struct {
	mu     sync.Mutex
	path   string
	logger *slog.Logger
}

func newStore(logger *slog.Logger) *Store {
	home, _ := os.UserHomeDir()
	return &Store{
		path:   filepath.Join(home, configDir, sessionsFile),
		logger: logger,
	}
}

// Save writes session info to disk using atomic rename.
func (st *Store) Save(infos []SessionInfo) {
	st.mu.Lock()
	defer st.mu.Unlock()

	data, err := json.MarshalIndent(infos, "", "  ")
	if err != nil {
		st.logger.Warn("failed to marshal sessions", "err", err)
		return
	}

	dir := filepath.Dir(st.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		st.logger.Warn("failed to create config dir", "err", err)
		return
	}

	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		st.logger.Warn("failed to write tmp sessions file", "err", err)
		return
	}
	if err := os.Rename(tmp, st.path); err != nil {
		st.logger.Warn("failed to rename sessions file", "err", err)
		os.Remove(tmp)
	}
}

// Load reads persisted sessions, filtering out entries older than maxAge.
// Returns (nil, nil) when the file does not exist (first run).
// Returns (nil, err) on read/parse errors so callers can distinguish
// "no sessions" from "failed to load" (important for orphan cleanup).
func (st *Store) Load() ([]SessionInfo, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	data, err := os.ReadFile(st.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		st.logger.Warn("failed to read sessions file", "err", err)
		return nil, err
	}

	var infos []SessionInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		st.logger.Warn("failed to parse sessions file", "err", err)
		return nil, err
	}

	cutoff := time.Now().Add(-maxAge)
	filtered := infos[:0]
	for _, info := range infos {
		t, err := time.Parse(time.RFC3339, info.CreatedAt)
		if err != nil {
			continue
		}
		if t.After(cutoff) {
			filtered = append(filtered, info)
		}
	}
	return filtered, nil
}
