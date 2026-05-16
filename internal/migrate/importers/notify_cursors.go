package importers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// notifyCursorsImporter walks <v0>/notify_cursors.json and inserts the
// rows into the v1 notify_cursors table. Domain key: "notify_cursors".
//
// v0 schema (from internal/agent/notify_poller.go):
//
//	notify_cursors.json: map[string]string  -- "<agentID>:<sourceID>" → cursor
//
// v1 schema (0001_initial.sql): id is a composite source identifier of the
// form "<agentID>:<source_type>:<source_id>" — the v0 key is missing the
// source_type segment because v0 stores the type alongside the source
// config in agents.json (NotifySources[].Type), not in the cursor key.
//
// The importer therefore reads agents.json *as well as* notify_cursors.json
// to look up each (agentID, sourceID)'s declared type, then composes the
// canonical v1 id. A cursor whose (agentID, sourceID) doesn't appear in
// any agent's NotifySources is an orphan (the source was deleted but the
// cursor wasn't) — orphans are warn-skipped rather than imported, because
// without the type we can't compose a canonical id and a re-imported v1
// runtime would surface them under the wrong source plugin.
//
// peer_id is stamped from opts.HomePeer because notify_cursors are
// global-scoped (other peers must see the same cursor to avoid
// re-delivering the same notification on device switch — see design doc
// §2.3 line 106), but the row records which peer last advanced the
// cursor for audit / debug.
type notifyCursorsImporter struct{}

func (notifyCursorsImporter) Domain() string { return "notify_cursors" }

func (notifyCursorsImporter) Run(ctx context.Context, st *store.Store, opts migrate.Options) error {
	if done, err := alreadyImported(ctx, st, "notify_cursors"); err != nil {
		return err
	} else if done {
		return nil
	}

	logger := slog.Default().With("importer", "notify_cursors")

	srcPaths, err := collectNotifyCursorsSourcePaths(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("collect source paths: %w", err)
	}
	checksum, err := domainChecksum(opts.V0Dir, srcPaths)
	if err != nil {
		return fmt.Errorf("checksum notify_cursors sources: %w", err)
	}

	cursorPath := filepath.Join(opts.V0Dir, "notify_cursors.json")
	data, err := readV0(opts.V0Dir, cursorPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return markImported(ctx, st, "notify_cursors", 0, checksum)
		}
		return err
	}
	if len(data) == 0 {
		return markImported(ctx, st, "notify_cursors", 0, checksum)
	}

	var cursors map[string]string
	if err := json.Unmarshal(data, &cursors); err != nil {
		// Malformed file: log and mark imported with 0 rows so a re-run
		// doesn't repeatedly fail on the same parse. Matches the posture
		// in sessions / tasks importers.
		logger.Warn("notify_cursors: skipping malformed file",
			"path", cursorPath, "err", err)
		return markImported(ctx, st, "notify_cursors", 0, checksum)
	}
	if len(cursors) == 0 {
		return markImported(ctx, st, "notify_cursors", 0, checksum)
	}

	// Build (agentID, sourceID) → sourceType lookup from agents.json. Read
	// directly from disk rather than the v1 agents table because:
	//   1. self-contained: no dependency on importer ordering for
	//      correctness (importerOrder() runs agents first, but the
	//      lookup logic is identical regardless).
	//   2. raw-json access: settings_json round-trips notifySources but
	//      re-extracting them costs a JSON unmarshal per agent anyway.
	//   3. read-once: agents.json is parsed exactly once for this domain.
	//
	// Agents whose id/name agentsImporter would have skipped (missing
	// either field) are also excluded here — keeping the same skip rule
	// in both importers prevents notify_cursors from inserting rows that
	// reference an agent the v1 store doesn't actually have. agent_id
	// is nullable in the schema (no FK), so the DB wouldn't reject such
	// a row, but it would be a logical orphan after migration.
	//
	// Missing agents.json (os.ErrNotExist) is tolerated and returns an
	// empty lookup; malformed JSON is fatal because notify_cursors.json
	// having data while agents.json fails to parse means we'd silently
	// drop every cursor as orphan and stamp imported(0). agentsImporter
	// runs ahead of us in importerOrder() and would already have failed
	// on the same file, so reaching this code with a malformed agents.json
	// shouldn't happen in practice — we surface it loudly anyway.
	sourceTypes, err := loadNotifySourceTypes(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("load notify source types: %w", err)
	}

	mtime := fileMTimeMillis(cursorPath)
	recs := make([]*store.NotifyCursorRecord, 0, len(cursors))
	skipped := 0
	for key, cursor := range cursors {
		if cursor == "" {
			// Empty cursor = "haven't polled yet". v0's poller writes
			// these on first attach; importing them is no-op churn that
			// would still be "haven't polled" after migration.
			continue
		}
		agentID, sourceID, ok := splitCursorKey(key)
		if !ok {
			logger.Warn("notify_cursors: skipping malformed key",
				"key", key)
			skipped++
			continue
		}
		sourceType, ok := sourceTypes[lookupKey(agentID, sourceID)]
		if !ok {
			// Orphan: source was deleted from agents.json but the cursor
			// wasn't cleaned up. Drop it — re-importing under an unknown
			// type is not safe.
			logger.Warn("notify_cursors: skipping orphan cursor (source not in agents.json)",
				"agent_id", agentID, "source_id", sourceID)
			skipped++
			continue
		}
		// Compose canonical v1 id. The schema example "agent:slack:Cxxx"
		// uses ":" as the segment separator, so re-use it. The composition
		// is lossless ONLY if no segment itself contains ':', otherwise
		// "ag_x:slack:foo:bar" and "ag_x:slack:foo:bar" could be produced
		// from two different (sourceType, sourceID) splits — and
		// ON CONFLICT DO NOTHING + map iteration order would make the
		// surviving row non-deterministic.
		//
		// agentID is colon-free by construction: splitCursorKey() splits
		// on the FIRST ':', and loadNotifySourceTypes refuses agents.json
		// ids that contain ':'. We re-check sourceType / sourceID here
		// because they're either kojo-controlled (sourceType: gmail /
		// slack / discord) or opaque external ids (sourceID: Slack
		// channel ids, Gmail labels). Today none contain ':', but
		// failing closed if one ever does keeps the v1 composite
		// unambiguous.
		if strings.ContainsRune(sourceType, ':') ||
			strings.ContainsRune(sourceID, ':') {
			logger.Warn("notify_cursors: skipping cursor with ':' in id segment (would collide on v1 composite id)",
				"agent_id", agentID, "source_type", sourceType, "source_id", sourceID)
			skipped++
			continue
		}
		v1ID := agentID + ":" + sourceType + ":" + sourceID

		agentIDLocal := agentID
		recs = append(recs, &store.NotifyCursorRecord{
			ID:        v1ID,
			Source:    sourceType,
			AgentID:   &agentIDLocal,
			Cursor:    cursor,
			CreatedAt: mtime,
			UpdatedAt: mtime,
		})
	}

	if len(recs) == 0 {
		if skipped > 0 {
			logger.Info("notify_cursors: all rows skipped (orphan / malformed)",
				"skipped", skipped)
		}
		return markImported(ctx, st, "notify_cursors", 0, checksum)
	}

	n, err := st.BulkInsertNotifyCursors(ctx, recs, store.NotifyCursorInsertOptions{PeerID: opts.HomePeer})
	if err != nil {
		return fmt.Errorf("bulk insert notify_cursors: %w", err)
	}
	if skipped > 0 {
		logger.Info("notify_cursors: import complete with skips",
			"inserted", n, "skipped", skipped)
	}
	return markImported(ctx, st, "notify_cursors", n, checksum)
}

// splitCursorKey decodes v0's "<agentID>:<sourceID>" cursor map key. The
// split is on the FIRST ':' so a sourceID that happens to contain ':'
// stays intact — agentIDs never contain ':' (kojo allocates them as
// random hex-ish ids), so first-':' is unambiguous.
func splitCursorKey(key string) (agentID, sourceID string, ok bool) {
	i := strings.IndexByte(key, ':')
	if i <= 0 || i == len(key)-1 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}

// lookupKey is the internal map key for sourceTypes — distinct from the
// v0 cursor key only in that it's never serialized, so the chosen
// separator is irrelevant.
func lookupKey(agentID, sourceID string) string {
	return agentID + "\x00" + sourceID
}

// loadNotifySourceTypes parses v0/agents.json and returns a map of
// (agentID, sourceID) → sourceType. Each agent's notifySources array
// (one of v0's settings_json keys) declares the type for each source
// it polls.
//
// Skip rules:
//   - missing agents.json (os.ErrNotExist) → empty map (no cursors will
//     resolve, importer's main loop downgrades them to orphan)
//   - empty / malformed JSON → hard error. We can't tell "no agents
//     declared notifySources" from "agents.json corrupted/truncated"
//     with an empty map, and orphaning every cursor in those cases
//     would silently lose the notification cadence on migration.
//     agentsImporter runs ahead of us and would already have failed on
//     the same file in practice.
//   - empty agentID/name (matches agentsImporter's skip rule, since v1
//     never inserts those rows) → skip the whole agent
//   - colon-bearing agentID → skip the whole agent. splitCursorKey
//     splits on the FIRST ':' so it can never return an agentID that
//     contains ':'; an agents.json id with ':' could therefore never be
//     matched against a v0 cursor key, and emitting a row with such an
//     id into v1 would also collide on the "<agent>:<type>:<source>"
//     v1 composite. Refuse loudly rather than producing a silently
//     unreachable lookup entry.
//   - empty sourceID/sourceType inside a notifySources entry → skip
//     just that entry
func loadNotifySourceTypes(v0Dir string) (map[string]string, error) {
	path := filepath.Join(agentsBaseDir(v0Dir), "agents.json")
	data, err := readV0(v0Dir, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		// A zero-byte agents.json on a disk that *has* the file is a
		// truncation signal (crash mid-write, partial copy, etc.) — not
		// a v0 contract. Refuse rather than orphan every cursor.
		return nil, fmt.Errorf("agents.json is empty")
	}
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("agents.json malformed: %w", err)
	}

	out := make(map[string]string)
	for _, ag := range raw {
		agentID, _ := ag["id"].(string)
		name, _ := ag["name"].(string)
		// Mirror agentsImporter's skip rule (agents.go:108): a v0 row
		// with no id/name is dropped, so cursors pointing at it would
		// be logical orphans even though agent_id is nullable in the
		// notify_cursors schema.
		if agentID == "" || name == "" {
			continue
		}
		// splitCursorKey returns agentIDs that never contain ':'; an
		// agents.json id with ':' is therefore unreachable through the
		// cursor lookup path. Skip rather than create a phantom entry
		// that will never be matched.
		if strings.ContainsRune(agentID, ':') {
			continue
		}
		sources, ok := ag["notifySources"].([]any)
		if !ok {
			continue
		}
		for _, s := range sources {
			sm, ok := s.(map[string]any)
			if !ok {
				continue
			}
			sourceID, _ := sm["id"].(string)
			sourceType, _ := sm["type"].(string)
			if sourceID == "" || sourceType == "" {
				continue
			}
			out[lookupKey(agentID, sourceID)] = sourceType
		}
	}
	return out, nil
}

// collectNotifyCursorsSourcePaths returns the v0 files this importer
// hashes for source_checksum. Includes notify_cursors.json (the actual
// data) AND agents.json (the type lookup): a change in either file can
// alter what gets imported, so both must be in the checksum to give
// operators a meaningful drift signal.
func collectNotifyCursorsSourcePaths(v0Dir string) ([]string, error) {
	var paths []string
	for _, p := range []string{
		filepath.Join(v0Dir, "notify_cursors.json"),
		filepath.Join(agentsBaseDir(v0Dir), "agents.json"),
	} {
		updated, err := addLeafIfRegular(v0Dir, p, paths)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		paths = updated
	}
	return paths, nil
}
