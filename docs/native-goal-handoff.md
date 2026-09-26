# Native Goal handoff

An active Codex Goal can request the existing self-migration API from its
current conversation:

```http
POST /api/v1/agents/{id}/handoff/switch
X-Kojo-Session-Key: <current conversation key>

{"target_peer_id":"<target>"}
```

Main WebUI uses an empty session key. Slack uses the exact current thread key.
GroupDM and other external surfaces are not supported by this queued path yet.
Ordinary, non-Goal moves retain the synchronous behavior.

## Acceptance is not completion

An active Goal receives HTTP 202 with `outcome: queued` and an `op_id`.
The agent must finish its current turn, not poll, sleep, repeat the move,
or continue unrelated work. The generated Codex switch skill explains this.

Kojo then:

1. Waits for the requesting native turn to finish.
2. Reads native Goal state. A completed/blocked/limited Goal cancels the move;
   it is never resurrected just to migrate.
3. Pauses native continuation, drains any competing next turn, reads the final
   state, and waits for normal app-server exit. Forced termination cannot make a
   valid checkpoint. The paused native database row supplies final accounting.
4. For Slack, waits for the original adapter's FIFO completion, including final
   delivery and local history persistence. This is a passive completion barrier,
   not a reused arrival capability. A missing/failed barrier aborts the move.
5. Quiesces other work, rechecks ownership and the operation identity, then
   invokes the existing sync/pull/lock-transfer/finalize flow. Missing required
   rollout, thread row, or native Goal row aborts before ownership transfer.
6. On the destination, verifies the operation and the origin's stop fence,
   then requests the exact Goal generation to resume. Slack retains its original
   Hub/thread; main WebUI follows the agent to the destination.

A resume admission is not a native ACK. `Move: resumed` is recorded only on the
native activation ACK. A successful resume also emits an operation-tagged notice
on the original response surface. There is **no ordinary main-WebUI arrival
fallback** for a queued Goal move.

All participants—source, destination, and any separate Slack Hub—must advertise
`X-Kojo-Goal-Handoff: v1`. Upgrade all of them before using this path.
Degraded state transfer is not allowed for a queued Goal move.

## Stop and failure behavior

- `!goal status` includes the portable move phase and operation ID.
- `!goal pause` cancels a pending move. Ordinary replies do not resume it.
- Slack `!stop` also fences a saved move when the source turn has already ended.
  The origin keeps a durable tombstone even if the holder cannot confirm stop.
- WebUI abort fences the main conversation's saved move as well.
- A stop cannot undo an ownership transfer already committed. It fences Goal
  resumption on the current holder; it does not move the agent back.
- While a move is pending, new work/replies are rejected rather than racing the
  checkpoint. Status and stop controls remain available, subject to the normal
  holder-switching admission window.
- If no checkpoint is reached within 15 minutes, the move fails and native work
  is asked to pause. Source-adapter settlement has a separate 30-second budget.
- The native objective, budget, accounting, and thread are transported; the Goal
  is not recreated with a new objective.

The source keeps a machine-local operation journal. Owners can inspect it even
after source has released the agent:

```http
GET /api/v1/goal-handoffs/{op_id}
```

`resume_requested` means the transfer/finalize returned successfully, **not**
proof that the destination's native resume completed. Inspect `!goal status`
on the current holder for the latter.

Interrupted external operations are **not replayed after restart**. For an
accepted Goal handoff whose resume command never reaches backend admission,
the destination may re-dispatch that exact identity up to three times after a
two-minute grace period. Each retry rechecks the origin stop fence; an
uncertain delivery that may already have been admitted remains blocked for
inspection rather than replayed. A lost origin completion barrier or unknown
transfer result likewise requires inspection. Never blindly repeat the switch
or force-reclaim. Check the current holder first; after confirming the data is
there, use
`!goal pause` to cancel any stale reservation, then `!goal resume` if desired.
A failed/uncertain operation never automatically rolls ownership back to source.
Post-checkpoint transport/finalize failures do not start an extra model turn
just to report failure. If no resume notice arrives, inspect the source journal
and the current holder's Goal status; the source journal retains the transport
error even when the destination cannot report it.

## Regression coverage

Deterministic tests cover:

- no pause before the requesting turn completes;
- a competing turn starting before the fresh get, before the pause ACK, or
  immediately after the ACK;
- completed/blocked Goals, pause failure, forced/unclean exit, and missing or
  still-active native database state;
- clean process exit and final native accounting using a fake app-server
  subprocess, plus once-only destination resume and stale-generation rejection;
- stop/duplicate-finalize fences and origin tombstones surviving cache loss;
- origin ownership, unsupported peer versions, source adapter completion/error,
  and refusal to replay a lost adapter barrier;
- no downgrade from Goal continuation to an ordinary arrival.

These tests do not replace an actual two-machine acceptance test after both
peers and the Slack Hub have been upgraded.
