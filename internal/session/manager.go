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
	"syscall"
	"time"

	"github.com/creack/pty/v2"
	"github.com/google/uuid"
)

var allowedTools = map[string]bool{
	"claude": true,
	"codex":  true,
	"gemini": true,
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	logger   *slog.Logger
	store    *Store

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
	m.loadPersistedSessions()
	return m
}

// loadPersistedSessions restores previously saved sessions as exited.
func (m *Manager) loadPersistedSessions() {
	infos := m.store.Load()
	for _, info := range infos {
		t, _ := time.Parse(time.RFC3339, info.CreatedAt)
		done := make(chan struct{})
		close(done)
		s := &Session{
			ID:              info.ID,
			Tool:            info.Tool,
			WorkDir:         info.WorkDir,
			Args:            info.Args,
			CreatedAt:       t,
			Status:          StatusExited,
			ExitCode:        info.ExitCode,
			YoloMode:        info.YoloMode,
			ToolSessionID: info.ToolSessionID,
			scrollback:      NewRingBuffer(defaultRingSize),
			subscribers:     make(map[chan []byte]struct{}),
			done:            done,
		}
		m.sessions[info.ID] = s
	}
	if len(infos) > 0 {
		m.logger.Info("restored persisted sessions", "count", len(infos))
	}
}

func (m *Manager) Create(tool, workDir string, args []string, yoloMode bool) (*Session, error) {
	if !allowedTools[tool] {
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
	// codex: session ID is captured from PTY output in readLoop
	// gemini: no session ID mechanism; uses --resume latest on restart

	cmd := exec.Command(toolPath, runArgs...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start pty: %w", err)
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
		ToolSessionID: toolSessionID,
		scrollback:      NewRingBuffer(defaultRingSize),
		subscribers:     make(map[chan []byte]struct{}),
		done:            make(chan struct{}),
	}

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	// read PTY output
	go m.readLoop(s)

	// wait for process exit
	go m.waitLoop(s)

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
	if s.Status == StatusRunning {
		s.mu.Unlock()
		return nil, fmt.Errorf("session is still running: %s", id)
	}
	tool := s.Tool
	workDir := s.WorkDir
	args := s.Args
	toolSessionID := s.ToolSessionID
	s.mu.Unlock()

	if !allowedTools[tool] {
		return nil, fmt.Errorf("unsupported tool: %s", tool)
	}

	toolPath, err := exec.LookPath(tool)
	if err != nil {
		return nil, fmt.Errorf("tool not found: %s", tool)
	}

	restartArgs := buildRestartArgs(tool, args, toolSessionID)

	cmd := exec.Command(toolPath, restartArgs...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start pty: %w", err)
	}

	s.mu.Lock()
	s.PTY = ptmx
	s.Cmd = cmd
	s.Args = args // Keep original args (without --resume), not restartArgs
	s.Status = StatusRunning
	s.ExitCode = nil
	s.done = make(chan struct{})
	s.mu.Unlock()

	go m.readLoop(s)
	go m.waitLoop(s)

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

func (m *Manager) Stop(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}

	s.mu.Lock()
	if s.Status != StatusRunning {
		s.mu.Unlock()
		return fmt.Errorf("session not running: %s", id)
	}
	cmd := s.Cmd
	s.mu.Unlock()

	if cmd.Process != nil {
		// SIGTERM to the process; closing PTY also sends SIGHUP
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
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		_ = m.Stop(id)
	}

	// wait for all to finish
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	for _, s := range sessions {
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
		}
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
	s.mu.Lock()
	ptmx := s.PTY
	s.mu.Unlock()

	buf := make([]byte, 32*1024)
	for {
		n, err := ptmx.Read(buf)
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

func (m *Manager) waitLoop(s *Session) {
	err := s.Cmd.Wait()

	s.mu.Lock()
	s.Status = StatusExited
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			s.ExitCode = &code
		}
	} else {
		code := 0
		s.ExitCode = &code
	}
	if s.PTY != nil {
		s.PTY.Close()
		s.PTY = nil
	}
	s.mu.Unlock()

	close(s.done)
	m.save()

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

// ToolAvailability checks which tools are available on this system.
func ToolAvailability() map[string]ToolInfo {
	result := make(map[string]ToolInfo)
	for tool := range allowedTools {
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
