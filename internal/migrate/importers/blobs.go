package importers

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// errSkipInvalidPath signals that publishBlob refused to publish the
// leaf because v1's blob layer rejected the logical path (NFC,
// reserved chars, etc.). The Run loop catches this so a single
// pathological leaf — say a `?` somebody dropped under books/ on
// macOS — doesn't take down the whole migration.
var errSkipInvalidPath = errors.New("blobs: v1 path validation refused leaf")

// blobsImporter migrates per-agent binary artefacts into the v1 native
// blob store. Domain key: "blobs". Runs after agents so blob_refs rows
// can FK against the agents table indirectly via URI convention (the
// schema does not enforce that FK because cas / handoff / orphan
// repair all expect blob_refs to outlive a deleted agent).
//
// Mapping (docs §2.4 / §5.5 step 7):
//
//	v0 agents/<id>/avatar.{png,svg,jpg,jpeg,webp} → kojo://global/agents/<id>/avatar.<ext>
//	v0 agents/<id>/books/**             → kojo://global/agents/<id>/books/**
//	v0 agents/<id>/outbox/**            → kojo://global/agents/<id>/outbox/**
//	v0 agents/<id>/temp/**              → kojo://local/agents/<id>/temp/**
//	v0 agents/<id>/index/memory.db      → kojo://local/agents/<id>/index/memory.db
//	v0 agents/<id>/credentials.{json,key} → kojo://machine/agents/<id>/credentials.<ext>
//
// Hidden CLI workspace dirs (.claude/, .codex/, .gemini/) are NOT
// blob_refs rows. The original plan was a separate dotfiles importer
// in a follow-up slice, but that was abandoned: these dirs are local
// CLI scratch that is either kojo-managed and regenerated on first
// Chat (backend_claude.go writes .claude/settings.local.json,
// backend_gemini.go's prepareGeminiDir writes .gemini/settings.json
// and GEMINI.md alongside it) or CLI-owned and outside kojo's write
// path (backend_codex.go only sets cmd.Dir — any .codex/ that
// appears under agentDir is whatever the codex binary chose to
// drop, and is recreated by codex itself if absent). The only
// continuity that matters across the v0→v1 path-hash boundary
// is the global CLI transcript stores at ~/.claude/projects/<hash>
// and ~/.codex/sessions/<hash> (and gemini's projects.json), which
// are handled separately by `--migrate-external-cli`
// (internal/migrate/externalcli) via symlink / projects.json
// append. v1 binaries that find a stray <v1>/agents/<id>/.claude/
// or similar from a pre-cutover dev install treat it as harmless
// cruft.
//
// credentials.{json,key} blob_refs rows ARE written by this
// importer for archival, but they are NOT consulted at runtime:
// internal/agent/credential.go owns the canonical encrypted store
// at <configdir>/credentials.db (with its host-bound encryption
// key at <configdir>/credentials.key), and its migrateLegacy
// helper reads from <v1>/agents/<id>/credentials.json (which v1
// never writes — only a pre-cutover dev install would have one).
// The blob_refs rows survive purely so an operator can recover the
// pre-migration v0 secret material if credentials.db is lost.
//
// .cron_last is also NOT a blob_refs row, but for a different reason:
// after Phase 2c-2 slice 12 the cron throttle moved to the kv table
// at (namespace="scheduler", key="cron_last/<agentID>", scope=
// machine). The v0 dotfile is no longer canonical state on either
// side of the cutover — its 50s mtime would be useless after the
// upgrade transient — and runtime acquireCronLock best-effort
// unlinks it on each tick. Nothing imports its value; a stray file
// surviving the migration is harmless cruft.
type blobsImporter struct{}

func (blobsImporter) Domain() string { return "blobs" }

// blobMapping captures one (v0 leaf → v1 blob URI) edge. relPath is the
// V0Dir-relative form used for the source-path checksum; absPath is
// the on-disk leaf opened by openV0 / streamPutBlob; scope/path is the
// destination address inside the v1 blob store.
type blobMapping struct {
	relPath string
	absPath string
	scope   blob.Scope
	path    string // logical path under scope (forward slashes)
}

func (blobsImporter) Run(ctx context.Context, st *store.Store, opts migrate.Options) error {
	if done, err := alreadyImported(ctx, st, "blobs"); err != nil {
		return err
	} else if done {
		return nil
	}

	logger := slog.Default().With("importer", "blobs")

	// Scan v0 first. The checksum (and the per-leaf op list) are both
	// derived from the same walk so source_checksum can never claim a
	// file the importer didn't actually consider. Pulls the known-agent
	// set from the v1 store rather than re-parsing agents.json so the
	// filter inherits whatever skip policy the agents importer applied
	// (empty name, schema-rejection rows, etc.).
	mappings, err := collectBlobMappings(ctx, st, opts.V0Dir)
	if err != nil {
		return fmt.Errorf("collect blob mappings: %w", err)
	}
	relPaths := make([]string, 0, len(mappings))
	for _, m := range mappings {
		relPaths = append(relPaths, m.relPath)
	}
	checksum, err := domainChecksum(opts.V0Dir, relPaths)
	if err != nil {
		return fmt.Errorf("checksum blobs sources: %w", err)
	}

	if len(mappings) == 0 {
		// Empty v0 dir / no agents — still record the domain so a re-run
		// can early-exit via alreadyImported.
		return markImported(ctx, st, "blobs", 0, checksum)
	}

	// Build a Store for the v1 blob tree. We construct it locally rather
	// than threading a *blob.Store through migrate.Options so internal/
	// migrate stays free of an internal/blob import; the importer is
	// the only consumer that cares.
	homePeer := opts.HomePeer
	if homePeer == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "kojo-local"
		}
		homePeer = h
	}
	bs := blob.New(opts.V1Dir,
		blob.WithRefs(blob.NewStoreRefs(st, homePeer)),
		blob.WithHomePeer(homePeer),
	)

	imported := 0
	skipped := 0
	skippedInvalid := 0
	for _, m := range mappings {
		// Resume decision: skip iff (a) the blob_refs row is present
		// AND (b) the on-disk body matches what the row claims AND
		// (c) the body matches the v0 source. Any divergence — row
		// missing, row ↔ fs drift, fs ↔ src drift — re-publishes so
		// the partial-run hole closes.
		decision, err := blobResumeDecision(opts.V0Dir, bs, m, logger)
		switch {
		case errors.Is(err, errSkipInvalidPath):
			logger.Warn("skipping v0 blob with v1-invalid path",
				"v0_path", m.absPath, "logical", m.path, "err", err)
			skippedInvalid++
			continue
		case err != nil:
			return err
		case decision == decisionSkipFresh:
			skipped++
			continue
		}

		if err := publishBlob(opts.V0Dir, bs, m); err != nil {
			if errors.Is(err, errSkipInvalidPath) {
				logger.Warn("skipping v0 blob with v1-invalid path",
					"v0_path", m.absPath, "logical", m.path, "err", err)
				skippedInvalid++
				continue
			}
			return fmt.Errorf("publish %s: %w", m.path, err)
		}
		imported++
	}

	if skipped > 0 || skippedInvalid > 0 {
		logger.Info("blobs partial-run resume",
			"imported", imported,
			"skipped_already_published", skipped,
			"skipped_invalid_path", skippedInvalid)
	}
	return markImported(ctx, st, "blobs", imported, checksum)
}

// blobResumeDecision encapsulates the resume policy for one mapping.
// Returns decisionSkipFresh when the v1 state is fully aligned with
// v0 (ref row, fs body, and source all agree); decisionPublish when
// any divergence requires a re-publish; or errSkipInvalidPath when
// either the path or the scope is rejected by v1's blob layer.
//
// Each branch logs at warn level so resume drift is visible in the
// migration log without re-walking the dir afterwards.
type resumeDecision int

const (
	decisionPublish   resumeDecision = 0
	decisionSkipFresh resumeDecision = 1
)

func blobResumeDecision(v0Dir string, bs *blob.Store, m blobMapping, logger *slog.Logger) (resumeDecision, error) {
	obj, hErr := bs.Head(m.scope, m.path)
	switch {
	case errors.Is(hErr, blob.ErrInvalidPath), errors.Is(hErr, blob.ErrInvalidScope):
		// Path validation surfaced the same v1 rejection publishBlob
		// would hit — bubble through the skip channel so the migration
		// doesn't abort on a single pathological leaf.
		return decisionPublish, fmt.Errorf("%w: %v", errSkipInvalidPath, hErr)
	case errors.Is(hErr, blob.ErrNotFound):
		// fs body missing — straightforward "first publish" case.
		return decisionPublish, nil
	case hErr != nil:
		return decisionPublish, fmt.Errorf("head %s: %w", m.path, hErr)
	}
	// Object exists on disk. obj.SHA256 comes from the blob_refs cache
	// (populateDigestFromRefs); empty means the row is missing — the
	// classic "fs publish committed, ref insert crashed" partial-run
	// case. Re-publish so Put rewrites both halves.
	if obj.SHA256 == "" {
		logger.Warn("blob fs body present but ref row missing — re-publishing",
			"uri", "kojo://"+string(m.scope)+"/"+m.path)
		return decisionPublish, nil
	}
	// Verify the on-disk body actually hashes to what the ref claims —
	// a row whose SHA256 disagrees with the file is exactly the scrub-
	// repairs case, and the cheapest repair is a fresh Put.
	actual, vErr := bs.Verify(m.scope, m.path)
	switch {
	case errors.Is(vErr, blob.ErrInvalidPath), errors.Is(vErr, blob.ErrInvalidScope):
		return decisionPublish, fmt.Errorf("%w: %v", errSkipInvalidPath, vErr)
	case errors.Is(vErr, blob.ErrNotFound):
		// Race against another writer between Head and Verify. Treat
		// like a missing fs body and re-publish.
		return decisionPublish, nil
	case vErr != nil:
		return decisionPublish, fmt.Errorf("verify %s: %w", m.path, vErr)
	}
	if actual.SHA256 != obj.SHA256 {
		logger.Warn("blob ref ↔ fs digest drift — re-publishing",
			"uri", "kojo://"+string(m.scope)+"/"+m.path,
			"ref_sha256", obj.SHA256, "fs_sha256", actual.SHA256)
		return decisionPublish, nil
	}
	srcDigest, cerr := fileChecksumRO(v0Dir, m.absPath)
	if cerr != nil {
		return decisionPublish, fmt.Errorf("hash %s: %w", m.absPath, cerr)
	}
	if actual.SHA256 == srcDigest {
		return decisionSkipFresh, nil
	}
	logger.Warn("blob fs body diverged from v0 source — re-publishing",
		"uri", "kojo://"+string(m.scope)+"/"+m.path,
		"fs_sha256", actual.SHA256, "src_sha256", srcDigest)
	return decisionPublish, nil
}

// publishBlob streams one v0 leaf into the v1 blob store. Open via
// openV0 (read-only + symlink-escape guard) so the migration cannot be
// coerced into reading outside V0Dir even by a hostile symlink planted
// during a partial run.
//
// A v1 path-validation failure (NFC, reserved chars, control bytes,
// etc.) is wrapped into errSkipInvalidPath so the importer's main loop
// can warn-and-skip rather than abort the entire migration on a single
// pathological leaf.
func publishBlob(v0Dir string, bs *blob.Store, m blobMapping) error {
	f, err := openV0(v0Dir, m.absPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := bs.Put(m.scope, m.path, f, blob.PutOptions{}); err != nil {
		if errors.Is(err, blob.ErrInvalidPath) || errors.Is(err, blob.ErrInvalidScope) {
			return fmt.Errorf("%w: %v", errSkipInvalidPath, err)
		}
		return err
	}
	return nil
}

// knownAgentIDs returns the set of agent ids that the agents importer
// actually committed. Reading from the v1 store (rather than re-parsing
// agents.json) means blobsImporter inherits whatever skip policy the
// agents importer applied — empty name, schema-rejection rows,
// malformed timestamps that surfaced as Unmarshal errors — without
// duplicating that decision tree here. The importer registration order
// guarantees agentsImporter ran first.
func knownAgentIDs(ctx context.Context, st *store.Store) (map[string]struct{}, error) {
	ags, err := st.ListAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	ids := make(map[string]struct{}, len(ags))
	for _, a := range ags {
		ids[a.ID] = struct{}{}
	}
	return ids, nil
}

// collectBlobMappings walks v0/agents/<id>/ for every leaf the design
// doc maps onto a blob URI. The list is stable (sorted by relPath) so
// re-runs under identical v0 state produce identical migration_status
// imported_count and identical mapping order in logs.
//
// Only agent ids declared in agents.json are included — orphan dirs
// (backup copies, partial-delete leftovers) are skipped to keep
// blob_refs from referencing agents that agentsImporter itself
// declined to migrate.
//
// Symlink leaves and parent-component symlinks are silently skipped —
// the importer already refuses to read through them via openV0, but
// here we keep the policy explicit so a hostile sync that planted
// `agents/<id>/avatar.png` as a symlink can't trick the source-path
// list into "covering" a file the publish loop will refuse.
func collectBlobMappings(ctx context.Context, st *store.Store, v0Dir string) ([]blobMapping, error) {
	var out []blobMapping
	base := agentsBaseDir(v0Dir)
	entries, err := readDirV0(v0Dir, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("readdir agents: %w", err)
	}

	known, err := knownAgentIDs(ctx, st)
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// "groupdms" is a sibling kind handled by groupdmsImporter; it
		// holds no blob artefacts.
		if e.Name() == "groupdms" {
			continue
		}
		agentID := e.Name()
		if _, ok := known[agentID]; !ok {
			// Orphan agent dir — skip outright. Surfacing it via the
			// checksum is acceptable (collectAgentsSourcePaths already
			// includes orphan persona/MEMORY for drift detection); the
			// blob copy belongs only to live agents.
			continue
		}
		agentRoot := filepath.Join(base, agentID)

		// Avatars: avatar.{png,svg,jpg,jpeg,webp}. Multiple could
		// exist (rare) — publish each independently because the URI
		// suffix differs. The probed list mirrors runtime
		// avatarExtProbe (internal/agent/avatar.go); a v0 install
		// with avatar.gif (never accepted by v0's IsAllowedImageExt
		// either, only possible from a hand-edited dir) is left in
		// the v0 dir untouched — runtime can't render it post-
		// cutover and migrating it would just create an orphan blob.
		for _, ext := range []string{"png", "svg", "jpg", "jpeg", "webp"} {
			leaf := filepath.Join(agentRoot, "avatar."+ext)
			if mapping, ok, err := blobLeaf(v0Dir, leaf,
				blob.ScopeGlobal,
				"agents/"+agentID+"/avatar."+ext); err != nil {
				return nil, err
			} else if ok {
				out = append(out, mapping)
			}
		}

		// `index/memory.db` is the single RAG-index file the design
		// doc enumerates (§2.4). The rest of `index/` (transient
		// shards, lock files, fts cache) is local-peer-only state
		// that v1 will rebuild on first use — copying it would
		// preserve broken layouts across v0/v1 schema bumps. We
		// publish it here as a known leaf and then suppress the
		// rest of `index/` in the catchall walk below.
		if mapping, ok, err := blobLeaf(v0Dir,
			filepath.Join(agentRoot, "index", "memory.db"),
			blob.ScopeLocal,
			"agents/"+agentID+"/index/memory.db"); err != nil {
			return nil, err
		} else if ok {
			out = append(out, mapping)
		}

		// Credentials live at fixed leaf names — never recurse into
		// agentRoot itself looking for them. A directory walk would
		// also pick up the per-agent CLI dotfile dirs (.claude/,
		// .codex/, .gemini/), which the catchall walk below skips
		// via skipBlobDir for the same reason: they are kojo-managed
		// scratch regenerated by claude/gemini backends on first
		// Chat, or CLI-owned scratch outside kojo's write path for
		// codex.
		for _, leaf := range []string{"credentials.json", "credentials.key"} {
			full := filepath.Join(agentRoot, leaf)
			if mapping, ok, err := blobLeaf(v0Dir, full,
				blob.ScopeMachine,
				"agents/"+agentID+"/"+leaf); err != nil {
				return nil, err
			} else if ok {
				out = append(out, mapping)
			}
		}

		// Catchall walk: every other leaf under agentRoot. Captures
		// books/, outbox/, temp/ (formerly hand-mapped), outputs/,
		// and the long tail of agent-created scratch (game design
		// docs, screenshots, sqlite journals, project subdirs,
		// arbitrary subdirs like research/, reports/, data/, work/,
		// etc.) without an enumerated whitelist — preserving v0
		// agent state that the design doc didn't enumerate but
		// users actually accumulated.
		//
		// Skip rules (see skipBlobDir / skipBlobFile):
		//   - dotfiles / dotdirs (.claude, .codex, .gemini, .DS_Store)
		//   - DB-canonical leaves already mirrored to a typed table
		//     (persona.md, MEMORY.md, memory/, tasks.json,
		//     messages.jsonl, autosummary_marker)
		//   - regenerable cache (persona_summary.md)
		//   - backup leaves (*.bak, *.bak1..N) — operator-owned
		//     scratch, no v1 surface consumes them
		//   - lock files (*.lock)
		//   - leaves already published by the explicit blocks above
		//     (avatar.<ext>, index/memory.db, credentials.{json,key})
		//
		// Scope assignment (see classifyBlobScope):
		//   - books/, outbox/ → global (replicated)
		//   - everything else → local (machine-specific scratch;
		//     too noisy to sync, often re-generated, sometimes
		//     huge)
		ms, err := walkAgentBlobMappings(v0Dir, agentRoot, agentID)
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	return out, nil
}

// walkAgentBlobMappings produces blob mappings for every regular file
// under agentRoot that isn't suppressed by skipBlobDir / skipBlobFile.
// Mirrors walkBlobSubtree but operates on the agent root with an
// agents/<id>/<rel> URI prefix and per-rel scope classification.
//
// Symlinks (both leaf and intermediate) are skipped: walkDirV0
// surfaces them as walkErr and we drop the entry rather than aborting
// the whole importer.
func walkAgentBlobMappings(v0Dir, agentRoot, agentID string) ([]blobMapping, error) {
	if _, err := os.Lstat(agentRoot); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("lstat %s: %w", agentRoot, err)
	}
	var out []blobMapping
	walkErr := walkDirV0(v0Dir, agentRoot, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d == nil {
			return nil
		}
		rel, err := filepath.Rel(agentRoot, p)
		if err != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		// Apply directory skip BEFORE descending. Two match modes:
		//   - basename match: dotdirs (.claude/, .codex/, .gemini/)
		//     are unwanted at ANY depth — a stray .claude/ inside
		//     outputs/ shouldn't end up in the blob store either.
		//   - top-level rel match: memory/ and chat_history/ are
		//     only DB-canonical when they sit at agentRoot/. A
		//     nested dir literally named "memory" inside outputs/
		//     is an operator's scratch and should be walked normally.
		if d.IsDir() {
			if skipBlobDir(d.Name()) {
				return filepath.SkipDir
			}
			if isTopLevelDBCanonicalDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if skipBlobFile(rel, d.Name()) {
			return nil
		}
		// Suppress leaves already published by the explicit
		// avatar / index / credentials blocks above. Re-emitting
		// them here would surface as a duplicate mapping for the
		// same blob URI; the second Put would either ETag-mismatch
		// or be a redundant write.
		if isExplicitlyPublishedLeaf(rel) {
			return nil
		}
		scope := classifyBlobScope(rel)
		logical := "agents/" + agentID + "/" + filepath.ToSlash(rel)
		v0Rel, err := filepath.Rel(v0Dir, p)
		if err != nil {
			return nil
		}
		out = append(out, blobMapping{
			relPath: filepath.ToSlash(v0Rel),
			absPath: p,
			scope:   scope,
			path:    logical,
		})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk %s: %w", agentRoot, walkErr)
	}
	return out, nil
}

// skipBlobDir reports whether a directory at or under agentRoot should
// be excluded from the catchall walk. The match is on the directory's
// own basename, not its full rel path — `.claude/` at any depth is
// skipped (a stray dotfile dir under outputs/ shouldn't end up in the
// blob store any more than the top-level one).
//
//   - .claude / .codex / .gemini : CLI workspace dirs, regenerated by
//     backends or owned by the CLI process; copying the v0 tree just
//     creates stale layouts that the runtime would regenerate anyway.
//
// Note: memory/ and chat_history/ are also suppressed but only when
// they sit at agentRoot/ — see isTopLevelDBCanonicalDir. A literal
// "memory" subdir nested under outputs/ is operator scratch, not a
// DB-canonical store.
func skipBlobDir(name string) bool {
	switch name {
	case ".claude", ".codex", ".gemini":
		return true
	}
	return false
}

// isTopLevelDBCanonicalDir reports whether rel addresses one of the
// agentRoot-level subdirs whose body is DB-canonical (memory/) or
// re-fetched from a remote platform (chat_history/). Nested dirs that
// happen to share the name are operator scratch and DO get walked.
func isTopLevelDBCanonicalDir(rel string) bool {
	switch filepath.ToSlash(rel) {
	case "memory", "chat_history":
		return true
	}
	return false
}

// canonicalDBLeaves enumerates v0 agent-dir leaves whose state moves
// to a typed DB table during migration; copying them as blobs would
// duplicate canonical state. Each entry is the rel-path under
// agentRoot (forward-slash separated even on Windows — the importer
// normalizes via filepath.ToSlash).
var canonicalDBLeaves = map[string]struct{}{
	"persona.md":         {},
	"MEMORY.md":          {},
	"tasks.json":         {},
	"messages.jsonl":     {},
	"autosummary_marker": {},
	"persona_summary.md": {}, // regenerable LLM cache
}

// blobBackupRe matches operator-owned backup leaves (*.bak,
// *.bak1..N, *.bak.<anything>). v0 left these around manual-edit
// snapshots; the user explicitly excluded them from migration scope.
var blobBackupRe = regexp.MustCompile(`\.bak\d*(\..+)?$`)

// blobOSJunkNames enumerates OS-generated metadata files that the v1
// blob layer rejects via internal/blob/path.go's reservedNames /
// reservedPrefixes lists. Filtering them at the importer side
// suppresses ErrInvalidPath warning spam during migration without
// changing the v1 layer's defensive posture (a hand-crafted call
// with one of these names still gets refused).
//
// Compared case-insensitively to match the blob layer's behavior.
var blobOSJunkNames = map[string]bool{
	"thumbs.db":   true,
	"desktop.ini": true,
}

// skipBlobFile reports whether a file leaf should be suppressed by the
// catchall walk. rel is the agentRoot-relative path with native
// separators; name is rel's filepath.Base (passed as a hint to avoid
// recomputing).
func skipBlobFile(rel, name string) bool {
	// Dotfiles at any depth — covers .DS_Store, .gitignore, agent
	// runtime markers like .cron_last, etc. (.claude/ etc. dirs are
	// already skipped via skipBlobDir before we descend.)
	if strings.HasPrefix(name, ".") {
		return true
	}
	relSlash := filepath.ToSlash(rel)
	if _, ok := canonicalDBLeaves[relSlash]; ok {
		return true
	}
	if strings.HasSuffix(name, ".lock") {
		return true
	}
	if blobBackupRe.MatchString(name) {
		return true
	}
	// OS junk: Thumbs.db / desktop.ini / etc. The blob layer rejects
	// these via reservedNames in internal/blob/path.go (see §4.3 of
	// the design doc), so without this filter every such file
	// surfaces as a warn-skip in the migration log. Match
	// case-insensitively to mirror the blob layer's compare.
	lower := strings.ToLower(name)
	if blobOSJunkNames[lower] {
		return true
	}
	// AppleDouble companions (._foo) created by macOS network
	// shares; Office lock files (~$foo). Both rejected by the blob
	// layer's reservedPrefixes; same suppression rationale as
	// blobOSJunkNames.
	if strings.HasPrefix(name, "._") || strings.HasPrefix(name, "~$") {
		return true
	}
	return false
}

// isExplicitlyPublishedLeaf reports whether rel is a leaf the
// explicit avatar / index / credentials blocks already emitted as a
// mapping. Returning true here suppresses the duplicate from the
// catchall walk.
//
// Avatar extensions mirror the probe list above. credentials.{json,key}
// at top level. index/memory.db is the only file we publish under
// the otherwise-skipped index/ subtree — index/* sidecars (memory.db-
// wal, memory.db-shm, FTS shards, lock files) are SQLite/RAG-internal
// regenerable state that v1 rebuilds on first use; copying them as
// blobs would freeze a stale layout across schema bumps and cost
// blob storage on every peer for state that is purely local.
func isExplicitlyPublishedLeaf(rel string) bool {
	relSlash := filepath.ToSlash(rel)
	switch relSlash {
	case "credentials.json", "credentials.key", "index/memory.db":
		return true
	}
	// Suppress the rest of the top-level index/ subtree. Match by
	// prefix so memory.db-wal / memory.db-shm / locks / future FTS
	// shard files all collapse to "skip" without an enumerated list
	// that the next SQLite version would invalidate.
	if strings.HasPrefix(relSlash, "index/") {
		return true
	}
	for _, ext := range []string{"png", "svg", "jpg", "jpeg", "webp"} {
		if relSlash == "avatar."+ext {
			return true
		}
	}
	return false
}

// classifyBlobScope assigns a blob.Scope based on the rel path's top-
// level segment. Top-level scratch files (rel has no slash) and
// arbitrary subdirs default to ScopeLocal — they're typically machine-
// specific work outputs, screenshots, sqlite journals, etc., and
// replicating them across peers wastes bandwidth without value.
//
// Globally-replicated subdirs are an explicit allow-list:
//   - books/  : user-curated reference material (PDFs, EPUBs)
//   - outbox/ : pending outbound payloads (messages waiting to send)
//
// Note: temp/ is intentionally local — historically the v0 design
// treated it as transient scratch, and the v1 docs §5.5 mapping
// confirms `temp/** → kojo://local/...`.
func classifyBlobScope(rel string) blob.Scope {
	relSlash := filepath.ToSlash(rel)
	parts := strings.SplitN(relSlash, "/", 2)
	if len(parts) > 0 {
		switch parts[0] {
		case "books", "outbox":
			return blob.ScopeGlobal
		}
	}
	return blob.ScopeLocal
}

// blobLeaf builds a blobMapping for a single leaf, returning ok=false
// if the leaf is missing or non-regular. Errors other than ErrNotExist
// surface up so a permission glitch on a known leaf doesn't silently
// drop the row from the migration.
func blobLeaf(v0Dir, leaf string, scope blob.Scope, logical string) (blobMapping, bool, error) {
	st, err := os.Lstat(leaf)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return blobMapping{}, false, nil
		}
		return blobMapping{}, false, fmt.Errorf("lstat %s: %w", leaf, err)
	}
	if !st.Mode().IsRegular() {
		return blobMapping{}, false, nil
	}
	if err := assertUnderRoot(v0Dir, leaf); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return blobMapping{}, false, nil
		}
		return blobMapping{}, false, err
	}
	rel, err := filepath.Rel(v0Dir, leaf)
	if err != nil {
		return blobMapping{}, false, fmt.Errorf("rel %s: %w", leaf, err)
	}
	return blobMapping{
		relPath: filepath.ToSlash(rel),
		absPath: leaf,
		scope:   scope,
		path:    logical,
	}, true, nil
}

// walkBlobSubtree enumerates every regular file under root and produces
// a blobMapping for each, addressed under uriPrefix. Symlinks (both leaf
// and intermediate) are skipped — walkDirV0 surfaces them as walkErr,
// and we drop those entries rather than aborting the whole importer.
func walkBlobSubtree(v0Dir, root string, scope blob.Scope, uriPrefix string) ([]blobMapping, error) {
	var out []blobMapping
	if _, err := os.Lstat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("lstat %s: %w", root, err)
	}
	walkErr := walkDirV0(v0Dir, root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			// Symlink / not-regular hits land here; skip and continue.
			return nil
		}
		if d == nil || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		// uriPrefix already ends in "/"; just append the subtree's rel
		// path with forward slashes.
		logical := uriPrefix + filepath.ToSlash(rel)
		// Rebuild relPath as v0-rooted for the checksum row. filepath.
		// Rel against v0Dir is the canonical form domainChecksum
		// expects.
		v0Rel, err := filepath.Rel(v0Dir, p)
		if err != nil {
			return nil
		}
		out = append(out, blobMapping{
			relPath: filepath.ToSlash(v0Rel),
			absPath: p,
			scope:   scope,
			path:    logical,
		})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk %s: %w", root, walkErr)
	}
	return out, nil
}

