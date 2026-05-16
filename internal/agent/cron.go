package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
	"github.com/robfig/cron/v3"
)

const cronTimeout = 10 * time.Minute // default timeout; per-agent override via TimeoutMinutes
const cronMinInterval = 50 * time.Second // minimum interval between runs for same agent

// cronLockFile is the legacy v0 throttle marker filename. Kept as
// a constant so runtime helpers (acquireCronLock + the reset path
// in manager_lifecycle.go) can recognise and best-effort unlink a
// stray file from a pre-cutover install. The v0 → v1 migration
// importer (blobs.go) does NOT reference this constant — its
// handling of the dotfile is documented inline there. After
// Phase 2c-2 slice 12 the throttle is canonically stored in the
// kv table; see cron_lock_kv.go.
const cronLockFile = ".cron_last"

// cronPrompt builds the periodic check-in prompt. See cronPromptAt for details.
func cronPrompt(nextRun time.Time, timeoutMinutes int, customMessage string) string {
	return cronPromptAt(time.Now(), nextRun, timeoutMinutes, customMessage)
}

// cronPromptAt is the time-injectable form of cronPrompt for unit testing.
// If customMessage is non-empty it replaces the default trailing instruction;
// the literal "{date}" inside customMessage is replaced with today's date in
// YYYY-MM-DD form. The custom section is separated from the meta header by a
// blank line so an injected "[system message]" prefix cannot blend in with the
// surrounding meta text.
func cronPromptAt(now, nextRun time.Time, timeoutMinutes int, customMessage string) string {
	today := now.Format("2006-01-02")
	msg := "[system message] " + now.Format("2006年1月2日 15:04") + "の定期チェックインです。"

	msg += fmt.Sprintf("（今回のタイムアウトは%d分", timeoutMinutes)
	if !nextRun.IsZero() {
		var nextFmt string
		if nextRun.YearDay() == now.YearDay() && nextRun.Year() == now.Year() {
			nextFmt = nextRun.Format("15:04")
		} else {
			nextFmt = nextRun.Format("1月2日 15:04")
		}
		msg += "。完了後の次回のチェックインは最短" + formatUntil(nextRun, now) + "後 (" + nextFmt + ")"
	}
	msg += "）"
	if trimmed := strings.TrimSpace(customMessage); trimmed != "" {
		// Blank line + heading make the user-supplied section visually
		// distinguishable from the meta header even if the value contains
		// "[system message]" or similar prompt-bracketing.
		msg += "\n\n--- 指示 ---\n" + strings.ReplaceAll(trimmed, "{date}", today)
	} else {
		msg += "最近の出来事や気づきがあれば memory/" + today + ".md に記録し、必要なタスクを実行してください。"
	}
	return msg
}

// checkinPrompt builds the manual check-in prompt. Unlike cronPromptAt this
// is fired on demand from the UI and has no scheduled successor, so it omits
// the "次回のチェックイン" footer and uses the wording "チェックイン" (not
// "定期チェックイン") to make the source visible in the transcript.
func checkinPrompt(now time.Time, timeoutMinutes int, customMessage string) string {
	today := now.Format("2006-01-02")
	msg := "[system message] " + now.Format("2006年1月2日 15:04") + "のチェックインです。"
	msg += fmt.Sprintf("（今回のタイムアウトは%d分）", timeoutMinutes)
	if trimmed := strings.TrimSpace(customMessage); trimmed != "" {
		msg += "\n\n--- 指示 ---\n" + strings.ReplaceAll(trimmed, "{date}", today)
	} else {
		msg += "最近の出来事や気づきがあれば memory/" + today + ".md に記録し、必要なタスクを実行してください。"
	}
	return msg
}

// formatUntil returns a Japanese-formatted duration from now until t,
// rounded to the nearest minute (minimum 1分).
func formatUntil(t, now time.Time) string {
	mins := int((t.Sub(now) + 30*time.Second) / time.Minute)
	if mins < 1 {
		mins = 1
	}
	if mins < 60 {
		return fmt.Sprintf("%d分", mins)
	}
	h := mins / 60
	m := mins % 60
	if m == 0 {
		return fmt.Sprintf("%d時間", h)
	}
	return fmt.Sprintf("%d時間%d分", h, m)
}

// cronWatchdogInterval is how often the watchdog goroutine looks for cron
// entries whose Next has slipped into the past. macOS sleep stops robfig/cron's
// monotonic-clock timer; on wake, the saved Next.Time is in the past and the
// timer never refires. Re-arming the entry forces Next to be recomputed.
const cronWatchdogInterval = 60 * time.Second

// cronWatchdogStaleThreshold is how far past `now` an entry's Next must be
// before the watchdog re-arms it. A small grace window avoids fighting
// in-flight ticks (the entry's Next briefly equals "just now" while the job
// is being dispatched).
const cronWatchdogStaleThreshold = 30 * time.Second

// cronEntry pairs the cron library's EntryID with the original expression
// used to register it. The expression is needed when the watchdog re-arms a
// stale entry — Schedule(...) only takes a string, not a cron.Schedule, and
// re-deriving the string from the parsed Schedule would require a stringifier
// the library doesn't provide.
type cronEntry struct {
	id   cron.EntryID
	expr string
}

// cronScheduler manages periodic agent executions.
type cronScheduler struct {
	mu      sync.Mutex
	c       *cron.Cron
	entries map[string]cronEntry // agent ID -> entry
	mgr     *Manager
	logger  *slog.Logger
	stop    chan struct{}
	stopped sync.Once
}

func newCronScheduler(mgr *Manager, logger *slog.Logger) *cronScheduler {
	cronLogger := cron.PrintfLogger(slog.NewLogLogger(logger.Handler(), slog.LevelDebug))
	return &cronScheduler{
		c:       cron.New(cron.WithChain(cron.SkipIfStillRunning(cronLogger))),
		entries: make(map[string]cronEntry),
		mgr:     mgr,
		logger:  logger,
		stop:    make(chan struct{}),
	}
}

func (cs *cronScheduler) Start() {
	cs.c.Start()
	go cs.watchdogLoop()
}

func (cs *cronScheduler) Stop() {
	cs.stopped.Do(func() { close(cs.stop) })
	ctx := cs.c.Stop()
	// Wait for running jobs to finish (with timeout)
	select {
	case <-ctx.Done():
	case <-time.After(cronTimeout + 30*time.Second):
		cs.logger.Warn("cron shutdown timed out waiting for jobs")
	}
}

// Schedule adds or updates a cron schedule for an agent.
// If cronExpr is empty, any existing schedule is removed.
// cronExpr must be a 5-field standard cron expression — shortcuts like @every
// or 6-field forms (with seconds) are rejected via cronStdParser.
func (cs *cronScheduler) Schedule(agentID string, cronExpr string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.scheduleLocked(agentID, cronExpr)
}

// scheduleLocked is the lock-held form of Schedule. Used by both the public
// Schedule and the watchdog re-arm path so the entry table mutation stays
// atomic relative to nextRun lookups.
func (cs *cronScheduler) scheduleLocked(agentID string, cronExpr string) error {
	// Remove existing entry
	if e, ok := cs.entries[agentID]; ok {
		cs.c.Remove(e.id)
		delete(cs.entries, agentID)
	}

	if cronExpr == "" {
		return nil
	}

	sched, err := ParseCronSchedule(cronExpr)
	if err != nil {
		return err
	}
	entryID := cs.c.Schedule(sched, cron.FuncJob(func() {
		cs.runCronJob(agentID)
	}))
	cs.entries[agentID] = cronEntry{id: entryID, expr: cronExpr}
	return nil
}

// nextRun returns the next scheduled run time for an agent, adjusted for
// silent hours. If the cron's Next falls inside the silent window, it
// iterates the cron schedule forward to find the first tick outside it.
// Returns zero time if no schedule exists.
//
// Self-heal: when robfig/cron's internal timer hasn't caught up after a
// macOS sleep skip, entry.Next can be zero or in the past. In that case we
// derive the next tick directly from entry.Schedule against `time.Now()` so
// the UI never displays a stale "X ago" value while waiting for the
// watchdog to actually re-arm.
func (cs *cronScheduler) nextRun(agentID string, silentStart, silentEnd string) time.Time {
	cs.mu.Lock()
	var entry cron.Entry
	if e, ok := cs.entries[agentID]; ok {
		entry = cs.c.Entry(e.id)
	}
	cs.mu.Unlock()

	if entry.Schedule == nil {
		return time.Time{}
	}

	next := entry.Next
	now := time.Now()
	if next.IsZero() || next.Before(now) {
		next = entry.Schedule.Next(now)
	}

	if silentStart == "" || silentEnd == "" {
		return next
	}

	return nextRunOutsideSilentWindow(entry.Schedule, next, silentStart, silentEnd)
}

// watchdogLoop runs every cronWatchdogInterval and re-arms any cron entry
// whose Next has slipped past now-cronWatchdogStaleThreshold. This is the
// macOS-sleep recovery path: robfig/cron uses a monotonic-clock timer that
// stops while the laptop sleeps, so on wake the entry's Next is still set
// to the pre-sleep value and the timer never fires.
func (cs *cronScheduler) watchdogLoop() {
	t := time.NewTicker(cronWatchdogInterval)
	defer t.Stop()
	for {
		select {
		case <-cs.stop:
			return
		case now := <-t.C:
			cs.reArmStaleEntries(now)
		}
	}
}

// reArmStaleEntries iterates the entry table and re-Schedules any entry whose
// Next is older than now-cronWatchdogStaleThreshold. Returns the list of
// agent IDs that were re-armed (mostly for tests / observability).
func (cs *cronScheduler) reArmStaleEntries(now time.Time) []string {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	stale := reArmStale(cs.entriesSnapshot(), now, cronWatchdogStaleThreshold)
	if len(stale) == 0 {
		return nil
	}
	rearmed := make([]string, 0, len(stale))
	for _, agentID := range stale {
		e, ok := cs.entries[agentID]
		if !ok {
			continue
		}
		entry := cs.c.Entry(e.id)
		oldNext := entry.Next
		if err := cs.scheduleLocked(agentID, e.expr); err != nil {
			cs.logger.Warn("watchdog failed to re-arm cron entry", "agent", agentID, "err", err)
			continue
		}
		cs.logger.Warn("cron entry re-armed after wall-clock skip",
			"agent", agentID, "staleNext", oldNext)
		rearmed = append(rearmed, agentID)
	}
	return rearmed
}

// entrySnapshot is the watchdog's view of one cron entry — agent ID, the
// current Next time, and the originally-registered expression. Decoupled
// from the live cron.Entry so reArmStale can be unit-tested without spinning
// up an actual cron.Cron.
type entrySnapshot struct {
	agentID string
	next    time.Time
	expr    string
}

// entriesSnapshot copies the live entry table into a slice of pure data
// structs that reArmStale can iterate without holding any cron internals.
// Caller must hold cs.mu.
//
// Pulls cron entries via a single Entries() call (one snapshot-channel
// round trip into the cron run loop) and joins them against cs.entries by
// EntryID. Calling cs.c.Entry(id) per agent would issue N round trips
// through the same channel and serialize against the watchdog's outer
// 1-minute tick, which is wasteful and risks contention with cron's own
// add/remove/snapshot path on busy schedulers.
func (cs *cronScheduler) entriesSnapshot() []entrySnapshot {
	live := cs.c.Entries()
	byID := make(map[cron.EntryID]cron.Entry, len(live))
	for _, e := range live {
		byID[e.ID] = e
	}
	out := make([]entrySnapshot, 0, len(cs.entries))
	for agentID, e := range cs.entries {
		entry := byID[e.id]
		out = append(out, entrySnapshot{
			agentID: agentID,
			next:    entry.Next,
			expr:    e.expr,
		})
	}
	return out
}

// reArmStale returns the agent IDs of entries whose Next has slipped past
// now-threshold. Pure function — no cron internals — so unit tests can
// drive it directly without a running scheduler.
func reArmStale(entries []entrySnapshot, now time.Time, threshold time.Duration) []string {
	cutoff := now.Add(-threshold)
	var stale []string
	for _, e := range entries {
		if e.next.IsZero() {
			continue
		}
		if e.next.Before(cutoff) {
			stale = append(stale, e.agentID)
		}
	}
	return stale
}

// nextRunOutsideSilentWindowHorizon caps how far ahead we search for the next
// cron tick that falls outside silent hours. 31 days covers a "first day of
// the month" schedule plus a full silent window — the realistic upper bound
// for what an agent owner would configure. Anything longer (annual cron with
// silent hours stomping on its only fire time) is genuinely "no next run in
// any reasonable horizon" and surfaces as "—" in the UI.
const nextRunOutsideSilentWindowHorizon = 31 * 24 * time.Hour

// nextRunOutsideSilentWindow finds the first cron tick at or after t that falls
// outside the silent hours window. Bounded by nextRunOutsideSilentWindowHorizon
// so a degenerate combination of cron expression and silent window can't loop
// forever; returns the zero time when no valid tick is found within that horizon.
//
// When a candidate falls inside the silent window we jump straight to the
// silent-window's end (in the candidate's local day) and let Schedule.Next
// pick up from there. This is much cheaper than walking sched.Next 1-tick at
// a time for high-frequency expressions like `* * * * *` paired with a long
// silent window — the old per-step walk needed thousands of iterations to
// cross an 8-hour silent window at 1-minute cadence.
func nextRunOutsideSilentWindow(sched cron.Schedule, t time.Time, start, end string) time.Time {
	s, _ := time.Parse("15:04", start)
	e, _ := time.Parse("15:04", end)
	startMin := s.Hour()*60 + s.Minute()
	endMin := e.Hour()*60 + e.Minute()

	deadline := t.Add(nextRunOutsideSilentWindowHorizon)
	candidate := t
	for !candidate.After(deadline) {
		cMin := candidate.Hour()*60 + candidate.Minute()
		inSilent := false
		if startMin <= endMin {
			inSilent = cMin >= startMin && cMin < endMin
		} else {
			inSilent = cMin >= startMin || cMin < endMin
		}
		if !inSilent {
			return candidate
		}
		// Skip the rest of the silent window in one hop instead of walking
		// per-tick. silentEndAfter computes the wall-clock end of the
		// silent window relative to `candidate`, then sched.Next yields the
		// first tick at or after that boundary.
		jumpTo := silentEndAfter(candidate, startMin, endMin)
		if !jumpTo.After(candidate) {
			// Defensive: a bug in silentEndAfter must not let the loop
			// stall on the same candidate. Fall back to a single tick
			// advance so the deadline guard still terminates.
			next := sched.Next(candidate)
			if !next.After(candidate) {
				return time.Time{}
			}
			candidate = next
			continue
		}
		next := sched.Next(jumpTo.Add(-time.Second))
		if !next.After(candidate) {
			return time.Time{}
		}
		candidate = next
	}
	// Couldn't find a non-silent tick within the horizon; return zero so the
	// UI shows "—" instead of a misleading time that would be skipped.
	return time.Time{}
}

// silentEndAfter returns the wall-clock instant when the silent window ending
// at endMin (minutes-since-midnight) closes, relative to t. Handles both
// normal (start < end, e.g. 01:00→07:00) and overnight (start > end, e.g.
// 23:00→09:00) ranges. The returned instant is always strictly after t when
// t is inside the silent window — the caller relies on that guarantee to
// make forward progress.
func silentEndAfter(t time.Time, startMin, endMin int) time.Time {
	tMin := t.Hour()*60 + t.Minute()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	endToday := day.Add(time.Duration(endMin) * time.Minute)
	if startMin <= endMin {
		// Normal range: silent window ends today at endMin if t is before
		// it, otherwise the next instance is "tomorrow at endMin" but at
		// that point t is already outside the window (the caller would
		// have returned). Defensive return for completeness.
		if tMin < endMin {
			return endToday
		}
		return endToday.Add(24 * time.Hour)
	}
	// Overnight range (silent crosses midnight): if t is in the
	// after-midnight portion (tMin < endMin), the window ends today at
	// endMin. If t is in the before-midnight portion (tMin >= startMin),
	// the window ends tomorrow at endMin.
	if tMin < endMin {
		return endToday
	}
	return endToday.Add(24 * time.Hour)
}

// Remove removes the cron schedule for an agent.
func (cs *cronScheduler) Remove(agentID string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if e, ok := cs.entries[agentID]; ok {
		cs.c.Remove(e.id)
		delete(cs.entries, agentID)
	}
}

// acquireCronLock throttles a per-agent cron firing. Returns
// (acquired, stampETag). When acquired==true the caller should run
// the job; stampETag identifies the kv row this call wrote so a
// later rollback (Chat() refused the tick) can target only its own
// stamp via rollbackCronLockDB.
//
// Implementation lives in cron_lock_kv.go: the throttle row is stored
// in the kv table at (namespace="scheduler", key="cron_last/<agentID>",
// scope=machine). The v0 `<agentDir>/.cron_last` lock file is no
// longer written; a stray legacy file from a v0 → v1 install is
// best-effort unlinked here so the agent dir is cleaned up over time.
//
// Fail-closed: a nil store, a kv read/write error, or a malformed row
// returns acquired=false. The throttle is defensive — at worst a
// transient failure costs one delayed cron fire that the next tick
// re-attempts.
func acquireCronLock(st *store.Store, agentID string) (bool, string) {
	removeLegacyCronLock(agentID)
	return acquireCronLockDB(st, agentID, store.NowMillis())
}

func (cs *cronScheduler) runCronJob(agentID string) {
	// Check global pause
	if cs.mgr.CronPaused() {
		cs.logger.Debug("cron job skipped (globally paused)", "agent", agentID)
		return
	}

	// Check silent hours and read agent config
	var silentStart, silentEnd, cronMessage string
	var timeoutMinutes int
	if a, ok := cs.mgr.Get(agentID); ok {
		// Archived guard: a tick may have queued just before Archive ran
		// cron.Remove. Bail out before ever calling Chat (which would log a
		// warn for ErrAgentArchived).
		if a.Archived {
			cs.logger.Debug("cron job skipped (archived)", "agent", agentID)
			return
		}
		silentStart, silentEnd = a.SilentStart, a.SilentEnd
		timeoutMinutes = a.TimeoutMinutes
		cronMessage = a.CronMessage
		if IsInSilentHours(silentStart, silentEnd) {
			cs.logger.Debug("cron job skipped (silent hours)", "agent", agentID,
				"silentStart", silentStart, "silentEnd", silentEnd)
			return
		}
	}

	// Reset guard: skip BEFORE the throttle stamp. Chat() would
	// reject a tick that lands during ResetData with
	// ErrAgentResetting, but if we'd already stamped the kv row the
	// next legitimate tick (post-reset) would be artificially
	// throttled for cronMinInterval. Bail out early so the stamp
	// only fires for ticks that actually run.
	cs.mgr.busyMu.Lock()
	resetting := cs.mgr.resetting[agentID]
	cs.mgr.busyMu.Unlock()
	if resetting {
		cs.logger.Debug("cron job skipped (resetting)", "agent", agentID)
		return
	}

	// Per-host throttle: the kv row "scheduler/cron_last/<agentID>"
	// (scope=machine) records the previous fire's timestamp. Block
	// re-firing within cronMinInterval. The throttle catches
	// accidental same-host re-fires from a pathologically short
	// cron expression and any pre-cutover install whose state
	// survived an upgrade.
	//
	// Cross-peer firing is OUT OF SCOPE for this check — see
	// cron_lock_kv.go's "Race posture (cross-peer)" comment for
	// the DESIGN ASSUMPTION (agent_locks lease holder is the only
	// cron-firing peer) and the TODO that this scheduler does NOT
	// yet enforce that lease check before runCronJob fires.
	acquired, stampETag := acquireCronLock(cs.mgr.Store(), agentID)
	if !acquired {
		cs.logger.Debug("cron job skipped (lock held)", "agent", agentID)
		return
	}

	cs.logger.Info("cron job triggered", "agent", agentID)

	nextRun := cs.nextRun(agentID, silentStart, silentEnd)

	// Per-agent timeout (0 = use default)
	timeout := cronTimeout
	if timeoutMinutes > 0 {
		timeout = time.Duration(timeoutMinutes) * time.Minute
	}
	effectiveTimeoutMin := int(timeout / time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	events, err := cs.mgr.Chat(ctx, agentID, cronPrompt(nextRun, effectiveTimeoutMin, cronMessage), "system", nil, BusySourceCron)
	if err != nil {
		// Rollback the throttle stamp if Chat refused this tick for
		// a reason that a future tick should be free to retry. The
		// pre-stamp resetting/archived/silent-hours guards above
		// catch the common cases, but a tiny TOCTOU window remains
		// (ResetData / Archive can flip the state between the
		// guard read and the m.Chat busy gate). Without the
		// rollback, the next legitimate cron tick after that brief
		// window would observe the stamp from this aborted fire
		// and be artificially throttled for cronMinInterval.
		//
		// CAS-guarded delete (rollbackCronLockDB) so we only erase
		// our own stamp: if Chat() took longer than cronMinInterval
		// to fail and a parallel acquire already past-window-stamped
		// the row, that legitimate stamp is preserved.
		if errors.Is(err, ErrAgentResetting) || errors.Is(err, ErrAgentArchived) || errors.Is(err, ErrAgentBusy) {
			if delErr := rollbackCronLockDB(cs.mgr.Store(), agentID, stampETag); delErr != nil {
				cs.logger.Debug("cron throttle stamp rollback failed",
					"agent", agentID, "err", delErr)
			}
		}
		cs.logger.Warn("cron chat failed", "agent", agentID, "err", err)
		return
	}

	// Drain events (we don't stream cron results anywhere, just persist them)
	for range events {
	}

	if ctx.Err() == context.DeadlineExceeded {
		cs.logger.Warn("cron job timed out", "agent", agentID, "timeout", timeout)
	} else {
		cs.logger.Info("cron job completed", "agent", agentID)
	}
}
