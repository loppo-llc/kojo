package importers

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// compactionsImporter is a no-op marker for the v1 `compactions` table.
// Domain key: "compactions".
//
// Why no-op: v0 reserves a top-level `<v0>/compactions/` directory in the
// layout (see design doc §5.5 row "compactions/" → `compactions`) but
// has NO writer for it — every shipped v0 build leaves the directory
// empty. v0's historical compaction implementation (since removed
// from the codebase) consolidated MEMORY.md in place and never
// emitted a per-range archive that would map onto the v1 compactions
// schema (id, agent_id, range_start, range_end, body, body_sha256).
// There is therefore no v0 file format for this importer to translate.
//
// What this importer does:
//   - Records the domain in migration_status with phase="imported" and
//     imported_count=0 so the migration log is *complete* — every
//     domain in the design doc's table appears, and an operator
//     auditing the run can confirm the directory was processed (rather
//     than silently omitted from the migration scope).
//   - Hashes whatever files happen to live under <v0>/compactions/
//     into source_checksum so a future re-run against a v0 dir that
//     someone hand-populated surfaces as a checksum drift instead of
//     vanishing into the no-op branch. domainChecksum opens each leaf
//     O_RDONLY (via openV0) to compute its sha256 — no parse / import
//     happens, but the bytes ARE read for the fingerprint.
//   - Logs a warning per unexpected leaf so an operator who DID place
//     content there sees it called out (the data is left untouched in
//     v0 — the importer is read-only by contract — and survives a
//     possible future cutover, which is the safest disposition for
//     bytes whose semantics we don't know).
//
// Order: the compactions schema declares a foreign key on agents.id
// (ON DELETE CASCADE), so any future non-empty importer would need to
// run after agentsImporter. We honour that ordering today even though
// no rows are emitted — flipping the order later would require a
// register.go reshuffle.
type compactionsImporter struct{}

func (compactionsImporter) Domain() string { return "compactions" }

func (compactionsImporter) Run(ctx context.Context, st *store.Store, opts migrate.Options) error {
	if done, err := alreadyImported(ctx, st, "compactions"); err != nil {
		return err
	} else if done {
		return nil
	}

	logger := slog.Default().With("importer", "compactions")

	srcPaths, err := collectCompactionsSourcePaths(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("collect compactions source paths: %w", err)
	}
	checksum, err := domainChecksum(opts.V0Dir, srcPaths)
	if err != nil {
		return fmt.Errorf("checksum compactions sources: %w", err)
	}

	// Warn on every non-empty leaf under <v0>/compactions/. Any content
	// here is unexpected (no v0 writer); the operator should see it
	// surfaced rather than have it silently swept past. The bytes ARE
	// read once (by domainChecksum above, O_RDONLY via openV0) for the
	// fingerprint, but never parsed or imported — the file format is
	// undefined and a future v1 reader would have nothing to do with
	// them.
	for _, rel := range srcPaths {
		logger.Warn("compactions: unexpected file under <v0>/compactions/ left untouched (no v0 writer / no v1 importer for this format)",
			"path", rel)
	}

	return markImported(ctx, st, "compactions", 0, checksum)
}

// collectCompactionsSourcePaths enumerates every regular file under
// <v0>/compactions/ in V0Dir-relative forward-slash form. The list is
// the input to domainChecksum so a re-run against a hand-populated
// directory surfaces as a checksum drift rather than disappearing into
// the no-op branch.
//
// Walks via walkDirV0 so symlinked directories are rejected (defense in
// depth: same reasoning as collectAgentsSourcePaths). An absent
// compactions/ dir is the common case (every shipped v0 build leaves it
// empty — and many installs lack the dir entirely) and is folded into
// an empty list.
//
// Error posture inside the walk callback:
//   - ErrNotRegular (walkDirV0 rejecting a symlink at any depth) →
//     log + skip. The leaf is NOT hashed. We log so an operator can
//     spot that they planted bytes that our drift-detection misses.
//   - any other walk error (permission denied, racing rmdir, etc.) →
//     surface upward. Silently dropping a subtree from source_checksum
//     would let drift slip through: a hand-populated subdir that
//     happens to be unreadable would land in v1 as "imported" with the
//     same checksum as an empty dir, defeating the only thing this
//     marker importer does.
//   - non-regular leaf (FIFO, device, etc.) that walkDirV0 *didn't*
//     reject (because Type() isn't symlink) → log + skip. Same
//     visibility rationale.
func collectCompactionsSourcePaths(v0Dir string) ([]string, error) {
	logger := slog.Default().With("importer", "compactions")
	root := filepath.Join(v0Dir, "compactions")
	info, err := os.Lstat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("lstat compactions: %w", err)
	}
	// Symlinked directory: refuse. Following a root symlink would let
	// domainChecksum open bytes from outside V0Dir O_RDONLY for the
	// fingerprint — the importer's own logic never *parses* those bytes,
	// but they would still be read off disk and folded into the
	// drift-detection hash, which is exactly the coverage hole the other
	// collect*SourcePaths helpers close.
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%s: %w", root, ErrNotRegular)
	}

	var paths []string
	walkErr := walkDirV0(v0Dir, root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			// walkDirV0's own injected ErrNotRegular for symlink entries
			// is recoverable — surface a warning so an operator notices
			// that bytes outside V0Dir's coverage are present. Anything
			// else (permission denied, OS-level walk failure) bubbles up:
			// silently dropping a subtree from the fingerprint defeats
			// the marker importer's only job.
			if errors.Is(werr, ErrNotRegular) {
				logger.Warn("compactions: skipping non-regular entry under <v0>/compactions/ (excluded from source_checksum)",
					"path", p, "err", werr)
				return nil
			}
			return werr
		}
		if d == nil || d.IsDir() {
			return nil
		}
		// Non-regular leaf (FIFO, device, socket, etc.). walkDirV0 only
		// rejects symlinks proactively, so other irregular types reach
		// here and would otherwise be silently excluded. Surface a
		// warning so the operator sees that drift detection has a gap
		// for this entry.
		if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
			logger.Warn("compactions: skipping non-regular leaf under <v0>/compactions/ (excluded from source_checksum)",
				"path", p, "type", d.Type().String())
			return nil
		}
		rel, err := filepath.Rel(v0Dir, p)
		if err != nil {
			// Rel can only fail on a path that filepath.Walk already
			// produced from filepath.Join(root, ...) under a cleaned
			// v0Dir — so a failure here is a programming error, not a
			// runtime condition. Surface it rather than silently drop
			// the leaf from the fingerprint.
			return fmt.Errorf("rel %s: %w", p, err)
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk compactions: %w", walkErr)
	}
	return paths, nil
}
