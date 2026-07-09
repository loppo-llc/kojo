//go:build windows

package agent

import "os/exec"

// setProcGroup is a no-op on Windows: process groups via Setpgid are
// unavailable. Callers fall back to cmd.Process.Signal / WaitDelay.
func setProcGroup(cmd *exec.Cmd) {}

// termProcGroup is a no-op on Windows (no process-group kill).
func termProcGroup(pid int) error { return nil }

// killProcGroup is a no-op on Windows (no process-group kill).
func killProcGroup(pid int) error { return nil }
