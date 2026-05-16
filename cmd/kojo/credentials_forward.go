package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// credentialKeySize is the AES-256 key length internal/agent/credential.go
// writes / expects in credentials.key. A short or oversized file is a
// fail-closed condition in loadOrCreateKey, so a malformed v0 key gets
// rejected here rather than copied into v1 and bricking startup.
const credentialKeySize = 32

// userOwnedCredentialTables lists the tables internal/agent/credential.go
// populates with per-user data. The carry-forward refuses to overwrite a
// v1 credentials.db that already has rows in ANY of these — a single
// `credentials` row check would silently destroy notify_tokens / settings
// the operator entered after first-launching v1 prior to --migrate.
var userOwnedCredentialTables = [...]string{
	"credentials", "notify_tokens", "settings",
}

// applyCredentialsCarryForward copies the v0 encrypted credential store
// (credentials.{db,db-wal,key}) into the v1 dir root. The migration's
// blobs importer archives the older per-agent credentials.{json,key}
// pattern as blob_refs, but the late-v0 SQLite store at
// <v0>/credentials.db is canonical for any install upgraded past commit
// 33feff0 and gets left behind otherwise, surfacing as "credentials
// reset on upgrade".
//
// Called BEFORE migrate.Run (see migrate.go for the timing rationale);
// migrate.Run's --migrate-restart wipe preserves credentialFiles, so
// landing the files first is safe across both fresh and restart paths.
//
// Safety rules:
//   - skip silently if v0 has no credentials.db (older v0 install).
//   - if v1 credentials.db holds rows in ANY user-owned table
//     (credentials / notify_tokens / settings), refuse to overwrite and
//     emit a warning.
//   - if probing v1 db fails for reasons other than "table absent"
//     (corrupt file, locked file, etc.), refuse — better to surface
//     than to silently destroy.
//   - v0 key must be a 32-byte regular file. A malformed key would
//     fail-close at v1 startup; better to refuse here.
//   - db and key are staged into *.tmp-* siblings; both must stage
//     successfully before either is renamed into place. A failure
//     during staging cleans up any tmp files and aborts without
//     partial state.
//   - db-shm is NEVER copied (it is a transient SQLite mapping
//     regenerable from db-wal); the destination shm is removed so the
//     v1 SQLite process can rebuild it.
//
// Best-effort: every failure becomes a warning so the v1 install can
// still boot. The returned warnings are surfaced by the caller.
func applyCredentialsCarryForward(ctx context.Context, v0Path, v1Path string, logger *slog.Logger) []string {
	v0DB := filepath.Join(v0Path, "credentials.db")
	v0Key := filepath.Join(v0Path, "credentials.key")
	v0WAL := filepath.Join(v0Path, "credentials.db-wal")

	if _, err := os.Stat(v0DB); err != nil {
		// No v0 SQLite store — nothing to forward. Pre-SQLite v0 installs
		// fall through to the blobs-importer archival path.
		return nil
	}
	if err := validateCredentialKey(v0Key); err != nil {
		return []string{fmt.Sprintf("credentials forward: v0 key not usable (%v) — refusing to copy db without a valid key", err)}
	}

	v1DB := filepath.Join(v1Path, "credentials.db")
	if hasRows, table, err := v1HasUserCredentialData(ctx, v1DB); err != nil {
		return []string{"credentials forward: probe v1 db: " + err.Error()}
	} else if hasRows {
		return []string{fmt.Sprintf("credentials forward: v1 credentials.db already has rows in table %q; not overwriting", table)}
	}

	// Stage db (+ optional wal) + key into siblings, then commit in order
	// db → wal → key. If any stage fails, every tmp is removed and the
	// operation aborts before touching the live files.
	type stagedFile struct {
		dst, tmp string
	}
	staged := make([]stagedFile, 0, 3)
	cleanupTmps := func() {
		for _, s := range staged {
			_ = os.Remove(s.tmp)
		}
	}

	// Stage order matches commit order (key → wal → db). Stage order
	// itself doesn't matter, but the slice order does: see the commit
	// loop below for the durability rationale.
	keyDst := filepath.Join(v1Path, "credentials.key")
	keyTmp, err := stageCopy(v0Key, keyDst)
	if err != nil {
		return []string{"credentials forward: stage key: " + err.Error()}
	}
	staged = append(staged, stagedFile{dst: keyDst, tmp: keyTmp})

	walDst := filepath.Join(v1Path, "credentials.db-wal")
	if _, err := os.Stat(v0WAL); err == nil {
		walTmp, err := stageCopy(v0WAL, walDst)
		if err != nil {
			cleanupTmps()
			return []string{"credentials forward: stage wal: " + err.Error()}
		}
		staged = append(staged, stagedFile{dst: walDst, tmp: walTmp})
	} else if !errors.Is(err, os.ErrNotExist) {
		cleanupTmps()
		return []string{"credentials forward: stat v0 wal: " + err.Error()}
	}

	dbTmp, err := stageCopy(v0DB, v1DB)
	if err != nil {
		cleanupTmps()
		return []string{"credentials forward: stage db: " + err.Error()}
	}
	staged = append(staged, stagedFile{dst: v1DB, tmp: dbTmp})

	// Remove stale destination shm BEFORE committing. shm is a transient
	// memory map and must be rebuilt by SQLite from the new wal, so we
	// drop it unconditionally rather than copying v0's shm. wal cleanup
	// is handled inline below: if v0 has no wal, the commit loop won't
	// stage one, and we drop any stale dst wal here so v1 SQLite does
	// not pair the new db with a prior wal.
	v0HasWAL := false
	if _, err := os.Stat(v0WAL); err == nil {
		v0HasWAL = true
	}
	if !v0HasWAL {
		// Best-effort: replaceFile handles existing wal gracefully on
		// Windows too via the same backup/restore dance the commit
		// loop uses.
		_ = os.Remove(walDst)
	}
	_ = os.Remove(filepath.Join(v1Path, "credentials.db-shm"))

	// Commit. Order is key → wal → db, with each rename using a
	// Windows-safe replace (POSIX os.Rename overwrites in place; on
	// Windows it errors EEXIST and we have to backup-then-rename).
	//
	// Two-phase batch commit: each replaceFile call returns
	// (commit, rollback) — the renamed file is on disk but its
	// previous content is preserved as a sibling backup until
	// `commit()` removes the backup. If any later step fails,
	// `rollback()` restores every previously-replaced file from
	// its backup. This avoids the "key/wal published, db replace
	// failed → mismatched credential store" hole.
	type undo struct {
		commit, rollback func()
	}
	undos := make([]undo, 0, len(staged))
	for _, s := range staged {
		commit, rollback, err := replaceFile(s.tmp, s.dst)
		if err != nil {
			// Walk back through every previously-replaced file.
			for i := len(undos) - 1; i >= 0; i-- {
				undos[i].rollback()
			}
			cleanupTmps()
			return []string{fmt.Sprintf("credentials forward: commit %s: %v", filepath.Base(s.dst), err)}
		}
		undos = append(undos, undo{commit: commit, rollback: rollback})
	}
	// fsync parent dir BEFORE removing backups so the renames are
	// durable on disk first. Without this ordering, a crash between
	// the unlink-bak step and the eventual fsync could leave the
	// renames pending in the page cache but the bak unlinks already
	// flushed — making the new content non-durable AND the rollback
	// path empty. fsync now ensures: post-crash, either the new dst
	// is durable + bak optionally still present (cosmetic), or the
	// old dst is intact + bak intact (clean retry).
	if dir, err := os.Open(v1Path); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	// All replaces succeeded; drop every backup file.
	for _, u := range undos {
		u.commit()
	}
	// Final fsync to flush the bak-unlink directory entries.
	if dir, err := os.Open(v1Path); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}

	for _, s := range staged {
		logger.Info("credentials forward: committed", "leaf", filepath.Base(s.dst))
	}
	return nil
}

// v1HasUserCredentialData reports whether the v1 credentials.db holds
// any rows the operator entered after a first-launch-before-migrate
// (credentials, notify_tokens, or settings). Returns the offending
// table name on a hit. A missing db file is "no rows, no error". A
// missing table is "no rows, no error" — older v1 builds may predate
// notify_tokens. Any OTHER sqlite error (corrupt, locked, permission)
// is propagated: silently treating those as empty would let the
// carry-forward overwrite a damaged but recoverable store.
func v1HasUserCredentialData(ctx context.Context, dbPath string) (bool, string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "", nil
		}
		return false, "", err
	}
	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return false, "", err
	}
	defer db.Close()
	for _, table := range userOwnedCredentialTables {
		var n int
		// Table identifier is a constant from userOwnedCredentialTables;
		// not user-supplied, so direct interpolation is safe.
		err := db.QueryRowContext(openCtx,
			fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, table)).Scan(&n)
		if err != nil {
			if isNoSuchTable(err) {
				continue
			}
			return false, "", fmt.Errorf("query %s: %w", table, err)
		}
		if n > 0 {
			return true, table, nil
		}
	}
	return false, "", nil
}

// isNoSuchTable matches modernc.org/sqlite's "no such table" surface,
// which is the only sqlite error v1HasUserCredentialData treats as "0
// rows" (everything else is an actual problem worth refusing on).
func isNoSuchTable(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no such table")
}

// validateCredentialKey rejects a v0 credentials.key that would
// fail-close at v1 startup: non-regular files (symlink / dir / device)
// or any size other than 32 bytes. Returns nil if usable.
func validateCredentialKey(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeType != 0 {
		return fmt.Errorf("not a regular file (%v)", info.Mode())
	}
	if info.Size() != credentialKeySize {
		return fmt.Errorf("expected %d bytes, got %d", credentialKeySize, info.Size())
	}
	return nil
}

// replaceFile renames tmp → dst with backup/restore so the operation
// is recoverable on Windows (where os.Rename refuses to overwrite an
// existing dst and returns EEXIST). On POSIX the underlying os.Rename
// is already atomic-overwrite, but the same backup-restore code path
// runs unconditionally for behavioral parity across platforms.
//
// Sequence:
//  1. If dst exists, rename it to dst.bak-<rand>.
//  2. Rename tmp → dst.
//  3. Return commit (remove backup) and rollback (restore backup)
//     callbacks so a batch caller can defer the choice across
//     multiple replaceFile calls. Without batch deferral, an earlier
//     replace's backup would be irretrievably gone by the time a
//     later step fails.
//
// On failure between steps 1 and 2 the caller sees an error, the
// (still-present) backup is restored to dst, and the tmp is dropped.
// A crash before step 2 leaves dst missing + dst.bak-* on disk; the
// next --migrate run probes "no v1 file" and either retries or
// carries forward fresh. A crash between 2 and 3 leaves the new dst
// in place plus a stale bak file (cosmetic — operator can rm). The
// commit/rollback callbacks are nil-safe; callers can always invoke
// one of them.
func replaceFile(tmp, dst string) (commit func(), rollback func(), err error) {
	var bak string
	if _, statErr := os.Stat(dst); statErr == nil {
		f, ferr := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".bak-*")
		if ferr != nil {
			return nil, nil, fmt.Errorf("backup dst: %w", ferr)
		}
		bak = f.Name()
		_ = f.Close()
		// CreateTemp made an empty file at bak; remove it so the
		// rename below can take that name. (Windows os.Rename
		// can't overwrite either.)
		if rerr := os.Remove(bak); rerr != nil {
			return nil, nil, fmt.Errorf("backup prep: %w", rerr)
		}
		if rerr := os.Rename(dst, bak); rerr != nil {
			return nil, nil, fmt.Errorf("backup dst: %w", rerr)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("stat dst: %w", statErr)
	}

	if rerr := os.Rename(tmp, dst); rerr != nil {
		// Restore the backup before surfacing the error so the
		// caller doesn't see a half-replaced state.
		if bak != "" {
			if restoreErr := os.Rename(bak, dst); restoreErr != nil {
				return nil, nil, fmt.Errorf("rename + restore failed: %w (restore: %v)", rerr, restoreErr)
			}
		}
		_ = os.Remove(tmp)
		return nil, nil, rerr
	}
	commit = func() {
		if bak != "" {
			_ = os.Remove(bak)
		}
	}
	rollback = func() {
		if bak == "" {
			// No backup means dst didn't exist before; reversing
			// the publish is just removing the new dst.
			_ = os.Remove(dst)
			return
		}
		// Best-effort restore. On Windows the new dst must be
		// removed before the backup can be renamed over it.
		_ = os.Remove(dst)
		_ = os.Rename(bak, dst)
	}
	return commit, rollback, nil
}

// stageCopy copies src to a freshly-named "<dst>.tmp-*" sibling and
// returns the tmp path. The caller is responsible for renaming the tmp
// into dst (single-file atomic publish) or removing it (failed batch).
// Preserves source file permissions — important for credentials.key
// (0o600).
func stageCopy(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Chmod(tmpName, info.Mode().Perm()); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return tmpName, nil
}
