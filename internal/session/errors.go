package session

import "errors"

// Sentinel errors for the session package.
var (
	ErrSessionNotFound  = errors.New("session not found")
	ErrSessionRunning   = errors.New("session is still running")
	ErrSessionNotRunning = errors.New("session not running")
	ErrToolNotFound     = errors.New("tool not found")
	ErrUnsupportedTool  = errors.New("unsupported tool")
	ErrHasRunningChildren = errors.New("cannot remove session with running children")
	ErrNotTerminal      = errors.New("not a terminal session")
	ErrNoTmuxID         = errors.New("session has no tmux ID")
)
