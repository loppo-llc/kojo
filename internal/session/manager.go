package session

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty/v2"
	"github.com/google/uuid"
)

const (
	// exitDrainTimeout is the maximum time to wait for readLoop to finish
	// draining output after the session process exits. On some platforms
	// (notably macOS), closing a FIFO fd opened with O_RDWR may not
	// reliably interrupt a blocked read(), so we use a timeout to prevent
	// finalizeTmuxSession from deadlocking and leaving the session stuck
	// in "running" state forever.
	exitDrainTimeout = 3 * time.Second

	// exitKillTimeout is the maximum time to wait for the attach process
	// to exit after being killed.
	exitKillTimeout = 5 * time.Second
)

var userTools = map[string]bool{
	"claude": true,
	"codex":  true,
	"gemini": true,
}

var internalTools = map[string]bool{
	"tmux": true,
}

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
	loadOK := m.loadPersistedSessions()
	// Only clean up orphaned tmux sessions when we successfully loaded
	// persisted state. On load failure, "known" would be empty and we'd
	// mistakenly kill all live kojo_ tmux sessions.
	if loadOK {
		m.cleanupOrphanedTmuxSessions()
	}
	return m
}

// loadPersistedSessions restores previously saved sessions.
// For tmux-backed sessions with live tmux processes, it reattaches and resumes monitoring.
// Returns true if the persisted state was loaded successfully (or was empty).
func (m *Manager) loadPersistedSessions() bool {
	infos, err := m.store.Load()
	if err != nil {
		m.logger.Error("failed to load persisted sessions, skipping orphan cleanup", "err", err)
		return false
	}
	for _, info := range infos {
		t, _ := time.Parse(time.RFC3339, info.CreatedAt)
		var lastOutput []byte
		if info.LastOutput != "" {
			lastOutput, _ = base64.StdEncoding.DecodeString(info.LastOutput)
		}
		s := &Session{
			ID:              info.ID,
			Tool:            info.Tool,
			WorkDir:         info.WorkDir,
			Args:            info.Args,
			CreatedAt:       t,
			Status:          StatusExited,
			ExitCode:        info.ExitCode,
			YoloMode:        info.YoloMode,
			Internal:        info.Internal || internalTools[info.Tool],
			ToolSessionID:   info.ToolSessionID,
			ParentID:        info.ParentID,
			TmuxSessionName: info.TmuxSessionName,
			lastCols:        info.LastCols,
			lastRows:        info.LastRows,
			scrollback:      NewRingBuffer(defaultRingSize),
			subscribers:     make(map[chan []byte]struct{}),
			done:            make(chan struct{}),
			lastOutput:      lastOutput,
		}

		restored := false

		// Check if tmux session is still alive and try to reattach
		if info.TmuxSessionName != "" && tmuxHasSession(info.TmuxSessionName) {
			dead, exitCode, err := tmuxPaneDead(info.TmuxSessionName)
			if err == nil && !dead {
				// Pane is running → reattach
				tmuxEnsureServerConfig()

				// Set up pipe-pane for raw output capture
				rawPipe, rawPipePath, pipeErr := tmuxStartPipePane(info.TmuxSessionName)
				if pipeErr != nil {
					m.logger.Warn("pipe-pane setup failed on restore", "id", info.ID, "err", pipeErr)
				}

				cmd := tmuxAttachCommand(info.TmuxSessionName)
				cmd.Env = append(os.Environ(), "TERM=xterm-256color")
				ws := defaultWinsize(info.LastCols, info.LastRows)
				ptmx, err := pty.StartWithSize(cmd, &ws)
				if err == nil {
					s.PTY = ptmx
					s.Cmd = cmd
					s.rawPipe = rawPipe
					s.rawPipePath = rawPipePath
					s.Status = StatusRunning
					s.ExitCode = nil
					s.lastOutput = nil
					s.readDone = make(chan struct{})
					restored = true

					// Capture current pane content so the terminal isn't blank
					// after server restart. pipe-pane only captures new output;
					// this seeds the scrollback with the existing screen state.
					// Only needed when rawPipe is active — in fallback mode the
					// attach PTY itself sends the initial screen redraw.
					if rawPipe != nil {
						if content := tmuxCapturePaneContent(info.TmuxSessionName); len(content) > 0 {
							s.scrollback.Write(content)
						}
					}

					go m.readLoop(s)
					if rawPipe != nil {
						go m.drainLoop(s)
					}
					go m.tmuxWaitLoop(s)

					m.logger.Info("reattached to persisted tmux session", "id", info.ID, "tmux", info.TmuxSessionName)
				} else {
					tmuxCleanupPipePane(info.TmuxSessionName, rawPipe, rawPipePath)
					m.logger.Error("failed to reattach persisted tmux session", "id", info.ID, "err", err)
					_ = tmuxKillSession(info.TmuxSessionName)
				}
			} else if dead {
				s.ExitCode = &exitCode
				_ = tmuxKillSession(info.TmuxSessionName)
			} else {
				// tmuxPaneDead returned error → can't determine state, kill to avoid orphan
				m.logger.Warn("failed to check tmux pane state, killing session", "id", info.ID, "tmux", info.TmuxSessionName, "err", err)
				_ = tmuxKillSession(info.TmuxSessionName)
			}
		}

		if !restored {
			close(s.done)
		}

		m.sessions[info.ID] = s
	}
	if len(infos) > 0 {
		m.logger.Info("restored persisted sessions", "count", len(infos))
	}
	return true
}

// cleanupOrphanedTmuxSessions kills kojo_ tmux sessions that are not tracked by the manager.
func (m *Manager) cleanupOrphanedTmuxSessions() {
	sessions, err := tmuxListKojoSessions()
	if err != nil {
		m.logger.Debug("failed to list tmux sessions for cleanup", "err", err)
		return
	}

	m.mu.Lock()
	known := make(map[string]bool)
	for _, s := range m.sessions {
		s.mu.Lock()
		if s.TmuxSessionName != "" && s.Status == StatusRunning {
			known[s.TmuxSessionName] = true
		}
		s.mu.Unlock()
	}
	m.mu.Unlock()

	for _, name := range sessions {
		if !known[name] {
			m.logger.Info("killing orphaned tmux session", "name", name)
			_ = tmuxKillSession(name)
		}
	}

	// Clean up stale FIFO files from previous crashes
	fifoDir := filepath.Join(os.TempDir(), "kojo")
	entries, err := os.ReadDir(fifoDir)
	if err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".pipe") {
				name := strings.TrimSuffix(e.Name(), ".pipe")
				if !known[name] {
					os.Remove(filepath.Join(fifoDir, e.Name()))
				}
			}
		}
	}
}

func (m *Manager) Create(tool, workDir string, args []string, yoloMode bool, parentID string) (*Session, error) {
	if !isAllowedTool(tool) {
		return nil, fmt.Errorf("unsupported tool: %s", tool)
	}

	toolPath, err := exec.LookPath(tool)
	if err != nil {
		return nil, fmt.Errorf("tool not found: %s", tool)
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
	if tool == "tmux" {
		toolSessionID = "kojo_" + id
		runArgs = []string{"new-session", "-A", "-s", toolSessionID, "-c", workDir}
	}

	// codex: session ID is captured from PTY output in readLoop
	// gemini: no session ID mechanism; uses --resume latest on restart

	var ptmx *os.File
	var cmd *exec.Cmd
	var tmuxName string

	var rawPipe *os.File
	var rawPipePath string

	if userTools[tool] {
		// User tools: run inside a tmux session for crash resilience
		tmuxName = tmuxSessionName(id)
		res, err := m.startTmuxAttach(tmuxName, workDir, toolPath, runArgs, 0, 0)
		if err != nil {
			return nil, err
		}
		ptmx, cmd, rawPipe, rawPipePath = res.ptmx, res.cmd, res.rawPipe, res.rawPipePath
	} else {
		// Internal tools (tmux): direct PTY as before
		cmd = exec.Command(toolPath, runArgs...)
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		ptmx, err = pty.Start(cmd)
		if err != nil {
			return nil, fmt.Errorf("failed to start pty: %w", err)
		}
	}

	s := &Session{
		ID:              id,
		Tool:            tool,
		WorkDir:         workDir,
		Args:            args,
		PTY:             ptmx,
		Cmd:             cmd,
		CreatedAt:       time.Now(),
		Status:          StatusRunning,
		YoloMode:        yoloMode,
		Internal:        internalTools[tool],
		ToolSessionID:   toolSessionID,
		ParentID:        parentID,
		TmuxSessionName: tmuxName,
		rawPipe:         rawPipe,
		rawPipePath:     rawPipePath,
		scrollback:      NewRingBuffer(defaultRingSize),
		subscribers:     make(map[chan []byte]struct{}),
		done:            make(chan struct{}),
		readDone:        make(chan struct{}),
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
					// Kill the PTY we just started and reap the process
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					ptmx.Close()
					tmuxCleanupPipePane(tmuxName, rawPipe, rawPipePath)
					if tmuxName != "" {
						_ = tmuxKillSession(tmuxName)
					}
					return existing, nil
				}
			}
		}
	}
	m.sessions[id] = s
	m.mu.Unlock()

	m.startLoops(s)

	m.logger.Info("session created", "id", id, "tool", tool, "workDir", workDir)
	m.save()
	return s, nil
}

func (m *Manager) Restart(id string) (*Session, error) {
	s, ok := m.Get(id)
	if !ok {
		return nil, fmt.Errorf("session not found: %s", id)
	}

	s.mu.Lock()
	if s.Status == StatusRunning || s.restarting {
		s.mu.Unlock()
		return nil, fmt.Errorf("session is still running: %s", id)
	}
	// Set restarting flag to prevent concurrent restarts and Stop during restart
	s.restarting = true
	tool := s.Tool
	workDir := s.WorkDir
	args := s.Args
	toolSessionID := s.ToolSessionID
	tmuxName := s.TmuxSessionName
	s.mu.Unlock()

	clearRestarting := func() {
		s.mu.Lock()
		s.restarting = false
		s.mu.Unlock()
	}

	if !isAllowedTool(tool) {
		clearRestarting()
		return nil, fmt.Errorf("unsupported tool: %s", tool)
	}

	toolPath, err := exec.LookPath(tool)
	if err != nil {
		clearRestarting()
		return nil, fmt.Errorf("tool not found: %s", tool)
	}

	// Clean up old pipe-pane FIFO if it exists
	s.mu.Lock()
	s.cleanupPipePane()
	s.mu.Unlock()

	// Clean up old tmux session if it still exists
	if tmuxName != "" && tmuxHasSession(tmuxName) {
		_ = tmuxKillSession(tmuxName)
	}

	restartArgs := buildRestartArgs(tool, args, toolSessionID)

	var ptmx *os.File
	var cmd *exec.Cmd
	var rawPipe *os.File
	var rawPipePath string

	if userTools[tool] {
		// User tools: run inside a tmux session
		if tmuxName == "" {
			tmuxName = tmuxSessionName(id)
		}
		s.mu.Lock()
		cols, rows := s.lastCols, s.lastRows
		s.mu.Unlock()
		res, err := m.startTmuxAttach(tmuxName, workDir, toolPath, restartArgs, cols, rows)
		if err != nil {
			clearRestarting()
			return nil, err
		}
		ptmx, cmd, rawPipe, rawPipePath = res.ptmx, res.cmd, res.rawPipe, res.rawPipePath
	} else {
		// Internal tools: direct PTY
		cmd = exec.Command(toolPath, restartArgs...)
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
		ptmx, err = pty.Start(cmd)
		if err != nil {
			clearRestarting()
			return nil, fmt.Errorf("failed to start pty: %w", err)
		}
	}

	s.mu.Lock()
	s.PTY = ptmx
	s.Cmd = cmd
	s.Args = args // Keep original args (without --resume), not restartArgs
	s.TmuxSessionName = tmuxName
	s.rawPipe = rawPipe
	s.rawPipePath = rawPipePath
	s.Status = StatusRunning
	s.ExitCode = nil
	s.lastOutput = nil
	s.restarting = false
	s.done = make(chan struct{})
	s.readDone = make(chan struct{})
	s.mu.Unlock()

	m.startLoops(s)

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
// Returns the first matching running session, or any matching session if none are running.
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

func (m *Manager) Stop(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}

	s.mu.Lock()
	if s.Status != StatusRunning || s.restarting {
		s.mu.Unlock()
		return fmt.Errorf("session not running: %s", id)
	}
	cmd := s.Cmd
	tool := s.Tool
	toolSessionID := s.ToolSessionID
	tmuxName := s.TmuxSessionName
	s.mu.Unlock()

	// Kill tmux session backing this session (sends SIGHUP to the CLI process)
	if tmuxName != "" {
		_ = tmuxKillSession(tmuxName)
	}

	// Kill tmux session for internal tmux tool
	if tool == "tmux" && toolSessionID != "" {
		_ = exec.Command("tmux", "kill-session", "-t", toolSessionID).Run()
	}

	// Also stop any child sessions (e.g. tmux terminal tab)
	for _, child := range m.findChildSessions(id, "tmux") {
		child.mu.Lock()
		childStatus := child.Status
		child.mu.Unlock()
		if childStatus == StatusRunning {
			_ = m.Stop(child.ID)
		}
	}

	if cmd != nil && cmd.Process != nil {
		// SIGTERM to the attach/direct process
		_ = cmd.Process.Signal(syscall.SIGTERM)

		// give 5 seconds then SIGKILL
		go func() {
			select {
			case <-s.done:
				return
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
			}
		}()
	}

	return nil
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	m.shuttingDown = true
	m.mu.Unlock()

	// Collect session IDs by type
	m.mu.Lock()
	var nonTmuxIDs []string
	var tmuxSessions []*Session
	for id, s := range m.sessions {
		s.mu.Lock()
		running := s.Status == StatusRunning
		isTmuxBacked := s.TmuxSessionName != ""
		s.mu.Unlock()
		if !running {
			continue
		}
		if isTmuxBacked {
			tmuxSessions = append(tmuxSessions, s)
		} else {
			nonTmuxIDs = append(nonTmuxIDs, id)
		}
	}
	m.mu.Unlock()

	// Stop non-tmux sessions (internal tools) and wait
	for _, id := range nonTmuxIDs {
		_ = m.Stop(id)
	}
	for _, id := range nonTmuxIDs {
		if s, ok := m.Get(id); ok {
			select {
			case <-s.done:
			case <-time.After(10 * time.Second):
			}
		}
	}

	// Detach tmux-backed sessions: kill attach process, close PTY, but keep tmux session alive
	for _, s := range tmuxSessions {
		s.mu.Lock()
		// Stop pipe-pane to avoid orphaned cat processes
		s.cleanupPipePane()
		if s.Cmd != nil && s.Cmd.Process != nil {
			_ = s.Cmd.Process.Kill()
		}
		if s.PTY != nil {
			s.PTY.Close()
			s.PTY = nil
		}
		s.mu.Unlock()
	}
}

// SaveAll persists all sessions to disk. Called on shutdown.
func (m *Manager) SaveAll() {
	m.save()
}

func (m *Manager) save() {
	m.mu.Lock()
	infos := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		infos = append(infos, s.Info())
	}
	m.mu.Unlock()
	m.store.Save(infos)
}

func (m *Manager) readLoop(s *Session) {
	defer close(s.readDone)

	s.mu.Lock()
	// Prefer raw pipe (pipe-pane FIFO) for complete output capture;
	// fall back to attach PTY output (may lose intermediate scroll content).
	var reader *os.File
	if s.rawPipe != nil {
		reader = s.rawPipe
	} else {
		reader = s.PTY
	}
	s.mu.Unlock()

	if reader == nil {
		return
	}

	buf := make([]byte, 32*1024)
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
				time.AfterFunc(100*time.Millisecond, func() {
					if !s.IsYoloMode() {
						return
					}
					if _, err := s.Write([]byte("\r")); err != nil {
						m.logger.Debug("yolo write error", "id", s.ID, "err", err)
					}
				})
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

// drainLoop reads and discards output from the attach PTY to prevent its buffer
// from filling up and blocking tmux. Only used when rawPipe is active (readLoop
// reads from the FIFO instead).
func (m *Manager) drainLoop(s *Session) {
	s.mu.Lock()
	ptmx := s.PTY
	s.mu.Unlock()
	if ptmx == nil {
		return
	}
	buf := make([]byte, 32*1024)
	for {
		if _, err := ptmx.Read(buf); err != nil {
			return
		}
	}
}

// waitLoop monitors a direct PTY process (internal tools only).
func (m *Manager) waitLoop(s *Session) {
	err := s.Cmd.Wait()

	// close PTY so readLoop drains remaining data and exits
	s.mu.Lock()
	if s.PTY != nil {
		s.PTY.Close()
		s.PTY = nil
	}
	s.mu.Unlock()

	// wait for readLoop to finish draining (PTY close above guarantees exit)
	<-s.readDone

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	m.completeExit(s, exitCode)
}

// tmuxWaitLoop monitors a tmux-backed session by polling pane status
// and watching the attach process.
func (m *Manager) tmuxWaitLoop(s *Session) {
	const maxConsecutiveErrors = 10

	// Goroutine to reap the attach process
	attachExited := make(chan struct{})
	go func() {
		s.mu.Lock()
		cmd := s.Cmd
		s.mu.Unlock()
		if cmd != nil {
			_ = cmd.Wait()
		}
		close(attachExited)
	}()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	consecutiveErrors := 0

	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			shuttingDown := m.shuttingDown
			m.mu.Unlock()
			if shuttingDown {
				return
			}

			s.mu.Lock()
			tmuxName := s.TmuxSessionName
			s.mu.Unlock()

			if !tmuxHasSession(tmuxName) {
				// tmux session gone entirely
				m.finalizeTmuxSession(s, 1, attachExited)
				return
			}

			dead, exitCode, err := tmuxPaneDead(tmuxName)
			if err != nil {
				consecutiveErrors++
				if consecutiveErrors >= maxConsecutiveErrors {
					m.logger.Error("tmux pane check failed repeatedly, finalizing session", "id", s.ID, "err", err)
					_ = tmuxKillSession(tmuxName)
					m.finalizeTmuxSession(s, 1, attachExited)
					return
				}
				continue
			}
			consecutiveErrors = 0
			if dead {
				_ = tmuxKillSession(tmuxName)
				m.finalizeTmuxSession(s, exitCode, attachExited)
				return
			}

			// Check if readLoop exited unexpectedly (FIFO failure).
			// If pipe-pane died but the pane is still running, force a
			// reattach by killing the attach process. The attachExited
			// handler will create a new pipe-pane and readLoop.
			s.mu.Lock()
			readDone := s.readDone
			hasRawPipe := s.rawPipe != nil
			s.mu.Unlock()
			if hasRawPipe {
				select {
				case <-readDone:
					m.logger.Warn("pipe-pane FIFO lost, forcing reattach", "id", s.ID)
					// Stop the stale pipe-pane command, close fd, remove FIFO
					s.mu.Lock()
					s.cleanupPipePane()
					cmd := s.Cmd
					s.mu.Unlock()
					// Kill attach → triggers attachExited → reattach with new pipe-pane
					if cmd != nil && cmd.Process != nil {
						_ = cmd.Process.Kill()
					}
				default:
					// readLoop still running, all good
				}
			}

		case <-attachExited:
			// Attach process exited. Check if we're shutting down.
			m.mu.Lock()
			shuttingDown := m.shuttingDown
			m.mu.Unlock()
			if shuttingDown {
				return
			}

			// Close only the attach PTY (drainLoop exits naturally).
			// Keep pipe-pane/FIFO alive so readLoop continues capturing output.
			s.mu.Lock()
			if s.PTY != nil {
				s.PTY.Close()
				s.PTY = nil
			}
			tmuxName := s.TmuxSessionName
			hasRawPipe := s.rawPipe != nil
			s.mu.Unlock()

			// If no pipe-pane, readLoop was using the PTY — wait for it.
			// If pipe-pane is active but readLoop died concurrently (FIFO
			// failure just before attach exit), clean up so reattach does
			// a full pipe-pane recreation instead of assuming it's healthy.
			if !hasRawPipe {
				m.awaitReadDone(s)
			} else {
				select {
				case <-s.readDone:
					// readLoop died — clean up stale pipe-pane
					s.mu.Lock()
					s.cleanupPipePane()
					s.mu.Unlock()
					hasRawPipe = false
				default:
					// readLoop still healthy
				}
			}

			if !tmuxHasSession(tmuxName) {
				if hasRawPipe {
					s.mu.Lock()
					s.cleanupPipePane()
					s.mu.Unlock()
					m.awaitReadDone(s)
				}
				m.completeExit(s, 1)
				return
			}

			dead, exitCode, _ := tmuxPaneDead(tmuxName)
			if dead {
				_ = tmuxKillSession(tmuxName)
				if hasRawPipe {
					s.mu.Lock()
					s.cleanupPipePane()
					s.mu.Unlock()
					m.awaitReadDone(s)
				}
				m.completeExit(s, exitCode)
				return
			}

			// tmux session still alive with running pane → reattach
			if err := m.reattachTmux(s); err != nil {
				m.logger.Error("failed to reattach tmux", "id", s.ID, "err", err)
				if hasRawPipe {
					s.mu.Lock()
					s.cleanupPipePane()
					s.mu.Unlock()
					m.awaitReadDone(s)
				}
				m.completeExit(s, 1)
				return
			}

			// Start new reaper for the new attach process
			attachExited = make(chan struct{})
			go func() {
				s.mu.Lock()
				cmd := s.Cmd
				s.mu.Unlock()
				if cmd != nil {
					_ = cmd.Wait()
				}
				close(attachExited)
			}()
		}
	}
}

// finalizeTmuxSession handles the case where the tmux pane is dead or gone,
// killing the still-running attach process and cleaning up.
func (m *Manager) finalizeTmuxSession(s *Session, exitCode int, attachExited <-chan struct{}) {
	// Kill attach process if still running
	s.mu.Lock()
	if s.Cmd != nil && s.Cmd.Process != nil {
		_ = s.Cmd.Process.Kill()
	}
	s.mu.Unlock()

	// Wait for attach process reaper (with timeout as safety net)
	select {
	case <-attachExited:
	case <-time.After(exitKillTimeout):
		m.logger.Warn("attach process did not exit in time after kill", "id", s.ID)
	}

	// Clean up pipe-pane and close PTY so readLoop/drainLoop exit
	s.mu.Lock()
	s.cleanupPipePane()
	if s.PTY != nil {
		s.PTY.Close()
		s.PTY = nil
	}
	s.mu.Unlock()

	// Wait for readLoop to drain remaining output
	m.awaitReadDone(s)

	m.completeExit(s, exitCode)
}

// tmuxAttachResult holds the outputs from startTmuxAttach.
type tmuxAttachResult struct {
	ptmx        *os.File
	cmd         *exec.Cmd
	rawPipe     *os.File
	rawPipePath string
}

// startTmuxAttach creates a tmux session, sets up pipe-pane, and attaches via PTY.
// Used by Create and Restart to avoid duplicating the tmux startup flow.
func (m *Manager) startTmuxAttach(tmuxName, workDir, toolPath string, args []string, cols, rows uint16) (*tmuxAttachResult, error) {
	shellCmd := buildShellCommand(toolPath, args)
	if err := tmuxNewSession(tmuxName, workDir, shellCmd, true); err != nil {
		return nil, fmt.Errorf("failed to create tmux session: %w", err)
	}

	var rawPipe *os.File
	var rawPipePath string
	rp, rpPath, pipeErr := tmuxStartPipePane(tmuxName)
	if pipeErr != nil {
		m.logger.Warn("pipe-pane setup failed", "tmux", tmuxName, "err", pipeErr)
	} else {
		rawPipe = rp
		rawPipePath = rpPath
	}

	cmd := tmuxAttachCommand(tmuxName)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ws := defaultWinsize(cols, rows)
	ptmx, err := pty.StartWithSize(cmd, &ws)
	if err != nil {
		tmuxCleanupPipePane(tmuxName, rawPipe, rawPipePath)
		_ = tmuxKillSession(tmuxName)
		return nil, fmt.Errorf("failed to attach to tmux session: %w", err)
	}

	return &tmuxAttachResult{ptmx: ptmx, cmd: cmd, rawPipe: rawPipe, rawPipePath: rawPipePath}, nil
}

// startLoops starts the background goroutines (readLoop, drainLoop, waitLoop) for a session.
func (m *Manager) startLoops(s *Session) {
	go m.readLoop(s)
	s.mu.Lock()
	hasRawPipe := s.rawPipe != nil
	hasTmux := s.TmuxSessionName != ""
	s.mu.Unlock()
	if hasRawPipe {
		go m.drainLoop(s)
	}
	if hasTmux {
		go m.tmuxWaitLoop(s)
	} else {
		go m.waitLoop(s)
	}
}

// reattachTmux creates a new PTY attach to an existing tmux session.
// If pipe-pane is already active (rawPipe != nil), it is kept running and only
// the attach PTY is recreated. Otherwise a full pipe-pane+readLoop is set up.
func (m *Manager) reattachTmux(s *Session) error {
	s.mu.Lock()
	tmuxName := s.TmuxSessionName
	pipeAlreadyActive := s.rawPipe != nil
	readDone := s.readDone
	s.mu.Unlock()

	// Double-check: if pipe-pane appears active but readLoop already died
	// (TOCTOU between caller's check and here), clean up and do full recreation.
	if pipeAlreadyActive {
		select {
		case <-readDone:
			s.mu.Lock()
			s.cleanupPipePane()
			s.mu.Unlock()
			pipeAlreadyActive = false
		default:
		}
	}

	tmuxEnsureServerConfig()

	// Only set up pipe-pane if there is no active one
	var rawPipe *os.File
	var rawPipePath string
	if !pipeAlreadyActive {
		rp, rpPath, pipeErr := tmuxStartPipePane(tmuxName)
		if pipeErr != nil {
			m.logger.Warn("pipe-pane setup failed on reattach", "id", s.ID, "err", pipeErr)
		} else {
			rawPipe = rp
			rawPipePath = rpPath
		}
	}

	cmd := tmuxAttachCommand(tmuxName)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	s.mu.Lock()
	ws := defaultWinsize(s.lastCols, s.lastRows)
	s.mu.Unlock()
	ptmx, err := pty.StartWithSize(cmd, &ws)
	if err != nil {
		if rawPipe != nil {
			tmuxCleanupPipePane(tmuxName, rawPipe, rawPipePath)
		}
		return fmt.Errorf("reattach pty.Start: %w", err)
	}

	s.mu.Lock()
	s.PTY = ptmx
	s.Cmd = cmd
	if rawPipe != nil {
		s.rawPipe = rawPipe
		s.rawPipePath = rawPipePath
		s.readDone = make(chan struct{})
	}
	s.mu.Unlock()

	// New readLoop only if we created a new pipe-pane (otherwise existing one continues)
	if rawPipe != nil {
		go m.readLoop(s)
	}
	// Always drain the new attach PTY when pipe-pane is capturing output
	s.mu.Lock()
	hasPipe := s.rawPipe != nil
	s.mu.Unlock()
	if hasPipe {
		go m.drainLoop(s)
	}

	m.logger.Info("reattached to tmux session", "id", s.ID, "tmux", tmuxName)
	return nil
}

// awaitReadDone waits for readLoop to finish with a timeout.
// If readLoop does not exit within exitDrainTimeout, it logs a warning
// and returns so that completeExit can proceed. This prevents deadlock
// when closing a FIFO fd does not reliably interrupt a blocked read().
func (m *Manager) awaitReadDone(s *Session) {
	select {
	case <-s.readDone:
	case <-time.After(exitDrainTimeout):
		m.logger.Warn("readLoop did not exit in time, proceeding with session exit", "id", s.ID)
	}
}

// completeExit captures final output, updates session state, and notifies.
// Shared by waitLoop (internal tools) and tmux exit paths.
func (m *Manager) completeExit(s *Session, exitCode int) {
	// capture last output from scrollback
	const maxLastOutput = 8192
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

	// Stop child sessions (e.g. tmux terminal tab) when parent exits
	for _, child := range m.findChildSessions(s.ID, "tmux") {
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
			// Strip any existing continuation/resume flags
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
		// codex uses a subcommand: `codex resume <SESSION_ID>`
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
				skipNext = true // skip the value after the flag
				continue
			}
			args = append(args, a)
		}
		return append(args, "--resume", "latest")

	case "tmux":
		if toolSessionID != "" {
			return []string{"new-session", "-A", "-s", toolSessionID}
		}
		return origArgs

	default:
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
