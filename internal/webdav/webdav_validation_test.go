package webdav

import (
	"errors"
	"io/fs"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// TestValidateSegment locks in the per-segment policy that drives
// resolve() and the listing filter. Behavior is contract — changing
// it would let a path that previously round-tripped to an OS-junk
// file land on the WebDAV surface.
func TestValidateSegment(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want error
	}{
		// Traversal
		{name: "dot", in: ".", want: fs.ErrPermission},
		{name: "dotdot", in: "..", want: fs.ErrPermission},

		// Byte-level
		{name: "NUL", in: "x\x00y", want: fs.ErrInvalid},
		{name: "backslash anywhere in segment", in: "x\\y", want: fs.ErrInvalid},

		// Reserved leaf names (case-insensitive)
		{name: ".DS_Store", in: ".DS_Store", want: errReservedSkip},
		{name: "thumbs.db lowercase", in: "thumbs.db", want: errReservedSkip},
		{name: "DESKTOP.INI uppercase", in: "DESKTOP.INI", want: errReservedSkip},

		// Reserved prefixes (case-insensitive)
		{name: "AppleDouble", in: "._foo", want: errReservedSkip},
		{name: "Office lock", in: "~$companion", want: errReservedSkip},
		{name: "blob temp", in: ".blob-abc", want: errReservedSkip},
		{name: "atomicfile temp", in: ".tmp-abc", want: errReservedSkip},

		// Acceptable shapes
		{name: "plain file", in: "report.pdf", want: nil},
		{name: "dot-prefixed but not reserved", in: ".config", want: nil},
		{name: "empty (passes through; resolve handles top-level)", in: "", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSegment(tc.in)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestIsReservedFilename fences the public predicate WebDAV callers
// use to log auto-discards. Anything validateSegment returns as
// errReservedSkip must round-trip through IsReservedFilename.
func TestIsReservedFilename(t *testing.T) {
	if !IsReservedFilename(errReservedSkip) {
		t.Errorf("IsReservedFilename(errReservedSkip) = false, want true")
	}
	if IsReservedFilename(fs.ErrInvalid) {
		t.Errorf("IsReservedFilename(fs.ErrInvalid) = true, want false")
	}
	if IsReservedFilename(nil) {
		t.Errorf("IsReservedFilename(nil) = true, want false")
	}
	// Wrapping must still report true so callers can decorate the
	// error chain (e.g. with the failing path) without losing the
	// classification.
	wrapped := errors.Join(errReservedSkip, errors.New("ctx"))
	if !IsReservedFilename(wrapped) {
		t.Errorf("IsReservedFilename(wrapped) = false, want true")
	}
}

// TestIsReservedName fences the listing-filter predicate. Same
// reserved set as validateSegment but as a plain bool (no error
// shape) — the filter does not distinguish between "permission
// denied" and "auto-discard".
func TestIsReservedName(t *testing.T) {
	hits := []string{
		".ds_store", ".DS_Store",
		"thumbs.db", "Thumbs.DB",
		"desktop.ini",
		"._foo", "._",
		"~$lock", "~$",
		".tmp-abc",
		".blob-abc",
	}
	for _, n := range hits {
		t.Run("hide/"+n, func(t *testing.T) {
			if !isReservedName(n) {
				t.Errorf("isReservedName(%q) = false, want true", n)
			}
		})
	}
	misses := []string{"report.pdf", ".config", "foo", "", ".", ".."}
	for _, n := range misses {
		t.Run("keep/"+n, func(t *testing.T) {
			if isReservedName(n) {
				t.Errorf("isReservedName(%q) = true, want false", n)
			}
		})
	}
}

// TestShouldHideFromListing covers the listing filter's full
// composition: reserved names, non-NFC entries, and anything
// validateSegment would refuse outright (NUL / backslash / dot
// traversal). Reserved-name hits go through the errReservedSkip
// branch, NOT the catch-all.
func TestShouldHideFromListing(t *testing.T) {
	// Go source literals are NFC by convention, so spell the NFD form
	// out explicitly so the round-trip really exercises the non-NFC
	// branch.
	nfdCafe := norm.NFD.String("café")
	if nfdCafe == "café" {
		t.Fatalf("test fixture invariant violated: NFD form equals NFC form")
	}
	hide := []string{
		".DS_Store",     // reserved leaf
		"._companion",   // reserved prefix
		"weird\x00name", // NUL → validateSegment fs.ErrInvalid
		"back\\slash",   // backslash → fs.ErrInvalid
		"..",            // traversal → fs.ErrPermission
		nfdCafe,         // NFD, non-NFC
	}
	for _, n := range hide {
		t.Run("hide/"+n, func(t *testing.T) {
			if !shouldHideFromListing(n) {
				t.Errorf("shouldHideFromListing(%q) = false, want true", n)
			}
		})
	}
	keep := []string{
		"report.pdf",
		".config",
		"café", // NFC (single composed char)
	}
	for _, n := range keep {
		t.Run("keep/"+n, func(t *testing.T) {
			if shouldHideFromListing(n) {
				t.Errorf("shouldHideFromListing(%q) = true, want false", n)
			}
		})
	}
}
