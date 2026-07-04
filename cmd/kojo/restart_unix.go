//go:build unix

package main

import (
	"log/slog"
	"os"
	"syscall"
)

// restartSupported gates the SetRestartTrigger wiring in main; when
// false the /api/v1/system/restart endpoint answers 501.
const restartSupported = true

// execRestart replaces the current process image with the binary at
// os.Executable() — same PID, same controlling terminal, same argv and
// environment. Called at the tail of main AFTER the ordered graceful
// shutdown, so listeners are closed (Go sets CLOEXEC on its fds; exec
// closes the rest) and the store is flushed. Because the path is
// re-resolved from disk, a `make build` that overwrote the binary
// makes this a full in-place deploy. Returns only on failure.
func execRestart(logger *slog.Logger) {
	exe, err := os.Executable()
	if err != nil {
		logger.Error("restart: cannot resolve executable path", "err", err)
		return
	}
	logger.Info("restart: re-exec", "exe", exe)
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		logger.Error("restart: exec failed", "exe", exe, "err", err)
	}
}
