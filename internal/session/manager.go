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
}

// internalTools is populated by platform-specific init() functions.
// Unix adds "tmux", Windows adds "shell".
var internalTools = map[string]bool{}

func isAllowedTool(name string) bool {
	return userTools[name] || internalTools[name]
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	logger   *slog.Logger
	store    *Store

	shuttingDown bool

	// callback for session events
	OnSessionExit func(s *Session)
}

func NewManager(logger *slog.Logger) *Manager {
	st := newStore(logger)
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
		return nil, fmt.Errorf("unsupported tool: %s", tool)
	}

	// Internal tools (tmux/shell) are resolved by platform functions, not LookPath
	var toolPath string
	var err error
	if userTools[tool] {
		toolPath, err = exec.LookPath(tool)
		if err != nil {
			return nil, fmt.Errorf("tool not found: %s", tool)
		}
	}

	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("working directory does not exist: %s", workDir)
	}

	id := generateID()

	// For claude, assign a stable session ID so we can --resume later
	var toolSessionID string
	runArgs := args
	if tool == "claude" {
		hasSessionID := false
		for i, a := range args {
			if a == "--session-id" {
				hasSessionID = true
				if i+1 < len(args) {
					toolSessionID = args[i+1]
				}
				break
			}
			if strings.HasPrefix(a, "--session-id=") {
				hasSessionID = true
				toolSessionID = strings.TrimPrefix(a, "--session-id=")
				break
			}
		}
		if !hasSessionID {
			toolSessionID = uuid.New().String()
			runArgs = make([]string, len(args), len(args)+2)
			copy(runArgs, args)
			runArgs = append(runArgs, "--session-id", toolSessionID)
		}
	}

	// For internal tools, build platform-specific args
	if !userTools[tool] {
		runArgs, toolSessionID = platformBuildInternalToolArgs(id, tool, workDir, args)
	}

	// codex: session ID is captured from PTY output in readLoop
	// gemini: no session ID mechanism; uses --resume latest on restart

	var res *startResult
	if userTools[tool] {
		res, err = m.platformStartUserTool(id, workDir, toolPath, runArgs, 0, 0)
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
		done:     make(chan struct{}),
		readDone: make(chan struct{}),
		attachments:     make(map[string]*Attachment),
	}

	// Initialize context estimator for user-facing tools
	if userTools[tool] {
		s.context = NewContextEstimator(tool, func() {
			(&CompactionOrchestrator{manager: m, session: s, logger: m.logger}).Run()
		})
		// Start transcript monitor for Claude (more accurate token tracking)
		if tool == "claude" && toolSessionID != "" {
			tm := NewTranscriptMonitor(m.logger, workDir, toolSessionID, s.context, func(info *ContextInfo) {
				s.BroadcastContext(info)
			})
			s.context.SetTranscript(tm)
		}
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
		return nil, fmt.Errorf("session not found: %s", id)
	}

	lc := Lifecycle(s.lifecycle.Load())
	if lc == LifecycleCompacting || lc == LifecycleRestarting {
		return nil, fmt.Errorf("session busy (lifecycle=%d): %s", lc, id)
	}

	s.mu.Lock()
	if s.Status == StatusRunning {
		s.mu.Unlock()
		return nil, fmt.Errorf("session is still running: %s", id)
	}
	s.mu.Unlock()

	if !s.lifecycle.CompareAndSwap(int32(lc), int32(LifecycleRestarting)) {
		return nil, fmt.Errorf("session busy: %s", id)
	}

	tool := s.Tool
	workDir := s.WorkDir
	s.mu.Lock()
	args := s.Args
	toolSessionID := s.ToolSessionID
	s.mu.Unlock()

	clearRestarting := func() {
		s.lifecycle.Store(int32(LifecycleRunning))
	}

	// Verify session wasn't removed between Get and setting restarting flag
	m.mu.Lock()
	_, stillExists := m.sessions[id]
	m.mu.Unlock()
	if !stillExists {
		clearRestarting()
		return nil, fmt.Errorf("session not found: %s", id)
	}

	if !isAllowedTool(tool) {
		clearRestarting()
		return nil, fmt.Errorf("unsupported tool: %s", tool)
	}

	// Internal tools are resolved by platform functions, not LookPath
	var toolPath string
	if userTools[tool] {
		var err error
		toolPath, err = exec.LookPath(tool)
		if err != nil {
			clearRestarting()
			return nil, fmt.Errorf("tool not found: %s", tool)
		}
	}

	// Platform-specific cleanup of old session resources
	m.platformPrepareRestart(s)

	restartArgs := buildRestartArgs(tool, args, toolSessionID)

	var res *startResult
	var err error
	if userTools[tool] {
		s.mu.Lock()
		cols, rows := s.lastCols, s.lastRows
		s.mu.Unlock()
		res, err = m.platformStartUserTool(id, workDir, toolPath, restartArgs, cols, rows)
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
	s.done = make(chan struct{})
	s.doneOnce = sync.Once{}
	s.readDone = make(chan struct{})
	s.mu.Unlock()
	s.lifecycle.Store(int32(LifecycleRunning))

	// Re-enable context tracking after restart
	if s.context != nil {
		s.context.Restart()
		// Recreate transcript monitor for Claude with current session ID.
		// ReplaceTranscript stops the old monitor's goroutine.
		if tool == "claude" && toolSessionID != "" {
			tm := NewTranscriptMonitor(m.logger, workDir, toolSessionID, s.context, func(info *ContextInfo) {
				s.BroadcastContext(info)
			})
			s.context.ReplaceTranscript(tm)
		}
	}

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
		return fmt.Errorf("session not found: %s", id)
	}
	// Check for running children first to avoid orphans
	for _, cs := range m.sessions {
		if cs.ParentID == id {
			cs.mu.Lock()
			cStatus := cs.Status
			cs.mu.Unlock()
			if cStatus == StatusRunning {
				m.mu.Unlock()
				return fmt.Errorf("cannot remove session with running children: %s", id)
			}
		}
	}
	lc := Lifecycle(s.lifecycle.Load())
	if lc == LifecycleCompacting || lc == LifecycleRestarting {
		m.mu.Unlock()
		return fmt.Errorf("cannot remove busy session (lifecycle=%d): %s", lc, id)
	}
	s.mu.Lock()
	if s.Status == StatusRunning {
		s.mu.Unlock()
		m.mu.Unlock()
		return fmt.Errorf("cannot remove running session: %s", id)
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
		return fmt.Errorf("session not found: %s", id)
	}

	lc := Lifecycle(s.lifecycle.Load())
	if lc != LifecycleRunning {
		return fmt.Errorf("session not stoppable (lifecycle=%d): %s", lc, id)
	}

	s.mu.Lock()
	if s.Status != StatusRunning {
		s.mu.Unlock()
		return fmt.Errorf("session not running: %s", id)
	}
	s.mu.Unlock()

	return m.platformStop(s, id)
}

// TmuxAction executes a whitelisted tmux action on a terminal session.
func (m *Manager) TmuxAction(id, action string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}

	s.mu.Lock()
	tool := s.Tool
	status := s.Status
	toolSessionID := s.ToolSessionID
	s.mu.Unlock()

	if !internalTools[tool] {
		return fmt.Errorf("not a terminal session: %s", id)
	}
	if status != StatusRunning {
		return fmt.Errorf("session not running: %s", id)
	}
	if toolSessionID == "" {
		return fmt.Errorf("session has no tmux ID: %s", id)
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

			mode := OutputMode(s.outputMode.Load())
			switch mode {
			case OutputNormal:
				s.scrollback.Write(data)
				s.broadcast(data)
				s.noteOutputTime()

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

				// context tracking
				s.TrackContext(data)

			case OutputCapturing:
				s.appendCapture(data)

			case OutputSuppressing:
				// Don't write to scrollback or broadcast — prevents leaking
				// startup output on reconnect.
				s.noteOutputTime()
				// Still capture tool session IDs during suppression (codex)
				s.CaptureToolSessionID(data)
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
// Safe to call multiple times; only the first non-compacting call takes effect.
func (m *Manager) completeExit(s *Session, exitCode int) {
	lc := Lifecycle(s.lifecycle.Load())

	if lc == LifecycleCompacting {
		// Compaction in progress: signal compactReady but don't close done.
		// WebSocket connections stay alive.
		// Hold s.mu to ensure compactReady is initialized (see Run()).
		s.mu.Lock()
		s.compactOnce.Do(func() { close(s.compactReady) })
		s.mu.Unlock()
		return
	}

	if lc == LifecycleExited {
		return // already exited
	}

	// Atomically claim the exit transition; bail if another goroutine won.
	if !s.lifecycle.CompareAndSwap(int32(lc), int32(LifecycleExited)) {
		return
	}

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

	s.doneOnce.Do(func() { close(s.done) })
	m.save()

	// Stop context estimator (s.context is immutable after creation)
	if s.context != nil {
		s.context.Stop()
	}

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
