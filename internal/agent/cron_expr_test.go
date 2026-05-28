package agent

import (
	"errors"
	"testing"
	"time"
)

func TestValidateCronExpr(t *testing.T) {
	t.Run("accept", func(t *testing.T) {
		valid := []string{
			"",                  // empty = scheduling disabled
			"*/5 * * * *",       // every 5 min
			"0 9 * * 1-5",       // weekday 09:00
			"0 */3 * * *",       // every 3 hours
			"30 14 * * *",       // daily 14:30
			"0 0 1 * *",         // monthly on the 1st
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
			"* * * * * *",   // 6 fields with seconds
			"@every 1m",     // shortcut
			"@hourly",       // shortcut
			"@reboot",       // shortcut
			"   ",           // whitespace only
			"foo bar",       // garbage
			"60 * * * *",    // out of range
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
