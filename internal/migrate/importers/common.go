// Package importers contains the per-domain v0→v1 importers wired into
// the migrate orchestrator's Register() list. They run in package init
// order: agents → messages → groupdms.
//
// Each importer:
//   - reads v0 artifacts under opts.V0Dir using O_RDONLY only — by
//     contract the importer must never write to opts.V0Dir.
//   - writes to the v1 store via the public Store APIs in internal/store.
//   - is idempotent: a second run skips records that already round-tripped
//     by id (or by natural key for memory_entries).
//   - calls markImported(ctx, st, domain, n, checksum) on success,
//     which writes phase="imported" and source_checksum to
//     migration_status. Orchestrator-level "complete" is stamped by
//     migrate.Run (not by per-domain importers).
package importers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// ErrNotRegular is returned by readV0 / openV0 when a path resolves to
// something that isn't a regular file (a symlink, directory, fifo,
// device, etc.). Distinct from os.ErrNotExist so callers don't quietly
// treat a malicious / stray symlink as "domain has no data" and stamp
// migration_status as imported with zero rows.
var ErrNotRegular = errors.New("importers: path is not a regular file")

// ErrEscapesRoot is returned when a v0 path resolves (via symlinks) to
// a target outside the canonicalized V0Dir. Phase 1 already guards
// reads via O_RDONLY, but a symlink anywhere along the path could let
// the importer pull bytes from a file the manifest didn't cover.
var ErrEscapesRoot = errors.New("importers: path resolves outside V0Dir")

// readV0 returns the bytes of a v0 file via O_RDONLY. By contract the
// importer must never write to opts.V0Dir.
//
// Two layered guards:
//  1. ErrNotRegular is returned for non-regular leaves so an attacker
//     (or a stray dotfile sync) cannot redirect a known v0 path at a
//     pipe / device / arbitrary file outside the manifest's coverage.
//  2. After EvalSymlinks, the resolved path must stay under v0Root so
//     a directory-component symlink (e.g. agents/<id> linked to /etc)
//     cannot smuggle content past the manifest.
//
// The manifest checksum in §5.5 step 3 already excludes content
// outside the v0 tree; these guards make sure the import path doesn't
// quietly outpace the manifest.
func readV0(v0Root, path string) ([]byte, error) {
	f, err := openV0(v0Root, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// openV0 returns an *os.File opened O_RDONLY for path, after asserting
// (a) the leaf is a regular file (not a symlink, not a directory, not
// a fifo/device) and (b) the resolved path stays under v0Root.
//
// The Lstat *must* run before assertUnderRoot — a broken symlink at
// the leaf would otherwise resolve to ErrNotExist via EvalSymlinks,
// and callers that treat ErrNotExist as "missing optional file" would
// silently mark the domain imported with zero rows. Lstat first
// surfaces broken / hostile symlinks as ErrNotRegular regardless of
// what they (don't) point at.
func openV0(v0Root, path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	if err := assertUnderRoot(v0Root, path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDONLY, 0)
}

// assertUnderRoot verifies that filepath.EvalSymlinks(path) lives under
// v0Root. v0Root is the importer's canonical V0Dir (already symlink-
// resolved by migrate.Run before importers see it). Both arguments are
// run through filepath.Clean to normalize trailing separators.
//
// EvalSymlinks fails with os.ErrNotExist if path doesn't exist; that
// error is returned verbatim so the caller's "missing file is fine"
// short-circuits keep working.
func assertUnderRoot(v0Root, path string) error {
	if v0Root == "" {
		// If the importer didn't thread a root, fall back to leaf-
		// only check (preserves existing test fixtures that don't
		// canonicalize a temp dir). A real --migrate invocation
		// always has v0Root populated by migrate.Run.
		return nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	// Resolve v0Root too so cases like /var/folders ↔ /private/var/folders
	// (macOS) match. EvalSymlinks may fail on a non-existent root,
	// which only happens in misconfigured test fixtures; fall back to
	// the cleaned value in that case so tests still get a useful
	// error path.
	rootResolved, err := filepath.EvalSymlinks(v0Root)
	if err != nil {
		rootResolved = filepath.Clean(v0Root)
	}
	root := filepath.Clean(rootResolved)
	resolved = filepath.Clean(resolved)
	if resolved == root {
		return nil
	}
	prefix := root + string(filepath.Separator)
	if !strings.HasPrefix(resolved, prefix) {
		return fmt.Errorf("%s -> %s: %w", path, resolved, ErrEscapesRoot)
	}
	return nil
}

// agentsBaseDir resolves <v0>/agents.
func agentsBaseDir(v0Dir string) string {
	return filepath.Join(v0Dir, "agents")
}

// agentDir resolves <v0>/agents/<agentID>.
func agentDir(v0Dir, agentID string) string {
	return filepath.Join(v0Dir, "agents", agentID)
}

// groupsDir resolves <v0>/agents/groupdms.
func groupsDir(v0Dir string) string {
	return filepath.Join(v0Dir, "agents", "groupdms")
}

// parseRFC3339Millis parses an RFC3339 timestamp into milliseconds since
// the Unix epoch. Returns 0 if the input is empty or unparseable; callers
// fall back to NowMillis() when this happens.
func parseRFC3339Millis(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t2, err2 := time.Parse("2006-01-02T15:04:05Z07:00", s)
		if err2 != nil {
			return 0
		}
		t = t2
	}
	return t.UnixMilli()
}

// fileMTimeMillis returns the file's mtime in milliseconds, or 0 if Stat
// fails. Used as a fallback when a v0 artifact has no embedded timestamp.
func fileMTimeMillis(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.ModTime().UnixMilli()
}

// markImported records a domain as "imported" once the v0 → v1 row copy
// finished. The schema's CHECK constraint only allows phase values
// ('pending','imported','cutover','complete'); the importer terminates at
// "imported" because cutover (Phase 2c-2) and migration-wide "complete"
// belong to higher-level orchestration, not the per-domain copier.
//
// `checksum` is the deterministic SHA256 over the v0 source files this
// importer scanned (see domainChecksum and the scan-vs-import gap note
// in collectAgentsSourcePaths — the hash covers the directory walk, not
// just the rows that ended up in the v1 store). Stored in
// migration_status.source_checksum for audit and drift detection. An
// empty string is acceptable — the column is left as NULL on a fresh
// row, or unchanged on an UPDATE — but every shipped importer should
// pass a real value so re-runs against a mutated v0 dir are diagnosable.
func markImported(ctx context.Context, st *store.Store, domain string, count int, checksum string) error {
	return migrate.MarkPhase(ctx, st, domain, "imported", count, checksum)
}

// alreadyImported returns true if a prior run already finished this domain.
// We treat any of imported / cutover / complete as terminal — once the
// rows are in, we never re-walk the v0 directory for that domain.
func alreadyImported(ctx context.Context, st *store.Store, domain string) (bool, error) {
	phase, err := migrate.PhaseOf(ctx, st, domain)
	if err != nil {
		return false, err
	}
	switch phase {
	case "imported", "cutover", "complete":
		return true, nil
	}
	return false, nil
}

// hasPathSep reports whether s contains either OS path separator. memory_entries
// names must not span directory boundaries; the importer maps subdirs into
// canonical kinds before stripping path components.
func hasPathSep(s string) bool {
	return strings.ContainsRune(s, filepath.Separator) || strings.ContainsRune(s, '/')
}

// readDirV0 lists path with the same root-escape protection as openV0.
// The directory itself must be a real (non-symlink) directory under
// v0Root; without this guard a symlink-directory pointing outside V0Dir
// would surface its contents as if they were v0 data.
//
// Returns os.ErrNotExist if the directory itself is missing, so callers
// can branch with errors.Is on the standard sentinel.
func readDirV0(v0Root, path string) ([]os.DirEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	if err := assertUnderRoot(v0Root, path); err != nil {
		return nil, err
	}
	return os.ReadDir(path)
}

// walkDirV0 wraps filepath.WalkDir with per-entry root-escape and
// no-symlink-dir checks. The walker fn receives the same arguments as
// filepath.WalkDir's callback; symlink directories surface as a
// per-entry ErrNotRegular fed into walkErr so the caller can log/skip,
// matching the warn-and-continue posture used elsewhere in the
// importer.
func walkDirV0(v0Root, root string, fn fs.WalkDirFunc) error {
	if err := assertUnderRoot(v0Root, root); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fn(path, d, walkErr)
		}
		// Refuse symlinks outright. DirEntry.IsDir() is false for any
		// symlink (its type is "symlink", not "dir"), so gating on
		// IsDir would silently let symlink-to-file *and* symlink-to-
		// dir entries through. Checking the symlink mode bit first
		// catches both cases.
		//
		// Note: neither filepath.WalkDir nor filepath.Walk follow
		// symlinks on their own, so a symlinked directory inside
		// `root` is visited as the symlink entry itself, not
		// descended into. This check is *defense in depth* — if a
		// future caller swaps to a custom traversal that does follow
		// links, this guard ensures we don't silently start
		// enumerating outside V0Dir. We surface the rejection
		// through the caller's fn so the importer's "warn and
		// continue" posture stays uniform.
		if d != nil && d.Type()&os.ModeSymlink != 0 {
			return fn(path, d, fmt.Errorf("%s: %w", path, ErrNotRegular))
		}
		return fn(path, d, nil)
	})
}

// domainChecksum returns a hex-encoded SHA256 fingerprint over the v0
// files a single importer scanned. The relPaths argument is a list of
// V0Dir-relative file paths in forward-slash form (typically produced
// by collect*SourcePaths, which records every file the importer
// traversed — including orphans the row-copy loop ultimately skipped);
// the helper sorts them so caller order doesn't affect the result.
//
// Row format mirrors migrate.ManifestSHA256 so a domain checksum is
// directly comparable against a slice of the global manifest:
//
//	"<rel>\0<size>\0<sha256>\n"   (regular file)
//	"<rel>\00\0missing\n"          (file absent at hash time; size=0)
//
// Symlinks and other non-regular entries fail closed — the importer
// already refuses to read through them, so a symlink showing up in
// relPaths is a contract violation and surfaces as an error here too.
//
// The function opens each file via openV0 (the read-only guard with
// root-escape protection) so the checksum cannot be coerced into reading
// outside V0Dir even if the caller's path collection is sloppy.
func domainChecksum(v0Dir string, relPaths []string) (string, error) {
	paths := append([]string(nil), relPaths...)
	sort.Strings(paths)

	h := sha256.New()
	var row strings.Builder
	for _, rel := range paths {
		row.Reset()
		row.WriteString(rel)
		row.WriteByte(0)

		abs := filepath.Join(v0Dir, filepath.FromSlash(rel))
		st, err := os.Lstat(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// File disappeared between collect*SourcePaths and
				// here. Match the row layout migrate.ManifestSHA256
				// uses for the same case ("<rel>\0<size>\0missing\n")
				// so a future audit tool can parse both checksums with
				// one parser. NOTE: the recorded size is 0 here, which
				// differs from ManifestSHA256 — the latter retains the
				// pre-disappear size from its initial WalkDir pass.
				// Layout matches; values may not be byte-equal across
				// the two implementations on a race.
				row.WriteString("0")
				row.WriteByte(0)
				row.WriteString("missing\n")
				h.Write([]byte(row.String()))
				continue
			}
			return "", fmt.Errorf("checksum lstat %s: %w", abs, err)
		}
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("checksum %s: %w", abs, ErrNotRegular)
		}

		fmt.Fprintf(&row, "%d", st.Size())
		row.WriteByte(0)

		sum, err := fileChecksumRO(v0Dir, abs)
		if err != nil {
			return "", err
		}
		row.WriteString(sum)
		row.WriteByte('\n')
		h.Write([]byte(row.String()))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileChecksumRO returns hex-encoded SHA256 of one v0 file via the
// read-only open guard. Buffered streaming via io.Copy so the helper
// stays memory-safe on multi-GB messages.jsonl files.
func fileChecksumRO(v0Dir, path string) (string, error) {
	f, err := openV0(v0Dir, path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("checksum read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// addLeafIfRegular appends path's V0Dir-relative form to paths if path
// is a real regular file (not a symlink, not a dir). Used by every
// collect*SourcePaths helper so the symlink-rejection rule is uniform
// — letting a symlink-as-leaf into the checksum would let a swap-in
// post-manifest mutate the hash without changing imported content.
//
// In addition to the leaf Lstat, the resolved path is fed through
// assertUnderRoot so a parent-component symlink that escapes V0Dir is
// rejected even when the leaf itself is a regular file. Without this
// guard, an attacker who can plant a `agents/<id>` symlink → /etc could
// smuggle external persona/MEMORY content into the checksum without
// the importer's reader ever opening those bytes.
//
// On any non-fs.ErrNotExist Lstat error we surface the failure: a
// permission glitch on a single v0 file should not silently produce
// a checksum that omits it.
func addLeafIfRegular(v0Dir, path string, paths []string) ([]string, error) {
	st, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return paths, nil
		}
		return paths, fmt.Errorf("lstat %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return paths, nil
	}
	if err := assertUnderRoot(v0Dir, path); err != nil {
		// fs.ErrNotExist from EvalSymlinks (broken symlink in a parent
		// component) should not crash the collector; treat it the same
		// way as a missing leaf and skip.
		if errors.Is(err, fs.ErrNotExist) {
			return paths, nil
		}
		return paths, err
	}
	rel, err := filepath.Rel(v0Dir, path)
	if err != nil {
		return paths, fmt.Errorf("rel %s: %w", path, err)
	}
	return append(paths, filepath.ToSlash(rel)), nil
}

// collectAgentsSourcePaths enumerates the v0 files the agents importer
// scans, in V0Dir-relative forward-slash form. Returned list is the
// input to domainChecksum — it covers every file the directory walk
// touches, including orphan agent dirs the row-copy loop skips. See
// the scan-vs-import gap note below.
//
// Includes:
//   - agents/agents.json (if present)
//   - agents/<id>/persona.md
//   - agents/<id>/MEMORY.md
//   - agents/<id>/memory/**/*.md
//
// Excludes the agents importer's *uninvolved* siblings:
//   - agents/<id>/messages.jsonl  (messages domain)
//   - agents/groupdms/**          (groupdms domain)
//
// Walks via readDirV0 / walkDirV0 — both refuse symlinked directories
// and assert resolved paths stay under V0Dir. A directory-component
// symlink that resolves outside V0Dir would otherwise let an attacker
// (or a misconfigured rsync) inject extra rows into the checksum
// without those bytes being visible to the importer's reader.
//
// NOTE: this enumerates whatever is on disk under v0/agents/, not the
// list of agent ids in agents.json. An orphan agent dir (subdir present,
// id absent from the manifest) therefore contributes to the checksum
// even though the importer does not actually copy its rows. This is the
// intended semantics: source_checksum records "what was scanned" rather
// than "what was imported", so a future v0 mutation that introduces an
// orphan dir is detectable as drift on a subsequent re-run.
//
// Missing files are simply omitted; the checksum row encoding handles
// the empty case (no rows hashed → SHA256 of "").
func collectAgentsSourcePaths(v0Dir string) ([]string, error) {
	var paths []string
	base := agentsBaseDir(v0Dir)

	manifest := filepath.Join(base, "agents.json")
	updated, err := addLeafIfRegular(v0Dir, manifest, paths)
	if err != nil {
		return nil, err
	}
	paths = updated

	entries, err := readDirV0(v0Dir, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return paths, nil
		}
		return nil, fmt.Errorf("readdir agents: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// "groupdms" is a sibling domain — its files belong to
		// collectGroupDMsSourcePaths, not this importer's checksum.
		if e.Name() == "groupdms" {
			continue
		}
		agentRoot := filepath.Join(base, e.Name())

		for _, leaf := range []string{"persona.md", "MEMORY.md"} {
			leafPath := filepath.Join(agentRoot, leaf)
			updated, err := addLeafIfRegular(v0Dir, leafPath, paths)
			if err != nil {
				return nil, err
			}
			paths = updated
		}

		memRoot := filepath.Join(agentRoot, "memory")
		// readDirV0 / walkDirV0 reject symlinked directories, so a
		// `memory/` linked outside V0Dir is a hard error rather than a
		// silent walk through someone else's filesystem. ErrNotExist
		// is the common case (no memory dir for this agent) and is
		// folded into a no-op below.
		if _, err := os.Lstat(memRoot); err != nil {
			continue
		}
		walkErr := walkDirV0(v0Dir, memRoot, func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				// Symlink-dir rejection from walkDirV0 surfaces here;
				// skip the offending entry rather than abort the whole
				// importer (matches the warn-and-continue posture
				// elsewhere). A genuine read error in v0 land is rare
				// and would fail at the read-content step anyway.
				return nil
			}
			if d == nil || d.IsDir() {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".md") {
				return nil
			}
			rel, err := filepath.Rel(v0Dir, p)
			if err != nil {
				return nil
			}
			paths = append(paths, filepath.ToSlash(rel))
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("walk memory %s: %w", memRoot, walkErr)
		}
	}
	return paths, nil
}

// collectMessagesSourcePaths enumerates each agent's messages.jsonl.
// Same scan-vs-import caveat as collectAgentsSourcePaths: an orphan
// agent dir on disk contributes to the checksum even when the messages
// importer skips it (because ListAgents won't return its id).
func collectMessagesSourcePaths(v0Dir string) ([]string, error) {
	var paths []string
	base := agentsBaseDir(v0Dir)
	entries, err := readDirV0(v0Dir, base)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return paths, nil
		}
		return nil, fmt.Errorf("readdir agents: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "groupdms" {
			continue
		}
		p := filepath.Join(base, e.Name(), "messages.jsonl")
		updated, err := addLeafIfRegular(v0Dir, p, paths)
		if err != nil {
			return nil, err
		}
		paths = updated
	}
	return paths, nil
}

// collectGroupDMsSourcePaths enumerates groups.json plus per-group
// messages.jsonl under v0/agents/groupdms. Same scan-vs-import caveat
// applies: an orphan group dir without a corresponding entry in
// groups.json is hashed but not imported.
func collectGroupDMsSourcePaths(v0Dir string) ([]string, error) {
	var paths []string
	root := groupsDir(v0Dir)

	manifest := filepath.Join(root, "groups.json")
	updated, err := addLeafIfRegular(v0Dir, manifest, paths)
	if err != nil {
		return nil, err
	}
	paths = updated

	entries, err := readDirV0(v0Dir, root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return paths, nil
		}
		return nil, fmt.Errorf("readdir groupdms: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name(), "messages.jsonl")
		updated, err := addLeafIfRegular(v0Dir, p, paths)
		if err != nil {
			return nil, err
		}
		paths = updated
	}
	return paths, nil
}
