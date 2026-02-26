package session

import (
	"log/slog"
	"os"

	"github.com/creack/pty/v2"
)

func (s *Session) Resize(cols, rows uint16) error {
	s.mu.Lock()
	ptmx := s.PTY
	tmuxName := s.TmuxSessionName
	prevCols := s.lastCols
	prevRows := s.lastRows
	s.mu.Unlock()

	if ptmx == nil {
		return os.ErrClosed
	}

	if err := pty.Setsize(ptmx, &pty.Winsize{
		Cols: cols,
		Rows: rows,
	}); err != nil {
		return err
	}

	// For tmux-backed sessions, also resize the tmux window.
	// Skip if dimensions haven't changed (debounce for mobile browsers
	// that fire frequent resize events from keyboard/rotation/address bar).
	if tmuxName != "" && (cols != prevCols || rows != prevRows) {
		if err := tmuxResizePane(tmuxName, cols, rows); err != nil {
			// Don't update dedup state so the resize is retried next time
			slog.Debug("tmux resize failed", "session", tmuxName, "err", err)
			return nil
		}
	}

	// Update dedup state only after all resize operations succeed
	s.mu.Lock()
	s.lastCols = cols
	s.lastRows = rows
	s.mu.Unlock()

	return nil
}
