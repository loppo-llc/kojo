-- 0011_peer_pending_join_secret.sql
--
-- Adds `peer_pending.join_secret_hash` so the /join-request POST
-- handler can atomically distinguish first contact from a repeat
-- attempt. Storing the hash (not the raw secret) keeps a Hub DB
-- read-only leak from yielding the per-join credential.
--
-- The handler MINTS a fresh raw secret on every POST and supplies
-- its sha256 via UpsertPeerPending. The ON CONFLICT clause in that
-- statement PRESERVES the existing hash, so the RETURNING projection
-- lets the handler check whether the just-supplied hash actually
-- landed (= fresh insert) or was discarded (= repeat). Only on a
-- fresh insert does the handler emit the raw secret in the response;
-- repeat callers must present the original secret in
-- Authorization: Bearer for the upsert to apply.

ALTER TABLE peer_pending ADD COLUMN join_secret_hash TEXT NOT NULL DEFAULT '';
