//go:build !linux

package sandbox

import (
	"context"
	"os/exec"
)

func wrapCommand(ctx context.Context, name string, args []string, _ Config) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// ExecSandboxed is a no-op on non-Linux platforms.  It should never be
// reached because the "sandbox" subcommand is only generated on Linux.
func ExecSandboxed(args []string) {
	// Unreachable: fall through to normal main().
}

// Available reports whether Landlock sandboxing is supported.
func Available() bool { return false }
