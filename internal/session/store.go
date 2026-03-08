package session

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/atomicfile"
)

const (
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
	return &Store{
		path:   filepath.Join(configDirPath(), sessionsFile),
		logger: logger,
	}
}

// Save writes session info to disk using atomic rename.
func (st *Store) Save(infos []SessionInfo) {
	st.mu.Lock()
	defer st.mu.Unlock()

	dir := filepath.Dir(st.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		st.logger.Warn("failed to create config dir", "err", err)
		return
	}

	if err := atomicfile.WriteJSON(st.path, infos, 0o644); err != nil {
		st.logger.Warn("failed to save sessions", "err", err)
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
