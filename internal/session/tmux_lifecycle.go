//go:build !windows

package session

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/creack/pty/v2"
)

// --- tmux lifecycle methods (extracted from platform_unix.go) ---

// loadPersistedSessions restores previously saved sessions.
func (m *Manager) loadPersistedSessions() bool {
	infos, err := m.store.Load()
	if err != nil {
		m.logger.Error("failed to load persisted sessions, skipping orphan cleanup", "err", err)
		return false
	}
	for _, info := range infos {
		s := m.restoreSession(info)
		m.sessions[info.ID] = s
	}
	if len(infos) > 0 {
		m.logger.Info("restored persisted sessions", "count", len(infos))
	}
	return true
}

// restoreSession creates a Session from persisted info, reattaching to a live tmux session if possible.
func (m *Manager) restoreSession(info SessionInfo) *Session {
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
		attachments:     make(map[string]*Attachment, len(info.Attachments)),
	}
	for _, att := range info.Attachments {
		if att == nil || att.Path == "" {
			continue
		}
		s.attachments[att.Path] = att
	}

	restored := false
	if info.TmuxSessionName != "" && tmuxHasSession(info.TmuxSessionName) {
		restored = m.tryReattachPersistedTmux(s, info)
	}

	if !restored {
		close(s.done)
	}
	return s
}

// tryReattachPersistedTmux attempts to reattach to a persisted tmux session.
func (m *Manager) tryReattachPersistedTmux(s *Session, info SessionInfo) bool {
	dead, exitCode, err := tmuxPaneDead(info.TmuxSessionName)
	if err != nil {
		m.logger.Warn("failed to check tmux pane state, killing session", "id", info.ID, "tmux", info.TmuxSessionName, "err", err)
		_ = tmuxKillSession(info.TmuxSessionName)
		return false
	}
	if dead {
		s.ExitCode = &exitCode
		_ = tmuxKillSession(info.TmuxSessionName)
		return false
	}

	tmuxEnsureServerConfig()

	rawPipe, rawPipePath, pipeErr := tmuxStartPipePane(info.TmuxSessionName)
	if pipeErr != nil {
		m.logger.Warn("pipe-pane setup failed on restore", "id", info.ID, "err", pipeErr)
	}

	cmd := tmuxAttachCommand(info.TmuxSessionName)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ws := defaultWinsize(info.LastCols, info.LastRows)
	ptmx, err := pty.StartWithSize(cmd, &ws)
	if err != nil {
		tmuxCleanupPipePane(info.TmuxSessionName, rawPipe, rawPipePath)
		m.logger.Error("failed to reattach persisted tmux session", "id", info.ID, "err", err)
		_ = tmuxKillSession(info.TmuxSessionName)
		return false
	}

	s.PTY = ptmx
	s.Cmd = cmd
	s.rawPipe = rawPipe
	s.rawPipePath = rawPipePath
	s.Status = StatusRunning
	s.ExitCode = nil
	s.lastOutput = nil
	s.readDone = make(chan struct{})

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
	return true
}

// cleanupOrphanedTmuxSessions kills kojo_ tmux sessions that are not tracked.
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

// drainLoop reads and discards output from the attach PTY to prevent its buffer
// from filling up and blocking tmux. Only used when rawPipe is active.
func (m *Manager) drainLoop(s *Session) {
	s.mu.Lock()
	ptmx := s.PTY
	s.mu.Unlock()
	if ptmx == nil {
		return
	}
	buf := make([]byte, readBufSize)
	for {
		if _, err := ptmx.Read(buf); err != nil {
			return
		}
	}
}

// tmuxWaitLoop monitors a tmux-backed session by polling pane status
// and watching the attach process.
func (m *Manager) tmuxWaitLoop(s *Session) {
	attachExited := m.startAttachReaper(s)

	ticker := time.NewTicker(paneStatusPollInterval)
	defer ticker.Stop()

	consecutiveErrors := 0

	for {
		select {
		case <-ticker.C:
			action := m.handlePanePoll(s, &consecutiveErrors, attachExited)
			switch action {
			case pollDone:
				return
			case pollRetry:
				continue
			}

		case <-attachExited:
			newCh, done := m.handleAttachExit(s)
			if done {
				return
			}
			attachExited = newCh
		}
	}
}

// pollAction represents the outcome of a pane status poll.
type pollAction int

const (
	pollOK    pollAction = iota
	pollDone
	pollRetry
)

// handlePanePoll checks tmux pane status on each tick.
func (m *Manager) handlePanePoll(s *Session, consecutiveErrors *int, attachExited <-chan struct{}) pollAction {
	m.mu.Lock()
	shuttingDown := m.shuttingDown
	m.mu.Unlock()
	if shuttingDown {
		return pollDone
	}

	s.mu.Lock()
	tmuxName := s.TmuxSessionName
	s.mu.Unlock()

	if !tmuxHasSession(tmuxName) {
		m.finalizeTmuxSession(s, 1, attachExited)
		return pollDone
	}

	dead, exitCode, err := tmuxPaneDead(tmuxName)
	if err != nil {
		*consecutiveErrors++
		if *consecutiveErrors >= maxPaneCheckErrors {
			m.logger.Error("tmux pane check failed repeatedly, finalizing session", "id", s.ID, "err", err)
			_ = tmuxKillSession(tmuxName)
			m.finalizeTmuxSession(s, 1, attachExited)
			return pollDone
		}
		return pollRetry
	}
	*consecutiveErrors = 0
	if dead {
		_ = tmuxKillSession(tmuxName)
		m.finalizeTmuxSession(s, exitCode, attachExited)
		return pollDone
	}

	// Check if readLoop exited unexpectedly (FIFO failure).
	s.mu.Lock()
	readDone := s.readDone
	hasRawPipe := s.rawPipe != nil
	s.mu.Unlock()
	if hasRawPipe {
		select {
		case <-readDone:
			m.logger.Warn("pipe-pane FIFO lost, forcing reattach", "id", s.ID)
			s.mu.Lock()
			s.cleanupPipePane()
			cmd := s.Cmd
			s.mu.Unlock()
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		default:
		}
	}

	return pollOK
}

// handleAttachExit handles the case when the tmux attach process exits.
func (m *Manager) handleAttachExit(s *Session) (chan struct{}, bool) {
	m.mu.Lock()
	shuttingDown := m.shuttingDown
	m.mu.Unlock()
	if shuttingDown {
		return nil, true
	}

	s.mu.Lock()
	if s.PTY != nil {
		s.PTY.Close()
		s.PTY = nil
	}
	tmuxName := s.TmuxSessionName
	hasRawPipe := s.rawPipe != nil
	s.mu.Unlock()

	if !hasRawPipe {
		m.awaitReadDone(s)
	} else {
		select {
		case <-s.readDone:
			s.mu.Lock()
			s.cleanupPipePane()
			s.mu.Unlock()
			hasRawPipe = false
		default:
		}
	}

	if !tmuxHasSession(tmuxName) {
		m.cleanupPipeAndExit(s, hasRawPipe, 1)
		return nil, true
	}

	dead, exitCode, _ := tmuxPaneDead(tmuxName)
	if dead {
		_ = tmuxKillSession(tmuxName)
		m.cleanupPipeAndExit(s, hasRawPipe, exitCode)
		return nil, true
	}

	if err := m.reattachTmux(s); err != nil {
		m.logger.Error("failed to reattach tmux", "id", s.ID, "err", err)
		m.cleanupPipeAndExit(s, hasRawPipe, 1)
		return nil, true
	}

	return m.startAttachReaper(s), false
}

// cleanupPipeAndExit cleans up pipe-pane if active and completes session exit.
func (m *Manager) cleanupPipeAndExit(s *Session, hasRawPipe bool, exitCode int) {
	if hasRawPipe {
		s.mu.Lock()
		s.cleanupPipePane()
		s.mu.Unlock()
		m.awaitReadDone(s)
	}
	m.completeExit(s, exitCode)
}

// startAttachReaper starts a goroutine that waits for the attach process to exit.
func (m *Manager) startAttachReaper(s *Session) chan struct{} {
	ch := make(chan struct{})
	go func() {
		s.mu.Lock()
		cmd := s.Cmd
		s.mu.Unlock()
		if cmd != nil {
			_ = cmd.Wait()
		}
		close(ch)
	}()
	return ch
}

// finalizeTmuxSession handles the case where the tmux pane is dead or gone.
func (m *Manager) finalizeTmuxSession(s *Session, exitCode int, attachExited <-chan struct{}) {
	s.mu.Lock()
	if s.Cmd != nil && s.Cmd.Process != nil {
		_ = s.Cmd.Process.Kill()
	}
	s.mu.Unlock()

	select {
	case <-attachExited:
	case <-time.After(exitKillTimeout):
		m.logger.Warn("attach process did not exit in time after kill", "id", s.ID)
	}

	s.mu.Lock()
	s.cleanupPipePane()
	if s.PTY != nil {
		s.PTY.Close()
		s.PTY = nil
	}
	s.mu.Unlock()

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

// reattachTmux creates a new PTY attach to an existing tmux session.
func (m *Manager) reattachTmux(s *Session) error {
	s.mu.Lock()
	tmuxName := s.TmuxSessionName
	pipeAlreadyActive := s.rawPipe != nil
	readDone := s.readDone
	s.mu.Unlock()

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

	if rawPipe != nil {
		go m.readLoop(s)
	}
	s.mu.Lock()
	hasPipe := s.rawPipe != nil
	s.mu.Unlock()
	if hasPipe {
		go m.drainLoop(s)
	}

	m.logger.Info("reattached to tmux session", "id", s.ID, "tmux", tmuxName)
	return nil
}
