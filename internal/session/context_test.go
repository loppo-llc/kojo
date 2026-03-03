package session

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestContextEstimator_ByteTracking(t *testing.T) {
	e := NewContextEstimator("claude", nil)

	// Track 700 bytes at 2.5 bytes/token = 280 tokens
	e.Track(make([]byte, 700))

	info := e.Info()
	if info.EstimatedTokens != 280 {
		t.Errorf("expected 280 tokens, got %d", info.EstimatedTokens)
	}
	if info.Source != "pty_bytes" {
		t.Errorf("expected source pty_bytes, got %s", info.Source)
	}
}

func TestContextEstimator_TranscriptOverride(t *testing.T) {
	e := NewContextEstimator("claude", nil)

	// Track some bytes
	e.Track(make([]byte, 1000))

	// Transcript tokens should override
	e.UpdateTranscriptTokens(50000)

	info := e.Info()
	if info.EstimatedTokens != 50000 {
		t.Errorf("expected 50000 tokens, got %d", info.EstimatedTokens)
	}
	if info.Source != "transcript" {
		t.Errorf("expected source transcript, got %s", info.Source)
	}
}

func TestContextEstimator_ThresholdCallback(t *testing.T) {
	var fired atomic.Int32
	done := make(chan struct{})

	e := NewContextEstimator("claude", func() {
		fired.Add(1)
		select {
		case done <- struct{}{}:
		default:
		}
	})

	// Claude: 200K window, 2.5 bytes/token, threshold 0.80 = 160K tokens = 400K bytes
	// Send enough data to cross threshold
	chunk := make([]byte, 100_000)
	for i := 0; i < 5; i++ {
		e.Track(chunk)
	}

	// Wait for goroutine
	<-done

	if fired.Load() != 1 {
		t.Errorf("expected threshold to fire once, got %d", fired.Load())
	}
}

func TestContextEstimator_ThresholdFiresOnce(t *testing.T) {
	var fired atomic.Int32
	done := make(chan struct{}, 1)

	e := NewContextEstimator("claude", func() {
		fired.Add(1)
		select {
		case done <- struct{}{}:
		default:
		}
	})

	chunk := make([]byte, 100_000)
	for i := 0; i < 10; i++ {
		e.Track(chunk)
	}

	<-done
	// Small sleep to ensure no additional fires
	time.Sleep(10 * time.Millisecond)

	if fired.Load() != 1 {
		t.Errorf("expected threshold to fire exactly once, got %d", fired.Load())
	}
}

func TestContextEstimator_BroadcastThreshold(t *testing.T) {
	e := NewContextEstimator("claude", nil)

	// First call should broadcast (from 0% to > 5%)
	// Claude: 200K window, 2.5 bytes/token
	// 25000 bytes = 10000 tokens = 5% of 200K
	info := e.Track(make([]byte, 25000))
	if info == nil {
		t.Error("expected broadcast on first significant change")
	}

	// Small change shouldn't broadcast
	info = e.Track(make([]byte, 100))
	if info != nil {
		t.Error("expected no broadcast on small change")
	}
}

func TestContextEstimator_Reset(t *testing.T) {
	e := NewContextEstimator("claude", nil)

	e.Track(make([]byte, 100_000))
	e.Reset()

	info := e.Info()
	if info.EstimatedTokens != 0 {
		t.Errorf("expected 0 tokens after reset, got %d", info.EstimatedTokens)
	}
	if info.CompactionCount != 1 {
		t.Errorf("expected compaction count 1, got %d", info.CompactionCount)
	}
}

func TestContextEstimator_UsagePercent(t *testing.T) {
	e := NewContextEstimator("claude", nil)

	// 200K window, 2.5 bytes/token
	// 50000 bytes = 20000 tokens = 10% of 200K
	e.Track(make([]byte, 50000))

	info := e.Info()
	if info.UsagePercent < 9.9 || info.UsagePercent > 10.1 {
		t.Errorf("expected ~10%% usage, got %.1f%%", info.UsagePercent)
	}
}

func TestContextEstimator_GeminiConfig(t *testing.T) {
	e := NewContextEstimator("gemini", nil)

	// Gemini: 1M window, 4.0 bytes/token
	// 400000 bytes = 100000 tokens = 10% of 1M
	e.Track(make([]byte, 400_000))

	info := e.Info()
	if info.ContextWindow != 1_000_000 {
		t.Errorf("expected 1M context window, got %d", info.ContextWindow)
	}
	if info.UsagePercent < 9.9 || info.UsagePercent > 10.1 {
		t.Errorf("expected ~10%% usage, got %.1f%%", info.UsagePercent)
	}
}
