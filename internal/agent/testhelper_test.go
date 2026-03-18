package agent

import (
	"log/slog"
	"os"
)

// testLogger returns a slog.Logger that only emits errors, suitable for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}
