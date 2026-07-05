-- Backfill groupdms.updated_at from the latest message so the room list's
-- "last active" time reflects real activity. Rooms created before
-- TouchGroupDM existed froze at creation time: UpdateGroupDM short-circuits
-- settings no-ops, so message posts never advanced updated_at. New posts
-- bump it going forward; this migration repairs the historical rows.
-- ETag is left as-is: it is compared as an opaque value and recomputed on
-- the next real write.
UPDATE groupdms
SET updated_at = (
  SELECT MAX(m.created_at)
  FROM groupdm_messages m
  WHERE m.groupdm_id = groupdms.id
)
WHERE deleted_at IS NULL
  AND (
    SELECT MAX(m.created_at)
    FROM groupdm_messages m
    WHERE m.groupdm_id = groupdms.id
  ) > updated_at;
