-- 0002_agent_fencing_counters.sql
--
-- Per-agent monotonic counter that backs `agent_locks.fencing_token`.
-- Without this, releasing the lock (or bulk-releasing on peer
-- decommission) would let the next AcquireAgentLock reuse
-- fencing_token=1 — a delayed write from a prior holder with
-- fencing_token=1 would then be silently accepted by the new holder.
--
-- The counter is the "last issued token" for the agent. Acquire
-- increments and reads it back; Release does NOT touch the counter so
-- the next Acquire keeps advancing past every prior token, even
-- across an empty-row gap.
--
-- A `same-peer Acquire` refresh does NOT advance the counter (the
-- existing token stays valid). Only "first acquire" and "steal" paths
-- advance it. See internal/store/agent_locks.go for the state machine.
--
-- The table is FK'd to agents with ON DELETE CASCADE — a hard delete
-- of the agent drops both the active lock row (CASCADE on agent_locks)
-- and its counter; a soft-deleted agent still has its counter so a
-- restore (Phase 5+) can keep advancing.

CREATE TABLE agent_fencing_counters (
  agent_id   TEXT PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
  next_token INTEGER NOT NULL CHECK (next_token >= 0)
);

-- Backfill from any agent_locks rows that already exist on this DB
-- (a Phase 1-3 build that acquired locks before 0002 ran). The
-- counter must start at >= the prior token so a steal after this
-- migration cannot reuse a value the old holder already issued.
-- INSERT OR IGNORE keeps existing counter rows untouched on a re-run.
INSERT OR IGNORE INTO agent_fencing_counters (agent_id, next_token)
SELECT agent_id, fencing_token FROM agent_locks;
