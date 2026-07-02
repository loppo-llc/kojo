package uploadpath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDir(t *testing.T) {
	want := filepath.Join(os.TempDir(), "kojo", "upload")
	if got := Dir(); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

// TestSanitizeName pins the unified upload-sanitization security
// invariant: filepath.Base, then map "/", "\\", NUL and any control
// character to "_", then collapse "", "." and ".." to "_".
func TestSanitizeName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "photo.jpg", "photo.jpg"},
		{"strips directory", "/etc/passwd", "passwd"},
		{"forward slash residual", "a/b.txt", "b.txt"},
		{"backslash to underscore", "a\\b.txt", "a_b.txt"},
		{"nul to underscore", "a\x00b.txt", "a_b.txt"},
		{"traversal", "../../secret", "secret"},
		{"mixed", "dir\\sub\x00name.png", "dir_sub_name.png"},
		{"newline to underscore", "a\nb.txt", "a_b.txt"},
		{"tab to underscore", "a\tb.txt", "a_b.txt"},
		{"escape to underscore", "a\x1bb.txt", "a_b.txt"},
		{"empty to underscore", "", "_"},    // filepath.Base("") == "." → collapsed
		{"dot to underscore", ".", "_"},     // bare "." is unsafe
		{"dotdot to underscore", "..", "_"}, // bare ".." is unsafe
		{"dotdot slash", "foo/..", "_"},     // filepath.Base → ".." → collapsed
		{"only control", "\n", "_"},         // Base → "\n" → "_" (not collapsed further)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeName(tt.in); got != tt.want {
				t.Errorf("SanitizeName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
