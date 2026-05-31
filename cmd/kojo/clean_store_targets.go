package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/loppo-llc/kojo/internal/blob"
	"github.com/loppo-llc/kojo/internal/store"
)

type blobGCEntry struct {
	URI  string
	Path string
}

type blobCleanPlan struct {
	OrphanFiles []string
	GCRefs      []blobGCEntry
	EmptyDirs   []string
}

func planBlobCleanup(ctx context.Context, st *store.Store, root string, maxAgeDays int) (*blobCleanPlan, error) {
	if st == nil {
		return nil, errors.New("store handle is required")
	}
	cutoff := store.NowMillis() - int64(maxAgeDays)*24*60*60*1000
	refs, err := st.ListBlobRefs(ctx, store.ListBlobRefsOptions{IncludeMarkedForGC: true})
	if err != nil {
		return nil, err
	}
	refByURI := make(map[string]*store.BlobRefRecord, len(refs))
	for _, r := range refs {
		refByURI[r.URI] = r
	}

	p := &blobCleanPlan{}
	plannedGC := make(map[string]bool)
	for _, scope := range []blob.Scope{blob.ScopeGlobal, blob.ScopeLocal, blob.ScopeMachine} {
		scopeRoot := filepath.Join(root, string(scope))
		if _, err := os.Stat(scopeRoot); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		counts := map[string]int{scopeRoot: 0}
		var dirs []string
		if err := filepath.WalkDir(scopeRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == scopeRoot {
				return nil
			}
			parent := filepath.Dir(path)
			if d.IsDir() {
				counts[path] = 0
				counts[parent]++
				dirs = append(dirs, path)
				return nil
			}
			if !d.Type().IsRegular() {
				counts[parent]++
				return nil
			}
			rel, err := filepath.Rel(scopeRoot, path)
			if err != nil {
				return err
			}
			uri := blob.BuildURI(scope, filepath.ToSlash(rel))
			if ref, ok := refByURI[uri]; ok {
				if ref.MarkedForGCAt > 0 && ref.MarkedForGCAt <= cutoff {
					p.GCRefs = append(p.GCRefs, blobGCEntry{URI: uri, Path: path})
					plannedGC[uri] = true
					return nil
				}
				counts[parent]++
				return nil
			}
			p.OrphanFiles = append(p.OrphanFiles, path)
			return nil
		}); err != nil {
			return nil, err
		}
		sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
		for _, dir := range dirs {
			if counts[dir] != 0 {
				continue
			}
			p.EmptyDirs = append(p.EmptyDirs, dir)
			counts[filepath.Dir(dir)]--
		}
	}
	for _, ref := range refs {
		if ref.MarkedForGCAt == 0 || ref.MarkedForGCAt > cutoff || plannedGC[ref.URI] {
			continue
		}
		scope, rel, err := blob.ParseURI(ref.URI)
		if err != nil {
			return nil, fmt.Errorf("parse GC blob URI %s: %w", ref.URI, err)
		}
		p.GCRefs = append(p.GCRefs, blobGCEntry{
			URI:  ref.URI,
			Path: filepath.Join(root, string(scope), filepath.FromSlash(rel)),
		})
	}
	sort.Strings(p.OrphanFiles)
	sort.Slice(p.GCRefs, func(i, j int) bool { return p.GCRefs[i].URI < p.GCRefs[j].URI })
	sort.Strings(p.EmptyDirs)
	return p, nil
}

func printBlobCleanPlan(p *blobCleanPlan, apply bool) {
	mode := "would remove"
	if apply {
		mode = "removing"
	}
	if p == nil || (len(p.OrphanFiles) == 0 && len(p.GCRefs) == 0 && len(p.EmptyDirs) == 0) {
		fmt.Fprintln(os.Stderr, "kojo: clean blobs: nothing to remove")
		return
	}
	fmt.Fprintf(os.Stderr, "kojo: clean blobs: %s %d orphan file(s), %d GC ref(s), %d empty dir(s)\n",
		mode, len(p.OrphanFiles), len(p.GCRefs), len(p.EmptyDirs))
	for _, path := range p.OrphanFiles {
		fmt.Fprintf(os.Stderr, "  orphan file: %s\n", path)
	}
	for _, ref := range p.GCRefs {
		fmt.Fprintf(os.Stderr, "  gc ref: %s (%s)\n", ref.URI, ref.Path)
	}
	for _, dir := range p.EmptyDirs {
		fmt.Fprintf(os.Stderr, "  empty dir: %s\n", dir)
	}
}

func applyBlobCleanPlan(ctx context.Context, p *blobCleanPlan, st *store.Store) []error {
	if p == nil {
		return nil
	}
	var errs []error
	for _, path := range p.OrphanFiles {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove orphan blob %s: %w", path, err))
		}
	}
	for _, ref := range p.GCRefs {
		if err := os.Remove(ref.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove GC blob %s: %w", ref.Path, err))
			continue
		}
		if err := st.DeleteBlobRef(ctx, ref.URI); err != nil {
			errs = append(errs, fmt.Errorf("delete blob ref %s: %w", ref.URI, err))
		}
	}
	sort.Slice(p.EmptyDirs, func(i, j int) bool { return len(p.EmptyDirs[i]) > len(p.EmptyDirs[j]) })
	for _, dir := range p.EmptyDirs {
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove empty blob dir %s: %w", dir, err))
		}
	}
	return errs
}

type deletedAgentEntry struct {
	ID        string
	Name      string
	DeletedAt int64
}

type agentCleanPlan struct {
	Cutoff int64
	Rows   []deletedAgentEntry
}

func planAgentCleanup(ctx context.Context, st *store.Store, maxAgeDays int) (*agentCleanPlan, error) {
	cutoff := store.NowMillis() - int64(maxAgeDays)*24*60*60*1000
	rows, err := st.DB().QueryContext(ctx, `
SELECT id, name, deleted_at
  FROM agents
 WHERE deleted_at IS NOT NULL AND deleted_at <= ?
 ORDER BY deleted_at ASC, id ASC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	p := &agentCleanPlan{Cutoff: cutoff}
	for rows.Next() {
		var e deletedAgentEntry
		if err := rows.Scan(&e.ID, &e.Name, &e.DeletedAt); err != nil {
			return nil, err
		}
		p.Rows = append(p.Rows, e)
	}
	return p, rows.Err()
}

func printAgentCleanPlan(p *agentCleanPlan, apply bool) {
	mode := "would hard-delete"
	if apply {
		mode = "hard-deleting"
	}
	if p == nil || len(p.Rows) == 0 {
		fmt.Fprintln(os.Stderr, "kojo: clean agents: nothing to remove")
		return
	}
	fmt.Fprintf(os.Stderr, "kojo: clean agents: %s %d soft-deleted agent(s)\n", mode, len(p.Rows))
	for _, row := range p.Rows {
		fmt.Fprintf(os.Stderr, "  agent: %s %q deleted_at=%s\n", row.ID, row.Name, time.UnixMilli(row.DeletedAt).Format(time.RFC3339))
	}
}

func applyAgentCleanPlan(ctx context.Context, p *agentCleanPlan, st *store.Store) error {
	if p == nil || len(p.Rows) == 0 {
		return nil
	}
	_, err := st.DB().ExecContext(ctx, `DELETE FROM agents WHERE deleted_at IS NOT NULL AND deleted_at <= ?`, p.Cutoff)
	return err
}

type eventCleanPlan struct {
	Cutoff      int64
	EventRows   int64
	MaxEventSeq int64
	OplogRows   int64
}

func planEventCleanup(ctx context.Context, st *store.Store, maxAgeDays int) (*eventCleanPlan, error) {
	cutoff := store.NowMillis() - int64(maxAgeDays)*24*60*60*1000
	p := &eventCleanPlan{Cutoff: cutoff}
	var maxSeq sql.NullInt64
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*), MAX(seq) FROM events WHERE ts < ?`, cutoff).Scan(&p.EventRows, &maxSeq); err != nil {
		return nil, err
	}
	if maxSeq.Valid {
		p.MaxEventSeq = maxSeq.Int64
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM oplog_applied WHERE applied_at < ?`, cutoff).Scan(&p.OplogRows); err != nil {
		return nil, err
	}
	return p, nil
}

func printEventCleanPlan(p *eventCleanPlan, apply bool) {
	mode := "would prune"
	if apply {
		mode = "pruning"
	}
	if p == nil || (p.EventRows == 0 && p.OplogRows == 0) {
		fmt.Fprintln(os.Stderr, "kojo: clean events: nothing to remove")
		return
	}
	fmt.Fprintf(os.Stderr, "kojo: clean events: %s %d event row(s), %d applied-op row(s); cutoff=%s\n",
		mode, p.EventRows, p.OplogRows, time.UnixMilli(p.Cutoff).Format(time.RFC3339))
}

func applyEventCleanPlan(ctx context.Context, p *eventCleanPlan, st *store.Store) error {
	if p == nil || (p.EventRows == 0 && p.OplogRows == 0) {
		return nil
	}
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE ts < ?`, p.Cutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oplog_applied WHERE applied_at < ?`, p.Cutoff); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if p.MaxEventSeq == 0 {
		return nil
	}
	prunedThrough := p.MaxEventSeq
	if rec, err := st.GetKV(ctx, store.ChangesKVNamespace, store.ChangesKVPrunedThrough); err == nil {
		if n, parseErr := strconv.ParseInt(rec.Value, 10, 64); parseErr == nil && n > prunedThrough {
			prunedThrough = n
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	_, err = st.PutKV(ctx, &store.KVRecord{
		Namespace: store.ChangesKVNamespace,
		Key:       store.ChangesKVPrunedThrough,
		Value:     strconv.FormatInt(prunedThrough, 10),
		Type:      store.KVTypeString,
		Scope:     store.KVScopeGlobal,
	}, store.KVPutOptions{})
	return err
}
