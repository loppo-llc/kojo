//go:build !windows

package blob

import (
	"errors"
	"syscall"
)

// isUnsupported reports whether err indicates the operation is not
// implemented for this filesystem / OS — used by fsyncDir to swallow
// "directories cannot be fsync'd" on filesystems that refuse it.
func isUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}
