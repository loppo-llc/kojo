package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// runCleanLegacy is the `--clean legacy` target: sweep post-cutover
// on-disk legacy files whose canonical home is now a kv row.
//
// Why a separate file:
//
//   - clean_cmd.go's snapshot target operates on filesystem layout
//     alone (manifest presence + mtime). The legacy target needs to
//     correlate disk paths against kv rows, which means opening
//     kojo.db. Separating the two keeps the snapshot path runnable
//     from the cmd layer with no DB handle when the operator only
//     wants snapshot housekeeping.
//
//   - It documents in one place EVERY legacy file that survived a
//     2c-2 cutover. The runtime's lazy migration paths (LoadCronPaused,
//     readMarker, acquireCronLock, NewTokenStore) are scattered; this
//     file is the inventory.
//
// Safety gate: a file is REMOVED only when a kv row at its mapped
// (namespace, key) is present AND passes the same row-shape /
// value-format check the runtime applies before treating the row as
// authoritative. A row that fails the check is treated like a kv miss
// (Pending) — the runtime would fall back to the disk file (or fail
// closed), so dropping the disk file would lose the canonical state.
//
// Re-validation at apply time: the scan can race against the runtime.
// scan→apply re-runs GetKV + the same validator so a kv row that
// disappeared (or got corrupted) between scan and apply doesn't
// strand the file as the only surviving copy.

// legacyKind enumerates the legacy file shapes the sweep recognizes.
// Each kind maps a filesystem path (or a pattern that yields one)
// to a (namespace, key) kv lookup PLUS a row validator that mirrors
// the runtime's shape gate for that kind.
type legacyKind string

const (
	legacyKindCronPaused        legacyKind = "cron_paused"
	legacyKindCronLast          legacyKind = ".cron_last"
	legacyKindAutosummaryMarker legacyKind = "autosummary_marker"
	legacyKindOwnerToken        legacyKind = "owner.token"
	legacyKindAgentToken        legacyKind = "agent_tokens/<id>"
)

// validateKVRow returns nil if rec passes the runtime's shape /
// value gate for the given kind; otherwise an error describing the
// mismatch. The validators MUST stay in lockstep with the runtime
// reader (see internal/agent/store.go parseCronPausedRow,
// internal/agent/cron_lock_kv.go acquireCronLockDB,
// internal/agent/autosummary.go decodeMarkerKVRow,
// internal/auth/store_kv.go parseAuthKVValue).
func validateKVRow(kind legacyKind, rec *store.KVRecord) error {
	if rec == nil {
		return errors.New("nil kv record")
	}
	switch kind {
	case legacyKindCronPaused:
		if rec.Type != store.KVTypeString || rec.Scope != store.KVScopeGlobal || rec.Secret {
			return fmt.Errorf("row shape mismatch (type=%q scope=%q secret=%t)", rec.Type, rec.Scope, rec.Secret)
		}
		if rec.Value != "true" && rec.Value != "false" {
			return fmt.Errorf("value not in {true,false}: %q", rec.Value)
		}
	case legacyKindCronLast:
		if rec.Type != store.KVTypeString || rec.Scope != store.KVScopeMachine || rec.Secret {
			return fmt.Errorf("row shape mismatch (type=%q scope=%q secret=%t)", rec.Type, rec.Scope, rec.Secret)
		}
		if _, err := strconv.ParseInt(rec.Value, 10, 64); err != nil {
			return fmt.Errorf("value not a millis int64: %w", err)
		}
	case legacyKindAutosummaryMarker:
		if rec.Type != store.KVTypeJSON || rec.Scope != store.KVScopeGlobal || rec.Secret {
			return fmt.Errorf("row shape mismatch (type=%q scope=%q secret=%t)", rec.Type, rec.Scope, rec.Secret)
		}
		// Unmarshal into a struct that mirrors internal/agent
		// autoSummaryMarker. The runtime uses json.Unmarshal into
		// that struct (decodeMarkerKVRow) and treats a failure as
		// "zero-value marker" — meaning the disk file would still be
		// the only readable copy. So a value like `[]` or
		// `{"lastN":"bad"}` is json.Valid but runtime-invalid;
		// matching the runtime's stricter Unmarshal here keeps
		// Redundant in lockstep with "the runtime accepts this row".
		//
		// MUST stay in sync with internal/agent.autoSummaryMarker.
		var marker struct {
			LastAt   time.Time `json:"lastAt"`
			LastHash string    `json:"lastHash"`
			LastN    int       `json:"lastN"`
		}
		if err := json.Unmarshal([]byte(rec.Value), &marker); err != nil {
			return fmt.Errorf("value not unmarshallable into autoSummaryMarker: %w", err)
		}
	case legacyKindOwnerToken, legacyKindAgentToken:
		if rec.Type != store.KVTypeString || rec.Scope != store.KVScopeGlobal || rec.Secret {
			return fmt.Errorf("row shape mismatch (type=%q scope=%q secret=%t)", rec.Type, rec.Scope, rec.Secret)
		}
		const prefix = "sha256:"
		hash, ok := strings.CutPrefix(rec.Value, prefix)
		if !ok {
			return fmt.Errorf("missing %q prefix", prefix)
		}
		if !isLowerHex64(hash) {
			return fmt.Errorf("malformed lowercase 64-hex (len=%d)", len(hash))
		}
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	return nil
}

// isLowerHex64 mirrors auth/store_kv.go's strict lowercase 64-hex rule.
// Local copy avoids exporting the auth helper just for the cmd layer.
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// legacyEntry is one disk path the sweep considered.
//
// State machine:
//
//   - Redundant: kv row exists AND validateKVRow == nil. Safe to drop;
//     re-validated at apply time.
//   - Pending:   kv row missing, malformed, or probe failed. Don't
//     drop. PendingReason holds the why.
type legacyEntry struct {
	Path          string
	Kind          legacyKind
	KVNamespace   string
	KVKey         string
	ModTime       time.Time
	PendingReason string // empty for Redundant
}

// legacyCleanPlan is the result of a scan: redundant entries are the
// removal candidates; pending entries are reported but never removed.
type legacyCleanPlan struct {
	Redundant []legacyEntry
	Pending   []legacyEntry
}

// legacyAgentIDPattern restricts <id> directory / file names to the
// same alphabet the auth store uses (see internal/auth/store.go).
// Matching here keeps the sweep from trying to GetKV on a path-
// traversal name like ".." or treating a stray dotfile as an agent
// dir.
var legacyAgentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,128}$`)

// planLegacyCleanup walks the legacy filesystem inventory and probes
// kv for the canonical mirror of each path. Returns a plan with
// Redundant (drop-eligible) and Pending entries. Errors that prevent
// the whole scan (e.g. configDir is inaccessible) are returned;
// per-entry errors land in entry.PendingReason so the dry-run output
// can show them.
func planLegacyCleanup(ctx context.Context, kv *store.Store, configDirPath string) (*legacyCleanPlan, error) {
	if kv == nil {
		return nil, errors.New("clean legacy: kv store is required")
	}
	if configDirPath == "" {
		return nil, errors.New("clean legacy: configDir is empty")
	}
	plan := &legacyCleanPlan{}

	// 1. <configdir>/agents/cron_paused — singleton.
	if err := addEntry(ctx, kv, plan, legacyEntry{
		Path:        filepath.Join(configDirPath, "agents", "cron_paused"),
		Kind:        legacyKindCronPaused,
		KVNamespace: "scheduler",
		KVKey:       "paused",
	}); err != nil {
		return nil, err
	}

	// 2. <configdir>/auth/owner.token — singleton.
	if err := addEntry(ctx, kv, plan, legacyEntry{
		Path:        filepath.Join(configDirPath, "auth", "owner.token"),
		Kind:        legacyKindOwnerToken,
		KVNamespace: "auth",
		KVKey:       "owner.token",
	}); err != nil {
		return nil, err
	}

	// 3. <configdir>/agents/<id>/.cron_last — per-agent throttle.
	// 4. <configdir>/agents/<id>/autosummary_marker — per-agent.
	if err := walkAgentDirs(ctx, kv, plan, configDirPath); err != nil {
		return nil, fmt.Errorf("clean legacy: walk agents dir: %w", err)
	}

	// 5. <configdir>/auth/agent_tokens/<id> — per-agent token hash.
	if err := walkAgentTokens(ctx, kv, plan, configDirPath); err != nil {
		return nil, fmt.Errorf("clean legacy: walk auth/agent_tokens: %w", err)
	}

	return plan, nil
}

// addEntry stats the path; if absent it is silently skipped. Real
// I/O errors on Stat are surfaced as a scan error rather than
// silently dropped — a permission/I/O issue against a file the
// operator believes should be on the cleanup list must not be
// invisible.
//
// On stat success, probes kv at (namespace, key). The entry routes
// to Redundant only when the row is present AND passes the kind's
// row-shape / value gate; otherwise it lands in Pending with the
// failure reason. We never delete on uncertainty.
func addEntry(ctx context.Context, kv *store.Store, plan *legacyCleanPlan, e legacyEntry) error {
	info, err := os.Stat(e.Path)
	switch {
	case err == nil:
		// continue
	case errors.Is(err, fs.ErrNotExist):
		// File already swept (previous --clean run or runtime lazy
		// migration). Nothing to do.
		return nil
	default:
		// EACCES / EIO etc. — operator must see this.
		return fmt.Errorf("stat %s: %w", e.Path, err)
	}
	if info.IsDir() {
		// Defensive: every kind in this file is a regular file. If
		// somebody mkdir'd cron_paused/ by accident, leave it alone.
		return nil
	}
	e.ModTime = info.ModTime()

	rec, gerr := kv.GetKV(ctx, e.KVNamespace, e.KVKey)
	switch {
	case gerr == nil:
		if vErr := validateKVRow(e.Kind, rec); vErr != nil {
			e.PendingReason = "kv row malformed: " + vErr.Error()
			plan.Pending = append(plan.Pending, e)
			return nil
		}
		plan.Redundant = append(plan.Redundant, e)
	case errors.Is(gerr, store.ErrNotFound):
		e.PendingReason = pendingMissReason(e.Kind)
		plan.Pending = append(plan.Pending, e)
	default:
		e.PendingReason = "kv probe error: " + gerr.Error()
		plan.Pending = append(plan.Pending, e)
	}
	return nil
}

// pendingMissReason explains why a kv-miss entry is being kept. The
// reasons differ per kind: cron_paused / autosummary_marker / auth
// tokens are migrated into kv on next runtime read; .cron_last is
// just unlinked (the value is a stale throttle stamp, not data the
// runtime needs to preserve).
func pendingMissReason(kind legacyKind) string {
	switch kind {
	case legacyKindCronLast:
		return "kv miss; runtime will unlink on next acquire"
	default:
		return "kv miss; runtime will migrate disk → kv on next read"
	}
}

// walkAgentDirs scans <configdir>/agents/<id>/ for the per-agent
// legacy files (.cron_last, autosummary_marker). agentID is taken
// from the dirname; entries failing the agentID pattern are skipped.
func walkAgentDirs(ctx context.Context, kv *store.Store, plan *legacyCleanPlan, configDirPath string) error {
	root := filepath.Join(configDirPath, "agents")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		id := ent.Name()
		if !legacyAgentIDPattern.MatchString(id) {
			continue
		}
		if err := addEntry(ctx, kv, plan, legacyEntry{
			Path:        filepath.Join(root, id, ".cron_last"),
			Kind:        legacyKindCronLast,
			KVNamespace: "scheduler",
			KVKey:       "cron_last/" + id,
		}); err != nil {
			return err
		}
		if err := addEntry(ctx, kv, plan, legacyEntry{
			Path:        filepath.Join(root, id, "autosummary_marker"),
			Kind:        legacyKindAutosummaryMarker,
			KVNamespace: "autosummary",
			KVKey:       id,
		}); err != nil {
			return err
		}
	}
	return nil
}

// walkAgentTokens scans <configdir>/auth/agent_tokens/ for per-agent
// token-hash files. agentID is the filename; entries failing the
// agentID pattern are skipped.
func walkAgentTokens(ctx context.Context, kv *store.Store, plan *legacyCleanPlan, configDirPath string) error {
	root := filepath.Join(configDirPath, "auth", "agent_tokens")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		id := ent.Name()
		if !legacyAgentIDPattern.MatchString(id) {
			continue
		}
		if err := addEntry(ctx, kv, plan, legacyEntry{
			Path:        filepath.Join(root, id),
			Kind:        legacyKindAgentToken,
			KVNamespace: "auth",
			KVKey:       "agent_tokens/" + id,
		}); err != nil {
			return err
		}
	}
	return nil
}

// printLegacyCleanPlan formats the dry-run / apply summary for the
// operator. Pending entries carry per-kind reason strings so a stuck
// migration is visible without grepping logs.
func printLegacyCleanPlan(plan *legacyCleanPlan, apply bool) {
	verb := "would remove"
	if apply {
		verb = "removing"
	}
	if n := len(plan.Redundant); n > 0 {
		fmt.Fprintf(os.Stderr, "%s %d redundant legacy file(s) (kv mirror exists and validates):\n", verb, n)
		for _, e := range plan.Redundant {
			fmt.Fprintf(os.Stderr, "  %s  [%s/%s]\n", e.Path, e.KVNamespace, e.KVKey)
		}
	}
	if n := len(plan.Pending); n > 0 {
		fmt.Fprintf(os.Stderr, "skipping %d legacy file(s) (no canonical kv row):\n", n)
		for _, e := range plan.Pending {
			fmt.Fprintf(os.Stderr, "  %s  [%s/%s]  (%s)\n", e.Path, e.KVNamespace, e.KVKey, e.PendingReason)
		}
	}
	if len(plan.Redundant)+len(plan.Pending) == 0 {
		fmt.Fprintln(os.Stderr, "no legacy cleanup needed")
	}
}

// applyLegacyCleanPlan re-validates each Redundant entry at apply
// time and removes only those whose kv row is still authoritative.
// scan→apply can race against the runtime; a row that disappeared
// (or got corrupted) between scan and apply must NOT cause the disk
// file to be deleted, since that would strand the value entirely.
//
// ENOENT on Remove is folded into success (a concurrent runtime
// migration could have unlinked the file already). Other errors are
// accumulated; entries that fail re-validation surface as
// "skipped at apply" log lines but not as errors — the dry-run
// preview already promised they'd be removed, and the operator
// needs to see why apply diverged.
func applyLegacyCleanPlan(plan *legacyCleanPlan, kv *store.Store) []error {
	if kv == nil {
		return []error{errors.New("clean legacy apply: kv store is required")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var errs []error
	for _, e := range plan.Redundant {
		rec, gerr := kv.GetKV(ctx, e.KVNamespace, e.KVKey)
		if gerr != nil {
			fmt.Fprintf(os.Stderr, "skipping %s at apply: kv re-probe failed: %v\n", e.Path, gerr)
			continue
		}
		if vErr := validateKVRow(e.Kind, rec); vErr != nil {
			fmt.Fprintf(os.Stderr, "skipping %s at apply: kv row no longer valid: %v\n", e.Path, vErr)
			continue
		}
		if err := os.Remove(e.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", e.Path, err))
		}
	}
	return errs
}
