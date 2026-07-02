package agent

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// Codex session transfer for §3.7 device switch.
//
// Codex app-server persists the actual conversation in the global
// Codex home:
//
//	$CODEX_HOME/state_*.sqlite
//	$CODEX_HOME/sessions/YYYY/MM/DD/rollout-...<threadID>.jsonl
//
// Kojo adds a small per-agent pointer under AgentDir so each agent
// (and each SessionKey-backed external thread) can deterministically
// resume the right Codex thread instead of starting a fresh one:
//
//	<agentDir>/.codex/threads/main.json
//	<agentDir>/.codex/threads/key-<uuid>.json
//
// A complete handoff therefore transfers the pointer file, its
// rollout JSONL, and the matching Codex sqlite rows. The sqlite rows
// are rewritten on target so cwd / rollout_path point at target's own
// AgentDir / Codex home.

type CodexSQLiteValue struct {
	Type  string  `json:"type"`
	Text  string  `json:"text,omitempty"`
	Int   int64   `json:"int,omitempty"`
	Float float64 `json:"float,omitempty"`
	Blob  []byte  `json:"blob,omitempty"`
}

type CodexSQLiteRow struct {
	Columns []string           `json:"columns"`
	Values  []CodexSQLiteValue `json:"values"`
}

type CodexThreadTransfer struct {
	RefName         string           `json:"ref_name"`
	ThreadID        string           `json:"thread_id"`
	RolloutRelPath  string           `json:"rollout_rel_path"`
	RolloutContent  []byte           `json:"rollout_content"`
	ThreadRow       *CodexSQLiteRow  `json:"thread_row,omitempty"`
	DynamicToolRows []CodexSQLiteRow `json:"dynamic_tool_rows,omitempty"`
}

type CodexSessionTransfer struct {
	Threads []CodexThreadTransfer `json:"threads"`
}

const codexSessionFileMaxBytes = 32 << 20
const codexSessionTransferMaxThreads = 128

var codexSessionTransferMaxTotalBytes int64 = 256 << 20

var codexSessionTransferLocks keyedMutex

func lockCodexSessionTransfer(agentID string) func() {
	return codexSessionTransferLocks.Lock(agentID)
}

func codexHome() string {
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func codexStateDBPath() string {
	root := codexHome()
	if root == "" {
		return ""
	}
	preferred := filepath.Join(root, "state_5.sqlite")
	if st, err := os.Stat(preferred); err == nil && st.Mode().IsRegular() {
		return preferred
	}
	matches, err := filepath.Glob(filepath.Join(root, "state_*.sqlite"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}

func codexSQLiteDSN(dbPath string, readOnly bool) string {
	var b strings.Builder
	b.WriteString("file:")
	b.WriteString(dbPath)
	b.WriteString("?_pragma=busy_timeout(5000)")
	b.WriteString("&_pragma=foreign_keys(ON)")
	if readOnly {
		b.WriteString("&mode=ro")
	} else {
		b.WriteString("&_txlock=immediate")
	}
	return b.String()
}

func lookupCodexRolloutPath(threadID string) string {
	if !isCodexThreadID(threadID) {
		return ""
	}
	dbPath := codexStateDBPath()
	if dbPath == "" {
		return ""
	}
	db, err := sql.Open("sqlite", codexSQLiteDSN(dbPath, true))
	if err != nil {
		return ""
	}
	defer db.Close()
	if !codexTableExists(db, "threads") {
		return ""
	}
	var out string
	if err := db.QueryRow(`SELECT rollout_path FROM threads WHERE id = ?`, threadID).Scan(&out); err != nil {
		return ""
	}
	return out
}

func ReadCodexSessionFiles(agentID string) (*CodexSessionTransfer, []string, error) {
	if agentID == "" {
		return nil, nil, nil
	}
	unlock := lockCodexSessionTransfer(agentID)
	defer unlock()

	refDir := codexThreadRefDir(agentID)
	entries, err := os.ReadDir(refDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("agent.ReadCodexSessionFiles: readdir refs: %w", err)
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })

	root := codexHome()
	if root == "" {
		return nil, nil, nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, fmt.Errorf("agent.ReadCodexSessionFiles: abs codex home: %w", err)
	}

	var db *sql.DB
	if dbPath := codexStateDBPath(); dbPath != "" {
		if opened, oerr := sql.Open("sqlite", codexSQLiteDSN(dbPath, true)); oerr == nil {
			db = opened
			defer db.Close()
		}
	}

	out := &CodexSessionTransfer{}
	skipped := make([]string, 0)
	var totalBytes int64

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		refName := e.Name()
		if !validCodexThreadRefName(refName) {
			skipped = append(skipped, refName)
			continue
		}
		ref, rerr := readCodexThreadRefFile(filepath.Join(refDir, refName))
		if rerr != nil || ref == nil || !isCodexThreadID(ref.ThreadID) {
			skipped = append(skipped, refName)
			continue
		}
		rolloutPath := ref.RolloutPath
		if rolloutPath == "" && db != nil && codexTableExists(db, "threads") {
			rolloutPath = lookupCodexRolloutPathFromDB(db, ref.ThreadID)
		}
		if rolloutPath == "" {
			skipped = append(skipped, refName)
			continue
		}
		rel, relErr := codexRolloutRelPath(absRoot, rolloutPath, ref.ThreadID)
		if relErr != nil {
			skipped = append(skipped, refName)
			continue
		}
		full := filepath.Join(absRoot, filepath.FromSlash(rel))
		st, statErr := os.Stat(full)
		if statErr != nil || !st.Mode().IsRegular() {
			skipped = append(skipped, rel)
			continue
		}
		if st.Size() > codexSessionFileMaxBytes {
			skipped = append(skipped, rel)
			continue
		}
		totalBytes += st.Size()
		if totalBytes > codexSessionTransferMaxTotalBytes {
			return nil, skipped, fmt.Errorf("agent.ReadCodexSessionFiles: session payload exceeds %d bytes", codexSessionTransferMaxTotalBytes)
		}
		body, readErr := os.ReadFile(full)
		if readErr != nil {
			return nil, skipped, fmt.Errorf("agent.ReadCodexSessionFiles: read %s: %w", rel, readErr)
		}

		var threadRow *CodexSQLiteRow
		var toolRows []CodexSQLiteRow
		if db != nil {
			if codexTableExists(db, "threads") {
				threadRow, _ = queryCodexSQLiteRowTx(db, "SELECT * FROM threads WHERE id = ?", ref.ThreadID)
			}
			if codexTableExists(db, "thread_dynamic_tools") {
				toolRows, _ = queryCodexSQLiteRowsTx(db, "SELECT * FROM thread_dynamic_tools WHERE thread_id = ? ORDER BY position, name", ref.ThreadID)
			}
		}

		out.Threads = append(out.Threads, CodexThreadTransfer{
			RefName:         refName,
			ThreadID:        ref.ThreadID,
			RolloutRelPath:  rel,
			RolloutContent:  body,
			ThreadRow:       threadRow,
			DynamicToolRows: toolRows,
		})
		if len(out.Threads) > codexSessionTransferMaxThreads {
			return nil, skipped, fmt.Errorf("agent.ReadCodexSessionFiles: too many codex thread refs (%d > %d)",
				len(out.Threads), codexSessionTransferMaxThreads)
		}
	}
	if len(out.Threads) == 0 {
		return nil, skipped, nil
	}
	return out, skipped, nil
}

func StageCodexSessionCleanup(agentID string) (commit func(), rollback func(), err error) {
	if agentID == "" {
		return nil, nil, fmt.Errorf("agent.StageCodexSessionCleanup: agent_id required")
	}
	releaseOnce := sync.OnceFunc(lockCodexSessionTransfer(agentID))
	lockReleasedToCallbacks := false
	defer func() {
		if !lockReleasedToCallbacks {
			releaseOnce()
		}
	}()

	refDir := codexThreadRefDir(agentID)
	if _, err := os.Stat(refDir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("agent.StageCodexSessionCleanup: stat refs: %w", err)
	}
	backup := refDir + ".purge"
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			break
		}
		backup = fmt.Sprintf("%s.%d", backup, i)
	}
	if err := os.Rename(refDir, backup); err != nil {
		return nil, nil, fmt.Errorf("agent.StageCodexSessionCleanup: backup refs: %w", err)
	}

	lockReleasedToCallbacks = true
	var done bool
	commit = func() {
		if done {
			return
		}
		done = true
		defer releaseOnce()
		if err := os.RemoveAll(backup); err != nil {
			slog.Default().Warn("StageCodexSessionCleanup: commit failed to drop backup",
				"agent", agentID, "path", backup, "err", err)
		}
	}
	rollback = func() {
		if done {
			return
		}
		done = true
		defer releaseOnce()
		_ = os.RemoveAll(refDir)
		if err := os.Rename(backup, refDir); err != nil {
			slog.Default().Warn("StageCodexSessionCleanup: rollback failed",
				"agent", agentID, "backup", backup, "final", refDir, "err", err)
		}
	}
	return commit, rollback, nil
}

func clearCodexSession(agentID string) {
	_, _, _ = clearCodexSessionCounted(agentID)
}

func clearCodexSessionCounted(agentID string) (filesRemoved, threadsRemoved int, err error) {
	if agentID == "" {
		return 0, 0, nil
	}
	release := lockCodexSessionTransfer(agentID)
	defer release()

	refDir := codexThreadRefDir(agentID)
	entries, rerr := os.ReadDir(refDir)
	if rerr != nil && !os.IsNotExist(rerr) {
		err = errors.Join(err, fmt.Errorf("read codex thread refs: %w", rerr))
	}

	threadIDs := map[string]struct{}{}
	root := codexHome()
	var absRoot string
	if root != "" {
		if abs, aerr := filepath.Abs(root); aerr == nil {
			absRoot = abs
		}
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		ref, refErr := readCodexThreadRefFile(filepath.Join(refDir, e.Name()))
		if refErr != nil || ref == nil || !isCodexThreadID(ref.ThreadID) {
			continue
		}
		threadIDs[ref.ThreadID] = struct{}{}
		rolloutPath := ref.RolloutPath
		if rolloutPath == "" {
			rolloutPath = lookupCodexRolloutPath(ref.ThreadID)
		}
		if rolloutPath == "" || absRoot == "" {
			continue
		}
		rel, relErr := codexRolloutRelPath(absRoot, rolloutPath, ref.ThreadID)
		if relErr != nil {
			continue
		}
		full := filepath.Join(absRoot, filepath.FromSlash(rel))
		if rmErr := os.Remove(full); rmErr == nil {
			filesRemoved++
		} else if !os.IsNotExist(rmErr) {
			err = errors.Join(err, fmt.Errorf("remove codex rollout %s: %w", rel, rmErr))
		}
	}

	if len(threadIDs) > 0 {
		threadsRemoved = len(threadIDs)
		if dbPath := codexStateDBPath(); dbPath != "" {
			if db, oerr := sql.Open("sqlite", codexSQLiteDSN(dbPath, false)); oerr == nil {
				func() {
					defer db.Close()
					if codexTableExists(db, "thread_dynamic_tools") {
						for id := range threadIDs {
							if _, derr := db.Exec("DELETE FROM thread_dynamic_tools WHERE thread_id = ?", id); derr != nil {
								err = errors.Join(err, fmt.Errorf("delete codex dynamic tools %s: %w", id, derr))
							}
						}
					}
					if codexTableExists(db, "threads") {
						for id := range threadIDs {
							if _, derr := db.Exec("DELETE FROM threads WHERE id = ?", id); derr != nil {
								err = errors.Join(err, fmt.Errorf("delete codex thread %s: %w", id, derr))
							}
						}
					}
				}()
			} else {
				err = errors.Join(err, fmt.Errorf("open codex state: %w", oerr))
			}
		}
	}

	if rmErr := os.RemoveAll(refDir); rmErr != nil && !os.IsNotExist(rmErr) {
		err = errors.Join(err, fmt.Errorf("remove codex refs: %w", rmErr))
	}
	return filesRemoved, threadsRemoved, err
}

func StageCodexSession(agentID string, transfer *CodexSessionTransfer) (commit func(), rollback func(), err error) {
	if agentID == "" {
		return nil, nil, fmt.Errorf("agent.StageCodexSession: agent_id required")
	}
	if transfer == nil || len(transfer.Threads) == 0 {
		return nil, nil, nil
	}
	if len(transfer.Threads) > codexSessionTransferMaxThreads {
		return nil, nil, fmt.Errorf("agent.StageCodexSession: too many threads (%d > %d)",
			len(transfer.Threads), codexSessionTransferMaxThreads)
	}

	releaseOnce := sync.OnceFunc(lockCodexSessionTransfer(agentID))
	lockReleasedToCallbacks := false
	defer func() {
		if !lockReleasedToCallbacks {
			releaseOnce()
		}
	}()

	absAgentDir, err := filepath.Abs(AgentDir(agentID))
	if err != nil {
		return nil, nil, fmt.Errorf("agent.StageCodexSession: abs agent dir: %w", err)
	}
	if err := os.MkdirAll(absAgentDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("agent.StageCodexSession: mkdir agent dir: %w", err)
	}
	root := codexHome()
	if root == "" {
		return nil, nil, fmt.Errorf("agent.StageCodexSession: cannot resolve codex home")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, fmt.Errorf("agent.StageCodexSession: abs codex home: %w", err)
	}

	type stagedFile struct {
		final  string
		tmp    string
		backup string
	}
	var staged []stagedFile
	cleanupTmps := func() {
		for _, s := range staged {
			if s.tmp != "" {
				_ = os.Remove(s.tmp)
			}
		}
	}
	stageFile := func(final string, body []byte) error {
		if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(final), ".codex-stage-*.tmp")
		if err != nil {
			return err
		}
		tmpPath := tmp.Name()
		if _, err := tmp.Write(body); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return err
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
		staged = append(staged, stagedFile{final: final, tmp: tmpPath})
		return nil
	}

	seenRefs := map[string]struct{}{}
	seenRollouts := map[string]struct{}{}
	var totalBytes int64
	for _, th := range transfer.Threads {
		if !isCodexThreadID(th.ThreadID) {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageCodexSession: invalid thread_id %q", th.ThreadID)
		}
		if !validCodexThreadRefName(th.RefName) {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageCodexSession: invalid ref name %q", th.RefName)
		}
		if _, dup := seenRefs[strings.ToLower(th.RefName)]; dup {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageCodexSession: duplicate ref %q", th.RefName)
		}
		seenRefs[strings.ToLower(th.RefName)] = struct{}{}

		rel, relErr := cleanCodexRolloutRelPath(th.RolloutRelPath, th.ThreadID)
		if relErr != nil {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageCodexSession: %w", relErr)
		}
		if _, dup := seenRollouts[strings.ToLower(rel)]; dup {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageCodexSession: duplicate rollout %q", rel)
		}
		seenRollouts[strings.ToLower(rel)] = struct{}{}

		if len(th.RolloutContent) > codexSessionFileMaxBytes {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageCodexSession: rollout %q exceeds per-file cap", rel)
		}
		totalBytes += int64(len(th.RolloutContent))
		if totalBytes > codexSessionTransferMaxTotalBytes {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageCodexSession: total payload exceeds %d bytes", codexSessionTransferMaxTotalBytes)
		}

		rolloutFinal := filepath.Join(absRoot, filepath.FromSlash(rel))
		if !pathInside(absRoot, rolloutFinal) {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageCodexSession: rollout path escapes codex home: %q", rel)
		}
		if err := stageFile(rolloutFinal, th.RolloutContent); err != nil {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageCodexSession: stage rollout %s: %w", rel, err)
		}
		refBody, _ := json.MarshalIndent(codexThreadRef{
			ThreadID:    th.ThreadID,
			RolloutPath: rolloutFinal,
		}, "", "  ")
		refFinal := filepath.Join(absAgentDir, ".codex", "threads", th.RefName)
		if err := stageFile(refFinal, append(refBody, '\n')); err != nil {
			cleanupTmps()
			return nil, nil, fmt.Errorf("agent.StageCodexSession: stage ref %s: %w", th.RefName, err)
		}
	}

	rollbackFiles := func() {
		for i := len(staged) - 1; i >= 0; i-- {
			s := staged[i]
			_ = os.Remove(s.final)
			if s.backup != "" {
				_ = os.Rename(s.backup, s.final)
			}
		}
	}
	for i := range staged {
		s := &staged[i]
		if _, err := os.Stat(s.final); err == nil {
			bk, berr := os.CreateTemp(filepath.Dir(s.final), ".codex-bk-*.tmp")
			if berr != nil {
				cleanupTmps()
				rollbackFiles()
				return nil, nil, fmt.Errorf("agent.StageCodexSession: backup temp: %w", berr)
			}
			s.backup = bk.Name()
			_ = bk.Close()
			if err := renameOverwrite(s.final, s.backup); err != nil {
				cleanupTmps()
				rollbackFiles()
				return nil, nil, fmt.Errorf("agent.StageCodexSession: backup %s: %w", s.final, err)
			}
		}
		if err := os.Rename(s.tmp, s.final); err != nil {
			cleanupTmps()
			rollbackFiles()
			return nil, nil, fmt.Errorf("agent.StageCodexSession: rename %s: %w", s.final, err)
		}
		s.tmp = ""
	}

	dbCommit, dbRollback, dbErr := stageCodexSQLiteRows(agentID, absAgentDir, absRoot, transfer)
	if dbErr != nil {
		rollbackFiles()
		return nil, nil, dbErr
	}

	lockReleasedToCallbacks = true
	var done bool
	commit = func() {
		if done {
			return
		}
		done = true
		defer releaseOnce()
		if dbCommit != nil {
			dbCommit()
		}
		for _, s := range staged {
			if s.backup != "" {
				_ = os.Remove(s.backup)
			}
		}
	}
	rollback = func() {
		if done {
			return
		}
		done = true
		defer releaseOnce()
		if dbRollback != nil {
			dbRollback()
		}
		rollbackFiles()
	}
	return commit, rollback, nil
}

func readCodexThreadRefFile(p string) (*codexThreadRef, error) {
	body, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var ref codexThreadRef
	if err := json.Unmarshal(body, &ref); err != nil {
		return nil, err
	}
	return &ref, nil
}

func validCodexThreadRefName(name string) bool {
	if filepath.Base(name) != name {
		return false
	}
	if name == "main.json" {
		return true
	}
	if !strings.HasPrefix(name, "key-") || !strings.HasSuffix(name, ".json") {
		return false
	}
	return isCodexThreadID(strings.TrimSuffix(strings.TrimPrefix(name, "key-"), ".json"))
}

func cleanCodexRolloutRelPath(rel, threadID string) (string, error) {
	rel = path.Clean(filepath.ToSlash(rel))
	if rel == "." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid rollout relpath %q", rel)
	}
	if !strings.HasPrefix(rel, "sessions/") || path.Ext(rel) != ".jsonl" {
		return "", fmt.Errorf("invalid rollout relpath %q", rel)
	}
	if threadID != "" && !strings.Contains(path.Base(rel), threadID) {
		return "", fmt.Errorf("rollout relpath %q does not contain thread id %q", rel, threadID)
	}
	return rel, nil
}

func codexRolloutRelPath(absRoot, rolloutPath, threadID string) (string, error) {
	if rolloutPath == "" {
		return "", errors.New("empty rollout path")
	}
	abs, err := filepath.Abs(rolloutPath)
	if err != nil {
		return "", err
	}
	if !pathInside(absRoot, abs) {
		return "", fmt.Errorf("rollout path outside codex home: %s", rolloutPath)
	}
	rel, err := filepath.Rel(absRoot, abs)
	if err != nil {
		return "", err
	}
	return cleanCodexRolloutRelPath(rel, threadID)
}

func pathInside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func codexTableExists(db *sql.DB, table string) bool {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
	return err == nil && name == table
}

func lookupCodexRolloutPathFromDB(db *sql.DB, threadID string) string {
	var out string
	if err := db.QueryRow(`SELECT rollout_path FROM threads WHERE id = ?`, threadID).Scan(&out); err != nil {
		return ""
	}
	return out
}

func codexSQLiteValueFromDB(v any) CodexSQLiteValue {
	switch x := v.(type) {
	case nil:
		return CodexSQLiteValue{Type: "null"}
	case int64:
		return CodexSQLiteValue{Type: "int", Int: x}
	case float64:
		return CodexSQLiteValue{Type: "float", Float: x}
	case []byte:
		return CodexSQLiteValue{Type: "blob", Blob: append([]byte(nil), x...)}
	case string:
		return CodexSQLiteValue{Type: "text", Text: x}
	default:
		return CodexSQLiteValue{Type: "text", Text: fmt.Sprint(x)}
	}
}

func (v CodexSQLiteValue) dbValue() any {
	switch v.Type {
	case "null", "":
		return nil
	case "int":
		return v.Int
	case "float":
		return v.Float
	case "blob":
		return v.Blob
	default:
		return v.Text
	}
}

func (r CodexSQLiteRow) copy() CodexSQLiteRow {
	cols := append([]string(nil), r.Columns...)
	vals := append([]CodexSQLiteValue(nil), r.Values...)
	return CodexSQLiteRow{Columns: cols, Values: vals}
}

func (r *CodexSQLiteRow) setText(col, value string) {
	for i, c := range r.Columns {
		if c == col {
			r.Values[i] = CodexSQLiteValue{Type: "text", Text: value}
			return
		}
	}
}

func (r CodexSQLiteRow) filtered(allowed map[string]bool) CodexSQLiteRow {
	out := CodexSQLiteRow{}
	for i, c := range r.Columns {
		if allowed[c] {
			out.Columns = append(out.Columns, c)
			out.Values = append(out.Values, r.Values[i])
		}
	}
	return out
}

func stageCodexSQLiteRows(agentID, absAgentDir, absCodexHome string, transfer *CodexSessionTransfer) (commit func(), rollback func(), err error) {
	dbPath := codexStateDBPath()
	if dbPath == "" {
		return nil, nil, nil
	}
	db, err := sql.Open("sqlite", codexSQLiteDSN(dbPath, false))
	if err != nil {
		return nil, nil, fmt.Errorf("agent.StageCodexSession: open codex state: %w", err)
	}
	defer db.Close()
	if !codexTableExists(db, "threads") {
		return nil, nil, nil
	}
	threadCols, err := codexTableColumns(db, "threads")
	if err != nil {
		return nil, nil, fmt.Errorf("agent.StageCodexSession: threads columns: %w", err)
	}
	toolTable := codexTableExists(db, "thread_dynamic_tools")
	var toolCols map[string]bool
	if toolTable {
		toolCols, err = codexTableColumns(db, "thread_dynamic_tools")
		if err != nil {
			return nil, nil, fmt.Errorf("agent.StageCodexSession: dynamic tool columns: %w", err)
		}
	}

	type backup struct {
		threadID string
		thread   *CodexSQLiteRow
		tools    []CodexSQLiteRow
	}
	backups := make([]backup, 0, len(transfer.Threads))
	tx, err := db.Begin()
	if err != nil {
		return nil, nil, fmt.Errorf("agent.StageCodexSession: begin codex state tx: %w", err)
	}
	defer tx.Rollback()

	for _, th := range transfer.Threads {
		b := backup{threadID: th.ThreadID}
		if row, qerr := queryCodexSQLiteRowTx(tx, "SELECT * FROM threads WHERE id = ?", th.ThreadID); qerr == nil {
			b.thread = row
		}
		if toolTable {
			if rows, qerr := queryCodexSQLiteRowsTx(tx, "SELECT * FROM thread_dynamic_tools WHERE thread_id = ? ORDER BY position, name", th.ThreadID); qerr == nil {
				b.tools = rows
			}
		}
		backups = append(backups, b)

		if toolTable {
			if _, err := tx.Exec("DELETE FROM thread_dynamic_tools WHERE thread_id = ?", th.ThreadID); err != nil {
				return nil, nil, fmt.Errorf("agent.StageCodexSession: delete dynamic tools: %w", err)
			}
		}
		if _, err := tx.Exec("DELETE FROM threads WHERE id = ?", th.ThreadID); err != nil {
			return nil, nil, fmt.Errorf("agent.StageCodexSession: delete thread: %w", err)
		}
		if th.ThreadRow != nil {
			row := th.ThreadRow.copy()
			row.setText("id", th.ThreadID)
			row.setText("cwd", absAgentDir)
			row.setText("rollout_path", filepath.Join(absCodexHome, filepath.FromSlash(th.RolloutRelPath)))
			row.setText("agent_path", absAgentDir)
			row = row.filtered(threadCols)
			if err := insertCodexSQLiteRow(tx, "threads", row); err != nil {
				return nil, nil, fmt.Errorf("agent.StageCodexSession: insert thread %s: %w", th.ThreadID, err)
			}
		}
		if toolTable && th.ThreadRow != nil {
			for _, src := range th.DynamicToolRows {
				row := src.copy()
				row.setText("thread_id", th.ThreadID)
				row = row.filtered(toolCols)
				if err := insertCodexSQLiteRow(tx, "thread_dynamic_tools", row); err != nil {
					return nil, nil, fmt.Errorf("agent.StageCodexSession: insert dynamic tool %s: %w", th.ThreadID, err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("agent.StageCodexSession: commit codex state stage: %w", err)
	}

	rollback = func() {
		db, err := sql.Open("sqlite", codexSQLiteDSN(dbPath, false))
		if err != nil {
			slog.Default().Warn("StageCodexSession: rollback open failed", "agent", agentID, "err", err)
			return
		}
		defer db.Close()
		tx, err := db.Begin()
		if err != nil {
			slog.Default().Warn("StageCodexSession: rollback begin failed", "agent", agentID, "err", err)
			return
		}
		defer tx.Rollback()
		for _, b := range backups {
			if toolTable {
				_, _ = tx.Exec("DELETE FROM thread_dynamic_tools WHERE thread_id = ?", b.threadID)
			}
			_, _ = tx.Exec("DELETE FROM threads WHERE id = ?", b.threadID)
			if b.thread != nil {
				_ = insertCodexSQLiteRow(tx, "threads", *b.thread)
			}
			if toolTable {
				for _, row := range b.tools {
					_ = insertCodexSQLiteRow(tx, "thread_dynamic_tools", row)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			slog.Default().Warn("StageCodexSession: rollback commit failed", "agent", agentID, "err", err)
		}
	}
	commit = func() {}
	return commit, rollback, nil
}

type codexSQLiteQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func queryCodexSQLiteRowTx(q codexSQLiteQuerier, query string, args ...any) (*CodexSQLiteRow, error) {
	rows, err := queryCodexSQLiteRowsFrom(q, query, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func queryCodexSQLiteRowsTx(q codexSQLiteQuerier, query string, args ...any) ([]CodexSQLiteRow, error) {
	return queryCodexSQLiteRowsFrom(q, query, args...)
}

func queryCodexSQLiteRowsFrom(q codexSQLiteQuerier, query string, args ...any) ([]CodexSQLiteRow, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []CodexSQLiteRow
	for rows.Next() {
		raw := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		vals := make([]CodexSQLiteValue, len(cols))
		for i, v := range raw {
			vals[i] = codexSQLiteValueFromDB(v)
		}
		out = append(out, CodexSQLiteRow{Columns: append([]string(nil), cols...), Values: vals})
	}
	return out, rows.Err()
}

func codexTableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + quoteCodexIdent(table) + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

type codexSQLiteExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertCodexSQLiteRow(exec codexSQLiteExecer, table string, row CodexSQLiteRow) error {
	if len(row.Columns) == 0 || len(row.Columns) != len(row.Values) {
		return fmt.Errorf("invalid row shape")
	}
	var cols []string
	var placeholders []string
	args := make([]any, 0, len(row.Values))
	for i, c := range row.Columns {
		cols = append(cols, quoteCodexIdent(c))
		placeholders = append(placeholders, "?")
		args = append(args, row.Values[i].dbValue())
	}
	stmt := "INSERT OR REPLACE INTO " + quoteCodexIdent(table) +
		" (" + strings.Join(cols, ",") + ") VALUES (" + strings.Join(placeholders, ",") + ")"
	_, err := exec.Exec(stmt, args...)
	return err
}

func quoteCodexIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
