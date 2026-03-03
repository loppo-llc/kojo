package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

const agentsFile = "agents.json"

// store persists agent metadata to disk using atomic rename.
type store struct {
	mu   sync.Mutex
	path string
}

func newStore(configDir string) *store {
	return &store{path: filepath.Join(configDir, agentsFile)}
}

func (s *store) Save(agents []*Agent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(agents, "", "  ")
	if err != nil {
		return err
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (s *store) Load() ([]*Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var agents []*Agent
	if err := json.Unmarshal(data, &agents); err != nil {
		return nil, err
	}
	return agents, nil
}
