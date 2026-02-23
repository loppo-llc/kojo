package session

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"
)

type Status string

const (
	StatusRunning Status = "running"
	StatusExited  Status = "exited"
)

type Session struct {
	mu              sync.Mutex
	ID              string
	Tool            string
	WorkDir         string
	Args            []string
	PTY             *os.File
	Cmd             *exec.Cmd
	CreatedAt       time.Time
	Status          Status
	ExitCode        *int
	YoloMode        bool
	ToolSessionID string // tool-specific session ID for resume

	// ring buffer for scrollback (1MB)
	scrollback *RingBuffer

	// broadcast channels
	subscribers map[chan []byte]struct{}
	subMu       sync.Mutex

	// done signal
	done chan struct{}

	// codex: trailing buffer for session ID capture across chunk boundaries
	codexCaptureBuf []byte

	// yolo: trailing output buffer for pattern detection
	yoloTail []byte

	// yolo debug subscribers
	yoloDebugSubs map[chan string]struct{}
}

// YoloApproval is broadcast when yolo auto-approves a prompt.
type YoloApproval struct {
	Matched  string `json:"matched"`
	Response string `json:"response"`
}

// strip ANSI escapes for pattern matching (replace with space to preserve word boundaries)
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\].*?(?:\x07|\x1b\\)|\x1b[()][0-9A-B]`)
var multiSpaceRe = regexp.MustCompile(`[ \t]{2,}`)

// "Do you ...? ... 1. Yes" pattern (allow blank lines between question and options)
var yoloPattern = regexp.MustCompile(`(?i)Do you \S[^\n]*\?[\s\S]{0,200}?1\.\s*Yes`)

// Codex outputs "session id: <UUID>" on startup
var codexSessionIDRe = regexp.MustCompile(`(?i)session id: ([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

type SessionInfo struct {
	ID              string   `json:"id"`
	Tool            string   `json:"tool"`
	WorkDir         string   `json:"workDir"`
	Args            []string `json:"args,omitempty"`
	Status          Status   `json:"status"`
	ExitCode        *int     `json:"exitCode,omitempty"`
	YoloMode        bool     `json:"yoloMode"`
	CreatedAt       string   `json:"createdAt"`
	ToolSessionID string   `json:"toolSessionId,omitempty"`
}

func (s *Session) Info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionInfo{
		ID:              s.ID,
		Tool:            s.Tool,
		WorkDir:         s.WorkDir,
		Args:            s.Args,
		Status:          s.Status,
		ExitCode:        s.ExitCode,
		YoloMode:        s.YoloMode,
		CreatedAt:       s.CreatedAt.UTC().Format(time.RFC3339),
		ToolSessionID: s.ToolSessionID,
	}
}

func (s *Session) Subscribe() (chan []byte, []byte) {
	ch := make(chan []byte, 256)
	s.subMu.Lock()
	s.subscribers[ch] = struct{}{}
	scrollback := s.scrollback.Bytes()
	s.subMu.Unlock()
	return ch, scrollback
}

func (s *Session) Unsubscribe(ch chan []byte) {
	s.subMu.Lock()
	delete(s.subscribers, ch)
	s.subMu.Unlock()
	close(ch)
}

func (s *Session) broadcast(data []byte) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- data:
		default:
			// slow consumer, drop
		}
	}
}

func (s *Session) SubscribeYoloDebug() chan string {
	ch := make(chan string, 16)
	s.subMu.Lock()
	if s.yoloDebugSubs == nil {
		s.yoloDebugSubs = make(map[chan string]struct{})
	}
	s.yoloDebugSubs[ch] = struct{}{}
	s.subMu.Unlock()
	return ch
}

func (s *Session) UnsubscribeYoloDebug(ch chan string) {
	s.subMu.Lock()
	delete(s.yoloDebugSubs, ch)
	s.subMu.Unlock()
	close(ch)
}

func (s *Session) BroadcastYoloDebug(tail string) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.yoloDebugSubs {
		select {
		case ch <- tail:
		default:
		}
	}
}

func (s *Session) Write(data []byte) (int, error) {
	s.mu.Lock()
	pty := s.PTY
	s.mu.Unlock()
	if pty == nil {
		return 0, os.ErrClosed
	}
	return pty.Write(data)
}

func (s *Session) Done() <-chan struct{} {
	return s.done
}

// CaptureToolSessionID tries to parse a tool-specific session ID from PTY output.
// Only captures once (when ToolSessionID is still empty).
// Accumulates data across chunk boundaries to handle split reads.
func (s *Session) CaptureToolSessionID(data []byte) {
	s.mu.Lock()
	if s.ToolSessionID != "" || s.Tool != "codex" {
		s.mu.Unlock()
		return
	}
	// accumulate data, keep last 256 bytes
	s.codexCaptureBuf = append(s.codexCaptureBuf, data...)
	if len(s.codexCaptureBuf) > 256 {
		s.codexCaptureBuf = s.codexCaptureBuf[len(s.codexCaptureBuf)-256:]
	}
	buf := make([]byte, len(s.codexCaptureBuf))
	copy(buf, s.codexCaptureBuf)
	s.mu.Unlock()

	clean := ansiRe.ReplaceAll(buf, []byte(" "))
	if m := codexSessionIDRe.FindSubmatch(clean); m != nil {
		s.mu.Lock()
		if s.ToolSessionID == "" {
			s.ToolSessionID = string(m[1])
			s.codexCaptureBuf = nil // done, free buffer
		}
		s.mu.Unlock()
	}
}

func (s *Session) SetYoloMode(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.YoloMode = enabled
	s.yoloTail = nil
}

func (s *Session) IsYoloMode() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.YoloMode
}

// CheckYolo appends data to a trailing buffer and checks for approval patterns.
// Returns non-nil YoloApproval if a match is found. Caller should write the response to PTY.
func (s *Session) CheckYolo(data []byte) (*YoloApproval, string) {
	s.mu.Lock()
	if !s.YoloMode {
		s.mu.Unlock()
		return nil, ""
	}

	// append to tail, keep last 512 bytes
	s.yoloTail = append(s.yoloTail, data...)
	if len(s.yoloTail) > 512 {
		s.yoloTail = s.yoloTail[len(s.yoloTail)-512:]
	}
	tail := make([]byte, len(s.yoloTail))
	copy(tail, s.yoloTail)
	s.mu.Unlock()

	// strip ANSI for matching (replace with space to keep word boundaries)
	clean := ansiRe.ReplaceAll(tail, []byte(" "))
	clean = bytes.ReplaceAll(clean, []byte("\r\n"), []byte("\n"))
	clean = bytes.ReplaceAll(clean, []byte("\r"), []byte("\n"))
	clean = multiSpaceRe.ReplaceAll(clean, []byte(" "))
	cleanStr := string(clean)

	loc := yoloPattern.FindIndex(clean)
	if loc == nil {
		return nil, cleanStr
	}

	matched := string(clean[loc[0]:loc[1]])

	// clear tail so we don't match again
	s.mu.Lock()
	s.yoloTail = nil
	s.mu.Unlock()

	return &YoloApproval{
		Matched:  matched,
		Response: "",
	}, cleanStr
}
