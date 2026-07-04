//go:build windows

package main

import "log/slog"

// restartSupported gates the SetRestartTrigger wiring in main. Windows
// has no exec(2) and the running .exe cannot be overwritten by a
// rebuild anyway, so the /api/v1/system/restart endpoint answers 501
// and this function is never reached in practice.
const restartSupported = false

func execRestart(logger *slog.Logger) {
	logger.Error("restart: not supported on windows")
}
