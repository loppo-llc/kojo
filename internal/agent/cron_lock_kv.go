package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// cron throttle is now backed by the kv table per design doc §2.3
// (table row "cron throttle (.cron_last)" → kv).
//
//	namespace = "scheduler"
//	key       = "cron_last/<agentID>"
//	scope     = "machine" — the throttle is a per-host concern. Two peers
//	            taking turns as the agent_locks lease holder each maintain
//	            their own last-fire timestamp; replicating the row globally
//	            would let a recent fire on peer A mute the next legitimate
//	            tick on peer B (which is the new owner and SHOULD fire).
//	type      = "string"
//	value     = millis-since-epoch of the previous fire
//	secret    = false (operator metadata, not a credential)
//
// In v0 the throttle was a `<agentDir>/.cron_last` file whose mtime
// recorded the last fire; lock acquisition used O_CREATE|O_EXCL with
// stale-reclaim past cronMinInterval. The kv mapping preserves the
// 50s no-refire window without a per-agent on-disk file — operators
// can purge throttle state by dropping the namespace via direct SQL,
// and the migration importer no longer has to special-case the
// dotfile. `kojo --clean legacy` (slice 19) sweeps surviving
// `<agentDir>/.cron_last` files ONLY when the matching kv row
// exists and validates; a file with no kv mirror waits for the next
// runtime acquireCronLock to unlink it (lazy cleanup — the legacy
// file's value is NOT migrated into the kv row, only unlinked).
const (
	cronLockKVNamespace   = "scheduler"
	cronLockKVKeyPrefix   = "cron_last/"
	cronLockKVTimeout     = 2 * time.Second
)

// cronLockKVKey returns the kv key used for agentID's throttle row.
// Centralised so a typo in one call site can't quietly diverge from the
// other.
func cronLockKVKey(agentID string) string {
	return cronLockKVKeyPrefix + agentID
}

// cronLockKVCASTestHook is a test-only injection point fired inside
// acquireCronLockDB AFTER the GetKV result has been examined (etag
// captured for the past-window IfMatchETag branch, OR ErrNotFound
// observed for the no-row IfMatchAny branch) but BEFORE the gated
// PutKV runs. Tests use it to land a colliding write so the
// CAS-mismatch branch is exercised end-to-end on either entry path
// without a goroutine race. Production keeps it nil — the if-guard
// is a single nil-pointer compare in a non-hot path (cron ticks fire
// at minute granularity at the busiest).
var cronLockKVCASTestHook func()

// acquireCronLockDB throttles per-agent cron firings via a kv row.
// Returns true iff at least cronMinInterval has elapsed since the
// previous fire recorded for this agent on this host, in which case
// the row is CAS-stamped with `now` and the caller should run the
// job.
//
// Race posture (same-store): the read-modify-write is gated by an
// If-Match etag (or IfMatchAny when no row exists yet). Two acquires
// inside the same kojo.db that both observe the same row (or both
// observe no row) lose to each other under store.PutKV's per-row
// optimistic locking — only one PutKV commits, the loser sees
// ErrETagMismatch and is treated as throttled. This catches an
// in-process race where two cron schedulers (test scaffolding, a
// future multi-tenant runtime) hit the same key, and a same-host
// race against a future helper that touches the throttle row out
// of band.
//
// Race posture (cross-peer): kv rows in this namespace are
// scope=machine — peer A and peer B do NOT replicate the row to
// each other, so the CAS does NOT serialise across peers. That
// dedup is a DESIGN ASSUMPTION delegated to agent_locks
// (§3.5–3.7): a single peer holds the lease per agent, only the
// lease holder is supposed to schedule cron ticks, and the
// fencing token rejects writes from a stale ex-holder.
//
// IMPORTANT: as of slice 12 the cron scheduler does NOT yet
// consult agent_locks before firing. Every peer with a copy of
// the agent runs cron schedules independently. In practice
// today's deployment is single-peer-per-agent (the multi-device
// runtime hasn't shipped), so the assumption holds; the gap is
// flagged here so a future slice that turns on multi-peer
// failover knows to add a "is this peer the lease holder?"
// check inside cs.runCronJob (or short-circuit cs.Schedule) and
// pair the cron-fire write with a CheckFencingTx.
//
// Failure posture: any unexpected kv error (read or write) returns
// false ("don't fire"). The throttle is defensive — at worst a
// transient DB hiccup costs one delayed cron fire that the next tick
// re-attempts. A failed PutKV after a successful read leaves the row
// untouched, so a follow-up acquire on the next tick still sees the
// pre-existing window.
//
// Malformed-row posture: a row with the wrong type/scope/secret (a
// peer-replicated junk write, manual edit) or an unparseable value
// is treated as "throttled" — the same fail-closed bias as
// cron_paused. The operator sees nothing fire and can repair the
// row; firing on garbage state would amount to ignoring the
// throttle. acquireCronLockDB short-circuits on the malformed row
// without writing, so the row will NOT be replaced by re-acquires
// alone — the operator must manually delete or repair it (direct
// SQL DELETE / UPDATE) before the next acquire can advance the
// stamp.
//
// `now` is millis-since-epoch; the runtime caller passes
// store.NowMillis(). Tests inject deterministic values via this
// parameter so the throttle window is observable without sleeping.
//
// Returns (acquired, stampETag). When acquired==true, stampETag is
// the etag of the row this call wrote — callers that need to roll
// back (runCronJob's rejected-tick branch) pass it to
// rollbackCronLockDB so only their own stamp is removed. When
// acquired==false, stampETag is empty.
func acquireCronLockDB(st *store.Store, agentID string, now int64) (bool, string) {
	if st == nil {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), cronLockKVTimeout)
	defer cancel()

	rec, err := st.GetKV(ctx, cronLockKVNamespace, cronLockKVKey(agentID))
	var ifMatch string
	switch {
	case err == nil:
		// Row shape gate. A row whose type/scope/secret disagrees
		// with the design contract (or whose value isn't a parseable
		// integer millis) is treated as "throttled" — fail closed
		// rather than silently re-fire on garbage state.
		if rec.Type != store.KVTypeString || rec.Scope != store.KVScopeMachine || rec.Secret {
			return false, ""
		}
		last, perr := strconv.ParseInt(rec.Value, 10, 64)
		if perr != nil {
			return false, ""
		}
		if now-last < cronMinInterval.Milliseconds() {
			return false, ""
		}
		// Past-window: CAS against the etag we just read so a
		// concurrent acquire that already advanced the row to a
		// fresh `now` causes our PutKV to lose with
		// ErrETagMismatch.
		ifMatch = rec.ETag
	case errors.Is(err, store.ErrNotFound):
		// First fire for this agent on this host (or post-clean
		// state). Use IfMatchAny so a peer that inserts a row
		// between our GetKV(miss) and PutKV(insert) wins; we
		// observe ErrETagMismatch and fall back to "throttled".
		ifMatch = store.IfMatchAny
	default:
		return false, ""
	}

	// Test-only hook: fires after the GetKV result has been
	// classified (etag captured for the past-window IfMatchETag
	// branch above; ErrNotFound observed for the no-row IfMatchAny
	// branch) but before the gated PutKV runs. Tests use it to
	// slip a colliding write into the row so the CAS fails
	// deterministically on either entry path. Production keeps
	// this nil.
	if cronLockKVCASTestHook != nil {
		cronLockKVCASTestHook()
	}

	upd := &store.KVRecord{
		Namespace: cronLockKVNamespace,
		Key:       cronLockKVKey(agentID),
		Value:     strconv.FormatInt(now, 10),
		Type:      store.KVTypeString,
		Scope:     store.KVScopeMachine,
	}
	written, err := st.PutKV(ctx, upd, store.KVPutOptions{IfMatchETag: ifMatch})
	if err != nil {
		// ErrETagMismatch = a concurrent acquire won the race; we
		// must NOT also fire. Other errors = transient DB issue;
		// fail-closed by the same logic as the read-side errors.
		return false, ""
	}
	return true, written.ETag
}

// rollbackCronLockDB removes the throttle row IFF its etag still
// matches stampETag — i.e. only when no other acquirer has
// over-written our stamp. Used by runCronJob when Chat() refused
// the tick for a reason (resetting/archived/busy) that a later
// tick should be free to retry: without rollback the next
// legitimate tick would be artificially throttled by our aborted
// fire's stamp.
//
// The CAS guard is critical. If acquireCronLockDB returns true at
// T0 and Chat() takes longer than cronMinInterval to fail (e.g.
// the agent transitions through reset → unreset → busy across
// several seconds), a parallel acquire at T1 > T0+50s could have
// observed the row past-window and re-stamped it with its own
// etag. An unguarded DELETE here would erase that legitimate
// stamp; the CAS-guarded delete sees ErrETagMismatch and exits
// cleanly. ErrNotFound (row already gone) is also folded into
// success — the post-condition is "our stamp is gone", which is
// already true.
func rollbackCronLockDB(st *store.Store, agentID, stampETag string) error {
	if st == nil || stampETag == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cronLockKVTimeout)
	defer cancel()
	if err := st.DeleteKV(ctx, cronLockKVNamespace, cronLockKVKey(agentID), stampETag); err != nil {
		// Both branches are non-errors from the rollback's point of
		// view: "row gone" (ErrNotFound) and "someone else owns the
		// row now" (ErrETagMismatch) both mean "our stamp is no
		// longer authoritative", which is the post-condition we
		// wanted.
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrETagMismatch) {
			return nil
		}
		return err
	}
	return nil
}

// deleteCronLockDB removes the kv throttle row for an agent. Used
// by the reset path (manager_lifecycle.ResetData) so a re-created
// agent inheriting the same id isn't artificially throttled by a
// stale row from the prior incarnation. runCronJob does NOT call
// this for its rejected-tick rollback — that path uses
// rollbackCronLockDB which CAS-guards on the caller's stamp etag
// to avoid erasing a foreign acquirer's stamp. Archive itself does
// NOT call this either — the kv row is per-host throttle state,
// not per-agent canonical state, so leaving it through an
// archive/unarchive cycle is harmless (50s window has long since
// elapsed by the time anyone unarchives).
//
// ErrNotFound is folded into success — the post-condition is "row
// absent", which is already true. Other errors propagate so the
// caller can log / surface them.
func deleteCronLockDB(st *store.Store, agentID string) error {
	if st == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cronLockKVTimeout)
	defer cancel()
	if err := st.DeleteKV(ctx, cronLockKVNamespace, cronLockKVKey(agentID), ""); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// cronLockLegacyPath returns the v0 / pre-cutover path of the
// per-agent cron lock file. After this slice the file is no longer
// written, but a v0 → v1 install or a v1 install rolled back through
// v0 may still have one on disk; callers best-effort unlink it so the
// agent dir is cleaned up over time.
func cronLockLegacyPath(agentID string) string {
	return filepath.Join(agentDir(agentID), cronLockFile)
}

// removeLegacyCronLock best-effort unlinks the legacy lock file.
// Failure is silent — kv is authoritative; a stray file does no harm
// other than wasting an inode until either the next runtime
// acquireCronLock unlinks it (lazy cleanup; the file's value is not
// migrated into kv) or `kojo --clean legacy` (slice 19) sweeps it
// once a valid kv row exists for that agent.
//
// Upgrade-window note: we deliberately do NOT seed the legacy file's
// mtime into the kv row. In a v0 → v1 upgrade where v0 fired a cron
// less than cronMinInterval before kojo restarted, the kv row starts
// empty and v1 will fire on its next tick — potentially within the
// 50s window. Acceptable because:
//
//   - The window is bounded by the upgrade event itself (a one-shot
//     cost, not steady-state).
//   - Production cron expressions have minute-level granularity, so
//     "v0 fired right before restart" + "v1 fires right after
//     restart" is at most one extra Chat per agent in the upgrade
//     transient.
//   - Mirroring the mtime would require us to trust the file's
//     timestamp across a clock skew or a manual `touch` on the
//     dotfile, neither of which is worth the migration complexity
//     for a 50s defensive throttle.
func removeLegacyCronLock(agentID string) {
	_ = os.Remove(cronLockLegacyPath(agentID))
}
