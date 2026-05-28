//go:build !windows

package agent

import (
	"errors"
	"os"
	"syscall"
)

// errNonRegular is returned by openNoFollow when the path is a
// symlink, FIFO, socket, device, or directory. Callers map it to
// a debug-level "skip" log rather than a hard error.
var errNonRegular = errors.New("attach: not a regular file")

// openNoFollow opens path with O_NOFOLLOW so a symlink the agent
// dropped between our directory scan and this call cannot redirect
// us to an arbitrary host file. The returned *os.File and its
// fstat'd FileInfo are bound to the SAME open fd so the size /
// regular-file gate is applied to the bytes we will actually read.
// Closing the file is the caller's responsibility.
//
// On Linux a symlink trips O_NOFOLLOW with ELOOP; on macOS with
// ELOOP or EFTYPE depending on the kernel. We collapse both to
// errNonRegular.
func openNoFollow(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		// Errno mapping: ELOOP is the canonical NOFOLLOW signal.
		// EFTYPE shows up on some BSD-ish kernels. Treat both as
		// "non-regular target" so the caller logs+skips quietly.
		var pe *os.PathError
		if errors.As(err, &pe) {
			if errno, ok := pe.Err.(syscall.Errno); ok {
				if errno == syscall.ELOOP {
					return nil, nil, errNonRegular
				}
			}
		}
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		// O_NOFOLLOW only blocks the final-segment symlink; a
		// FIFO / socket / device opened successfully still fails
		// the regular-file gate here.
		f.Close()
		return nil, nil, errNonRegular
	}
	return f, info, nil
}
