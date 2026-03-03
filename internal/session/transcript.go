package session

import (
	"bufio"
	"bytes"
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
	transcriptPath string // computed once at construction
	estimator      *ContextEstimator
	broadcaster    func(*ContextInfo)
	stopCh         chan struct{}
	stopped        bool
	lastOffset     int64
	scanBuf        []byte // reusable scanner buffer
}

// NewTranscriptMonitor creates a monitor for the given Claude session.
func NewTranscriptMonitor(logger *slog.Logger, workDir, claudeSessionID string, estimator *ContextEstimator, broadcaster func(*ContextInfo)) *TranscriptMonitor {
	t := &TranscriptMonitor{
		logger:         logger,
		transcriptPath: buildTranscriptPath(workDir, claudeSessionID),
		estimator:      estimator,
		broadcaster:    broadcaster,
		stopCh:         make(chan struct{}),
		scanBuf:        make([]byte, 256*1024),
	}
	go t.pollLoop()
	return t
}

// buildTranscriptPath computes the transcript file path once.
// Claude Code stores transcripts at ~/.claude/projects/<projectDir>/<sessionId>.jsonl
func buildTranscriptPath(workDir, claudeSessionID string) string {
	if claudeSessionID == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	projectDir := strings.ReplaceAll(filepath.ToSlash(workDir), "/", "-")
	return filepath.Join(home, ".claude", "projects", projectDir, claudeSessionID+".jsonl")
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
	if t.transcriptPath == "" {
		return
	}

	t.mu.Lock()
	offset := t.lastOffset
	t.mu.Unlock()

	tokens := t.scanUsage(t.transcriptPath, offset)
	if tokens > 0 {
		info := t.estimator.UpdateTranscriptTokens(tokens)
		if info != nil && t.broadcaster != nil {
			t.broadcaster(info)
		}
	}
}

// transcriptUsage holds token counts including cache tokens.
type transcriptUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// Total returns the sum of all token fields.
func (u *transcriptUsage) Total() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// transcriptEntry is the minimal structure of a Claude transcript JSONL entry.
type transcriptEntry struct {
	Type    string           `json:"type"`
	Usage   *transcriptUsage `json:"usage,omitempty"`
	Message *struct {
		Usage *transcriptUsage `json:"usage,omitempty"`
	} `json:"message,omitempty"`
}

// usagePrefix is used to skip JSONL lines that don't contain usage data.
var usagePrefix = []byte(`"usage"`)

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
	if t.scanBuf != nil {
		scanner.Buffer(t.scanBuf, len(t.scanBuf))
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Skip lines that don't contain usage data at all
		if !bytes.Contains(line, usagePrefix) {
			continue
		}

		var entry transcriptEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		// Extract token counts from usage fields (including cache tokens)
		if entry.Usage != nil {
			if total := entry.Usage.Total(); total > lastTokens {
				lastTokens = total
			}
		}
		if entry.Message != nil && entry.Message.Usage != nil {
			if total := entry.Message.Usage.Total(); total > lastTokens {
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
