package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/loppo-llc/kojo/internal/store"
)

const (
	// exitDrainTimeout is the maximum time to wait for readLoop to finish
	// draining output after the session process exits.
	exitDrainTimeout = 3 * time.Second

	// exitKillTimeout is the maximum time to wait for the attach process
	// to exit after being killed.
	exitKillTimeout = 5 * time.Second

	// stopKillTimeout is the grace period before SIGKILL after SIGTERM in Stop().
	stopKillTimeout = 5 * time.Second

	// shutdownTimeout is the maximum time to wait for non-tmux sessions to exit on shutdown.
	shutdownTimeout = 10 * time.Second

	// paneStatusPollInterval is how often tmuxWaitLoop checks the tmux pane status.
	paneStatusPollInterval = 500 * time.Millisecond

	// maxPaneCheckErrors is the number of consecutive tmux pane check errors
	// before the session is forcibly finalized.
	maxPaneCheckErrors = 10

	// readBufSize is the buffer size for PTY/FIFO reads.
	readBufSize = 32 * 1024

	// maxLastOutput is the maximum bytes of scrollback captured on session exit.
	maxLastOutput = 8192

	// yoloApproveDelay is the delay before sending an auto-approve response.
	yoloApproveDelay = 100 * time.Millisecond

	// writeRetryDelay is the delay between retries when PTY is nil during reattach.
	writeRetryDelay = 50 * time.Millisecond

	// maxWriteRetries is the number of retries for PTY write during reattach.
	maxWriteRetries = 5
)

var userTools = map[string]bool{
	"claude": true,
	"codex":  true,
	"gemini": true,
	"custom": true,
}

// internalTools is populated by platform-specific init() functions.
// Unix adds "tmux", Windows adds "shell".
var internalTools = map[string]bool{}

func isAllowedTool(name string) bool {
	return userTools[name] || internalTools[name]
}

func hasArg(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	logger   *slog.Logger
	store    *Store

	shuttingDown bool

	// customBaseURL is the base URL for a custom Anthropic Messages API endpoint.
	customBaseURL string

	// callback for session events
	OnSessionExit func(s *Session)
}

// SetCustomBaseURL configures the base URL for custom Anthropic API sessions.
func (m *Manager) SetCustomBaseURL(baseURL string) {
	m.mu.Lock()
	m.customBaseURL = baseURL
	m.mu.Unlock()
}

// GetCustomBaseURL returns the custom API base URL (empty if not configured).
func (m *Manager) GetCustomBaseURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.customBaseURL
}

// ManagerOptions tunes Manager construction. Zero-value is the
// minimal-effects posture: no v0 → v1 session fallback. Callers that
// know migration is complete (the startup gate confirmed v1Complete)
// supply a non-empty V0LegacyDir so the v0 dir's sessions.json can
// be picked up on first Load.
type ManagerOptions struct {
	// V0LegacyDir is the v0 config directory (configdir.V0Path()).
	// Empty means: do not consult the v0 dir at all — the runtime
	// opted out (e.g. --fresh) or there is no v0 install to fall
	// back to. Non-empty enables the v0-side fallback inside
	// internal/session.Store.Load(): kv miss → v1 dir → v0 dir.
	V0LegacyDir string
}

// NewManager constructs a session.Manager. db is the kv-backed
// persistence layer (Phase 2c-2 slice 28); pass nil to disable
// persistence (test scaffolding that exercises Manager methods
// without a configured store). The runtime path always passes a
// real *store.Store via server.Config.
func NewManager(logger *slog.Logger, db *store.Store, opts ManagerOptions) *Manager {
	st := newStore(logger, db, opts.V0LegacyDir)
	m := &Manager{
		sessions: make(map[string]*Session),
		logger:   logger,
		store:    st,
	}
	m.platformInit()
	return m
}

func (m *Manager) Create(tool, workDir string, args []string, yoloMode bool, parentID string) (*Session, error) {
	if !isAllowedTool(tool) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedTool, tool)
	}

	// Resolve custom → claude with ANTHROPIC_BASE_URL; may modify args to extract --model.
	customResult := m.resolveCustomAPI(tool, args)
	actualTool := customResult.actualTool
	args = customResult.args

	toolPath, err := resolveToolPath(tool, actualTool)
	if err != nil {
		return nil, err
	}

	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("working directory does not exist: %s", workDir)
	}

	id := generateID()

	// Assign session ID and build run args based on tool type.
	var toolSessionID string
	var runArgs []string
	if !userTools[tool] {
		runArgs, toolSessionID = platformBuildInternalToolArgs(id, tool, workDir, args)
	} else {
		toolSessionID, runArgs = assignClaudeSessionID(actualTool, args)
	}

	extraEnv := m.buildCustomEnv(customResult)

	var res *startResult
	if userTools[tool] {
		res, err = m.platformStartUserTool(id, workDir, toolPath, runArgs, 0, 0, extraEnv)
	} else {
		res, err = m.platformStartInternalTool(id, tool, toolPath, workDir, runArgs, toolSessionID)
	}
	if err != nil {
		return nil, err
	}

	s := &Session{
		ID:              id,
		Tool:            tool,
		WorkDir:         workDir,
		Args:            args,
		PTY:             res.pty,
		Cmd:             res.cmd,
		CreatedAt:       time.Now(),
		Status:          StatusRunning,
		YoloMode:        yoloMode,
		Internal:        internalTools[tool],
		ToolSessionID:   toolSessionID,
		ParentID:        parentID,
		TmuxSessionName: res.tmuxName,
		rawPipe:         res.rawPipe,
		rawPipePath:     res.rawPipePath,
		scrollback:      NewRingBuffer(defaultRingSize),
		subscribers:     make(map[chan []byte]struct{}),
		done:            make(chan struct{}),
		readDone:        make(chan struct{}),
		attachments:     make(map[string]*Attachment),
	}

	m.mu.Lock()
	// Atomic check-and-register: if a duplicate child was created concurrently, discard ours
	if parentID != "" {
		for _, existing := range m.sessions {
			if existing.ParentID == parentID && existing.Tool == tool {
				existing.mu.Lock()
				status := existing.Status
				existing.mu.Unlock()
				if status == StatusRunning {
					m.mu.Unlock()
					m.platformCleanupDuplicate(res)
					return existing, nil
				}
			}
		}
	}
	m.sessions[id] = s
	m.mu.Unlock()

	m.platformStartLoops(s)

	m.logger.Info("session created", "id", id, "tool", tool, "workDir", workDir)
	m.save()
	return s, nil
}

func (m *Manager) Restart(id string) (*Session, error) {
	s, ok := m.Get(id)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}

	s.mu.Lock()
	if s.Status == StatusRunning || s.restarting {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrSessionRunning, id)
	}
	s.restarting = true
	tool := s.Tool
	workDir := s.WorkDir
	args := s.Args
	toolSessionID := s.ToolSessionID
	s.mu.Unlock()

	clearRestarting := func() {
		s.mu.Lock()
		s.restarting = false
		s.mu.Unlock()
	}

	// Verify session wasn't removed between Get and setting restarting flag
	m.mu.Lock()
	_, stillExists := m.sessions[id]
	m.mu.Unlock()
	if !stillExists {
		clearRestarting()
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}

	if !isAllowedTool(tool) {
		clearRestarting()
		return nil, fmt.Errorf("unsupported tool: %s", tool)
	}

	customResult := m.resolveCustomAPI(tool, args)
	actualTool := customResult.actualTool

	toolPath, err := resolveToolPath(tool, actualTool)
	if err != nil {
		clearRestarting()
		return nil, err
	}

	// Platform-specific cleanup of old session resources
	m.platformPrepareRestart(s)

	restartArgs := buildRestartArgs(actualTool, args, toolSessionID)

	extraEnv := m.buildCustomEnv(customResult)

	var res *startResult
	if userTools[tool] {
		s.mu.Lock()
		cols, rows := s.lastCols, s.lastRows
		s.mu.Unlock()
		res, err = m.platformStartUserTool(id, workDir, toolPath, restartArgs, cols, rows, extraEnv)
	} else {
		res, err = m.platformStartInternalTool(id, tool, toolPath, workDir, restartArgs, toolSessionID)
	}
	if err != nil {
		clearRestarting()
		return nil, err
	}

	s.mu.Lock()
	s.PTY = res.pty
	s.Cmd = res.cmd
	s.Args = args // Keep original args (without --resume), not restartArgs
	s.TmuxSessionName = res.tmuxName
	s.rawPipe = res.rawPipe
	s.rawPipePath = res.rawPipePath
	s.Status = StatusRunning
	s.ExitCode = nil
	s.lastOutput = nil
	s.restarting = false
	s.done = make(chan struct{})
	s.readDone = make(chan struct{})
	s.mu.Unlock()

	m.platformStartLoops(s)

	m.logger.Info("session restarted", "id", id, "tool", tool)
	m.save()
	return s, nil
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *Manager) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	return list
}

// FindChildSession returns a child session of the given parent with the specified tool.
func (m *Manager) FindChildSession(parentID, tool string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var fallback *Session
	for _, s := range m.sessions {
		if s.ParentID == parentID && s.Tool == tool {
			s.mu.Lock()
			status := s.Status
			s.mu.Unlock()
			if status == StatusRunning {
				return s, true
			}
			fallback = s
		}
	}
	if fallback != nil {
		return fallback, true
	}
	return nil, false
}

// findChildSessions returns all child sessions of the given parent with the specified tool.
func (m *Manager) findChildSessions(parentID, tool string) []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*Session
	for _, s := range m.sessions {
		if s.ParentID == parentID && s.Tool == tool {
			result = append(result, s)
		}
	}
	return result
}

// Remove removes an exited session and its internal children from memory and persists the change.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	// Check for running children first to avoid orphans
	for _, cs := range m.sessions {
		if cs.ParentID == id {
			cs.mu.Lock()
			cStatus := cs.Status
			cs.mu.Unlock()
			if cStatus == StatusRunning {
				m.mu.Unlock()
				return fmt.Errorf("%w: %s", ErrHasRunningChildren, id)
			}
		}
	}
	s.mu.Lock()
	if s.Status == StatusRunning || s.restarting {
		s.mu.Unlock()
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSessionRunning, id)
	}
	delete(m.sessions, id)
	s.mu.Unlock()
	for cid, cs := range m.sessions {
		if cs.ParentID == id {
			delete(m.sessions, cid)
		}
	}
	m.mu.Unlock()
	m.save()
	return nil
}

func (m *Manager) Stop(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}

	s.mu.Lock()
	if s.Status != StatusRunning || s.restarting {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSessionNotRunning, id)
	}
	s.mu.Unlock()

	return m.platformStop(s, id)
}

// TmuxAction executes a whitelisted tmux action on a terminal session.
func (m *Manager) TmuxAction(id, action string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}

	s.mu.Lock()
	tool := s.Tool
	status := s.Status
	toolSessionID := s.ToolSessionID
	s.mu.Unlock()

	if !internalTools[tool] {
		return fmt.Errorf("%w: %s", ErrNotTerminal, id)
	}
	if status != StatusRunning {
		return fmt.Errorf("%w: %s", ErrSessionNotRunning, id)
	}
	if toolSessionID == "" {
		return fmt.Errorf("%w: %s", ErrNoTmuxID, id)
	}

	return tmuxRunAction(toolSessionID, action)
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	m.shuttingDown = true
	m.mu.Unlock()

	m.platformStopAll()
}

// SaveAll persists all sessions to disk. Called on shutdown.
func (m *Manager) SaveAll() {
	m.save()
}

func (m *Manager) save() {
	m.mu.Lock()
	infos := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		infos = append(infos, s.InfoForSave())
	}
	m.mu.Unlock()
	m.store.Save(infos)
}

func (m *Manager) readLoop(s *Session) {
	defer close(s.readDone)

	s.mu.Lock()
	// Prefer raw pipe (pipe-pane FIFO) for complete output capture;
	// fall back to PTY output.
	var reader io.Reader
	if s.rawPipe != nil {
		reader = s.rawPipe
	} else {
		reader = s.PTY
	}
	s.mu.Unlock()

	if reader == nil {
		return
	}

	buf := make([]byte, readBufSize)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			s.scrollback.Write(data)
			s.broadcast(data)

			// capture tool session ID from output (e.g. codex)
			s.CaptureToolSessionID(data)

			// yolo auto-approve check
			approval, debugTail := s.CheckYolo(data)
			if debugTail != "" {
				s.BroadcastYoloDebug(debugTail)
			}
			if approval != nil {
				m.logger.Info("yolo auto-approve", "id", s.ID, "matched", approval.Matched)
				time.AfterFunc(yoloApproveDelay, func() {
					if !s.IsYoloMode() {
						return
					}
					if _, err := s.Write([]byte("\r")); err != nil {
						m.logger.Debug("yolo write error", "id", s.ID, "err", err)
					}
				})
			}

			// attachment detection
			if newAttachments := s.CheckAttachments(data); len(newAttachments) > 0 {
				s.BroadcastAttachments(newAttachments)
			}
		}
		if err != nil {
			if err != io.EOF {
				m.logger.Debug("pty read error", "id", s.ID, "err", err)
			}
			return
		}
	}
}

// waitLoop monitors a direct PTY process (non-tmux sessions).
func (m *Manager) waitLoop(s *Session) {
	err := s.Cmd.Wait()

	// close PTY so readLoop drains remaining data and exits
	s.mu.Lock()
	if s.PTY != nil {
		s.PTY.Close()
		s.PTY = nil
	}
	s.mu.Unlock()

	// wait for readLoop to finish draining
	<-s.readDone

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	m.completeExit(s, exitCode)
}

// awaitReadDone waits for readLoop to finish with a timeout.
func (m *Manager) awaitReadDone(s *Session) {
	select {
	case <-s.readDone:
	case <-time.After(exitDrainTimeout):
		m.logger.Warn("readLoop did not exit in time, proceeding with session exit", "id", s.ID)
	}
}

// completeExit captures final output, updates session state, and notifies.
func (m *Manager) completeExit(s *Session, exitCode int) {
	// capture last output from scrollback
	scrollback := s.scrollback.Bytes()
	if len(scrollback) > maxLastOutput {
		scrollback = scrollback[len(scrollback)-maxLastOutput:]
	}

	s.mu.Lock()
	s.Status = StatusExited
	s.lastOutput = scrollback
	s.ExitCode = &exitCode
	s.mu.Unlock()

	close(s.done)
	m.save()

	// Stop child sessions when parent exits
	shellTool := ShellToolName()
	for _, child := range m.findChildSessions(s.ID, shellTool) {
		child.mu.Lock()
		childStatus := child.Status
		child.mu.Unlock()
		if childStatus == StatusRunning {
			_ = m.Stop(child.ID)
		}
	}

	m.logger.Info("session exited", "id", s.ID, "exitCode", s.ExitCode)

	if m.OnSessionExit != nil {
		m.OnSessionExit(s)
	}
}

// customAPIResult holds the result of resolving custom API configuration.
type customAPIResult struct {
	actualTool string   // the actual tool to execute ("claude" if custom)
	args       []string // args (unchanged for non-custom)
	baseURL    string   // custom API base URL
}

// resolveCustomAPI resolves custom → claude with ANTHROPIC_BASE_URL.
func (m *Manager) resolveCustomAPI(tool string, args []string) customAPIResult {
	result := customAPIResult{actualTool: tool, args: args}
	if tool != "custom" {
		return result
	}

	m.mu.Lock()
	baseURL := m.customBaseURL
	m.mu.Unlock()

	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	result.actualTool = "claude"
	result.baseURL = baseURL
	return result
}

// buildCustomEnv builds extra environment variables for custom API sessions.
func (m *Manager) buildCustomEnv(r customAPIResult) []string {
	if r.baseURL == "" {
		return nil
	}
	return []string{
		"ANTHROPIC_BASE_URL=" + r.baseURL,
		"ANTHROPIC_API_KEY=dummy",
	}
}

// resolveToolPath resolves the executable path for a tool.
// Internal tools (tmux/shell) are resolved by platform functions, not LookPath.
func resolveToolPath(tool, actualTool string) (string, error) {
	if !userTools[tool] {
		return "", nil
	}
	toolPath, err := exec.LookPath(actualTool)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrToolNotFound, actualTool)
	}
	return toolPath, nil
}

// assignClaudeSessionID assigns a stable session ID for claude sessions
// and builds the run args. For non-claude tools, returns empty sessionID
// and the original args.
func assignClaudeSessionID(actualTool string, args []string) (string, []string) {
	if actualTool != "claude" {
		return "", args
	}

	var toolSessionID string
	for i, a := range args {
		if a == "--session-id" {
			if i+1 < len(args) {
				toolSessionID = args[i+1]
			}
			return toolSessionID, args
		}
		if strings.HasPrefix(a, "--session-id=") {
			toolSessionID = strings.TrimPrefix(a, "--session-id=")
			return toolSessionID, args
		}
	}

	// No --session-id found; generate one and append.
	toolSessionID = uuid.New().String()
	runArgs := make([]string, len(args), len(args)+2)
	copy(runArgs, args)
	runArgs = append(runArgs, "--session-id", toolSessionID)
	return toolSessionID, runArgs
}

// buildRestartArgs produces the command arguments for restarting a session.
func buildRestartArgs(tool string, origArgs []string, toolSessionID string) []string {
	switch tool {
	case "claude":
		args := make([]string, 0, len(origArgs)+2)
		skipNext := false
		for _, a := range origArgs {
			if skipNext {
				skipNext = false
				continue
			}
			if a == "--resume" || a == "-r" {
				skipNext = true
				continue
			}
			if a == "--continue" || a == "-c" {
				continue
			}
			args = append(args, a)
		}
		if toolSessionID != "" {
			return append(args, "--resume", toolSessionID)
		}
		return append(args, "--continue")

	case "codex":
		if toolSessionID != "" {
			return []string{"resume", toolSessionID}
		}
		return []string{"resume", "--last"}

	case "gemini":
		args := make([]string, 0, len(origArgs)+2)
		skipNext := false
		for _, a := range origArgs {
			if skipNext {
				skipNext = false
				continue
			}
			if a == "--resume" || a == "-r" {
				skipNext = true
				continue
			}
			args = append(args, a)
		}
		return append(args, "--resume", "latest")

	default:
		// Internal tools (tmux/shell) use platform-specific restart args
		if internalTools[tool] {
			return buildInternalToolRestartArgs(origArgs, toolSessionID)
		}
		out := make([]string, len(origArgs))
		copy(out, origArgs)
		return out
	}
}

func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "s_" + hex.EncodeToString(b)
}

// ToolAvailability checks which user-facing tools are available on this system.
func ToolAvailability() map[string]ToolInfo {
	result := make(map[string]ToolInfo)
	for tool := range userTools {
		if tool == "custom" {
			// custom requires claude CLI (used as client with ANTHROPIC_BASE_URL).
			claudePath, err := exec.LookPath("claude")
			result[tool] = ToolInfo{Available: err == nil, Path: claudePath}
			continue
		}
		path, err := exec.LookPath(tool)
		result[tool] = ToolInfo{
			Available: err == nil,
			Path:      path,
		}
	}
	return result
}

type ToolInfo struct {
	Available bool   `json:"available"`
	Path      string `json:"path"`
}
