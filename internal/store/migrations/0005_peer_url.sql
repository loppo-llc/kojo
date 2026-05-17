-- 0005_peer_url.sql
--
-- Split peer_registry.name into two columns:
--   * url  — the dial address (was `name`). The registrar stamps this
--            from tsnet's FQDN / Tailscale IPv4 once known.
--   * name — a human-readable device label (OS hostname by default).
--            Operator-overridable from the UI; agents reference peers
--            by name, not by URL.
--
-- Backfill: for existing rows the URL string is the only identifier
-- we have, so copy it into name as well. Operators rename later.
--
-- SQLite ≥3.25 supports ALTER TABLE RENAME COLUMN. modernc.org/sqlite
-- pins a newer build so the syntax is safe.
ALTER TABLE peer_registry RENAME COLUMN name TO url;

-- ADD COLUMN with DEFAULT '' so existing rows survive the NOT NULL
-- gate; the UPDATE below backfills every row to url so the human
-- listing isn't immediately empty.
ALTER TABLE peer_registry ADD COLUMN name TEXT NOT NULL DEFAULT '';
UPDATE peer_registry SET name = url WHERE name = '';
