package session

import (
	"sync"
)

// ContextInfo represents the estimated context usage for a session.
type ContextInfo struct {
	EstimatedTokens int64   `json:"estimatedTokens"`
	ContextWindow   int64   `json:"contextWindow"`
	UsagePercent    float64 `json:"usagePercent"`
	Source          string  `json:"source"` // "pty_bytes" | "transcript"
	CompactionCount int     `json:"compactionCount"`
}

// contextConfig holds per-tool context estimation parameters.
type contextConfig struct {
	contextWindow  int64
	bytesPerToken  float64
	flushThreshold float64
}

var contextConfigs = map[string]contextConfig{
	"claude": {contextWindow: 200_000, bytesPerToken: 3.5, flushThreshold: 0.80},
	"codex":  {contextWindow: 200_000, bytesPerToken: 3.5, flushThreshold: 0.80},
	"gemini": {contextWindow: 1_000_000, bytesPerToken: 4.0, flushThreshold: 0.80},
}

// broadcastPctDelta is the minimum usage% change to trigger a broadcast.
const broadcastPctDelta = 5.0

// ContextEstimator tracks context usage for a session.
type ContextEstimator struct {
	mu               sync.Mutex
	tool             string
	config           contextConfig
	totalBytes       int64
	transcriptTokens int64
	compactionCount  int
	lastBroadcastPct float64
	onThreshold      func()
	thresholdFired   bool
	transcript       *TranscriptMonitor
	stopped          bool
}

// NewContextEstimator creates a new estimator for the given tool.
func NewContextEstimator(tool string, onThreshold func()) *ContextEstimator {
	cfg, ok := contextConfigs[tool]
	if !ok {
		cfg = contextConfig{contextWindow: 200_000, bytesPerToken: 3.5, flushThreshold: 0.80}
	}
	return &ContextEstimator{
		tool:        tool,
		config:      cfg,
		onThreshold: onThreshold,
	}
}

// SetTranscript attaches a TranscriptMonitor for more accurate token tracking.
func (e *ContextEstimator) SetTranscript(t *TranscriptMonitor) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.transcript = t
}

// ReplaceTranscript stops the current TranscriptMonitor (if any) and sets a new one.
func (e *ContextEstimator) ReplaceTranscript(t *TranscriptMonitor) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.transcript != nil {
		e.transcript.Stop()
	}
	e.transcript = t
}

// Track adds PTY output bytes and returns a ContextInfo if a broadcast is warranted.
func (e *ContextEstimator) Track(data []byte) *ContextInfo {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.stopped {
		return nil
	}

	e.totalBytes += int64(len(data))
	return e.checkAndBroadcastLocked()
}

// UpdateTranscriptTokens sets the token count from transcript monitoring.
func (e *ContextEstimator) UpdateTranscriptTokens(tokens int64) *ContextInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return nil
	}
	e.transcriptTokens = tokens
	return e.checkAndBroadcastLocked()
}

// checkAndBroadcastLocked checks the threshold and returns a ContextInfo
// copy if a broadcast is warranted. Must be called with e.mu held.
func (e *ContextEstimator) checkAndBroadcastLocked() *ContextInfo {
	info := e.infoLocked()

	// Check threshold
	if !e.thresholdFired && info.UsagePercent >= e.config.flushThreshold*100 {
		e.thresholdFired = true
		if e.onThreshold != nil {
			go e.onThreshold()
		}
	}

	// Broadcast if usage changed by >= broadcastPctDelta
	delta := info.UsagePercent - e.lastBroadcastPct
	if delta < 0 {
		delta = -delta
	}
	if delta >= broadcastPctDelta {
		e.lastBroadcastPct = info.UsagePercent
		cp := *info
		return &cp
	}
	return nil
}

// Info returns the current context estimation snapshot.
func (e *ContextEstimator) Info() *ContextInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.infoLocked()
}

func (e *ContextEstimator) infoLocked() *ContextInfo {
	var estimatedTokens int64
	source := "pty_bytes"

	if e.transcriptTokens > 0 {
		estimatedTokens = e.transcriptTokens
		source = "transcript"
	} else {
		estimatedTokens = int64(float64(e.totalBytes) / e.config.bytesPerToken)
	}

	usagePct := float64(estimatedTokens) / float64(e.config.contextWindow) * 100
	if usagePct > 100 {
		usagePct = 100
	}

	return &ContextInfo{
		EstimatedTokens: estimatedTokens,
		ContextWindow:   e.config.contextWindow,
		UsagePercent:    usagePct,
		Source:          source,
		CompactionCount: e.compactionCount,
	}
}

// clearCountersLocked resets tracking counters. Must be called with e.mu held.
func (e *ContextEstimator) clearCountersLocked() {
	e.totalBytes = 0
	e.transcriptTokens = 0
	e.lastBroadcastPct = 0
	e.thresholdFired = false
}

// Reset clears the estimator after a compaction.
func (e *ContextEstimator) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.compactionCount++
	e.clearCountersLocked()
}

// Restart re-enables a stopped estimator (e.g. after session Restart).
func (e *ContextEstimator) Restart() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped = false
	e.clearCountersLocked()
}

// CompactionCount returns the current compaction count.
func (e *ContextEstimator) CompactionCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.compactionCount
}

// Stop disables the estimator.
func (e *ContextEstimator) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped = true
	if e.transcript != nil {
		e.transcript.Stop()
	}
}
