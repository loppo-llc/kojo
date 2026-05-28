// Package oplog implements the bounded per-agent operation log
// described in docs/multi-device-storage.md §3.13.1.
//
// The op-log captures agent-runtime writes when the peer cannot reach
// the Hub. Entries are appended to `current.jsonl` and fsync'd; once
// any of the size / count / age thresholds is hit the file is rotated
// to `rotated.<seq>.jsonl` and a fresh `current.jsonl` is opened. On
// reconnection a flush loop drains rotated files in order, then
// current, replaying entries to the Hub via idempotency-key gated
// writes (the replay logic itself lives in a higher layer; this
// package owns the on-disk format and the size/age accounting only).
//
// Phase 4 slice 2 ships the filesystem primitives:
//
//   - Entry: the JSON-Lines record format
//   - Log: per-agent open handle with Append / Drain / Truncate
//   - threshold-based Rotate triggered from Append
//
// HTTP wiring (POST /api/v1/oplog/flush, fencing_token verification,
// idempotency key replay) layers on top in slice 3.
package oplog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Entry is the JSON-Lines record format for one op-log entry. The
// serialization is canonical (lower-camel keys) so a future Hub-side
// re-parser produces byte-identical bodies even when the producer
// language differs.
type Entry struct {
	OpID         string          `json:"op_id"`
	AgentID      string          `json:"agent_id"`
	FencingToken int64           `json:"fencing_token"`
	Seq          int64           `json:"seq"`
	Table        string          `json:"table"`
	Op           string          `json:"op"` // insert | update | delete
	Body         json.RawMessage `json:"body"`
	ClientTS     int64           `json:"client_ts"` // unix millis
}

// Limits caps the on-disk footprint of a single agent's log. Append
// rotates `current.jsonl` to a new `rotated.<seq>.jsonl` when any
// threshold is reached AFTER the entry has been written; the policy
// is "the entry that crossed the threshold sits at the top of the new
// rotated file" so an external observer sees deterministic boundaries.
//
// MaxAgeMillis is the age of the OLDEST entry in `current.jsonl`
// (computed from ClientTS). Zero on any field disables that limit.
type Limits struct {
	MaxBytes     int64
	MaxEntries   int64
	MaxAgeMillis int64
	// MaxQueuedRotated caps how many rotated.<seq>.jsonl files may
	// pile up awaiting Drain before Append starts returning
	// ErrLimitExceeded. Without this cap a partition that lasts long
	// enough to fill the per-current limits over and over would grow
	// the agent's dir without bound. Default 16 (~ 16 × MaxBytes
	// total backlog) — tune up via Limits at Open time when the
	// peer is expected to ride out long partitions.
	MaxQueuedRotated int
}

// DefaultLimits matches §3.13.1's tunable defaults: 10 MB, 5000
// entries, 1 hour. Production callers can override via Open's opts.
// DefaultLimits matches §3.13.1's per-agent caps. MaxBytes and
// MaxEntries describe the rotation trigger for current.jsonl, so the
// total per-agent on-disk footprint is roughly (MaxQueuedRotated + 1)
// × MaxBytes. We default MaxQueuedRotated to 1 so the agent dir
// holds at most ~20 MB / ~10 000 entries while a single rotated file
// awaits flush — matching the docs' "session terminated above this
// point" intent without inflating the cap by 16×.
var DefaultLimits = Limits{
	MaxBytes:         10 << 20,
	MaxEntries:       5_000,
	MaxAgeMillis:     60 * 60 * 1000,
	MaxQueuedRotated: 1,
}

// ErrAgentDirRequired signals a misuse: Open was called with an
// empty path. The caller is expected to thread configdir.Path() +
// "/oplog/<agent_id>".
var ErrAgentDirRequired = errors.New("oplog: agent dir is required")

// ErrLimitExceeded is returned by Append when the per-agent backlog
// (rotated files awaiting flush) has hit Limits.MaxQueuedRotated.
// docs §3.13.1 step 4 mandates that the agent session be force-
// stopped at this point so partial-loss risk is surfaced loudly
// instead of silently growing the queue forever. The caller (the
// agent runtime) is expected to translate this error into a session
// kill and a UI warning. Subsequent Append calls keep returning
// ErrLimitExceeded until Drain or Truncate reduces the backlog.
var ErrLimitExceeded = errors.New("oplog: per-agent backlog limit exceeded")

// Log is the per-agent open handle. The zero value is not usable;
// obtain one via Open. Concurrent Append calls from a single Log are
// safe (mu serializes file IO); concurrent Drain + Append is also
// safe (Drain reads rotated files first; Append never touches them).
//
// Drain is single-threaded — calling Drain concurrently from two
// goroutines is undefined; in practice the flush loop is one
// goroutine per agent.
type Log struct {
	dir    string
	limits Limits

	mu sync.Mutex
	// Current append handle. Lazily opened so an agent that never
	// hits a partition costs nothing.
	curFile  *os.File
	curBytes int64
	curCount int64
	// Oldest ClientTS in current.jsonl, 0 = no entries yet. Used by
	// the age threshold; computed on Append (not on Open) so the
	// open path stays cheap and so a clock skew on a stale file
	// doesn't pin the threshold to a misleading value.
	curOldestTS int64

	// rotateSeq is the next rotated file index. Initialized from the
	// max suffix found at Open so a crash-resume picks up where the
	// prior run left off.
	rotateSeq int64
	// rotatedCount tracks how many rotated.* files currently exist
	// in dir. Used by Append to enforce MaxQueuedRotated without a
	// directory walk on every call. Drain / Truncate reset it.
	rotatedCount int
}

// Open returns a Log handle anchored at agentDir. The directory is
// created if missing; existing rotated.<seq>.jsonl files are scanned
// to seed the rotate counter. current.jsonl is NOT opened eagerly —
// the first Append takes care of that.
//
// limits.Zero values fall back to DefaultLimits per-field so partial
// overrides work (caller passes Limits{MaxBytes: 1<<20} to lower only
// the size cap and keeps the default count / age).
func Open(agentDir string, limits Limits) (*Log, error) {
	if agentDir == "" {
		return nil, ErrAgentDirRequired
	}
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return nil, fmt.Errorf("oplog.Open: mkdir %s: %w", agentDir, err)
	}
	if limits.MaxBytes == 0 {
		limits.MaxBytes = DefaultLimits.MaxBytes
	}
	if limits.MaxEntries == 0 {
		limits.MaxEntries = DefaultLimits.MaxEntries
	}
	if limits.MaxAgeMillis == 0 {
		limits.MaxAgeMillis = DefaultLimits.MaxAgeMillis
	}
	if limits.MaxQueuedRotated == 0 {
		limits.MaxQueuedRotated = DefaultLimits.MaxQueuedRotated
	}

	l := &Log{dir: agentDir, limits: limits}

	// Walk for existing rotated.<N>.jsonl to seed rotateSeq. The
	// counter starts at max(seq)+1 so the next rotation gets a fresh
	// suffix and a Drain that hasn't run yet still finds files in
	// monotonic order.
	entries, err := os.ReadDir(agentDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("oplog.Open: readdir %s: %w", agentDir, err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if seq, ok := parseRotatedName(e.Name()); ok {
			l.rotatedCount++
			if seq >= l.rotateSeq {
				l.rotateSeq = seq + 1
			}
		}
	}

	// Pre-populate curBytes / curCount / curOldestTS from any
	// surviving current.jsonl (a prior process appended but didn't
	// rotate). The threshold accounting is the same on a hot path
	// or a resume; recomputing once at Open is cheap.
	if err := l.loadCurrentStats(); err != nil {
		return nil, err
	}
	return l, nil
}

// Close releases the current.jsonl handle if open. Idempotent. The
// rotated files are left untouched — the next Open re-discovers them.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.curFile == nil {
		return nil
	}
	err := l.curFile.Close()
	l.curFile = nil
	return err
}

// Append serializes ent to JSON Lines, fsyncs, and rotates the file
// if any threshold is crossed. The fsync is the durability guarantee
// the docs §3.13.1 step 6 requires — without it a crash between
// agent-CLI 200 and the partition-recover flush would silently lose
// entries.
//
// Returns the path of the file the entry landed in (current.jsonl,
// or the rotated.<seq>.jsonl produced if this Append triggered a
// rotation). Mostly useful for tests; production callers ignore it.
func (l *Log) Append(ctx context.Context, ent *Entry) (string, error) {
	if ent == nil {
		return "", errors.New("oplog.Append: nil entry")
	}
	if ent.OpID == "" {
		return "", errors.New("oplog.Append: op_id required")
	}
	if ent.AgentID == "" {
		return "", errors.New("oplog.Append: agent_id required")
	}
	if ent.FencingToken <= 0 {
		return "", errors.New("oplog.Append: fencing_token must be > 0")
	}
	if ent.ClientTS <= 0 {
		return "", errors.New("oplog.Append: client_ts must be > 0")
	}
	body, err := json.Marshal(ent)
	if err != nil {
		return "", fmt.Errorf("oplog.Append: marshal: %w", err)
	}
	body = append(body, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Enforce the per-agent backlog cap before opening / writing. A
	// partition long enough to keep tripping the per-current rotate
	// limits would otherwise grow the dir without bound; surfacing
	// ErrLimitExceeded gives the agent runtime a single signal to
	// terminate the session and warn the operator (docs §3.13.1
	// step 4).
	if l.limits.MaxQueuedRotated > 0 && l.rotatedCount >= l.limits.MaxQueuedRotated {
		return "", ErrLimitExceeded
	}

	if l.curFile == nil {
		f, err := os.OpenFile(filepath.Join(l.dir, "current.jsonl"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return "", fmt.Errorf("oplog.Append: open current: %w", err)
		}
		l.curFile = f
	}
	if _, err := l.curFile.Write(body); err != nil {
		return "", fmt.Errorf("oplog.Append: write: %w", err)
	}
	if err := l.curFile.Sync(); err != nil {
		return "", fmt.Errorf("oplog.Append: fsync: %w", err)
	}
	l.curBytes += int64(len(body))
	l.curCount++
	if l.curOldestTS == 0 || ent.ClientTS < l.curOldestTS {
		l.curOldestTS = ent.ClientTS
	}

	currentPath := filepath.Join(l.dir, "current.jsonl")
	if !l.shouldRotate(ent.ClientTS) {
		return currentPath, nil
	}
	rotated, err := l.rotateLocked(ent.ClientTS)
	if err != nil {
		return "", err
	}
	return rotated, nil
}

// shouldRotate evaluates the threshold predicates against the staged
// state. The "age" check uses the entry's own ClientTS as `now` so a
// caller threading a deterministic clock for tests gets stable
// rotation boundaries; production passes the wall-clock-derived ts
// implicit in NowMillis(). Zero limits disable that field.
func (l *Log) shouldRotate(now int64) bool {
	if l.limits.MaxBytes > 0 && l.curBytes >= l.limits.MaxBytes {
		return true
	}
	if l.limits.MaxEntries > 0 && l.curCount >= l.limits.MaxEntries {
		return true
	}
	if l.limits.MaxAgeMillis > 0 && l.curOldestTS > 0 &&
		now-l.curOldestTS >= l.limits.MaxAgeMillis {
		return true
	}
	return false
}

// rotateLocked moves current.jsonl to rotated.<seq>.jsonl, parent-
// dir-fsyncs, and resets the in-memory accounting. Caller must hold
// l.mu.
//
// `now` is the ClientTS of the entry that crossed the threshold —
// used only for log breadcrumbs, not for correctness.
func (l *Log) rotateLocked(now int64) (string, error) {
	if l.curFile != nil {
		if err := l.curFile.Close(); err != nil {
			return "", fmt.Errorf("oplog.rotate: close: %w", err)
		}
		l.curFile = nil
	}
	src := filepath.Join(l.dir, "current.jsonl")
	dst := filepath.Join(l.dir, fmt.Sprintf("rotated.%d.jsonl", l.rotateSeq))
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("oplog.rotate: rename: %w", err)
	}
	l.rotateSeq++
	l.rotatedCount++
	if err := fsyncDir(l.dir); err != nil {
		return "", fmt.Errorf("oplog.rotate: fsync dir: %w", err)
	}
	l.curBytes = 0
	l.curCount = 0
	l.curOldestTS = 0
	_ = now // reserved for future structured logging
	return dst, nil
}

// Pending reports the on-disk footprint without doing any IO beyond a
// directory walk. Used by the UI to surface the "pending: N op
// queued" badge §3.13.1 step 3 calls for, and by the partition-
// recovery scheduler to decide when to start a flush.
type Pending struct {
	Bytes   int64
	Entries int64
	// OldestTS is the ClientTS of the oldest entry across all rotated
	// files plus current. Zero = no entries.
	OldestTS int64
}

// Pending returns size / count / age summary across all log files.
// Cheap (no body parsing — counts come from line counts via Stat
// where possible) but does open every file.
func (l *Log) Pending() (Pending, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var p Pending
	files, err := l.orderedFilesLocked()
	if err != nil {
		return Pending{}, err
	}
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return Pending{}, err
		}
		p.Bytes += st.Size()
		// Line count + first ClientTS via a streaming parse — files
		// can be multi-MB on a partition, so we avoid ReadFile.
		count, oldest, err := scanFileStats(f)
		if err != nil {
			return Pending{}, err
		}
		p.Entries += count
		if oldest > 0 && (p.OldestTS == 0 || oldest < p.OldestTS) {
			p.OldestTS = oldest
		}
	}
	return p, nil
}

// VisitFunc is invoked for each entry during Drain in op-id order
// (which is the same as on-disk order: rotated files asc by seq, then
// current). Returning a non-nil error halts the drain immediately;
// the entry is NOT removed and will be re-visited on the next call.
//
// The slice is reused across calls — callers must NOT retain the
// pointer past the visit call. Body is the raw JSON bytes (no copy);
// if the visitor needs to keep them, it must copy first.
type VisitFunc func(ctx context.Context, ent *Entry) error

// Drain calls visit on every entry in op_id order — which equals
// append order on this log because op_id is a client-generated UUID
// v7 produced immediately before Append, so its time-component is
// monotonic per agent. Files whose entries all succeed are deleted.
// On a visitor error, the file currently being processed is left
// intact (its remaining entries plus any later files stay queued
// for the next Drain call).
//
// Drain is intended to be idempotent under crash: a successful visit
// followed by a process kill before file deletion will cause the
// entry to be re-visited; the visitor is expected to be idempotent
// (the docs §3.13.1 step 5.2 wires the Hub's idempotency key path
// for exactly this reason).
//
// Concurrency: under l.mu we rotate any pending current.jsonl into a
// fresh rotated.<seq>.jsonl before unlocking, so a concurrent Append
// running after Drain releases the mu writes into a brand-new
// current.jsonl and cannot overwrite an already-EOF'd file we're
// about to remove.
func (l *Log) Drain(ctx context.Context, visit VisitFunc) error {
	if visit == nil {
		return errors.New("oplog.Drain: visit must not be nil")
	}
	l.mu.Lock()
	// Critical: snapshot via rotation. If current.jsonl has any
	// entries we rotate it into a rotated.<seq>.jsonl first; the
	// drain loop then operates only on rotated files that no
	// concurrent Append will ever touch. Without this step, an
	// Append landing between this goroutine's EOF read and Remove()
	// would silently lose the entry along with the file.
	if l.curCount > 0 {
		// rotateLocked closes curFile, renames, increments rotateSeq
		// + rotatedCount, fsyncs the dir. now=0 because the breadcrumb
		// timestamp is purely informational here.
		if _, err := l.rotateLocked(0); err != nil {
			l.mu.Unlock()
			return err
		}
	} else if l.curFile != nil {
		// Empty current.jsonl — nothing to drain from it. Close the
		// handle so the next Append reopens cleanly; leave the empty
		// file on disk (Truncate / next rotate cleans up).
		_ = l.curFile.Close()
		l.curFile = nil
	}
	files, err := l.orderedFilesLocked()
	if err != nil {
		l.mu.Unlock()
		return err
	}
	// At this point `files` holds only rotated.<seq>.jsonl entries
	// — orderedFilesLocked still appends current.jsonl at the end,
	// but a fresh-empty current.jsonl is fine: drainFile yields zero
	// entries for it and then Remove() drops the empty file.
	l.mu.Unlock()

	// recountErr captures any failure from the post-drain stat refresh
	// so the caller learns about it even on an otherwise-successful
	// drain. drainErr captures the visitor / IO errors that abort
	// the loop. Both go through the deferred recount-on-exit so the
	// counters never lie about what's on disk after this call returns.
	var drainErr error
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			drainErr = err
			break
		}
		if err := drainFile(ctx, path, visit); err != nil {
			drainErr = err
			break
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			drainErr = fmt.Errorf("oplog.Drain: remove %s: %w", path, err)
			break
		}
	}
	if drainErr == nil {
		if err := fsyncDir(l.dir); err != nil {
			drainErr = fmt.Errorf("oplog.Drain: fsync dir: %w", err)
		}
	}

	// Recount under-lock from the actual on-disk state so a
	// concurrent Append that landed during the drain loop is
	// reflected in the counters. Without this, a Drain that
	// completes alongside an Append that just rotated a new file
	// would zero rotatedCount even though a real backlog file
	// exists, and the next MaxQueuedRotated check would
	// undercount.
	l.mu.Lock()
	if rerr := l.recountLocked(); rerr != nil && drainErr == nil {
		drainErr = rerr
	}
	l.mu.Unlock()
	return drainErr
}

// Truncate removes every log file under the agent's dir without
// invoking the visitor. Used by §3.13.1 step 5.1 — a fencing
// mismatch on flush archives the log for human review (caller does
// the archive copy first, then calls Truncate).
//
// Returns the number of files removed.
func (l *Log) Truncate() (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.curFile != nil {
		_ = l.curFile.Close()
		l.curFile = nil
	}
	files, err := l.orderedFilesLocked()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, p := range files {
		if err := os.Remove(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return removed, fmt.Errorf("oplog.Truncate: %w", err)
		}
		removed++
	}
	l.curBytes = 0
	l.curCount = 0
	l.curOldestTS = 0
	l.rotatedCount = 0
	return removed, fsyncDir(l.dir)
}

// orderedFilesLocked returns absolute paths of all rotated files in
// ascending seq order followed by current.jsonl. Caller must hold
// l.mu.
func (l *Log) orderedFilesLocked() ([]string, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("oplog: readdir: %w", err)
	}
	type rotEntry struct {
		seq  int64
		path string
	}
	var rots []rotEntry
	hasCurrent := false
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if name == "current.jsonl" {
			hasCurrent = true
			continue
		}
		if seq, ok := parseRotatedName(name); ok {
			rots = append(rots, rotEntry{seq: seq, path: filepath.Join(l.dir, name)})
		}
	}
	sort.Slice(rots, func(i, j int) bool { return rots[i].seq < rots[j].seq })
	out := make([]string, 0, len(rots)+1)
	for _, r := range rots {
		out = append(out, r.path)
	}
	if hasCurrent {
		out = append(out, filepath.Join(l.dir, "current.jsonl"))
	}
	return out, nil
}

// recountLocked re-derives every counter (rotatedCount + curBytes /
// curCount / curOldestTS) from the actual on-disk state. Used by
// Drain on completion / error so a concurrent Append that ran while
// the drain loop was outside the mutex is reflected in the counters
// — without this, the post-Drain reset would zero rotatedCount even
// though the concurrent Append may have just rotated a fresh file
// into existence, and the next Append's MaxQueuedRotated check
// would falsely under-count.
//
// Caller must hold l.mu.
func (l *Log) recountLocked() error {
	l.rotatedCount = 0
	entries, err := os.ReadDir(l.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("oplog.recount: readdir: %w", err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if _, ok := parseRotatedName(e.Name()); ok {
			l.rotatedCount++
		}
	}
	// curBytes / curCount / curOldestTS come from a re-stat + line
	// scan on current.jsonl. loadCurrentStats handles the missing-
	// file case (sets them all to 0).
	l.curBytes = 0
	l.curCount = 0
	l.curOldestTS = 0
	return l.loadCurrentStats()
}

// loadCurrentStats reopens an existing current.jsonl on Open and
// reconstructs curBytes / curCount / curOldestTS so subsequent Append
// calls evaluate the same thresholds the prior process would have.
func (l *Log) loadCurrentStats() error {
	path := filepath.Join(l.dir, "current.jsonl")
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("oplog.loadCurrentStats: stat: %w", err)
	}
	count, oldest, err := scanFileStats(path)
	if err != nil {
		return err
	}
	l.curBytes = st.Size()
	l.curCount = count
	l.curOldestTS = oldest
	return nil
}

func parseRotatedName(name string) (int64, bool) {
	if !strings.HasPrefix(name, "rotated.") || !strings.HasSuffix(name, ".jsonl") {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, "rotated."), ".jsonl")
	v, err := strconv.ParseInt(mid, 10, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// scanFileStats returns (entry count, oldest client_ts) by streaming
// the file. Uses bufio.Reader.ReadBytes('\n') rather than
// bufio.Scanner so an arbitrarily-large valid Append (the
// Limits.MaxBytes cap is per-current.jsonl, but a single entry is
// allowed to be large) doesn't trip Scanner's bounded buffer and
// silently truncate the count. Lines that fail to parse are counted
// but contribute zero to the timestamp tracker — a corrupt entry
// shouldn't take down Pending() — and the visitor will surface the
// parse error during the Drain pass.
func scanFileStats(path string) (int64, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("oplog.scanFileStats: %w", err)
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	var count, oldest int64
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			count++
			line = bytes_TrimRight(line, '\n')
			var ent struct {
				ClientTS int64 `json:"client_ts"`
			}
			if jerr := json.Unmarshal(line, &ent); jerr == nil {
				if ent.ClientTS > 0 && (oldest == 0 || ent.ClientTS < oldest) {
					oldest = ent.ClientTS
				}
			}
		}
		if err == io.EOF {
			return count, oldest, nil
		}
		if err != nil {
			return count, oldest, fmt.Errorf("oplog.scanFileStats: read: %w", err)
		}
	}
}

// drainFile streams entries from path into visit. Returns the
// visitor's error verbatim so the caller can branch on it (e.g. a
// fencing mismatch from the Hub aborts the whole flush; a transient
// 5xx asks the caller to retry the same file).
func drainFile(ctx context.Context, path string, visit VisitFunc) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("oplog.drainFile: open: %w", err)
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes_TrimRight(line, '\n')
			var ent Entry
			if err := json.Unmarshal(line, &ent); err != nil {
				return fmt.Errorf("oplog.drainFile: parse %s: %w", path, err)
			}
			if verr := visit(ctx, &ent); verr != nil {
				return verr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("oplog.drainFile: read: %w", err)
		}
	}
}

// bytes_TrimRight strips a single trailing byte b. We avoid pulling
// in `bytes` for a one-byte test on the hot path.
func bytes_TrimRight(line []byte, b byte) []byte {
	if n := len(line); n > 0 && line[n-1] == b {
		return line[:n-1]
	}
	return line
}

// fsyncDir mirrors the recipe used elsewhere in kojo (see
// internal/blob/atomic.go) — open(O_RDONLY), Sync, swallow the
// "directories don't fsync on this platform" error.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		// Windows / some network FSes refuse fsync on dirs — non-
		// fatal; the rename is still on disk.
		return nil
	}
	return nil
}
