//go:build windows

package blob

// isUnsupported on Windows always returns true: the OS does not provide
// fsync semantics on directory handles, so any error from a
// directory-fsync attempt is by definition "not supported here" rather
// than a real failure of an otherwise-completed rename.
func isUnsupported(error) bool { return true }
