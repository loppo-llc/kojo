// Package selfupdate checks GitHub Releases for a newer kojo binary and
// installs it in place. This file owns version-string parsing only: the
// daemon stamps itself via
//
//	-ldflags "-X main.version=$(git describe --tags --always --dirty)"
//
// so runtime strings range from clean tags ("v0.116.3") through dirty
// describe output ("v0.116.2-4-g5ace25a-dirty") to bare hashes / "dev"
// for untagged builds. Auto-update must never treat an unparseable
// string as older than a release, or a developer build would rewrite
// itself with a tagged binary.
package selfupdate

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a semver-like triple plus the commits-ahead count that
// git-describe emits after an exact tag. Ahead is 0 for a clean tag
// match; Dirty records a "-dirty" working tree but is ignored by
// Compare so ordering stays stable across local dirtiness.
type Version struct {
	Major int
	Minor int
	Patch int
	Ahead int
	Dirty bool
}

// ParseVersion accepts optional "v" prefix, "X.Y.Z", optional "-N-gHEX"
// (git-describe ahead suffix), and optional trailing "-dirty". X/Y/Z/N
// are decimal digits only; the hash after 'g' must be lowercase hex.
// Empty strings, "dev", bare hashes, and any other shape return an error
// so callers can refuse to auto-update unstamped builds.
func ParseVersion(s string) (Version, error) {
	if s == "" {
		return Version{}, fmt.Errorf("empty version string")
	}
	orig := s
	// Only the lowercase "v" from git tags / git-describe; uppercase
	// "V" is not a stamp we produce and would paper over typos.
	if strings.HasPrefix(s, "v") {
		s = s[1:]
	}
	if s == "" {
		return Version{}, fmt.Errorf("invalid version %q", orig)
	}

	dirty := false
	if strings.HasSuffix(s, "-dirty") {
		dirty = true
		s = strings.TrimSuffix(s, "-dirty")
		if s == "" {
			return Version{}, fmt.Errorf("invalid version %q", orig)
		}
	}

	ahead := 0
	// git-describe appends "-N-gHEX" after the tag when HEAD is N
	// commits past it. We require the literal 'g' and lowercase hex so
	// accidental suffixes (or uppercase hashes from other tools) fail
	// closed rather than silently zeroing Ahead.
	if i := strings.Index(s, "-"); i >= 0 {
		core, suffix := s[:i], s[i+1:]
		nStr, hex, ok := strings.Cut(suffix, "-g")
		if !ok || nStr == "" || hex == "" {
			return Version{}, fmt.Errorf("invalid version %q", orig)
		}
		if !isAllDigits(nStr) || !isLowerHex(hex) {
			return Version{}, fmt.Errorf("invalid version %q", orig)
		}
		n, err := strconv.Atoi(nStr)
		if err != nil {
			return Version{}, fmt.Errorf("invalid version %q: %w", orig, err)
		}
		ahead = n
		s = core
	}

	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("invalid version %q", orig)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		if p == "" || !isAllDigits(p) {
			return Version{}, fmt.Errorf("invalid version %q", orig)
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, fmt.Errorf("invalid version %q: %w", orig, err)
		}
		nums[i] = n
	}

	return Version{
		Major: nums[0],
		Minor: nums[1],
		Patch: nums[2],
		Ahead: ahead,
		Dirty: dirty,
	}, nil
}

// Compare orders by Major, Minor, Patch, then Ahead. Dirty is ignored
// so a dirty tree and a clean tree at the same describe point compare
// equal; update decisions should not flip solely on local dirt.
func (v Version) Compare(other Version) int {
	if c := cmpInt(v.Major, other.Major); c != 0 {
		return c
	}
	if c := cmpInt(v.Minor, other.Minor); c != 0 {
		return c
	}
	if c := cmpInt(v.Patch, other.Patch); c != 0 {
		return c
	}
	return cmpInt(v.Ahead, other.Ahead)
}

// IsNewer reports whether latest is strictly newer than current.
// Either side failing ParseVersion yields false: unparseable inputs
// (dev builds, bare hashes) must never trigger an automatic update.
func IsNewer(latest, current string) bool {
	lv, err := ParseVersion(latest)
	if err != nil {
		return false
	}
	cv, err := ParseVersion(current)
	if err != nil {
		return false
	}
	return lv.Compare(cv) > 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// isAllDigits requires ASCII '0'-'9' only. unicode.IsDigit would also
// accept fullwidth and other numeral forms that git-describe never emits.
func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
