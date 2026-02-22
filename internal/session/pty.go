package session

import (
	"os"

	"github.com/creack/pty/v2"
)

func (s *Session) Resize(cols, rows uint16) error {
	s.mu.Lock()
	ptmx := s.PTY
	s.mu.Unlock()
	if ptmx == nil {
		return os.ErrClosed
	}
	return pty.Setsize(ptmx, &pty.Winsize{
		Cols: cols,
		Rows: rows,
	})
}
