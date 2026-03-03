package session

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

type Status string

const (
	StatusRunning Status = "running"
	StatusExited  Status = "exited"
)

// Lifecycle represents the session's lifecycle state.
type Lifecycle int32

const (
	LifecycleRunning    Lifecycle = iota
	LifecycleCompacting           // compaction in progress, blocks Stop/Restart/Remove
	LifecycleRestarting           // restart in progress
	LifecycleExited
)

// OutputMode controls how readLoop handles PTY output.
type OutputMode int32

const (
	OutputNormal      OutputMode = iota
	OutputCapturing              // accumulate output for summary extraction
	OutputSuppressing            // discard output (startup suppression after compaction)
)

// maxCaptureSize is the maximum bytes to accumulate in capture mode.
const maxCaptureSize = 1024 * 1024

type Session struct {
	mu              sync.Mutex
	ID              string
	Tool            string
	WorkDir         string
	Args            []string
	PTY             io.ReadWriteCloser
	Cmd             *exec.Cmd
	CreatedAt       time.Time
	Status          Status
	ExitCode        *int
	YoloMode        bool
	Internal        bool   // internal session (e.g. tmux), not user-facing
	AgentID         string // owning agent ID (empty for manual sessions)
	ToolSessionID string // tool-specific session ID for resume
	ParentID        string // parent session ID (e.g. tmux child of a CLI session)
	TmuxSessionName string // tmux session name (kojo_<id>) for tmux-backed sessions
	lifecycle atomic.Int32 // Lifecycle enum; replaces restarting bool

	// pipe-pane: raw pane output captured via FIFO (bypasses tmux screen-diff batching)
	rawPipe     *os.File // FIFO reader, nil if pipe-pane is not active
	rawPipePath string   // FIFO path on disk for cleanup

	// last resize dimensions for deduplication (mobile sends frequent resize events)
	lastCols uint16
	lastRows uint16

	// ring buffer for scrollback (1MB)
	scrollback *RingBuffer

	// broadcast channels
	subscribers map[chan []byte]struct{}
	subMu       sync.Mutex

	// done is the session-wide lifecycle signal.
	// Only closed when the session fully exits (not during compaction).
	// Recreated on Restart (exited→running transition; WS has already received exit).
	done     chan struct{}
	doneOnce sync.Once // protects close(done) from double-close panic

	// outputMode controls readLoop behavior (atomic for hot path)
	outputMode atomic.Int32

	// capture buffer for compaction summary extraction
	captureMu  sync.Mutex
	captureBuf []byte

	// compaction synchronization
	compactReady chan struct{} // closed by completeExit when lifecycle==compacting
	compactOnce  sync.Once

	// context estimation
	context     *ContextEstimator
	contextSubs map[chan *ContextInfo]struct{}

	// lifecycle broadcast (compacting/running)
	lifecycleSubs map[chan string]struct{}

	// last output timestamp for ready detection during suppressing mode
	lastOutputAt atomic.Int64

	// codex: trailing buffer for session ID capture across chunk boundaries
	codexCaptureBuf []byte

	// yolo: trailing output buffer for pattern detection
	yoloTail []byte

	// yolo debug subscribers
	yoloDebugSubs map[chan string]struct{}

	// attachment tracking
	attachTail  []byte
	attachments map[string]*Attachment
	attachSubs  map[chan []*Attachment]struct{}

	// last terminal output captured on exit (for persistence)
	lastOutput []byte

	// readDone is closed when readLoop exits
	readDone chan struct{}
}

// YoloApproval is broadcast when yolo auto-approves a prompt.
type YoloApproval struct {
	Matched  string `json:"matched"`
	Response string `json:"response"`
}

// yoloTailSize is the trailing output buffer size for yolo pattern detection.
const yoloTailSize = 4096

// AnsiRe strips ANSI escapes for pattern matching.
var AnsiRe = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\].*?(?:\x07|\x1b\\)|\x1b[()][0-9A-B]`)

// StripANSI removes all ANSI escape sequences from data.
func StripANSI(data []byte) []byte {
	return AnsiRe.ReplaceAll(data, nil)
}
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
	Internal        bool     `json:"internal,omitempty"`
	AgentID         string   `json:"agentId,omitempty"`
	CreatedAt       string   `json:"createdAt"`
	ToolSessionID string   `json:"toolSessionId,omitempty"`
	ParentID        string   `json:"parentId,omitempty"`
	TmuxSessionName string        `json:"tmuxSessionName,omitempty"`
	LastOutput      string        `json:"lastOutput,omitempty"`
	LastCols        uint16        `json:"lastCols,omitempty"`
	LastRows        uint16        `json:"lastRows,omitempty"`
	Attachments     []*Attachment `json:"attachments,omitempty"`
}

func (s *Session) Info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := SessionInfo{
		ID:              s.ID,
		Tool:            s.Tool,
		WorkDir:         s.WorkDir,
		Args:            s.Args,
		Status:          s.Status,
		ExitCode:        s.ExitCode,
		YoloMode:        s.YoloMode,
		Internal:        s.Internal,
		AgentID:         s.AgentID,
		CreatedAt:       s.CreatedAt.UTC().Format(time.RFC3339),
		ToolSessionID: s.ToolSessionID,
		ParentID:        s.ParentID,
		TmuxSessionName: s.TmuxSessionName,
	}
	if len(s.lastOutput) > 0 {
		info.LastOutput = base64.StdEncoding.EncodeToString(s.lastOutput)
	}
	info.LastCols = s.lastCols
	info.LastRows = s.lastRows
	return info
}

// InfoForSave returns session info including attachment metadata for persistence.
func (s *Session) InfoForSave() SessionInfo {
	info := s.Info()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.attachments) > 0 {
		atts := make([]*Attachment, 0, len(s.attachments))
		for _, att := range s.attachments {
			atts = append(atts, att)
		}
		info.Attachments = atts
	}
	return info
}

func (s *Session) Subscribe() (chan []byte, []byte) {
	ch := make(chan []byte, 1024)
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

func (s *Session) SubscribeAttachments() chan []*Attachment {
	ch := make(chan []*Attachment, 16)
	s.subMu.Lock()
	if s.attachSubs == nil {
		s.attachSubs = make(map[chan []*Attachment]struct{})
	}
	s.attachSubs[ch] = struct{}{}
	s.subMu.Unlock()
	return ch
}

func (s *Session) UnsubscribeAttachments(ch chan []*Attachment) {
	s.subMu.Lock()
	delete(s.attachSubs, ch)
	s.subMu.Unlock()
	close(ch)
}

func (s *Session) BroadcastAttachments(attachments []*Attachment) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.attachSubs {
		select {
		case ch <- attachments:
		default:
		}
	}
}

// ScrollbackBytes returns the current scrollback buffer contents.
func (s *Session) ScrollbackBytes() []byte {
	return s.scrollback.Bytes()
}

// InjectOutput writes synthetic data directly to scrollback and broadcast,
// bypassing the PTY. Used for compaction banners.
func (s *Session) InjectOutput(data []byte) {
	out := make([]byte, len(data))
	copy(out, data)
	s.scrollback.Write(out)
	s.broadcast(out)
}

// SubscribeContext returns a channel that receives context info updates.
func (s *Session) SubscribeContext() chan *ContextInfo {
	ch := make(chan *ContextInfo, 16)
	s.subMu.Lock()
	if s.contextSubs == nil {
		s.contextSubs = make(map[chan *ContextInfo]struct{})
	}
	s.contextSubs[ch] = struct{}{}
	s.subMu.Unlock()
	return ch
}

// UnsubscribeContext removes a context subscriber.
func (s *Session) UnsubscribeContext(ch chan *ContextInfo) {
	s.subMu.Lock()
	delete(s.contextSubs, ch)
	s.subMu.Unlock()
	close(ch)
}

// BroadcastContext sends context info to all subscribers.
func (s *Session) BroadcastContext(info *ContextInfo) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.contextSubs {
		select {
		case ch <- info:
		default:
		}
	}
}

// SubscribeLifecycle returns a channel that receives lifecycle state changes.
func (s *Session) SubscribeLifecycle() chan string {
	ch := make(chan string, 4)
	s.subMu.Lock()
	if s.lifecycleSubs == nil {
		s.lifecycleSubs = make(map[chan string]struct{})
	}
	s.lifecycleSubs[ch] = struct{}{}
	s.subMu.Unlock()
	return ch
}

// UnsubscribeLifecycle removes a lifecycle subscriber.
func (s *Session) UnsubscribeLifecycle(ch chan string) {
	s.subMu.Lock()
	delete(s.lifecycleSubs, ch)
	s.subMu.Unlock()
	close(ch)
}

// BroadcastLifecycle sends lifecycle state to all subscribers.
func (s *Session) BroadcastLifecycle(state string) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.lifecycleSubs {
		select {
		case ch <- state:
		default:
		}
	}
}

// ContextInfo returns the current context estimation, or nil if unavailable.
// context is set once at creation and never changed, so no mutex needed.
func (s *Session) ContextInfo() *ContextInfo {
	if s.context == nil {
		return nil
	}
	return s.context.Info()
}

// appendCapture adds data to the capture buffer (capped at maxCaptureSize).
func (s *Session) appendCapture(data []byte) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	remaining := maxCaptureSize - len(s.captureBuf)
	if remaining <= 0 {
		return
	}
	if len(data) > remaining {
		data = data[:remaining]
	}
	s.captureBuf = append(s.captureBuf, data...)
}

// noteOutputTime records the current time for silence-based ready detection.
func (s *Session) noteOutputTime() {
	s.lastOutputAt.Store(time.Now().UnixMilli())
}

// GetLifecycle returns the current lifecycle state.
func (s *Session) GetLifecycle() Lifecycle {
	return Lifecycle(s.lifecycle.Load())
}

func (s *Session) Write(data []byte) (int, error) {
	// Block user input during compaction to avoid corrupting flush/inject flow.
	if Lifecycle(s.lifecycle.Load()) == LifecycleCompacting {
		return len(data), nil // silently discard
	}
	return s.writePTY(data)
}

// writePTY writes to PTY, bypassing lifecycle checks. Used by compaction orchestrator.
func (s *Session) writePTY(data []byte) (int, error) {
	// Retry briefly when PTY is nil (e.g. during tmux reattach) to avoid
	// silently dropping user input during the short reconnection window.
	// Uses s.done to bail out early if the session exits during the wait.
	for i := 0; i < maxWriteRetries; i++ {
		s.mu.Lock()
		pty := s.PTY
		s.mu.Unlock()
		if pty != nil {
			return pty.Write(data)
		}
		if i < maxWriteRetries-1 {
			select {
			case <-time.After(writeRetryDelay):
			case <-s.done:
				return 0, os.ErrClosed
			}
		}
	}
	return 0, os.ErrClosed
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

	clean := AnsiRe.ReplaceAll(buf, []byte(" "))
	if m := codexSessionIDRe.FindSubmatch(clean); m != nil {
		s.mu.Lock()
		if s.ToolSessionID == "" {
			s.ToolSessionID = string(m[1])
			s.codexCaptureBuf = nil // done, free buffer
		}
		s.mu.Unlock()
	}
}

func (s *Session) SetAgentID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.AgentID = id
}

func (s *Session) GetAgentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.AgentID
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

	// append to tail, keep last yoloTailSize bytes
	s.yoloTail = append(s.yoloTail, data...)
	if len(s.yoloTail) > yoloTailSize {
		s.yoloTail = s.yoloTail[len(s.yoloTail)-yoloTailSize:]
	}
	tail := make([]byte, len(s.yoloTail))
	copy(tail, s.yoloTail)
	s.mu.Unlock()

	// strip ANSI for matching (replace with space to keep word boundaries)
	clean := AnsiRe.ReplaceAll(tail, []byte(" "))
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

// TrackContext feeds output data to the context estimator.
// context is set once at creation and never changed, so no mutex needed.
func (s *Session) TrackContext(data []byte) {
	if s.context == nil {
		return
	}
	info := s.context.Track(data)
	if info != nil {
		s.BroadcastContext(info)
	}
}
