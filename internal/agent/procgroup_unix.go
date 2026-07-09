//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// setProcGroup runs the command in its own process group so background
// children can be signalled as a group (see call sites for why).
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// termProcGroup sends SIGTERM to the process group for pid.
// Negative pid → signal the whole process group (parent + children).
func termProcGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGTERM)
}

// killProcGroup sends SIGKILL to the process group for pid.
// Negative pid → signal the whole process group (parent + children).
func killProcGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}
