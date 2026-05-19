package server

import (
	"testing"
	"time"
)

// TestAgentRuntimeSnapshot_DisplayETag pins the composite HTTP ETag
// shape so the cache-busting suffixes don't get accidentally
// dropped. The whole point of the composite is that runtime field
// changes (nextCronAt advancing on cron fire, cronPausedGlobal
// flipping when the Dashboard toggle is hit) MUST flip the etag —
// otherwise a 304 fast-path lets a browser serve the cached body
// with stale values forever. See the bug fixed alongside this test:
// "agent settings Next check-in 表示が過去日時に張り付く".
func TestAgentRuntimeSnapshot_DisplayETag(t *testing.T) {
	t1 := time.Date(2026, 5, 19, 11, 24, 0, 0, time.FixedZone("JST", 9*3600))
	t2 := time.Date(2026, 5, 19, 11, 54, 0, 0, time.FixedZone("JST", 9*3600))

	cases := []struct {
		name string
		snap agentRuntimeSnapshot
		want string
	}{
		{
			name: "empty row etag → empty composite (signals skip-header)",
			snap: agentRuntimeSnapshot{rowETag: "", nextCron: t1, cronPaused: true},
			want: "",
		},
		{
			name: "row etag only, no schedule, not paused",
			snap: agentRuntimeSnapshot{rowETag: "7-aaa"},
			want: "7-aaa",
		},
		{
			name: "row + paused suffix",
			snap: agentRuntimeSnapshot{rowETag: "7-aaa", cronPaused: true},
			want: "7-aaa.p",
		},
		{
			name: "row + nextCron suffix (the bug case)",
			snap: agentRuntimeSnapshot{rowETag: "7-aaa", nextCron: t1},
			// 2026-05-19T11:24+09:00 = unix 1779755040 = base36 "tjfizc"
			want: "7-aaa.n" + base36Of(t1.Unix()),
		},
		{
			name: "row + paused + nextCron (full composite)",
			snap: agentRuntimeSnapshot{rowETag: "7-aaa", nextCron: t1, cronPaused: true},
			want: "7-aaa.p.n" + base36Of(t1.Unix()),
		},
		{
			name: "nextCron advance flips the composite",
			snap: agentRuntimeSnapshot{rowETag: "7-aaa", nextCron: t2},
			want: "7-aaa.n" + base36Of(t2.Unix()),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.snap.displayETag()
			if got != c.want {
				t.Errorf("displayETag() = %q, want %q", got, c.want)
			}
		})
	}

	// Cross-case invariant: the same row etag with different
	// nextCron MUST yield distinct composites. This is the
	// regression guard for the cache-stale bug.
	a := agentRuntimeSnapshot{rowETag: "7-aaa", nextCron: t1}.displayETag()
	b := agentRuntimeSnapshot{rowETag: "7-aaa", nextCron: t2}.displayETag()
	if a == b {
		t.Fatalf("nextCron advance must change displayETag, both = %q", a)
	}
}

// TestStripAgentDisplayETagSuffix exercises the production strip
// helper directly so the precondition path keeps accepting an
// If-Match that was copied verbatim from a GET's HTTP ETag header.
// The full grammar displayETag can emit is "<row>", "<row>.p",
// "<row>.n<base36>", and "<row>.p.n<base36>"; everything else MUST
// pass through intact so a malformed precondition surfaces as a 412
// rather than silently relaxing the row check.
func TestStripAgentDisplayETagSuffix(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"bare row", "7-aaa", "7-aaa"},
		{"row + paused", "7-aaa.p", "7-aaa"},
		{"row + nextCron", "7-aaa.ntjfizc", "7-aaa"},
		{"row + paused + nextCron", "7-aaa.p.ntjfizc", "7-aaa"},
		{"wildcard preserved", "*", "*"},
		{"empty passes through", "", ""},
		// Defensive: an unknown suffix MUST NOT be stripped — that
		// would let "7-aaa.garbage" silently match row "7-aaa" and
		// relax the precondition.
		{"unknown suffix kept", "7-aaa.x", "7-aaa.x"},
		{"empty .n discarded as unknown", "7-aaa.n", "7-aaa.n"},
		{"non-base36 .n kept", "7-aaa.nFOO", "7-aaa.nFOO"},
		// .p without trailing .n still strips.
		{"trailing .p only", "1-bbb.p", "1-bbb"},
		// Out-of-order ".nXX.p" is NOT emitted by displayETag; the
		// strip degrades gracefully — peels the trailing .p but
		// rejects the malformed .n suffix ("XX.p" fails the base36
		// check). The remaining "1-bbb.nabc" will mismatch the row
		// etag and the precondition will 412, which is the safer
		// outcome on malformed input.
		{"out-of-order .n.p partial", "1-bbb.nabc.p", "1-bbb.nabc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripAgentDisplayETagSuffix(c.in); got != c.want {
				t.Errorf("stripAgentDisplayETagSuffix(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	// Round-trip invariant: every value displayETag could plausibly
	// emit must strip back to the bare row etag.
	row := "7-abc"
	t1 := time.Date(2026, 5, 19, 11, 24, 0, 0, time.UTC)
	for _, composite := range []string{
		(agentRuntimeSnapshot{rowETag: row}).displayETag(),
		(agentRuntimeSnapshot{rowETag: row, cronPaused: true}).displayETag(),
		(agentRuntimeSnapshot{rowETag: row, nextCron: t1}).displayETag(),
		(agentRuntimeSnapshot{rowETag: row, cronPaused: true, nextCron: t1}).displayETag(),
	} {
		if got := stripAgentDisplayETagSuffix(composite); got != row {
			t.Errorf("round-trip %q stripped to %q, want %q", composite, got, row)
		}
	}
}

func base36Of(n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%36]
		n /= 36
	}
	return string(buf[i:])
}
