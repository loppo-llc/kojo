package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const tmuxPrefix = "kojo_"

// tmuxEnsureServerConfig ensures the tmux server has terminal-overrides set
// to disable alternate screen (smcup/rmcup) for the outer terminal.
//
// Without this, tmux attach sends \e[?1049h which puts xterm.js into
// alternate screen mode. In that mode xterm.js has no scrollback and
// converts mouse wheel to up/down arrow keys — the shell then cycles
// through command history instead of scrolling.
//
// This is idempotent: it checks whether the override already exists before
// appending it. Safe to call before every attach — handles tmux server
// restarts that would lose the previous setting.
func tmuxEnsureServerConfig() {
	out, err := exec.Command("tmux", "show-options", "-s", "terminal-overrides").Output()
	if err != nil {
		return // tmux server not running; will be set when a session is created
	}
	if strings.Contains(string(out), "smcup@:rmcup@") {
		return // already set
	}
	_ = exec.Command("tmux", "set-option", "-s", "-a", "terminal-overrides", ",xterm-256color:smcup@:rmcup@").Run()
}

// tmuxSessionName returns the tmux session name for a kojo session ID.
func tmuxSessionName(id string) string {
	return tmuxPrefix + id
}

// shellQuote wraps a string in single quotes, escaping any embedded single quotes.
// e.g. "it's" → "'it'\''s'"
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildShellCommand constructs a shell-safe command string from a tool path and arguments.
func buildShellCommand(toolPath string, args []string) string {
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, shellQuote(toolPath))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// tmuxLoginShellCmd returns a shell command string that launches the user's
// login shell. Used as tmux shell-command to ensure PATH matches the standard
// macOS terminal. The shell path is properly quoted to handle spaces/metacharacters.
// loginShellPath returns the user's login shell path from $SHELL,
// falling back to /bin/zsh on macOS.
func loginShellPath() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	return shell
}

func tmuxLoginShellCmd() string {
	// Unset PATH so the login shell rebuilds it from scratch via
	// /etc/zprofile (path_helper) + user profile, matching Terminal.app.
	return "unset PATH; exec " + shellQuote(loginShellPath()) + " -l"
}

// tmuxSetLoginShell configures the named tmux session to use a login shell
// for new windows/panes.
func tmuxSetLoginShell(name string) {
	cmd := "unset PATH; exec " + shellQuote(loginShellPath()) + " -l"
	_ = exec.Command("tmux", "set-option", "-t", name, "default-command", cmd).Run()
}

// tmuxNewSession creates a detached tmux session with remain-on-exit enabled.
// If disablePrefix is true, it also disables prefix keys, status bar, and mouse
// to make tmux transparent for user-facing tools.
func tmuxNewSession(name, workDir, shellCmd string, disablePrefix bool) error {
	// Wrap in login shell so PATH, SSH agent, credential helpers etc.
	// match the user's standard terminal environment.
	// Unset PATH first so the login shell rebuilds it from scratch.
	shell := loginShellPath()
	wrappedCmd := "unset PATH; " + shellQuote(shell) + " -lc " + shellQuote(shellCmd)

	// Create a detached session running the shell command
	args := []string{
		"new-session", "-d",
		"-s", name,
		"-c", workDir,
		"-x", "120", "-y", "36",
		wrappedCmd,
	}
	if err := exec.Command("tmux", args...).Run(); err != nil {
		return fmt.Errorf("tmux new-session: %w", err)
	}

	// Set remain-on-exit so the pane stays after the process exits
	if err := exec.Command("tmux", "set-option", "-t", name, "remain-on-exit", "on").Run(); err != nil {
		return fmt.Errorf("tmux set remain-on-exit: %w", err)
	}

	// Set TERM for the session
	if err := exec.Command("tmux", "set-option", "-t", name, "default-terminal", "xterm-256color").Run(); err != nil {
		return fmt.Errorf("tmux set default-terminal: %w", err)
	}

	if disablePrefix {
		// Disable prefix keys so Ctrl+B passes through to the CLI tool
		_ = exec.Command("tmux", "set-option", "-t", name, "prefix", "None").Run()
		_ = exec.Command("tmux", "set-option", "-t", name, "prefix2", "None").Run()
		// Hide status bar to prevent it from leaking into the mobile UI
		_ = exec.Command("tmux", "set-option", "-t", name, "status", "off").Run()
		// Disable mouse mode to avoid interference with xterm.js
		_ = exec.Command("tmux", "set-option", "-t", name, "mouse", "off").Run()
	}

	// Ensure server-level config is applied (idempotent)
	tmuxEnsureServerConfig()

	return nil
}

// tmuxAttachCommand returns an exec.Cmd that attaches to the named tmux session.
func tmuxAttachCommand(name string) *exec.Cmd {
	return exec.Command("tmux", "attach-session", "-t", name)
}

// tmuxKillSession kills the named tmux session.
func tmuxKillSession(name string) error {
	return exec.Command("tmux", "kill-session", "-t", name).Run()
}

// tmuxHasSession returns true if the named tmux session exists.
func tmuxHasSession(name string) bool {
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

// tmuxPaneDead checks whether the pane in the named tmux session is dead.
// Returns dead=true and the exit code if the process has exited.
func tmuxPaneDead(name string) (dead bool, exitCode int, err error) {
	out, err := exec.Command("tmux", "display-message", "-t", name, "-p", "#{pane_dead}:#{pane_dead_status}").Output()
	if err != nil {
		return false, 0, fmt.Errorf("tmux display-message: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), ":", 2)
	if len(parts) != 2 {
		return false, 0, fmt.Errorf("unexpected tmux output: %s", out)
	}
	if parts[0] != "1" {
		return false, 0, nil
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return true, 1, nil // dead but can't parse exit code
	}
	return true, code, nil
}

// tmuxEnableMouse enables mouse mode on the named tmux session so it receives
// mouse-wheel escape sequences from the web UI for per-pane scrolling.
func tmuxEnableMouse(name string) {
	_ = exec.Command("tmux", "set-option", "-t", name, "mouse", "on").Run()
}

// tmuxActions is the whitelist of tmux actions that can be executed server-side.
// Each entry maps an action name to a function that returns tmux CLI arguments.
var tmuxActions = map[string]func(string) []string{
	"kill-pane":     func(s string) []string { return []string{"kill-pane", "-t", s} },
	"new-window":    func(s string) []string { return []string{"new-window", "-t", s} },
	"prev-window":   func(s string) []string { return []string{"previous-window", "-t", s} },
	"next-window":   func(s string) []string { return []string{"next-window", "-t", s} },
	"split-h":       func(s string) []string { return []string{"split-window", "-v", "-t", s} },
	"split-v":       func(s string) []string { return []string{"split-window", "-h", "-t", s} },
	"select-pane":   func(s string) []string { return []string{"select-pane", "-t", s + ":.+"} },
	"resize-pane-z": func(s string) []string { return []string{"resize-pane", "-t", s, "-Z"} },
	"choose-tree":   func(s string) []string { return []string{"choose-tree", "-t", s} },
	"copy-mode":     func(s string) []string { return []string{"copy-mode", "-t", s} },
}

// tmuxRunAction executes a whitelisted tmux action targeting the named session.
func tmuxRunAction(sessionName, action string) error {
	fn, ok := tmuxActions[action]
	if !ok {
		return fmt.Errorf("unknown tmux action: %s", action)
	}
	out, err := exec.Command("tmux", fn(sessionName)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %s: %w (%s)", action, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// tmuxResizePane resizes the window of the named tmux session.
func tmuxResizePane(name string, cols, rows uint16) error {
	return exec.Command("tmux", "resize-window", "-t", name, "-x", strconv.Itoa(int(cols)), "-y", strconv.Itoa(int(rows))).Run()
}

// tmuxStartPipePane sets up pipe-pane to capture raw pane output via a named FIFO.
// Returns the opened FIFO reader and its path. The caller must eventually call
// tmuxCleanupPipePane to release resources.
//
// pipe-pane captures the raw bytes written by the CLI tool to its PTY, before
// tmux's terminal emulator processes them. This avoids the content loss that
// occurs when tmux batches screen-diff updates to attached clients during fast
// output (intermediate scrolled lines are never sent to the attach PTY).
func tmuxStartPipePane(sessionName string) (*os.File, string, error) {
	fifoDir := filepath.Join(os.TempDir(), "kojo")
	if err := os.MkdirAll(fifoDir, 0700); err != nil {
		return nil, "", fmt.Errorf("mkdir: %w", err)
	}

	fifoPath := filepath.Join(fifoDir, sessionName+".pipe")

	// Remove stale FIFO from a previous run
	os.Remove(fifoPath)

	if err := syscall.Mkfifo(fifoPath, 0600); err != nil {
		return nil, "", fmt.Errorf("mkfifo: %w", err)
	}

	// Open FIFO with O_RDWR so the fd acts as both reader and writer.
	// This prevents read() from returning EOF when the pipe-pane writer
	// (cat) hasn't opened the FIFO yet (startup race), or when it exits
	// momentarily during reattach. O_NONBLOCK ensures open() doesn't block.
	fd, err := syscall.Open(fifoPath, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		os.Remove(fifoPath)
		return nil, "", fmt.Errorf("open fifo: %w", err)
	}
	// Clear O_NONBLOCK so reads block normally until data/EOF
	if err := syscall.SetNonblock(fd, false); err != nil {
		syscall.Close(fd)
		os.Remove(fifoPath)
		return nil, "", fmt.Errorf("set blocking: %w", err)
	}
	f := os.NewFile(uintptr(fd), fifoPath)

	// Now start pipe-pane. The writer (cat) can open the FIFO immediately
	// because our reader fd is already registered.
	// -o = output only (data written by the program in the pane).
	// exec cat avoids leaving an extra sh process.
	if err := exec.Command("tmux", "pipe-pane", "-t", sessionName, "-o",
		fmt.Sprintf("exec cat > %s", shellQuote(fifoPath))).Run(); err != nil {
		f.Close()
		os.Remove(fifoPath)
		return nil, "", fmt.Errorf("pipe-pane: %w", err)
	}

	return f, fifoPath, nil
}

// tmuxCleanupPipePane stops pipe-pane and removes the FIFO.
func tmuxCleanupPipePane(sessionName string, f *os.File, fifoPath string) {
	if tmuxHasSession(sessionName) {
		// Calling pipe-pane without a command stops the active pipe
		_ = exec.Command("tmux", "pipe-pane", "-t", sessionName).Run()
	}
	if f != nil {
		f.Close()
	}
	if fifoPath != "" {
		os.Remove(fifoPath)
	}
}

// tmuxCapturePaneContent captures the current visible pane content (with ANSI escapes)
// using tmux capture-pane. Returns nil on failure.
func tmuxCapturePaneContent(name string) []byte {
	out, err := exec.Command("tmux", "capture-pane", "-t", name, "-p", "-e").Output()
	if err != nil {
		return nil
	}
	return out
}

// tmuxListKojoSessions returns names of all tmux sessions with the kojo_ prefix.
func tmuxListKojoSessions() ([]string, error) {
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		// tmux returns error if no server is running (no sessions)
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var sessions []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, tmuxPrefix) {
			sessions = append(sessions, line)
		}
	}
	return sessions, nil
}
