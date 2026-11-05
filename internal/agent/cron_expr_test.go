package agent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidateCronExpr(t *testing.T) {
	t.Run("accept", func(t *testing.T) {
		valid := []string{
			"",                   // empty = scheduling disabled
			"*/5 * * * *",        // every 5 min
			"0 9 * * 1-5",        // weekday 09:00
			"0 */3 * * *",        // every 3 hours
			"30 14 * * *",        // daily 14:30
			"0 0 1 * *",          // monthly on the 1st
			"0,15,30,45 * * * *", // explicit 15-min cadence
		}
		for _, expr := range valid {
			if err := ValidateCronExpr(expr); err != nil {
				t.Errorf("expected %q to be valid, got %v", expr, err)
			}
		}
	})

	t.Run("reject", func(t *testing.T) {
		// 6-field (with seconds) and shortcut forms must be rejected so the
		// strict 5-field UI editor and the runtime stay in lockstep.
		invalid := []string{
			"* * * * * *", // 6 fields with seconds
			"@every 1m",   // shortcut
			"@hourly",     // shortcut
			"@reboot",     // shortcut
			"   ",         // whitespace only
			"foo bar",     // garbage
			"60 * * * *",  // out of range
		}
		for _, expr := range invalid {
			err := ValidateCronExpr(expr)
			if err == nil {
				t.Errorf("expected %q to be rejected", expr)
				continue
			}
			if !errors.Is(err, ErrInvalidCronExpr) {
				t.Errorf("expected ErrInvalidCronExpr for %q, got %v", expr, err)
			}
		}
	})

	t.Run("length cap", func(t *testing.T) {
		long := make([]byte, MaxCronExprLen+1)
		for i := range long {
			long[i] = '*'
		}
		err := ValidateCronExpr(string(long))
		if err == nil || !errors.Is(err, ErrInvalidCronExpr) {
			t.Errorf("expected length cap to reject %d-byte expression, got %v", len(long), err)
		}
	})
}

func TestParseCronSchedule_Roundtrip(t *testing.T) {
	cases := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		{
			expr: "*/5 * * * *",
			from: time.Date(2026, 5, 15, 12, 1, 0, 0, time.UTC),
			want: time.Date(2026, 5, 15, 12, 5, 0, 0, time.UTC),
		},
		{
			expr: "0 9 * * *",
			from: time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
			want: time.Date(2026, 5, 16, 9, 0, 0, 0, time.UTC),
		},
		{
			expr: "0 */3 * * *",
			from: time.Date(2026, 5, 15, 1, 30, 0, 0, time.UTC),
			want: time.Date(2026, 5, 15, 3, 0, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		t.Run(c.expr, func(t *testing.T) {
			sched, err := ParseCronSchedule(c.expr)
			if err != nil {
				t.Fatalf("parse %q: %v", c.expr, err)
			}
			got := sched.Next(c.from)
			if !got.Equal(c.want) {
				t.Errorf("Next(%v) = %v, want %v", c.from, got, c.want)
			}
		})
	}
}

func TestParseCronSchedule_EmptyRejected(t *testing.T) {
	if _, err := ParseCronSchedule(""); err == nil {
		t.Error("expected ParseCronSchedule(\"\") to return an error")
	}
}

func TestValidateCronExpr_MultiLine(t *testing.T) {
	valid := []string{
		"*/30 * * * 1-5\n0 */3 * * 0,6",
		"*/30 * * * 1-5\r\n0 */3 * * 0,6",   // CRLF tolerated
		"  */30 * * * 1-5  \n 0 9 * * 0,6 ", // per-line whitespace tolerated
	}
	for _, expr := range valid {
		if err := ValidateCronExpr(expr); err != nil {
			t.Errorf("expected %q to be valid, got %v", expr, err)
		}
	}
	invalid := []string{
		"*/30 * * * 1-5\n",              // trailing blank line
		"*/30 * * * 1-5\n\n0 9 * * 0,6", // interior blank line
		"*/30 * * * 1-5\n60 * * * *",    // second line out of range
		"*/30 * * * 1-5\n@preset:30",    // sentinel can't be mixed in
		"@preset:30\n*/30 * * * 1-5",
		"*/30 * * * 1-5\nTZ=UTC 0 9 * * *",
	}
	for _, expr := range invalid {
		err := ValidateCronExpr(expr)
		if err == nil {
			t.Errorf("expected %q to be rejected", expr)
			continue
		}
		if !errors.Is(err, ErrInvalidCronExpr) {
			t.Errorf("expected ErrInvalidCronExpr for %q, got %v", expr, err)
		}
	}

	lines := make([]string, MaxCronExprLines+1)
	for i := range lines {
		lines[i] = "0 9 * * *"
	}
	tooMany := strings.Join(lines, "\n")
	if err := ValidateCronExpr(tooMany); err == nil || !errors.Is(err, ErrInvalidCronExpr) {
		t.Errorf("expected %d lines to be rejected, got %v", len(lines), err)
	}
	if err := ValidateCronExpr(strings.Join(lines[:MaxCronExprLines], "\n")); err != nil {
		t.Errorf("expected %d lines to be accepted, got %v", MaxCronExprLines, err)
	}
}

func TestParseCronSchedule_MultiLineUnion(t *testing.T) {
	sched, err := ParseCronSchedule("*/30 * * * 1-5\n0 */3 * * 0,6")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	loc := time.Local
	cases := []struct {
		from, want time.Time
	}{
		// Fri 2026-10-23 10:05 → weekday line: 10:30
		{time.Date(2026, 10, 23, 10, 5, 0, 0, loc), time.Date(2026, 10, 23, 10, 30, 0, 0, loc)},
		// Fri 23:45 → crosses into Sat: weekend line fires at 00:00
		{time.Date(2026, 10, 23, 23, 45, 0, 0, loc), time.Date(2026, 10, 24, 0, 0, 0, 0, loc)},
		// Sat 00:00 → next is Sat 03:00 (weekday line silent)
		{time.Date(2026, 10, 24, 0, 0, 0, 0, loc), time.Date(2026, 10, 24, 3, 0, 0, 0, loc)},
		// Sun 22:10 → Mon 00:00 matches both lines; fires once at 00:00
		{time.Date(2026, 10, 25, 22, 10, 0, 0, loc), time.Date(2026, 10, 26, 0, 0, 0, 0, loc)},
		// Mon 00:00 → Mon 00:30
		{time.Date(2026, 10, 26, 0, 0, 0, 0, loc), time.Date(2026, 10, 26, 0, 30, 0, 0, loc)},
	}
	for _, c := range cases {
		if got := sched.Next(c.from); !got.Equal(c.want) {
			t.Errorf("Next(%s) = %s, want %s", c.from, got, c.want)
		}
	}
}

func TestParseCronSchedule_MultiLineSkipsNeverFiring(t *testing.T) {
	// "0 0 30 2 *" (Feb 30) never fires; robfig returns the zero time.
	// The union must ignore it instead of returning zero.
	sched, err := ParseCronSchedule("0 0 30 2 *\n0 9 * * *")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	from := time.Date(2026, 10, 25, 10, 0, 0, 0, time.Local)
	want := time.Date(2026, 10, 26, 9, 0, 0, 0, time.Local)
	if got := sched.Next(from); !got.Equal(want) {
		t.Errorf("Next = %s, want %s", got, want)
	}
}
