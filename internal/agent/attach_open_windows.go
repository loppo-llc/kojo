//go:build windows

package agent

import (
	"errors"
	"os"
)

// errNonRegular is returned by openNoFollow when the path is a
// symlink, FIFO, socket, device, or directory.
var errNonRegular = errors.New("attach: not a regular file")

// openNoFollow on Windows: O_NOFOLLOW + reparse-point handling
// is not exposed by syscall in a portable way, so we approximate
// it with a pre-flight Lstat (rejects symlinks / reparse points
// by mode) and a post-open re-Stat (catches non-regular targets
// the open survived). The window between Lstat and Open is a
// TOCTOU race a local attacker could exploit by swapping a
// reparse point in place — kojo-attach runs inside the agent's
// own data directory where the agent already has write access,
// so the attack surface is "agent reads a file the agent could
// already read", which we accept. Operators concerned about a
// hardened multi-tenant Windows deployment should disable the
// attach skill until proper ReOpenFile + FILE_FLAG_OPEN_REPARSE_POINT
// support is wired through.
func openNoFollow(path string) (*os.File, os.FileInfo, error) {
	li, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, nil, errNonRegular
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, errNonRegular
	}
	return f, info, nil
}
