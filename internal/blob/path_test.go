package blob

import (
	"errors"
	"testing"
)

// TestValidatePath fences the doc §4.3 path rules at the unit level.
// store_test.go covers the happy path through Put; these are the
// boundary cases — refused for security or cross-OS portability —
// pinned so a refactor cannot silently lift a defense.
func TestValidatePath(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantClean string  // empty when wantErr is non-nil
		wantErr   error   // nil means "must succeed and return wantClean"
	}{
		// Happy paths
		{name: "single segment", in: "a", wantClean: "a"},
		{name: "nested", in: "agents/ag_1/avatar.png", wantClean: "agents/ag_1/avatar.png"},
		{name: "dot-prefixed segment (not reserved)", in: "agents/.config", wantClean: "agents/.config"},

		// Empty / structural
		{name: "empty", in: "", wantErr: ErrInvalidPath},
		{name: "leading slash (absolute)", in: "/agents/a", wantErr: ErrInvalidPath},
		{name: "trailing slash (path.Clean would mutate)", in: "agents/", wantErr: ErrInvalidPath},
		{name: "double slash", in: "agents//a", wantErr: ErrInvalidPath},
		{name: "dot segment", in: "./a", wantErr: ErrInvalidPath},
		{name: "lone dot", in: ".", wantErr: ErrInvalidPath},
		{name: "lone dotdot", in: "..", wantErr: ErrInvalidPath},
		{name: "traversal", in: "agents/../etc/passwd", wantErr: ErrInvalidPath},
		{name: "embedded traversal", in: "a/../b", wantErr: ErrInvalidPath},

		// Byte-level
		{name: "NUL byte", in: "a\x00b", wantErr: ErrInvalidPath},
		{name: "windows backslash", in: "a\\b", wantErr: ErrInvalidPath},

		// macOS / Windows OS-junk
		{name: "reserved leaf .DS_Store (case-insensitive)", in: "x/.DS_Store", wantErr: ErrInvalidPath},
		{name: "reserved leaf Thumbs.db", in: "Thumbs.db", wantErr: ErrInvalidPath},
		{name: "reserved leaf desktop.ini in nested dir", in: "x/y/desktop.INI", wantErr: ErrInvalidPath},
		{name: "AppleDouble prefix in segment", in: "._companion", wantErr: ErrInvalidPath},
		{name: "Office lock prefix in segment", in: "x/~$lockfile", wantErr: ErrInvalidPath},
		{name: "blob temp prefix in nested segment", in: "x/.blob-abc123", wantErr: ErrInvalidPath},

		// Windows constraints
		{name: "trailing dot", in: "a/file.", wantErr: ErrInvalidPath},
		{name: "trailing space", in: "a/file ", wantErr: ErrInvalidPath},
		{name: "windows-illegal char colon", in: `a/with:colon`, wantErr: ErrInvalidPath},
		{name: "windows-illegal char pipe", in: `a/with|pipe`, wantErr: ErrInvalidPath},
		{name: "windows-illegal char asterisk", in: `a/with*star`, wantErr: ErrInvalidPath},
		{name: "control char (tab)", in: "a/with\ttab", wantErr: ErrInvalidPath},
		{name: "control char (CR)", in: "a/with\rcr", wantErr: ErrInvalidPath},

		// NT device names (stem-only match, case-insensitive)
		{name: "CON literal", in: "CON", wantErr: ErrInvalidPath},
		{name: "con.txt (stem reserved)", in: "con.txt", wantErr: ErrInvalidPath},
		{name: "NUL literal", in: "NUL", wantErr: ErrInvalidPath},
		{name: "COM1.log", in: "com1.log", wantErr: ErrInvalidPath},
		{name: "LPT9.bin", in: "lpt9.bin", wantErr: ErrInvalidPath},
		// Intermediate directory must obey the same rules — a "con/"
		// path would surface to WebDAV listings as a hostile dir name.
		{name: "reserved intermediate dir", in: "con/file.txt", wantErr: ErrInvalidPath},

		// Unicode
		{name: "NFC-valid ascii", in: "a/é", wantClean: "a/é"},
		// "café" with combining acute (NFD) — refuse so two distinct
		// inputs cannot round-trip to the same blob.
		{name: "non-NFC (NFD) refused", in: "café", wantErr: ErrInvalidPath},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validatePath(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.wantClean {
				t.Errorf("clean = %q, want %q", got, tc.wantClean)
			}
		})
	}
}

func TestValidatePrefix(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{name: "empty (match all)", in: ""},
		{name: "directory prefix without trailing slash", in: "agents/ag_1"},
		{name: "directory prefix with trailing slash", in: "agents/ag_1/"},
		{name: "single segment", in: "agents"},
		// Reserved names ARE allowed in List prefixes per the doc — a
		// salvage tool may need to enumerate them — so a Thumbs.db
		// prefix is NOT rejected here even though validatePath rejects
		// it as a destination.
		{name: "reserved leaf allowed in prefix", in: "agents/Thumbs.db"},
		// Traversal / structural still refused.
		{name: "NUL refused", in: "a\x00b", wantErr: ErrInvalidPath},
		{name: "backslash refused", in: "a\\b", wantErr: ErrInvalidPath},
		{name: "leading slash refused", in: "/agents", wantErr: ErrInvalidPath},
		{name: "traversal refused", in: "agents/../x", wantErr: ErrInvalidPath},
		{name: "lone dotdot refused", in: "..", wantErr: ErrInvalidPath},
		// Trailing-only slash on otherwise-empty is "/", which has
		// leading slash → refused.
		{name: "lone slash", in: "/", wantErr: ErrInvalidPath},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePrefix(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
		})
	}
}
