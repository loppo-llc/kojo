package session

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	transcriptPollSlow = 10 * time.Second
	transcriptPollFast = 3 * time.Second
	transcriptFastPct  = 70.0
)

// TranscriptMonitor watches Claude Code's transcript JSONL files for token usage.
type TranscriptMonitor struct {
	mu             sync.Mutex
	logger         *slog.Logger
	workDir        string
	claudeSessionID string
	estimator      *ContextEstimator
	broadcaster    func(*ContextInfo)
	stopCh         chan struct{}
	stopped        bool
	lastOffset     int64
	lastPath       string
}

// NewTranscriptMonitor creates a monitor for the given Claude session.
func NewTranscriptMonitor(logger *slog.Logger, workDir, claudeSessionID string, estimator *ContextEstimator, broadcaster func(*ContextInfo)) *TranscriptMonitor {
	t := &TranscriptMonitor{
		logger:          logger,
		workDir:         workDir,
		claudeSessionID: claudeSessionID,
		estimator:       estimator,
		broadcaster:     broadcaster,
		stopCh:          make(chan struct{}),
	}
	go t.pollLoop()
	return t
}

// Stop halts the polling loop.
func (t *TranscriptMonitor) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.stopped {
		t.stopped = true
		close(t.stopCh)
	}
}

func (t *TranscriptMonitor) pollLoop() {
	interval := transcriptPollSlow
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			t.poll()

			// Adaptive polling: speed up when context is high
			info := t.estimator.Info()
			newInterval := transcriptPollSlow
			if info != nil && info.UsagePercent >= transcriptFastPct {
				newInterval = transcriptPollFast
			}
			if newInterval != interval {
				interval = newInterval
				ticker.Reset(interval)
			}
		}
	}
}

func (t *TranscriptMonitor) poll() {
	path := t.findTranscriptPath()
	if path == "" {
		return
	}

	t.mu.Lock()
	if path != t.lastPath {
		t.lastOffset = 0
		t.lastPath = path
	}
	offset := t.lastOffset
	t.mu.Unlock()

	tokens := t.scanUsage(path, offset)
	if tokens > 0 {
		info := t.estimator.UpdateTranscriptTokens(tokens)
		if info != nil && t.broadcaster != nil {
			t.broadcaster(info)
		}
	}
}

// findTranscriptPath locates the Claude Code transcript JSONL for this session.
// Claude Code stores transcripts at ~/.claude/projects/<projectDir>/<sessionId>/transcript.jsonl
func (t *TranscriptMonitor) findTranscriptPath() string {
	if t.claudeSessionID == "" {
		return ""
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	// Convert workDir to Claude's project path format:
	// /Users/foo/bar → -Users-foo-bar
	projectDir := strings.ReplaceAll(filepath.ToSlash(t.workDir), "/", "-")

	path := filepath.Join(home, ".claude", "projects", projectDir, t.claudeSessionID, "transcript.jsonl")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

// transcriptEntry is the minimal structure of a Claude transcript JSONL entry.
type transcriptEntry struct {
	Type  string `json:"type"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage,omitempty"`
	Message *struct {
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message,omitempty"`
}

// scanUsage reads the transcript file from the given offset and extracts the latest token usage.
func (t *TranscriptMonitor) scanUsage(path string, offset int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return 0
		}
	}

	var lastTokens int64
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry transcriptEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		// Extract token counts from usage fields
		if entry.Usage != nil {
			total := entry.Usage.InputTokens + entry.Usage.OutputTokens
			if total > lastTokens {
				lastTokens = total
			}
		}
		if entry.Message != nil && entry.Message.Usage != nil {
			total := entry.Message.Usage.InputTokens + entry.Message.Usage.OutputTokens
			if total > lastTokens {
				lastTokens = total
			}
		}
	}

	// Update offset
	newOffset, _ := f.Seek(0, 1) // current position
	t.mu.Lock()
	t.lastOffset = newOffset
	t.mu.Unlock()

	return lastTokens
}
