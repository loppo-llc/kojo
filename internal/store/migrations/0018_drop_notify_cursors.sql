-- 0018_drop_notify_cursors.sql
--
-- The notify-source subsystem (Gmail) was removed in
-- feature/remove-gmail-notify; the Go-side code in
-- internal/notifysource/, internal/store/notify_cursors.go and the
-- internal/migrate/importers/notify_cursors.go importer are all gone.
-- Drop the now-orphaned table so existing v2 installs don't keep a
-- dead table (and its index) sitting in the schema.
--
-- 0001_initial.sql has been edited in lockstep so fresh installs
-- never create the table in the first place.

DROP INDEX IF EXISTS idx_notify_cursors_agent;
DROP TABLE IF EXISTS notify_cursors;
