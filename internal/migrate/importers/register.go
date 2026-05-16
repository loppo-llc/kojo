package importers

import "github.com/loppo-llc/kojo/internal/migrate"

// importerOrder is the canonical registration sequence. init() ranges
// over it to call migrate.Register, and tests use the same slice so the
// order assertion can't be quietly bypassed by editing only init().
//
// Order rationale:
//   - agents must run first because every other domain FK's against
//     agents.id.
//   - messages and groupdms can run in either order after agents, but
//     groupdms depends on agents being present for member validation;
//     messages does not. Running messages before groupdms means a
//     groupdm-only crash leaves the messages domain in an "imported"
//     state that the operator can ignore on rerun.
func importerOrder() []migrate.Importer {
	return []migrate.Importer{
		agentsImporter{},
		messagesImporter{},
		groupdmsImporter{},
		// tasksImporter copies <v0>/agents/<id>/tasks.json into
		// agent_tasks. Runs after agents (FK on agent_id) and is order-
		// independent w.r.t. messages/groupdms — placed after the
		// transcript domains so a crash in tasks doesn't abandon
		// already-imported messages/groupdms in a weird intermediate
		// state on the next rerun.
		tasksImporter{},
		// sessionsImporter copies <v0>/sessions.json into the v1
		// sessions table, forcing every row to status='archived' per
		// design doc §5.5: the v0 PTY is gone the moment v0 stopped,
		// so a "running" status would lie to v1 callers about what's
		// actually live. peer_id is stamped from opts.HomePeer.
		// Order: doesn't matter w.r.t. agents/messages/groupdms (no
		// FK), placed after them so a sessions-only failure on a
		// fresh re-run doesn't abandon already-imported transcripts
		// in a partial state.
		sessionsImporter{},
		// notifyCursorsImporter copies <v0>/notify_cursors.json into
		// notify_cursors. The v0 cursor key is "<agentID>:<sourceID>"
		// and lacks the source type; the importer reads agents.json
		// to resolve each (agentID, sourceID) → type and composes the
		// canonical v1 id "<agent>:<type>:<source_id>". Cursors whose
		// source isn't declared in agents.json are orphan and warn-
		// skipped. Order: must run after agentsImporter has finished
		// (we re-read agents.json directly, so the dependency is on
		// the *file* not the v1 row, but placing it after agents in
		// the list keeps the "data-domain dependencies first" rule
		// uniform).
		notifyCursorsImporter{},
		// vapidImporter copies <v0>/vapid.json into kv (namespace=
		// "notify"). The public key lands plaintext (scope=global) and
		// the private key is envelope-sealed with the host-bound KEK
		// at <v1>/auth/kek.bin (see design doc §3.4)
		// (scope=machine, secret=true). Order: must run before
		// pushSubscriptionsImporter — push_subscriptions still reads
		// vapid.json directly today (its vapid_public_key column is
		// stamped from the file, not the kv row), but the dependency
		// will flip in a future slice that has push_subscriptions
		// resolve vapid_public from kv. Placing vapid first now keeps
		// the migration log ordering aligned with the notify domain's
		// logical layering: pair → subscribers.
		vapidImporter{},
		// pushSubscriptionsImporter copies <v0>/push_subscriptions.json
		// into push_subscriptions, filling vapid_public_key from
		// <v0>/vapid.json. Order: independent w.r.t. agents/messages/
		// groupdms/tasks/sessions/notify_cursors (no FK in either
		// direction — the schema deliberately omits agent_id from this
		// table, see 0001_initial.sql §3.3 exception). Placed after
		// notify_cursors so the notify-domain importers run as a group
		// in the migration log.
		pushSubscriptionsImporter{},
		// externalChatCursorsImporter walks each agent's
		// <v0>/agents/<id>/chat_history/<platform>/<channel>/<thread>.jsonl
		// and records the LastPlatformTS of each file as a cursor in
		// external_chat_cursors with id "<agent>:<source>:<channel>:
		// <thread>". _channel.jsonl is excluded — v0's channel fetch is
		// a sliding-window overwrite that doesn't drive a delta cursor,
		// so importing its last ts would invite a future v1 channel
		// poll to mistake it for a delta starting point and silently
		// drop messages. The chat_history JSONL bodies themselves are
		// NOT imported (re-fetch on first poll covers that — design
		// doc §5.5 marks chat_history body as "再取得対象、import
		// しない"). Order: depends on agents being valid (the importer
		// reads agents.json directly to filter out orphan agent dirs)
		// and is otherwise independent of every other domain. Placed
		// after the notify-domain importers and before blobs so the
		// migration log groups all "external integration" state
		// (notify cursors, push subs, vapid pair, chat cursors)
		// together before binary-artefact publication starts.
		externalChatCursorsImporter{},
		// compactionsImporter is a no-op marker: v0 reserves
		// <v0>/compactions/ in the layout but has no writer for it
		// (v0's historical disk-based MEMORY.md compaction — since
		// removed from the codebase — consolidated MEMORY.md in
		// place and never emitted per-range archives that would map
		// onto the v1 compactions schema). The
		// importer hashes whatever happens to live under the dir for
		// drift detection, warns on unexpected leaves, and stamps
		// migration_status.imported_count=0 so the migration log is
		// complete (every domain in design doc §5.5 appears). Order:
		// FK targets agents (ON DELETE CASCADE), so this must run after
		// agentsImporter — honoured today even though no rows are
		// emitted, in case a future v0 build starts populating the dir
		// and the importer needs to start producing real rows.
		compactionsImporter{},
		// blobsImporter publishes per-agent binary artefacts (avatar /
		// books / outbox / temp / index / credentials) into the v1
		// native blob store. Runs after the agent-row importers because
		// blob_refs URIs are namespaced by agent id; an orphan blob
		// pointing at a non-existent agent is harmless (FK is not
		// enforced — see schema rationale in 2.4 / 4.2) but the
		// natural order keeps the migration log readable.
		blobsImporter{},
	}
}

// init is the *only* place that calls migrate.Register so the contract
// is reviewable at a glance. Per-file init() registration would silently
// reorder when files are added/renamed because Go init order across
// files in a package is governed by source-file name — making
// registration order an emergent property of the directory listing
// rather than a documented contract.
func init() {
	for _, imp := range importerOrder() {
		migrate.Register(imp)
	}
}
