package blob

import (
	"errors"
	"path"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// ErrInvalidPath signals a path that violates the blob namespace rules
// (traversal, absolute, reserved name, reserved suffix). Callers that
// surface to HTTP map this to 400.
var ErrInvalidPath = errors.New("blob: invalid path")

// reservedNames is the OS-junk allowlist documented in §4.3 ("予約
// ファイル名"). Compared case-insensitively so `THUMBS.DB` / `thumbs.db`
// / `Thumbs.db` all reject — without that the native API would let
// callers stage payloads that the WebDAV layer auto-discards.
var reservedNames = map[string]bool{
	".ds_store":   true,
	"thumbs.db":   true,
	"desktop.ini": true,
}

// reservedPrefixes are prefix-matched OS-junk patterns. Compared
// case-insensitively for the same reason as reservedNames.
//
//   - AppleDouble companion files: `._*` (macOS network shares)
//   - Office lock files: `~$*`
//   - atomicWrite temp files: `.blob-*` — the rename window between
//     CreateTemp and the final rename leaves these visible to a
//     concurrent List, and a malicious caller could otherwise Put a
//     legit `.blob-foo` blob that would later collide with a temp.
var reservedPrefixes = []string{
	"._",
	"~$",
	".blob-",
}

// reservedSuffixes are suffixes the blob layer claims for its own
// metadata. Today the list is empty — slice 2 may add `.sha256` if we
// decide to keep a per-file digest sidecar — but the helper exists so
// path validation has a single hook to extend.
var reservedSuffixes []string

// windowsReservedBases is the canonical NT device-name table. Windows
// refuses to create files whose stem (the part before the first ".")
// matches any of these regardless of case or extension; storing such a
// path on a POSIX peer would later prevent that peer from syncing the
// blob to a Windows host. We reject up front to keep the cluster
// homogeneously addressable.
var windowsReservedBases = func() map[string]bool {
	out := map[string]bool{
		"con": true, "prn": true, "aux": true, "nul": true,
	}
	for i := 1; i <= 9; i++ {
		out["com"+string(rune('0'+i))] = true
		out["lpt"+string(rune('0'+i))] = true
	}
	return out
}()

// validatePath checks p as a blob-relative logical path. The returned
// string is the cleaned form callers should use to derive a filesystem
// path; the original is rejected if cleaning would alter it (so
// "a/./b", "a//b", "a/b/" never store under different names than what
// the caller passed).
//
// Rules (mirrors the doc):
//   - non-empty
//   - no leading "/" (absolute)
//   - no ".." components (traversal — would escape the scope dir)
//   - cleaned form must equal the input (no implicit normalization on
//     the caller's behalf; surface the mismatch as an error so log lines
//     show the caller's broken path)
//   - no Windows backslashes — keep one canonical separator
//   - no NUL byte (POSIX would silently truncate; SQLite blob_refs PK
//     is TEXT and would store the truncated form)
//   - leaf must not be a reserved name / prefix / suffix
//   - every intermediate dir must obey the reserved-name rule too,
//     because List() returns them and the WebDAV mount would expose them
func validatePath(p string) (string, error) {
	if p == "" {
		return "", ErrInvalidPath
	}
	// docs §4.3 mandates NFC. Rather than silently rewriting the
	// caller's input (two distinct-looking strings would round-trip
	// to the same blob, which surprises log readers), refuse non-NFC
	// up front. macOS Finder write paths go through the WebDAV
	// adapter (slice 4) which normalizes there; native API callers
	// already control the bytes they send.
	if !norm.NFC.IsNormalString(p) {
		return "", ErrInvalidPath
	}
	if strings.ContainsRune(p, 0) {
		return "", ErrInvalidPath
	}
	if strings.ContainsRune(p, '\\') {
		return "", ErrInvalidPath
	}
	if strings.HasPrefix(p, "/") {
		return "", ErrInvalidPath
	}
	cleaned := path.Clean(p)
	if cleaned != p {
		return "", ErrInvalidPath
	}
	if cleaned == "." || cleaned == ".." {
		return "", ErrInvalidPath
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if err := validateSegment(seg); err != nil {
			return "", err
		}
	}
	return cleaned, nil
}

// validateSegment checks one path component (no "/" inside) against
// every namespace rule that applies element-wise: empty / dot /
// dotdot, reserved file names, reserved prefixes / suffixes, and
// Windows reserved device names. All comparisons are case-insensitive
// so `Thumbs.db` and `THUMBS.DB` both reject.
func validateSegment(seg string) error {
	if seg == "" || seg == "." || seg == ".." {
		return ErrInvalidPath
	}
	// Win32 silently strips trailing dot/space and refuses
	// `<>:"|?*` outright; a path that lands fine on POSIX but breaks
	// when a Windows peer pulls the blob over webdav is a sync hazard
	// we can avoid by refusing on write.
	if strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
		return ErrInvalidPath
	}
	if strings.ContainsAny(seg, `<>:"|?*`) {
		return ErrInvalidPath
	}
	// Win32 also refuses every C0 control rune (0x01–0x1F) in file
	// names. NUL is already screened at the top-level validator but
	// the rest (BS, TAB, CR, LF, …) would land us with sync-incompat
	// names; reject up front.
	for _, r := range seg {
		if r < 0x20 {
			return ErrInvalidPath
		}
	}
	lower := strings.ToLower(seg)
	if reservedNames[lower] {
		return ErrInvalidPath
	}
	for _, pref := range reservedPrefixes {
		if strings.HasPrefix(lower, pref) {
			return ErrInvalidPath
		}
	}
	for _, suf := range reservedSuffixes {
		if strings.HasSuffix(lower, suf) {
			return ErrInvalidPath
		}
	}
	// Windows device-name check operates on the stem (text before the
	// first '.') so `con.txt` is rejected as forcefully as bare `con`.
	stem := lower
	if dot := strings.Index(lower, "."); dot >= 0 {
		stem = lower[:dot]
	}
	if windowsReservedBases[stem] {
		return ErrInvalidPath
	}
	return nil
}

// validatePrefix relaxes validatePath for List() — partial paths are
// allowed (a directory-like prefix such as `agents/ag_1/`), but the
// dangerous shapes (NUL, backslash, leading `/`, traversal) are still
// refused. Empty prefix is accepted as "match all". Each non-empty
// segment runs through the segment validator only for traversal
// rejection; reserved-name segments are NOT rejected here because the
// fs walk filters them anyway and a future scrub job may want to List
// them under a salvage tool.
func validatePrefix(p string) error {
	if p == "" {
		return nil
	}
	if strings.ContainsRune(p, 0) || strings.ContainsRune(p, '\\') {
		return ErrInvalidPath
	}
	if strings.HasPrefix(p, "/") {
		return ErrInvalidPath
	}
	// A trailing "/" is a legitimate "directory prefix" form; strip
	// before splitting so the empty tail segment doesn't false-trip
	// the empty-segment check.
	q := strings.TrimSuffix(p, "/")
	if q == "" {
		return ErrInvalidPath
	}
	for _, seg := range strings.Split(q, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrInvalidPath
		}
	}
	return nil
}
