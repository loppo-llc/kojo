package importers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// agentsImporter migrates v0's agents.json plus per-agent persona.md,
// MEMORY.md, and memory/**/*.md files into the v1 store. Domain key:
// "agents". Runs first because everything else FK's against agents.
type agentsImporter struct{}

func (agentsImporter) Domain() string { return "agents" }

// settingsKeysToStrip lists every key in agents.json that does NOT belong
// in AgentRecord.Settings:
//   - id / name / createdAt / updatedAt are AgentRecord columns.
//   - persona is the inline copy of agent_persona.body and is migrated
//     separately by importAgentPersona.
//   - cronExpr / activeStart / activeEnd are legacy v0 fields. v0's
//     store.Load() rewrites them into the canonical fields the next time
//     v0 boots — by which point --migrate cannot run because v0 still
//     holds the lock. The importer therefore re-runs the same translation
//     in memory before stripping them so a v0 binary that was killed
//     mid-startup (rare but possible) doesn't lose its schedule on the
//     way into v1.
var settingsKeysToStrip = map[string]struct{}{
	"id":          {},
	"name":        {},
	"createdAt":   {},
	"updatedAt":   {},
	"persona":     {},
	"cronExpr":    {},
	"activeStart": {},
	"activeEnd":   {},
	// lastMessage is derived (recomputed from agent_messages on every
	// list render in v0). Persisting the v0 snapshot would surface a
	// stale preview after migration until the first new message — and
	// would couple Settings to a concept the v1 model already covers
	// via LatestMessage().
	"lastMessage": {},
}

func (agentsImporter) Run(ctx context.Context, st *store.Store, opts migrate.Options) error {
	if done, err := alreadyImported(ctx, st, "agents"); err != nil {
		return err
	} else if done {
		return nil
	}

	logger := slog.Default().With("importer", "agents")

	// Collect scanned source paths up front. The checksum records the
	// set of v0 files this domain WALKED, not the subset it actually
	// imported (orphan agent dirs missing from agents.json show up here
	// even though importOneAgent skips them — see the scan-vs-import
	// gap note in collectAgentsSourcePaths). This is intentional: the
	// orchestrator's pre/post manifest comparison rejects post-import
	// drift, and the per-domain checksum gives operators a finer-
	// grained audit trail when something does change.
	srcPaths, err := collectAgentsSourcePaths(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("collect source paths: %w", err)
	}
	checksum, err := domainChecksum(opts.V0Dir, srcPaths)
	if err != nil {
		return fmt.Errorf("checksum agents sources: %w", err)
	}

	manifest, err := readV0(opts.V0Dir, filepath.Join(agentsBaseDir(opts.V0Dir), "agents.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No agents to migrate — perfectly valid for a fresh v0 dir.
			return markImported(ctx, st, "agents", 0, checksum)
		}
		return fmt.Errorf("read agents.json: %w", err)
	}

	// Decode as []map[string]any so unknown / forward-introduced keys
	// round-trip unchanged into Settings, and zero-valued numeric/bool
	// fields are preserved (a typed struct with omitempty would silently
	// drop intervalMinutes:0, which v0 uses as the "schedule disabled"
	// sentinel — losing the sentinel would resurrect the default 30-min
	// cadence after migration).
	var raw []map[string]any
	if err := json.Unmarshal(manifest, &raw); err != nil {
		return fmt.Errorf("decode agents.json: %w", err)
	}

	count := 0
	for i := range raw {
		agentJSON := raw[i]
		id, _ := agentJSON["id"].(string)
		name, _ := agentJSON["name"].(string)
		if id == "" || name == "" {
			logger.Warn("skipping agent: missing id or name", "id", id)
			continue
		}
		if err := importOneAgent(ctx, st, opts.V0Dir, agentJSON, logger); err != nil {
			return fmt.Errorf("agent %s: %w", id, err)
		}
		count++
	}

	return markImported(ctx, st, "agents", count, checksum)
}

// importOneAgent inserts the agent row, then per-agent persona.md,
// MEMORY.md, and memory/* entries. Each step is independently idempotent
// so a crash mid-loop converges on the same final state on retry.
func importOneAgent(ctx context.Context, st *store.Store, v0Dir string, raw map[string]any, logger *slog.Logger) error {
	id, _ := raw["id"].(string)
	name, _ := raw["name"].(string)
	createdAt, _ := raw["createdAt"].(string)
	updatedAt, _ := raw["updatedAt"].(string)

	// Skip the row insert if it already happened — but always continue
	// to the per-agent files so a crash that landed between InsertAgent
	// and the persona/memory writes still converges on retry.
	if _, err := st.GetAgent(ctx, id); err == nil {
		// already inserted; fall through
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	} else {
		settings := buildSettings(raw, logger)
		created := parseRFC3339Millis(createdAt)
		updated := parseRFC3339Millis(updatedAt)
		if created == 0 {
			created = store.NowMillis()
		}
		if updated == 0 {
			updated = created
		}
		if _, err := st.InsertAgent(ctx, &store.AgentRecord{
			ID:       id,
			Name:     name,
			Settings: settings,
		}, store.AgentInsertOptions{
			CreatedAt: created,
			UpdatedAt: updated,
		}); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
	}

	// persona.md
	if err := importAgentPersona(ctx, st, v0Dir, id, raw); err != nil {
		return fmt.Errorf("persona: %w", err)
	}
	// MEMORY.md
	if err := importAgentMemoryBlob(ctx, st, v0Dir, id, raw); err != nil {
		return fmt.Errorf("memory blob: %w", err)
	}
	// memory/**/*.md
	if err := importMemoryEntries(ctx, st, v0Dir, id, updatedAt, logger); err != nil {
		return fmt.Errorf("memory entries: %w", err)
	}
	return nil
}

// buildSettings copies every non-stripped key from the v0 JSON into the
// settings map and folds legacy fields into their canonical successors.
//
// CAVEAT — workDir: v0 stores a machine-local absolute path here. In v1
// the same value belongs in workspace_paths (per-peer), not Settings
// (global). Phase 2c-2 will read settings.workDir as a fallback when
// workspace_paths has no row for the current peer; once peer_id
// allocation lands in Phase 4, Settings becomes the legacy fallback
// and the row in workspace_paths becomes authoritative.
func buildSettings(raw map[string]any, logger *slog.Logger) map[string]any {
	settings := make(map[string]any, len(raw))
	for k, v := range raw {
		if _, strip := settingsKeysToStrip[k]; strip {
			continue
		}
		settings[k] = v
	}

	// Legacy: cronExpr → intervalMinutes. v0's parseLegacyCron triggers
	// when LegacyCronExpr != "" AND IntervalMinutes == 0, so we must
	// match both shapes:
	//   - intervalMinutes key absent (older v0 schema, never written)
	//   - intervalMinutes key present but holding 0 (sentinel for
	//     "disabled"; if cronExpr is also present, v0 hasn't yet
	//     migrated this row in store.Load).
	if needsCronExprFallback(settings) {
		if expr, ok := raw["cronExpr"].(string); ok && expr != "" {
			if iv := parseLegacyCronExpr(expr, logger); iv != 0 {
				settings["intervalMinutes"] = iv
			}
		}
	}

	// Legacy: activeStart/activeEnd → silentStart/silentEnd. Active
	// hours invert to silent hours: silentStart = old activeEnd,
	// silentEnd = old activeStart. Existing agents get
	// notifyDuringSilent=true for backward compat (matches v0's
	// store.Load).
	if _, ok := settings["silentStart"]; !ok {
		if as, _ := raw["activeStart"].(string); as != "" {
			if ae, _ := raw["activeEnd"].(string); ae != "" {
				settings["silentStart"] = ae
				settings["silentEnd"] = as
				if _, has := settings["notifyDuringSilent"]; !has {
					settings["notifyDuringSilent"] = true
				}
			}
		}
	}
	return settings
}

// needsCronExprFallback returns true when the agent's settings need the
// legacy cronExpr → intervalMinutes translation applied. Mirrors v0's
// store.Load() trigger: missing key OR explicit 0. The map decoder
// makes numeric values float64; we coerce both int and float64 zero so
// the same row on either decode path takes the same branch.
func needsCronExprFallback(settings map[string]any) bool {
	v, ok := settings["intervalMinutes"]
	if !ok {
		return true
	}
	switch n := v.(type) {
	case float64:
		return n == 0
	case int:
		return n == 0
	}
	return false
}

// legacyCronRe matches the only cron form v0's parseLegacyCron knew
// about: "*/N * * * *". Anything else is recorded as 0 (disabled).
var legacyCronRe = regexp.MustCompile(`^\*/(\d+)\s+\*\s+\*\s+\*\s+\*$`)

// legacyCronAllowedIntervals mirrors internal/agent's allowedIntervals
// whitelist. Duplicated here (rather than imported) to keep the
// migration package free of an internal/agent dependency that would
// turn agent-package tests into a transitive concern of every
// importer test. Keep in sync with internal/agent/agent.go.
var legacyCronAllowedIntervals = map[int]bool{
	0: true, 5: true, 10: true, 30: true, 60: true,
	180: true, 360: true, 720: true, 1440: true,
}

func parseLegacyCronExpr(expr string, logger *slog.Logger) int {
	m := legacyCronRe.FindStringSubmatch(expr)
	if m == nil {
		logger.Warn("legacy cronExpr unrecognized — treating as disabled", "expr", expr)
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		logger.Warn("legacy cronExpr: bad N — treating as disabled", "expr", expr, "err", err)
		return 0
	}
	if !legacyCronAllowedIntervals[n] {
		// Mirror v0's parseLegacyCron behaviour: a recognized form with
		// an N outside the whitelist falls back to 0 so a hand-edited
		// agents.json with `*/7 * * * *` doesn't sneak through into v1
		// where ValidInterval would later reject it on every PATCH.
		logger.Warn("legacy cronExpr: N outside allowedIntervals — treating as disabled",
			"expr", expr, "n", n)
		return 0
	}
	return n
}

func importAgentPersona(ctx context.Context, st *store.Store, v0Dir, agentID string, raw map[string]any) error {
	personaPath := filepath.Join(agentDir(v0Dir, agentID), "persona.md")
	body, err := readV0(v0Dir, personaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No persona file — fall back to the inline copy in
			// agents.json (older v0 builds left the JSON copy as the
			// authoritative source).
			inline, _ := raw["persona"].(string)
			if inline == "" {
				return nil
			}
			body = []byte(inline)
		} else {
			return err
		}
	}

	// Idempotency: skip the upsert when the existing row's body hash
	// already matches. Avoids version churn on every re-run.
	if cur, err := st.GetAgentPersona(ctx, agentID); err == nil && cur != nil {
		if cur.BodySHA256 == store.SHA256Hex(body) {
			return nil
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	createdAt, _ := raw["createdAt"].(string)
	updatedAt, _ := raw["updatedAt"].(string)
	updated := fileMTimeMillis(personaPath)
	if updated == 0 {
		updated = parseRFC3339Millis(updatedAt)
	}
	_, err = st.UpsertAgentPersona(ctx, agentID, string(body), "", store.AgentInsertOptions{
		CreatedAt:      parseRFC3339Millis(createdAt),
		UpdatedAt:      updated,
		AllowOverwrite: true,
	})
	return err
}

func importAgentMemoryBlob(ctx context.Context, st *store.Store, v0Dir, agentID string, raw map[string]any) error {
	memPath := filepath.Join(agentDir(v0Dir, agentID), "MEMORY.md")
	body, err := readV0(v0Dir, memPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	if cur, err := st.GetAgentMemory(ctx, agentID); err == nil && cur != nil {
		if cur.BodySHA256 == store.SHA256Hex(body) {
			return nil
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	createdAt, _ := raw["createdAt"].(string)
	updatedAt, _ := raw["updatedAt"].(string)
	updated := fileMTimeMillis(memPath)
	if updated == 0 {
		updated = parseRFC3339Millis(updatedAt)
	}
	_, err = st.UpsertAgentMemory(ctx, agentID, string(body), "", store.AgentMemoryInsertOptions{
		CreatedAt:      parseRFC3339Millis(createdAt),
		UpdatedAt:      updated,
		AllowOverwrite: true,
	})
	return err
}

// daily file name pattern for memory/<YYYY-MM-DD>.md.
var dailyDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// canonicalKindDirs maps a top-level memory subdirectory to its DB kind.
// Anything not listed is folded into "topic" with the relative path
// (including the unknown directory name) baked into the entry name so
// the round-trip is lossless and the partial unique index treats them
// as distinct rows.
var canonicalKindDirs = map[string]string{
	"projects": "project",
	"people":   "people",
	"topics":   "topic",
	"archive":  "archive",
}

func importMemoryEntries(ctx context.Context, st *store.Store, v0Dir, agentID, fallbackUpdated string, logger *slog.Logger) error {
	root := filepath.Join(agentDir(v0Dir, agentID), "memory")
	entries, err := readDirV0(v0Dir, root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	for _, e := range entries {
		full := filepath.Join(root, e.Name())
		if e.IsDir() {
			kind, ok := canonicalKindDirs[e.Name()]
			namePrefix := ""
			if !ok {
				logger.Warn("memory: unrecognized subdirectory, importing as topic with dir-prefixed name",
					"agent", agentID, "dir", e.Name())
				kind = "topic"
				// Keep the original directory in the entry's name so
				// `memory/foo/x.md` does not collide with
				// `memory/foo/y.md` or with a top-level `memory/x.md`.
				namePrefix = e.Name() + "/"
			}
			if err := importMemoryDir(ctx, st, v0Dir, agentID, fallbackUpdated, kind, namePrefix, full, logger); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".md")
		kind := "topic"
		if dailyDateRe.MatchString(stem) {
			kind = "daily"
		}
		if err := importMemoryFile(ctx, st, v0Dir, agentID, fallbackUpdated, kind, stem, full); err != nil {
			return err
		}
	}
	return nil
}

// importMemoryDir walks one canonical memory subdir (projects/people/topics/archive)
// or an unknown subdir (kind=topic, namePrefix=<dir>/). Files at top level
// of the subdir are imported with name = namePrefix + stem; deeper nested
// files retain their full relative path (with "/") in the name.
func importMemoryDir(ctx context.Context, st *store.Store, v0Dir, agentID, fallbackUpdated, kind, namePrefix, dir string, logger *slog.Logger) error {
	return walkDirV0(v0Dir, dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			logger.Warn("memory walk error",
				"agent", agentID, "path", path, "err", walkErr)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		// Drop the .md suffix and prefix the original (unknown)
		// directory name when the subdir wasn't canonical, so we
		// don't lose hierarchy information.
		name := namePrefix + strings.TrimSuffix(filepath.ToSlash(rel), ".md")
		return importMemoryFile(ctx, st, v0Dir, agentID, fallbackUpdated, kind, name, path)
	})
}

func importMemoryFile(ctx context.Context, st *store.Store, v0Dir, agentID, fallbackUpdated, kind, name, path string) error {
	if name == "" {
		return nil
	}
	body, err := readV0(v0Dir, path)
	if err != nil {
		return err
	}

	// Idempotency by natural key.
	if cur, err := st.FindMemoryEntryByName(ctx, agentID, kind, name); err == nil && cur != nil {
		if cur.BodySHA256 == store.SHA256Hex(body) {
			return nil
		}
		// Body changed since the last run — overwrite via Update path.
		// This is the rerun-after-edit case; we treat the on-disk copy
		// as authoritative.
		bodyStr := string(body)
		_, uerr := st.UpdateMemoryEntry(ctx, cur.ID, "", store.MemoryEntryPatch{Body: &bodyStr})
		return uerr
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	updated := fileMTimeMillis(path)
	if updated == 0 {
		updated = parseRFC3339Millis(fallbackUpdated)
	}
	_, err = st.InsertMemoryEntry(ctx, &store.MemoryEntryRecord{
		ID:      newMemoryEntryID(),
		AgentID: agentID,
		Kind:    kind,
		Name:    name,
		Body:    string(body),
	}, store.MemoryEntryInsertOptions{
		UpdatedAt: updated,
		CreatedAt: updated,
	})
	if err != nil {
		return fmt.Errorf("insert memory_entry kind=%s name=%s: %w", kind, name, err)
	}
	return nil
}

// newMemoryEntryID generates a 16-byte random "me_" prefixed id. We don't
// derive it from (agent,kind,name) because the natural key already
// dedupes via FindMemoryEntryByName — using a random id keeps the
// generator consistent with v0's m_/gd_/gm_ schemes.
func newMemoryEntryID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "me_fallback_" + hex.EncodeToString([]byte(fmt.Sprintf("%d", store.NowMillis())))[:8]
	}
	return "me_" + hex.EncodeToString(b[:])
}
