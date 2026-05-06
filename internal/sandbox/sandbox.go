// Package sandbox provides filesystem sandboxing for agent processes.
//
// On Linux 5.13+ (with Landlock LSM enabled), agent processes are restricted
// to writing only within explicitly allowed directories.  On other platforms
// the sandbox is a no-op: WrapCommand returns the original command unchanged.
//
// Architecture:
//
//	[kojo server]
//	  → fork+exec: kojo sandbox --rw /path1 --rw /path2 -- <cmd> <args...>
//	    → [kojo sandbox process]
//	      1. Apply Landlock ruleset (restrict self)
//	      2. syscall.Exec() replaces process with <cmd>
//	        → [cmd] inherits Landlock restrictions
//
// The self-re-exec design is necessary because Go's exec.Command (fork+exec)
// does not allow injecting code between fork and exec, and Landlock's
// landlock_restrict_self applies to the calling process.
package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Config defines the sandbox restrictions for an agent process.
type Config struct {
	// RWPaths lists directories where the process may write.
	// On Linux with Landlock, writes outside these paths are denied.
	// Read and execute access is unrestricted (Phase 1).
	RWPaths []string

	// Enabled controls whether sandboxing is applied.
	// When false, WrapCommand returns the command unchanged.
	Enabled bool
}

// WrapCommand returns an *exec.Cmd that, on supported platforms, runs the
// target command inside a Landlock sandbox.  On unsupported platforms it
// returns exec.CommandContext unchanged.
//
// The returned Cmd has no Dir, Env, Stdin, Stdout, or Stderr set — the
// caller must configure those as usual.
func WrapCommand(ctx context.Context, name string, args []string, cfg Config) *exec.Cmd {
	return wrapCommand(ctx, name, args, cfg)
}

// parseSandboxArgs extracts --rw paths and the trailing command from the
// argument list.  The "--" separator is required between flags and the command.
func parseSandboxArgs(args []string) (rwPaths []string, cmdArgs []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			cmdArgs = args[i+1:]
			return rwPaths, cmdArgs, nil
		}
		if args[i] == "--rw" {
			i++
			if i >= len(args) {
				return nil, nil, fmt.Errorf("--rw requires a path argument")
			}
			rwPaths = append(rwPaths, args[i])
			continue
		}
		if strings.HasPrefix(args[i], "--rw=") {
			rwPaths = append(rwPaths, strings.TrimPrefix(args[i], "--rw="))
			continue
		}
		return nil, nil, fmt.Errorf("unknown flag: %s", args[i])
	}
	return nil, nil, fmt.Errorf("missing -- separator")
}
