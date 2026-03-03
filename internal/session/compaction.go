package session

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	flushTimeout     = 60 * time.Second
	cleanupTimeout   = 30 * time.Second
	readyMaxTimeout  = 15 * time.Second
	readySilenceDur  = 3 * time.Second
	injectPasteDelay = 100 * time.Millisecond
)

// CLI-specific flush commands
var flushCommands = map[string]string{
	"claude": "Before this session ends, save ALL important context to memory files. Then output a brief summary of current task state wrapped in <kojo-summary> tags.\n",
}

var summaryTagRe = regexp.MustCompile(`(?s)<kojo-summary>(.*?)</kojo-summary>`)

// CompactionOrchestrator manages the compaction lifecycle for a session.
type CompactionOrchestrator struct {
	manager *Manager
	session *Session
	logger  *slog.Logger
}

// Run executes the full compaction flow.
// On any failure, the session is restored to LifecycleRunning.
func (o *CompactionOrchestrator) Run() {
	s := o.session

	// Only one compaction at a time.
	// Hold s.mu across CAS + init so completeExit never sees Compacting
	// with a nil/stale compactReady.
	s.mu.Lock()
	if !s.lifecycle.CompareAndSwap(int32(LifecycleRunning), int32(LifecycleCompacting)) {
		s.mu.Unlock()
		return
	}
	s.compactReady = make(chan struct{})
	s.compactOnce = sync.Once{}
	s.mu.Unlock()

	restartFailed := false

	defer func() {
		s.outputMode.Store(int32(OutputNormal))
		// If still compacting, the flow failed. Restore lifecycle.
		if !s.lifecycle.CompareAndSwap(int32(LifecycleCompacting), int32(LifecycleRunning)) {
			return // already transitioned (success or other)
		}
		if restartFailed {
			// freshRestart killed old process but failed to start new one.
			// Don't broadcast "running" — session is about to exit.
			o.logger.Error("compaction failed, finalizing session", "id", s.ID)
			o.manager.completeExit(s, 1)
			return
		}
		s.BroadcastLifecycle("running")
	}()

	o.logger.Info("compaction started", "id", s.ID)
	s.BroadcastLifecycle("compacting")

	// 1. Banner
	s.InjectOutput([]byte("\r\n\x1b[90m── Compacting context ──\x1b[0m\r\n"))

	// 2. Flush and capture summary
	summary := o.flushAndCaptureSummary()

	// 3. Save summary to disk
	o.saveSummary(summary)

	// 4. Fresh restart
	if err := o.freshRestart(); err != nil {
		o.logger.Error("compaction restart failed", "id", s.ID, "err", err)
		restartFailed = true
		return
	}

	// 5. Wait for CLI ready
	o.waitForReady()

	// 6. Inject summary (paste is suppressed, then switch to normal)
	o.injectSummary(summary)

	// 7. Finalize — clear terminal and show ready banner via same output channel
	// to guarantee ordering (lifecycle channel is separate, so clear must not
	// depend on it).
	s.InjectOutput([]byte("\x1b[2J\x1b[3J\x1b[H")) // clear screen + scrollback + cursor home
	s.InjectOutput([]byte("\x1b[90m── Ready ──\x1b[0m\r\n"))
	s.lifecycle.Store(int32(LifecycleRunning))
	s.BroadcastLifecycle("running")

	// If the new process died during compaction finalization (steps 5-7),
	// its waitLoop called completeExit which saw Compacting and only signaled
	// compactReady. Now that lifecycle is Running, trigger proper exit.
	if dead, exitCode := o.checkNewProcessDead(); dead {
		o.logger.Warn("new process exited during compaction finalization", "id", s.ID)
		o.manager.completeExit(s, exitCode)
		return
	}

	// Reset context estimator and recreate transcript monitor with new session ID
	if s.context != nil {
		s.context.Reset()
		// Recreate transcript monitor for new Claude session ID
		s.mu.Lock()
		newSessionID := s.ToolSessionID
		workDir := s.WorkDir
		s.mu.Unlock()
		if s.Tool == "claude" && newSessionID != "" {
			tm := NewTranscriptMonitor(o.logger, workDir, newSessionID, s.context, func(info *ContextInfo) {
				s.BroadcastContext(info)
			})
			s.context.ReplaceTranscript(tm)
		}
	}
	s.scrollback.ResetTotalWritten()

	// Broadcast reset context
	if info := s.ContextInfo(); info != nil {
		s.BroadcastContext(info)
	}

	// Free capture buffer
	s.captureMu.Lock()
	s.captureBuf = nil
	s.captureMu.Unlock()

	o.logger.Info("compaction completed", "id", s.ID)
}

func (o *CompactionOrchestrator) flushAndCaptureSummary() string {
	s := o.session

	flushCmd, ok := flushCommands[s.Tool]
	if !ok || flushCmd == "" {
		return ""
	}

	// Prepare capture
	s.captureMu.Lock()
	s.captureBuf = nil
	s.captureMu.Unlock()

	// Switch to capturing mode
	s.outputMode.Store(int32(OutputCapturing))

	// Send flush command via bracketed paste
	SafePaste(s, flushCmd)

	// Wait for summary tag or timeout (poll-based)
	timer := time.NewTimer(flushTimeout)
	defer timer.Stop()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if summary := o.extractSummary(); summary != "" {
				return summary
			}
		case <-timer.C:
			o.logger.Warn("flush timeout, using captured output as summary", "id", s.ID)
			if summary := o.extractSummary(); summary != "" {
				return summary
			}
			// Fallback: use whatever we captured
			s.captureMu.Lock()
			buf := string(s.captureBuf)
			s.captureMu.Unlock()
			clean := AnsiRe.ReplaceAllString(buf, "")
			if len(clean) > 2000 {
				clean = clean[len(clean)-2000:]
			}
			return clean
		}
	}
}

// summaryCloseTag is checked before doing expensive ANSI stripping.
var summaryCloseTag = []byte("</kojo-summary>")

func (o *CompactionOrchestrator) extractSummary() string {
	o.session.captureMu.Lock()
	// Quick check: closing tag must be present before copying + regex
	if !bytes.Contains(o.session.captureBuf, summaryCloseTag) {
		o.session.captureMu.Unlock()
		return ""
	}
	buf := make([]byte, len(o.session.captureBuf))
	copy(buf, o.session.captureBuf)
	o.session.captureMu.Unlock()

	// Strip ANSI for tag matching
	clean := AnsiRe.ReplaceAll(buf, nil)
	if m := summaryTagRe.FindSubmatch(clean); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

func (o *CompactionOrchestrator) saveSummary(summary string) {
	if summary == "" {
		return
	}

	s := o.session
	count := 0
	if s.context != nil {
		count = s.context.CompactionCount()
	}

	dir := filepath.Join(ConfigDirPath(), "compactions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		o.logger.Error("failed to create compaction dir", "err", err)
		return
	}

	filename := fmt.Sprintf("%s_%d.md", s.ID, count)
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(summary), 0o600); err != nil {
		o.logger.Error("failed to save compaction summary", "err", err)
	}
}

func (o *CompactionOrchestrator) freshRestart() error {
	s := o.session
	m := o.manager

	// Reset compactOnce for this restart (compactReady already initialized in Run()).
	// Hold s.mu to synchronize with completeExit which reads compactOnce under s.mu.
	s.mu.Lock()
	s.compactOnce = sync.Once{}
	s.mu.Unlock()

	// Kill the current process
	if err := m.platformStop(s, s.ID); err != nil {
		o.logger.Error("platform stop failed during compaction", "id", s.ID, "err", err)
	}

	// Wait for process cleanup
	select {
	case <-s.compactReady:
		// completeExit signaled
	case <-time.After(cleanupTimeout):
		o.logger.Error("compaction cleanup timeout", "id", s.ID)
		return fmt.Errorf("cleanup timeout")
	}

	// Wait for readLoop to finish
	select {
	case <-s.readDone:
	case <-time.After(3 * time.Second):
		o.logger.Warn("readLoop did not exit during compaction", "id", s.ID)
	}

	// Build fresh args (no resume)
	s.mu.Lock()
	args := s.Args
	tool := s.Tool
	workDir := s.WorkDir
	s.mu.Unlock()

	freshArgs, newSessionID := buildFreshArgs(tool, args)

	// Resolve tool path
	toolPath, err := exec.LookPath(tool)
	if err != nil {
		return fmt.Errorf("tool not found: %s", tool)
	}

	// Platform cleanup
	m.platformPrepareRestart(s)

	// Start new PTY
	s.mu.Lock()
	cols, rows := s.lastCols, s.lastRows
	s.mu.Unlock()

	res, err := m.platformStartUserTool(s.ID, workDir, toolPath, freshArgs, cols, rows)
	if err != nil {
		return fmt.Errorf("failed to start fresh process: %w", err)
	}

	// Swap session fields — done is NOT replaced
	s.mu.Lock()
	s.PTY = res.pty
	s.Cmd = res.cmd
	s.TmuxSessionName = res.tmuxName
	s.rawPipe = res.rawPipe
	s.rawPipePath = res.rawPipePath
	s.Status = StatusRunning
	s.ExitCode = nil
	s.lastOutput = nil
	if newSessionID != "" {
		s.ToolSessionID = newSessionID
	}
	s.readDone = make(chan struct{})
	s.mu.Unlock()

	// Switch to suppressing mode before starting loops
	s.outputMode.Store(int32(OutputSuppressing))
	s.lastOutputAt.Store(time.Now().UnixMilli())

	// Start background goroutines
	m.platformStartLoops(s)

	return nil
}

func (o *CompactionOrchestrator) waitForReady() {
	WaitForReady(o.session, o.logger)
}

// WaitForReady blocks until the session's CLI appears ready (silence-based detection).
func WaitForReady(s *Session, logger *slog.Logger) {
	deadline := time.After(readyMaxTimeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			if logger != nil {
				logger.Warn("ready detection timeout", "id", s.ID)
			}
			return
		case <-ticker.C:
			lastMs := s.lastOutputAt.Load()
			if lastMs > 0 {
				elapsed := time.Since(time.UnixMilli(lastMs))
				if elapsed >= readySilenceDur {
					return
				}
			}
		}
	}
}

func (o *CompactionOrchestrator) injectSummary(summary string) {
	s := o.session

	if summary == "" {
		s.scrollback.Reset()
		s.outputMode.Store(int32(OutputNormal))
		return
	}

	// Keep OutputSuppressing during paste so the injected text is hidden.
	// Build the injection prompt
	prompt := fmt.Sprintf("Here is a summary of our previous conversation before context compaction. Use this to maintain continuity:\n\n%s\n\nPlease acknowledge this context and continue from where we left off.", summary)

	// Reset timestamp so WaitForReady doesn't return immediately from stale values
	// left over by the previous waitForReady (step 5).
	// Use current time (not 0) so WaitForReady enters silence detection immediately
	// rather than waiting for the first output.
	s.lastOutputAt.Store(time.Now().UnixMilli())

	// Send via bracketed paste (outputMode is still Suppressing — echo is discarded)
	SafePaste(s, prompt)

	// Wait for paste echo to be fully consumed by readLoop
	WaitForReady(s, o.logger)

	// Clear scrollback (old session output + captured flush output)
	// so reconnecting clients start clean.
	s.scrollback.Reset()

	// Switch to normal — CLI response from here is visible
	s.outputMode.Store(int32(OutputNormal))
}

// sanitizeForPaste removes ESC sequences and control characters from text.
func sanitizeForPaste(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\x1b' {
			continue
		}
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// SafePaste sends text to a session using bracketed paste mode.
// Uses writePTY to bypass lifecycle-based input blocking during compaction.
func SafePaste(s *Session, text string) {
	sanitized := sanitizeForPaste(text)
	paste := fmt.Sprintf("\x1b[200~%s\x1b[201~", sanitized)
	if _, err := s.writePTY([]byte(paste)); err != nil {
		return
	}
	time.Sleep(injectPasteDelay)
	s.writePTY([]byte("\r"))
}

// checkNewProcessDead checks whether the freshly started process has exited.
// For tmux sessions, checks pane status directly (readDone can close on FIFO
// loss which is recoverable). For non-tmux, checks readDone.
// Returns (dead, exitCode).
func (o *CompactionOrchestrator) checkNewProcessDead() (bool, int) {
	s := o.session
	s.mu.Lock()
	tmuxName := s.TmuxSessionName
	s.mu.Unlock()

	if tmuxName != "" {
		return checkTmuxPaneDead(tmuxName)
	}
	select {
	case <-s.readDone:
		return true, 1
	default:
		return false, 0
	}
}

// buildFreshArgs removes resume/continue arguments and generates a new session ID.
// Returns the new args and the new session ID (if applicable).
func buildFreshArgs(tool string, origArgs []string) ([]string, string) {
	switch tool {
	case "claude":
		args := make([]string, 0, len(origArgs)+2)
		skipNext := false
		for _, a := range origArgs {
			if skipNext {
				skipNext = false
				continue
			}
			// Remove --resume / -r with value
			if a == "--resume" || a == "-r" {
				skipNext = true
				continue
			}
			if strings.HasPrefix(a, "--resume=") {
				continue
			}
			// Remove --continue / -c
			if a == "--continue" || a == "-c" {
				continue
			}
			// Remove old --session-id (will be replaced)
			if a == "--session-id" {
				skipNext = true
				continue
			}
			if strings.HasPrefix(a, "--session-id=") {
				continue
			}
			args = append(args, a)
		}
		newID := uuid.New().String()
		args = append(args, "--session-id", newID)
		return args, newID

	case "codex":
		// Codex fresh start: remove "resume" subcommand and its argument.
		// e.g. ["resume", "old-id"] → [] or ["resume", "--last"] → []
		args := make([]string, 0, len(origArgs))
		skipNext := false
		for _, a := range origArgs {
			if skipNext {
				skipNext = false
				continue
			}
			if a == "resume" {
				skipNext = true // skip the next arg (session ID or --last)
				continue
			}
			args = append(args, a)
		}
		return args, ""

	case "gemini":
		args := make([]string, 0, len(origArgs))
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
			if strings.HasPrefix(a, "--resume=") {
				continue
			}
			args = append(args, a)
		}
		return args, ""

	default:
		out := make([]string, len(origArgs))
		copy(out, origArgs)
		return out, ""
	}
}
