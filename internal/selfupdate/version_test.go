package selfupdate

import (
	"testing"
)

func TestParseVersion_success(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want Version
	}{
		{"v0.116.3", Version{Major: 0, Minor: 116, Patch: 3}},
		{"0.110.0", Version{Major: 0, Minor: 110, Patch: 0}},
		{"v1.2.3", Version{Major: 1, Minor: 2, Patch: 3}},
		{"v0.116.2-4-g5ace25a", Version{Major: 0, Minor: 116, Patch: 2, Ahead: 4}},
		{"v0.116.2-4-g5ace25a-dirty", Version{Major: 0, Minor: 116, Patch: 2, Ahead: 4, Dirty: true}},
		{"0.1.0-dirty", Version{Major: 0, Minor: 1, Patch: 0, Dirty: true}},
		{"v10.0.0-1-gabcdef0", Version{Major: 10, Minor: 0, Patch: 0, Ahead: 1}},
		{"v0.0.0-0-g0", Version{Major: 0, Minor: 0, Patch: 0, Ahead: 0}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseVersion(tc.in)
			if err != nil {
				t.Fatalf("ParseVersion(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseVersion(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseVersion_failure(t *testing.T) {
	t.Parallel()
	// Strict failures: anything we cannot map onto X.Y.Z[+ahead][+dirty]
	// must error so IsNewer stays false for unstamped builds.
	cases := []string{
		"",
		"dev",
		"5ace25a",
		"v5ace25a",
		"v0.116",
		"0.116.3.1",
		"v0.116.2-4",
		"v0.116.2-4-g",
		"v0.116.2-4-G5ace25a",
		"v0.116.2-4-g5ACE25A",
		"v0.116.2-abc-g5ace25a",
		"v0.116.2-4-g5ace25a-extra",
		"v01a.2.3",
		"v-1.2.3",
		"V0.1.0",
		"-dirty",
		"v",
		"latest",
	}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseVersion(in); err == nil {
				t.Fatalf("ParseVersion(%q) succeeded, want error", in)
			}
		})
	}
}

func TestVersion_Compare(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b Version
		want int
	}{
		{"equal clean", Version{1, 2, 3, 0, false}, Version{1, 2, 3, 0, false}, 0},
		// Dirty is ignored for ordering: same describe point, dirty or not.
		{"dirty ignored", Version{1, 2, 3, 0, true}, Version{1, 2, 3, 0, false}, 0},
		{"major less", Version{0, 9, 0, 0, false}, Version{1, 0, 0, 0, false}, -1},
		{"major greater", Version{2, 0, 0, 0, false}, Version{1, 9, 9, 0, false}, 1},
		{"minor", Version{1, 1, 0, 0, false}, Version{1, 2, 0, 0, false}, -1},
		{"patch", Version{1, 2, 3, 0, false}, Version{1, 2, 4, 0, false}, -1},
		// Ahead is the final tiebreak: more commits past the tag is newer.
		{"ahead less", Version{1, 2, 3, 1, false}, Version{1, 2, 3, 4, false}, -1},
		{"ahead greater", Version{1, 2, 3, 5, false}, Version{1, 2, 3, 2, false}, 1},
		{"ahead equal", Version{1, 2, 3, 4, true}, Version{1, 2, 3, 4, false}, 0},
		{"patch beats ahead", Version{1, 2, 4, 0, false}, Version{1, 2, 3, 99, false}, 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.a.Compare(tc.b); got != tc.want {
				t.Fatalf("Compare(%+v, %+v) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
			// Antisymmetry: reverse should flip the sign (0 stays 0).
			if got := tc.b.Compare(tc.a); got != -tc.want {
				t.Fatalf("reverse Compare = %d, want %d", got, -tc.want)
			}
		})
	}
}

func TestIsNewer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		latest, current string
		want            bool
	}{
		{"tag newer", "v0.117.0", "v0.116.3", true},
		{"tag older", "v0.116.0", "v0.116.3", false},
		{"equal", "v0.116.3", "v0.116.3", false},
		{"ahead current older", "v0.116.3", "v0.116.2-4-g5ace25a", true},
		{"release vs dirty same tag", "v0.116.3", "v0.116.3-dirty", false},
		// Unparseable sides never auto-update.
		{"latest unparseable", "dev", "v0.116.3", false},
		{"current unparseable", "v0.116.3", "dev", false},
		{"current bare hash", "v0.116.3", "5ace25a", false},
		{"both empty", "", "", false},
		{"latest empty", "", "v0.116.3", false},
		{"current empty", "v0.116.3", "", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsNewer(tc.latest, tc.current); got != tc.want {
				t.Fatalf("IsNewer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
			}
		})
	}
}
