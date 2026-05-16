package agent

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/robfig/cron/v3"
)

// MaxCronExprLen caps the byte size of a stored cron expression. The 5-field
// standard form never needs anywhere near this many characters; the limit is
// purely a defence against pathological input getting persisted to agents.json
// or echoed back into log lines.
const MaxCronExprLen = 256

// cronStdParser is the canonical parser used everywhere in the agent package.
// It accepts only the standard 5-field form (M H DOM Mon DOW) — no seconds,
// no @every / @hourly / @reboot shortcuts. Restricting the surface keeps the
// UI editor and the runtime in lockstep: if it parses here, the human-readable
// preview the UI shows is always going to match what cron actually fires.
var cronStdParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
)

// CronPresetSentinelPrefix is the marker for the "expand-with-per-agent-
// offset" sentinel form. The UI sends "@preset:30" when the user picks the
// "30m" preset chip; the manager resolves it via intervalToCronExpr at
// Save time so each agent gets a distinct minute-of-hour and stops the
// "all bots fire at :00" thundering-herd that fixed cron strings caused.
const CronPresetSentinelPrefix = "@preset:"

// ValidateCronExpr returns nil for an empty expression (= scheduling disabled),
// for a valid "@preset:N" sentinel (N must be in legacyAllowedIntervals), or
// for a valid 5-field cron expression. Anything else is rejected with a
// wrapped parse error.
func ValidateCronExpr(expr string) error {
	if expr == "" {
		return nil
	}
	if len(expr) > MaxCronExprLen {
		return fmt.Errorf("%w: expression exceeds %d bytes", ErrInvalidCronExpr, MaxCronExprLen)
	}
	if n, ok := parseCronPresetSentinel(expr); ok {
		if _, allowed := legacyAllowedIntervals[n]; !allowed {
			return fmt.Errorf("%w: preset interval %d not in allowed set", ErrInvalidCronExpr, n)
		}
		return nil
	}
	// robfig/cron's parser strips a leading TZ=/CRON_TZ= descriptor before
	// counting fields, so "TZ=UTC * * * * *" would slip past a count-after-
	// parse check (and worse, "TZ=UTC" alone panics inside cron.Parse on a
	// nil schedule deref). Enforce exactly five whitespace-separated fields
	// with no embedded descriptor before delegating.
	if err := requireFiveFields(expr); err != nil {
		return err
	}
	if _, err := cronStdParser.Parse(expr); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCronExpr, err)
	}
	return nil
}

// ResolveCronPreset returns the per-agent-offset cron expression for a
// "@preset:N" sentinel, or the input unchanged for any other (validated)
// expression. Used at Save time so the persisted form is always a real
// 5-field cron string — the sentinel never reaches the cron scheduler.
//
// Returns "" for an unknown / out-of-whitelist N so the caller can
// surface the schedule as disabled rather than picking a wrong cadence.
func ResolveCronPreset(expr, agentID string) string {
	n, ok := parseCronPresetSentinel(expr)
	if !ok {
		return expr
	}
	return intervalToCronExpr(n, agentID)
}

// parseCronPresetSentinel returns (intervalMinutes, true) when s matches
// the "@preset:N" form; (0, false) otherwise. Accepts only positive
// integers with no leading zero — any leading sign, whitespace, leading 0,
// or trailing character bounces. Mirrors the frontend regex /^[1-9]\d*$/
// so the two parsers can't drift on a "@preset:05" edge case.
func parseCronPresetSentinel(s string) (int, bool) {
	if !strings.HasPrefix(s, CronPresetSentinelPrefix) {
		return 0, false
	}
	rest := s[len(CronPresetSentinelPrefix):]
	if rest == "" {
		return 0, false
	}
	if rest[0] == '0' { // strict: no leading zero, no "00", no "0"
		return 0, false
	}
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 { // overflow guard, well above any sensible interval
			return 0, false
		}
	}
	return n, true
}

// ParseCronSchedule parses an expression into a cron.Schedule using the same
// strict parser as ValidateCronExpr. Empty input is an error here — callers
// that want to allow "disabled" must check upfront.
func ParseCronSchedule(expr string) (cron.Schedule, error) {
	if expr == "" {
		return nil, fmt.Errorf("%w: empty expression", ErrInvalidCronExpr)
	}
	if err := requireFiveFields(expr); err != nil {
		return nil, err
	}
	sched, err := cronStdParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCronExpr, err)
	}
	return sched, nil
}

// requireFiveFields rejects any expression that doesn't tokenize to exactly
// five whitespace-separated fields, OR that begins with a TZ=/CRON_TZ=
// descriptor. The standard parser would otherwise accept either by
// pre-stripping the descriptor and re-counting, which defeats the
// "5-field-only" surface the editor and validators promise.
func requireFiveFields(expr string) error {
	trimmed := strings.TrimSpace(expr)
	upper := strings.ToUpper(trimmed)
	if strings.HasPrefix(upper, "TZ=") || strings.HasPrefix(upper, "CRON_TZ=") {
		return fmt.Errorf("%w: timezone descriptors are not supported", ErrInvalidCronExpr)
	}
	fields := strings.Fields(trimmed)
	if len(fields) != 5 {
		return fmt.Errorf("%w: expected 5 fields (M H DOM Mon DOW), got %d", ErrInvalidCronExpr, len(fields))
	}
	return nil
}

// legacyAllowedIntervals lists every value the pre-CronExpr UI ever accepted
// for intervalMinutes. Mirrors the old `allowedIntervals` map exactly so a
// migration of an existing 3h / 6h / 12h agent doesn't silently lose its
// schedule. The helper below refuses anything outside this set so a hand-
// edited or schema-drifted JSON value (1, 90, 999, …) can't get translated
// into a confidently-wrong cron expression that fires at the wrong cadence
// forever. Callers receive "" for rejected inputs and are expected to warn +
// persist scheduling-disabled.
var legacyAllowedIntervals = map[int]struct{}{
	5: {}, 10: {}, 30: {}, 60: {}, 180: {}, 360: {}, 720: {}, 1440: {},
}

// intervalToCronExpr converts a legacy intervalMinutes value to a 5-field cron
// expression with a deterministic per-agent offset derived from the ID hash.
// Used during agent migration from the IntervalMinutes era — the live API and
// store now persist CronExpr directly, so no production path calls this on
// freshly-created agents.
//
// Returns "" for non-positive intervals OR for values outside the legacy
// whitelist (legacyAllowedIntervals). The per-agent offset spreads agents
// across the day so they don't all fire on the minute boundary.
func intervalToCronExpr(intervalMinutes int, agentID string) string {
	if intervalMinutes <= 0 {
		return ""
	}
	if _, ok := legacyAllowedIntervals[intervalMinutes]; !ok {
		return ""
	}

	h := fnv.New32a()
	h.Write([]byte(agentID))
	hash := int(h.Sum32())

	if intervalMinutes >= 60 {
		hours := intervalMinutes / 60
		minuteOfDay := hash % 1440
		minuteOffset := minuteOfDay % 60
		hourOffset := (minuteOfDay / 60) % hours

		if hours >= 24 {
			return fmt.Sprintf("%d %d * * *", minuteOffset, minuteOfDay/60%24)
		}
		hourList := make([]string, 0, 24/hours)
		for hr := hourOffset; hr < 24; hr += hours {
			hourList = append(hourList, fmt.Sprintf("%d", hr))
		}
		return fmt.Sprintf("%d %s * * *", minuteOffset, strings.Join(hourList, ","))
	}

	offset := hash % intervalMinutes
	mins := make([]string, 0, 60/intervalMinutes)
	for m := offset; m < 60; m += intervalMinutes {
		mins = append(mins, fmt.Sprintf("%d", m))
	}
	return fmt.Sprintf("%s * * * *", strings.Join(mins, ","))
}
