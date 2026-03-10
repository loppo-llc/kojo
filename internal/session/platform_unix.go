//go:build !windows

package session

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty/v2"
)

func init() {
	internalTools["tmux"] = true
}

// ShellToolName returns the internal tool name for terminal sessions.
func ShellToolName() string { return "tmux" }

// platformInit loads persisted sessions and cleans up orphaned tmux sessions.
func (m *Manager) platformInit() {
	loadOK := m.loadPersistedSessions()
	if loadOK {
		m.cleanupOrphanedTmuxSessions()
	}
}

// platformStartUserTool starts a user-facing tool inside a tmux session.
func (m *Manager) platformStartUserTool(id, workDir, toolPath string, args []string, cols, rows uint16) (*startResult, error) {
	tmuxName := tmuxSessionName(id)
	res, err := m.startTmuxAttach(tmuxName, workDir, toolPath, args, cols, rows)
	if err != nil {
		return nil, err
	}
	return &startResult{
		pty:         res.ptmx,
		cmd:         res.cmd,
		rawPipe:     res.rawPipe,
		rawPipePath: res.rawPipePath,
		tmuxName:    tmuxName,
	}, nil
}

// platformStartInternalTool starts an internal tool (tmux) with a direct PTY.
func (m *Manager) platformStartInternalTool(id, tool, toolPath, workDir string, args []string, toolSessionID string) (*startResult, error) {
	// Internal tools resolve their own executable (toolPath may be empty)
	if toolPath == "" {
		var err error
		toolPath, err = exec.LookPath(tool)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrToolNotFound, tool)
		}
	}
	cmd := exec.Command(toolPath, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start pty: %w", err)
	}
	if tool == "tmux" && toolSessionID != "" {
		tmuxEnableMouse(toolSessionID)
		tmuxSetLoginShell(toolSessionID)
	}
	return &startResult{pty: ptmx, cmd: cmd}, nil
}

// platformStartLoops starts the background goroutines for a session.
func (m *Manager) platformStartLoops(s *Session) {
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

// platformStop stops a session, killing the tmux session and sending SIGTERM.
func (m *Manager) platformStop(s *Session, id string) error {
	s.mu.Lock()
	cmd := s.Cmd
	tmuxName := s.TmuxSessionName
	tool := s.Tool
	toolSessionID := s.ToolSessionID
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
		_ = sendTermSignal(cmd.Process)

		go func() {
			select {
			case <-s.done:
				return
			case <-time.After(stopKillTimeout):
				_ = cmd.Process.Kill()
			}
		}()
	}

	return nil
}

// platformStopAll stops all sessions on shutdown.
// Tmux-backed sessions are detached (keep alive); non-tmux sessions are killed.
func (m *Manager) platformStopAll() {
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
			case <-time.After(shutdownTimeout):
			}
		}
	}

	// Detach tmux-backed sessions: kill attach process, close PTY, but keep tmux session alive
	for _, s := range tmuxSessions {
		s.mu.Lock()
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

// platformCleanupDuplicate cleans up resources when a duplicate child is found.
func (m *Manager) platformCleanupDuplicate(res *startResult) {
	if res.cmd != nil && res.cmd.Process != nil {
		_ = res.cmd.Process.Kill()
		_ = res.cmd.Wait()
	}
	if res.pty != nil {
		res.pty.Close()
	}
	tmuxCleanupPipePane(res.tmuxName, res.rawPipe, res.rawPipePath)
	if res.tmuxName != "" {
		_ = tmuxKillSession(res.tmuxName)
	}
}

// platformBuildInternalToolArgs builds the arguments for an internal tool session.
func platformBuildInternalToolArgs(id, tool, workDir string, args []string) (runArgs []string, toolSessionID string) {
	if tool == "tmux" {
		toolSessionID = "kojo_" + id
		loginCmd := tmuxLoginShellCmd()
		runArgs = []string{"new-session", "-A", "-s", toolSessionID, "-c", workDir, loginCmd}
		return
	}
	return args, ""
}

// cleanupPipePane stops pipe-pane and cleans up the FIFO, clearing Session fields.
// Caller must hold s.mu.
func (s *Session) cleanupPipePane() {
	if s.rawPipePath == "" && s.rawPipe == nil {
		return
	}
	tmuxCleanupPipePane(s.TmuxSessionName, s.rawPipe, s.rawPipePath)
	s.rawPipe = nil
	s.rawPipePath = ""
}

// NeedsTmuxCheck returns whether the platform requires a tmux check at startup.
func NeedsTmuxCheck() bool { return true }

// platformPrepareRestart cleans up old session resources before restart.
func (m *Manager) platformPrepareRestart(s *Session) {
	s.mu.Lock()
	s.cleanupPipePane()
	tmuxName := s.TmuxSessionName
	s.mu.Unlock()
	if tmuxName != "" && tmuxHasSession(tmuxName) {
		_ = tmuxKillSession(tmuxName)
	}
}

// buildInternalToolRestartArgs builds restart arguments for internal tools (tmux).
func buildInternalToolRestartArgs(origArgs []string, toolSessionID string) []string {
	if toolSessionID != "" {
		return []string{"new-session", "-A", "-s", toolSessionID, tmuxLoginShellCmd()}
	}
	return origArgs
}

