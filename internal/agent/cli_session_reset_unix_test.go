//go:build !windows

package agent

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestContextMeasureRefusesFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		lastCodexContextTokens(path)
		lastAgyContextTokens(path)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("measuring a FIFO blocked")
	}
}
