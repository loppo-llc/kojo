package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loppo-llc/kojo/internal/session"
)

const harvestTimeout = 5 * time.Minute

// Manager provides CRUD operations and Run for agents.
type Manager struct {
	mu          sync.Mutex
	agents      map[string]*Agent
	running     map[string]bool // tracks agents with active runs
	workDirMu   sync.Mutex
	workDirLock map[string]*sync.Mutex // per-workDir locks for CLAUDE.local.md
	store       *store
	sessions    *session.Manager
	logger      *slog.Logger
	baseDir     string // {configDir}/agents

	scheduler *Scheduler
}

// NewManager creates an agent manager.
func NewManager(logger *slog.Logger, sessions *session.Manager) *Manager {
	baseDir := filepath.Join(session.ConfigDirPath(), "agents")
	m := &Manager{
		agents:      make(map[string]*Agent),
		running:     make(map[string]bool),
		workDirLock: make(map[string]*sync.Mutex),
		store:       newStore(session.ConfigDirPath()),
		sessions:    sessions,
		logger:      logger,
		baseDir:     baseDir,
	}
	m.load()
	m.scheduler = NewScheduler(logger, m)
	m.scheduler.Start()
	return m
}

func (m *Manager) load() {
	agents, err := m.store.Load()
	if err != nil {
		m.logger.Warn("failed to load agents", "err", err)
		return
	}
	for _, a := range agents {
		m.agents[a.ID] = a
	}
}

func (m *Manager) save() {
	m.mu.Lock()
	list := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		list = append(list, copyAgent(a))
	}
	m.mu.Unlock()
	if err := m.store.Save(list); err != nil {
		m.logger.Warn("failed to save agents", "err", err)
	}
}

// Create adds a new agent.
func (m *Manager) Create(name, tool, workDir string, args []string, yoloMode bool, schedule string) (*Agent, error) {
	a := NewAgent(generateAgentID(), name, tool, workDir)
	a.Args = args
	a.YoloMode = yoloMode
	a.Schedule = schedule

	if msg := a.Validate(); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}

	// Ensure agent directory
	if err := os.MkdirAll(m.agentDir(a.ID), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create agent dir: %w", err)
	}

	// Initialize default files
	m.writeFileIfNotExist(a.ID, "SOUL.md", "# Soul\n\nDefine this agent's persona here.\n")
	m.writeFileIfNotExist(a.ID, "MEMORY.md", "# Memory\n\nPersistent memory across sessions.\n")
	m.writeFileIfNotExist(a.ID, "GOALS.md", "# Goals\n\nCurrent objectives.\n")
	m.writeFileIfNotExist(a.ID, "BOOTSTRAP.md", bootstrapTemplate)

	m.mu.Lock()
	m.agents[a.ID] = a
	m.mu.Unlock()
	m.save()
	m.scheduler.Refresh()

	return copyAgent(a), nil
}

// Get returns a deep copy of an agent by ID.
func (m *Manager) Get(id string) *Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.agents[id]
	if a == nil {
		return nil
	}
	return copyAgent(a)
}

// List returns deep copies of all agents.
func (m *Manager) List() []*Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		list = append(list, copyAgent(a))
	}
	return list
}

// Update modifies an agent's mutable fields.
func (m *Manager) Update(id string, updates map[string]any) (*Agent, error) {
	m.mu.Lock()
	a, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if v, ok := updates["name"].(string); ok && v != "" {
		a.Name = v
	}
	if v, ok := updates["tool"].(string); ok && v != "" {
		a.Tool = v
	}
	if v, ok := updates["workDir"].(string); ok && v != "" {
		a.WorkDir = v
	}
	// JSON arrays decode as []any, convert to []string
	if v, ok := updates["args"].([]any); ok {
		args := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				args = append(args, s)
			}
		}
		a.Args = args
	}
	if v, ok := updates["yoloMode"].(bool); ok {
		a.YoloMode = v
	}
	if v, ok := updates["schedule"].(string); ok {
		a.Schedule = v
	}
	if v, ok := updates["enabled"].(bool); ok {
		a.Enabled = v
	}
	cp := copyAgent(a)
	m.mu.Unlock()
	m.save()
	m.scheduler.Refresh()
	return cp, nil
}

// Delete removes an agent.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	_, ok := m.agents[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	delete(m.agents, id)
	m.mu.Unlock()
	m.save()
	m.scheduler.Refresh()
	return nil
}

// IsRunning reports whether an agent currently has an active run.
func (m *Manager) IsRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[id]
}

// Run executes an agent session manually or from the scheduler.
func (m *Manager) Run(id string) (*session.Session, error) {
	a := m.Get(id)
	if a == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	// Prevent overlapping runs for the same agent
	m.mu.Lock()
	if m.running[id] {
		m.mu.Unlock()
		return nil, fmt.Errorf("agent already running: %s", id)
	}
	m.running[id] = true
	m.mu.Unlock()

	cleanupRun := func() {
		m.mu.Lock()
		delete(m.running, id)
		m.mu.Unlock()
	}

	// 1. Read memory files
	soul := m.readFile(id, "SOUL.md")
	memory := m.readFile(id, "MEMORY.md")
	goals := m.readFile(id, "GOALS.md")
	tmpl := m.readFile(id, "BOOTSTRAP.md")
	if tmpl == "" {
		tmpl = bootstrapTemplate
	}

	// 2. FTS5 search for relevant memories
	relevantMemories := ""
	idx, err := m.openIndex(id)
	if err == nil {
		results, _ := idx.Search(goals, 5)
		idx.Close()
		if len(results) > 0 {
			var sb strings.Builder
			for _, r := range results {
				sb.WriteString("- ")
				sb.WriteString(r.Content)
				sb.WriteString("\n")
			}
			relevantMemories = sb.String()
		}
	}

	// 3. Recent logs
	recentLogs := m.readRecentLogs(id, 3)

	// 4. Expand bootstrap template
	bootstrap := tmpl
	bootstrap = strings.ReplaceAll(bootstrap, tmplSoul, soul)
	bootstrap = strings.ReplaceAll(bootstrap, tmplMemory, memory)
	bootstrap = strings.ReplaceAll(bootstrap, tmplGoals, goals)
	bootstrap = strings.ReplaceAll(bootstrap, tmplRecentLogs, recentLogs)
	bootstrap = strings.ReplaceAll(bootstrap, tmplRelevantMemories, relevantMemories)

	// 5. Inject CLAUDE.local.md (with workDir-level lock held until restore)
	// This blocks if another agent with the same workDir is running, which is correct
	// because only one agent can own CLAUDE.local.md at a time.
	// The scheduler launches Run() in a goroutine to avoid stalling other agents.
	wdMu := m.acquireWorkDirLock(a.WorkDir)
	restore, injectErr := injectCLAUDEmd(a.WorkDir, soul)
	if injectErr != nil {
		m.logger.Warn("failed to inject CLAUDE.local.md", "err", injectErr)
	}

	// 6. Create session (after CLAUDE.local.md injection so CLI reads the persona)
	sess, err := m.sessions.Create(a.Tool, a.WorkDir, a.Args, a.YoloMode, "")
	if err != nil {
		if restore != nil {
			restore()
		}
		wdMu.Unlock()
		cleanupRun()
		return nil, fmt.Errorf("failed to create session: %w", err)
	}
	sess.SetAgentID(id)

	// 7. Background: wait for ready, bootstrap, handle exit
	go func() {
		defer cleanupRun()

		// Wait for CLI to be ready
		session.WaitForReady(sess, m.logger)

		// Inject bootstrap via SafePaste
		session.SafePaste(sess, bootstrap)

		// Wait for session to exit
		<-sess.Done()

		// Restore CLAUDE.local.md and release workDir lock
		if restore != nil {
			restore()
		}
		wdMu.Unlock()

		// Harvest session with timeout; cancel summary session if it takes too long
		var summaryRef atomic.Pointer[session.Session]
		harvestDone := make(chan struct{})
		go func() {
			m.harvestSession(sess, a, &summaryRef)
			close(harvestDone)
		}()
		select {
		case <-harvestDone:
		case <-time.After(harvestTimeout):
			m.logger.Warn("harvest timeout, stopping summary session", "agent", a.ID)
			if ss := summaryRef.Load(); ss != nil {
				m.sessions.Stop(ss.Info().ID)
			}
		}

		// Update last run
		m.mu.Lock()
		if orig, ok := m.agents[id]; ok {
			orig.LastRunAt = time.Now().UTC().Format(time.RFC3339)
			orig.LastRunID = sess.Info().ID
		}
		m.mu.Unlock()
		m.save()
	}()

	return sess, nil
}

// Sessions returns all sessions belonging to an agent.
func (m *Manager) Sessions(agentID string) ([]session.SessionInfo, error) {
	if !m.has(agentID) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, agentID)
	}
	all := m.sessions.List()
	var result []session.SessionInfo
	for _, s := range all {
		if s.GetAgentID() == agentID {
			result = append(result, s.Info())
		}
	}
	return result, nil
}

// ReadFile returns the contents of an agent file (SOUL.md, MEMORY.md, GOALS.md).
func (m *Manager) ReadFile(agentID, filename string) (string, error) {
	if !m.has(agentID) {
		return "", fmt.Errorf("%w: %s", ErrNotFound, agentID)
	}
	content := m.readFile(agentID, filename)
	return content, nil
}

// WriteFile writes content to an agent file.
func (m *Manager) WriteFile(agentID, filename, content string) error {
	if !m.has(agentID) {
		return fmt.Errorf("%w: %s", ErrNotFound, agentID)
	}
	if !allowedAgentFiles[filename] {
		return fmt.Errorf("invalid filename: %s", filename)
	}
	path := filepath.Join(m.agentDir(agentID), filename)
	return os.WriteFile(path, []byte(content), 0o644)
}

// LogNames returns the list of log file names for an agent.
func (m *Manager) LogNames(agentID string) ([]string, error) {
	if !m.has(agentID) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, agentID)
	}
	logsDir := filepath.Join(m.agentDir(agentID), "logs")
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// ReadLog returns the contents of a specific log file.
func (m *Manager) ReadLog(agentID, name string) (string, error) {
	if !m.has(agentID) {
		return "", fmt.Errorf("%w: %s", ErrNotFound, agentID)
	}
	path := filepath.Join(m.agentDir(agentID), "logs", filepath.Base(name))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Search queries the FTS5 index for an agent.
func (m *Manager) Search(agentID, query string, limit int) ([]MemoryResult, error) {
	if !m.has(agentID) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, agentID)
	}
	idx, err := m.openIndex(agentID)
	if err != nil {
		return nil, err
	}
	defer idx.Close()
	return idx.Search(query, limit)
}

// Stop shuts down the scheduler.
func (m *Manager) Stop() {
	m.scheduler.Stop()
}

// allowedAgentFiles is the set of filenames that can be written via WriteFile.
var allowedAgentFiles = map[string]bool{
	"SOUL.md":   true,
	"MEMORY.md": true,
	"GOALS.md":  true,
}

// has returns true if an agent with the given ID exists (no deep copy).
func (m *Manager) has(id string) bool {
	m.mu.Lock()
	_, ok := m.agents[id]
	m.mu.Unlock()
	return ok
}

// --- helpers ---

func (m *Manager) agentDir(id string) string {
	return filepath.Join(m.baseDir, id)
}

func (m *Manager) readFile(id, name string) string {
	data, err := os.ReadFile(filepath.Join(m.agentDir(id), name))
	if err != nil {
		return ""
	}
	return string(data)
}

func (m *Manager) writeFileIfNotExist(id, name, content string) {
	path := filepath.Join(m.agentDir(id), name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return // file already exists or other error
	}
	f.Write([]byte(content))
	f.Close()
}

func (m *Manager) readRecentLogs(id string, count int) string {
	logsDir := filepath.Join(m.agentDir(id), "logs")
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return "(no previous logs)"
	}
	start := len(entries) - count
	if start < 0 {
		start = 0
	}
	var sb strings.Builder
	for _, e := range entries[start:] {
		data, err := os.ReadFile(filepath.Join(logsDir, e.Name()))
		if err != nil {
			continue
		}
		sb.WriteString("### ")
		sb.WriteString(e.Name())
		sb.WriteString("\n")
		sb.Write(data)
		sb.WriteString("\n\n")
	}
	if sb.Len() == 0 {
		return "(no previous logs)"
	}
	return sb.String()
}

func (m *Manager) openIndex(id string) (*MemoryIndex, error) {
	dbPath := filepath.Join(m.agentDir(id), "memory.db")
	return NewMemoryIndex(dbPath)
}

// copyAgent returns a deep copy of an Agent.
func copyAgent(a *Agent) *Agent {
	cp := *a
	if a.Args != nil {
		cp.Args = make([]string, len(a.Args))
		copy(cp.Args, a.Args)
	}
	return &cp
}

func (m *Manager) acquireWorkDirLock(workDir string) *sync.Mutex {
	workDir = filepath.Clean(workDir)
	m.workDirMu.Lock()
	mu, ok := m.workDirLock[workDir]
	if !ok {
		mu = &sync.Mutex{}
		m.workDirLock[workDir] = mu
	}
	m.workDirMu.Unlock()
	mu.Lock()
	return mu
}

func generateAgentID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "a_" + hex.EncodeToString(b)
}
