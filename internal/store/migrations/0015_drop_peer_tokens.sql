-- 0015_drop_peer_tokens.sql
--
-- Drops the peer_tokens table. The Bearer-issuance flow it served
-- has been retired in favour of tsnet identity (docs/peer-tsnet-
-- identity.md); the table sits dead-weight from this migration
-- onwards. Cleanup is deferred to a single DROP TABLE — there are
-- no readers left in code.
--
-- Also drops the legacy peer/out_bearer and peer/pairing_bearer_stash
-- kv rows that the outbound-Bearer + pairing-stash plumbing used.
-- Both namespaces are abandoned after migration 0015; deleting the
-- rows keeps a future migration that re-uses the namespace from
-- accidentally inheriting stale entries.
DROP TABLE IF EXISTS peer_tokens;
DELETE FROM kv WHERE namespace IN ('peer/out_bearer', 'peer/pairing_bearer_stash', 'peer/discovery_join_secret');
