//go:build windows

package session

import (
	"os"
	"os/exec"
)

// defaultShell returns the default shell on Windows.
// Priority: pwsh.exe (PowerShell 7+) > powershell.exe > ComSpec > cmd.exe.
func defaultShell() string {
	if ps, err := exec.LookPath("pwsh.exe"); err == nil {
		return ps
	}
	if ps, err := exec.LookPath("powershell.exe"); err == nil {
		return ps
	}
	if comspec := os.Getenv("ComSpec"); comspec != "" {
		return comspec
	}
	return "cmd.exe"
}
