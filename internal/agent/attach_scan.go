package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/loppo-llc/kojo/internal/blob"
)

// httpDetectContentType is a thin wrapper around
// net/http.DetectContentType. Wrapped so guessMime can be unit-
// tested without coupling to net/http's symbol surface (and so a
// future swap to a richer detector is one line, not a sed across
// callers).
func httpDetectContentType(body []byte) string {
	return http.DetectContentType(body)
}

// AttachmentForwarder is the callback Manager invokes when the
// daemon is running as a non-hub peer and the bytes for an
// agent-attached file must also be pushed to the hub blob store
// (the UI lives on hub, so without this push the assistant's
// reply renders an unreachable kojo:// URI).
//
// The forwarder is given the canonical (scope, path) pair plus
// the body, the digest (pre-computed during the local Put), and
// the size. Implementations should use the peer Identity to sign
// the outbound request and target hub's blob ingest endpoint.
//
// A non-nil error is logged but not fatal — the local copy lands
// regardless. Caller continues with the next file. Returning nil
// when the forward succeeded is the normal path.
type AttachmentForwarder func(ctx context.Context, scope blob.Scope, path string, sha256Hex string, body io.Reader, size int64) error

// SetAttachmentForwarder wires the hub-push callback. cmd/kojo
// calls this from --peer mode after the peer identity is loaded;
// leaving it nil (hub mode) is the correct posture there because
// the local Put IS the canonical copy.
func (m *Manager) SetAttachmentForwarder(f AttachmentForwarder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attachForwarder = f
}

func (m *Manager) attachmentForwarder() AttachmentForwarder {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attachForwarder
}

// attachMaxBytes caps the per-file size accepted from the agent's
// staging directory. Matches the user-upload ceiling so an agent
// can echo back any file the user originally uploaded without
// being asymmetrically restricted.
const attachMaxBytes int64 = 10 << 30

// scanAndIngestAttachments walks <agentDir>/.kojo/attach/ once,
// moves every regular file from there into the blob store under
//
//	scope=global, path=agents/{agentID}/attach/{messageID}/{filename}
//
// and returns one MessageAttachment per successful ingest. The
// staging directory is emptied (including any files we refused
// to ingest) before returning so a misbehaved agent cannot stash
// state across turns.
//
// Per-file failures are logged at warn level and the file is
// removed from disk; the rest of the batch continues. Whole-batch
// failures (cannot open the dir, blob store not wired) return
// (nil, nil) — the assistant message goes out with no attachments,
// which is strictly better than refusing to persist the reply.
func (m *Manager) scanAndIngestAttachments(ctx context.Context, agentID string, messageID string) []MessageAttachment {
	if m.blobStore == nil {
		return nil
	}
	logger := m.logger
	if logger == nil {
		logger = slog.Default()
	}
	stageDir, ok := safeStageDir(agentID, logger)
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		logger.Warn("attach scan: read dir", "agent", agentID, "err", err)
		return nil
	}
	if len(entries) == 0 {
		return nil
	}

	// Sort by name so the order in the resulting MessageAttachment
	// slice is stable across runs (the UI renders chips in slice
	// order; without sort, fs-dependent inode ordering would shuffle
	// them across machines).
	sortedNames := make([]string, 0, len(entries))
	for _, e := range entries {
		sortedNames = append(sortedNames, e.Name())
	}
	// Lexical sort is enough; we don't need a stable case-insensitive
	// pass because the basename sanitizer collapses to a deterministic
	// form anyway.
	strSliceSortInPlace(sortedNames)

	out := make([]MessageAttachment, 0, len(entries))
	forwarder := m.attachmentForwarder()
	used := map[string]struct{}{}

	for _, name := range sortedNames {
		full := filepath.Join(stageDir, name)
		att, ok := m.ingestOneAttachment(ctx, logger, agentID, messageID, full, name, used, forwarder)
		if ok {
			out = append(out, att)
		}
	}

	// Empty the staging dir wholesale (files we accepted, files we
	// rejected, dotfiles, and any subdirectories the agent
	// accidentally created). RemoveAll on a real directory is
	// scoped — Lstat above proved stageDir is not a symlink, so
	// this only walks inside agentDir/.kojo/attach. Ignore
	// errors; leaving residue is harmless and only makes the next
	// scan a no-op.
	if err := os.RemoveAll(stageDir); err != nil {
		logger.Warn("attach scan: cleanup stage dir", "agent", agentID, "err", err)
	}
	return out
}

// ingestOneAttachment processes a single staged file. Returns the
// resulting MessageAttachment and ok=true on success; ok=false on
// any failure (logged). `used` is mutated to reserve sanitized
// names within a batch so two files that sanitize to the same name
// get distinct numeric suffixes.
func (m *Manager) ingestOneAttachment(
	ctx context.Context,
	logger *slog.Logger,
	agentID string,
	messageID string,
	full string,
	rawName string,
	used map[string]struct{},
	forwarder AttachmentForwarder,
) (MessageAttachment, bool) {
	cleanName, ok := sanitizeAttachBasename(rawName)
	if !ok {
		logger.Warn("attach scan: reject bad basename", "raw", rawName)
		return MessageAttachment{}, false
	}

	// Open with O_NOFOLLOW so a symlink the agent dropped between
	// our directory scan and this open can't redirect us to an
	// arbitrary host file. After open we fstat the same fd (NOT a
	// fresh os.Lstat on the path) so the size / regular-file check
	// is bound to the bytes we will actually read — no TOCTOU
	// window. fstatNoFollow falls back on Windows where the flag
	// is unavailable; the path is already inside agentDir which
	// the kojo-attach contract treats as agent-owned territory.
	f, info, err := openNoFollow(full)
	if err != nil {
		if errors.Is(err, errNonRegular) {
			logger.Debug("attach scan: skip non-regular / symlink",
				"path", full)
		} else {
			logger.Warn("attach scan: open", "path", full, "err", err)
		}
		return MessageAttachment{}, false
	}
	defer f.Close()
	if info.Size() <= 0 {
		logger.Debug("attach scan: skip empty file", "path", full)
		return MessageAttachment{}, false
	}
	if info.Size() > attachMaxBytes {
		logger.Warn("attach scan: file exceeds size cap",
			"path", full, "size", info.Size(), "cap", attachMaxBytes)
		return MessageAttachment{}, false
	}
	cleanName = reserveUniqueName(cleanName, used)
	used[cleanName] = struct{}{}

	// Sniff the first 512 bytes (DetectContentType's read budget)
	// while leaving the fd positioned at offset 0 — we re-Seek
	// before streaming into blob.Put. Sniff is best-effort: a
	// short read or a Seek error degrades gracefully to "no
	// sniff", and guessMime falls back to the extension table.
	sniff := make([]byte, 512)
	n, _ := f.Read(sniff)
	sniff = sniff[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		logger.Warn("attach scan: seek 0 after sniff", "path", full, "err", err)
		return MessageAttachment{}, false
	}

	scope := blob.ScopeGlobal
	blobPath := buildAttachBlobPath(agentID, messageID, cleanName)
	uri := blob.BuildURI(scope, blobPath)

	// Stream directly from the fd into blob.Put. blob.Put hashes
	// the body in flight and the LimitReader caps the read at the
	// fstat'd size so a file that grew between fstat and now is
	// truncated to the validated ceiling (and does not bypass the
	// 10 GiB cap). The +1 sentinel lets us detect the growth case
	// after the fact.
	capped := &io.LimitedReader{R: f, N: info.Size() + 1}
	obj, err := m.blobStore.Put(scope, blobPath, capped, blob.PutOptions{})
	if err != nil {
		if !errors.Is(err, blob.ErrDurabilityDegraded) || obj == nil {
			logger.Warn("attach scan: blob put", "uri", uri, "err", err)
			return MessageAttachment{}, false
		}
		// Degraded durability: body + row are committed, only the
		// parent-dir fsync was skipped. Same posture as the device-
		// switch pull path — surface but don't roll back.
		logger.Warn("attach scan: blob put committed degraded", "uri", uri, "err", err)
	}
	if obj.Size > info.Size() {
		// File grew past its fstat'd size while we were reading.
		// The blob row now contains the larger body, which is
		// technically valid (Put hashed what landed) but bypasses
		// the size cap we promised the agent. Drop the row and
		// refuse the attachment so the contract holds.
		_ = m.blobStore.Delete(scope, blobPath, blob.DeleteOptions{})
		logger.Warn("attach scan: file grew during read; refusing",
			"path", full, "stat_size", info.Size(), "put_size", obj.Size)
		return MessageAttachment{}, false
	}

	if forwarder != nil {
		// Re-open the staged file for the hub stream. The
		// scanAndIngestAttachments cleanup (RemoveAll on stageDir)
		// fires AFTER this loop returns, so the path is still
		// valid here. openNoFollow again so a between-Put-and-
		// forward symlink swap can't redirect us.
		fwdFile, _, openErr := openNoFollow(full)
		if openErr != nil {
			logger.Warn("attach scan: re-open for forward",
				"path", full, "err", openErr)
			// Keep the attachment — the local blob exists on
			// this peer, and a future device-switch back to here
			// (or a manual blob pull) will surface the bytes.
			// Dropping silently meant the user-visible reply
			// lost the file metadata entirely, which is worse
			// than rendering a chip that 404s until the bytes
			// sync.
		} else {
			fwdCtx, cancel := context.WithTimeout(ctx, forwardTimeoutFor(obj.Size))
			fwdErr := forwarder(fwdCtx, scope, blobPath, obj.SHA256, fwdFile, obj.Size)
			cancel()
			fwdFile.Close()
			if fwdErr != nil {
				// Error elevated to Error (not Warn) so it
				// surfaces in default-level operator log
				// scraping. Includes the full error chain
				// from the push retry loop so the failure
				// mode (no peers / TLS handshake / 4xx with
				// body / hub side regex reject / ...) is
				// visible without enabling debug logs.
				logger.Error("attach scan: hub forward failed; keeping attachment for later sync",
					"uri", uri,
					"size", obj.Size,
					"sha256", obj.SHA256,
					"err", fwdErr)
				// Same rationale as the openErr branch above —
				// retain the metadata so the user-visible chat
				// is honest about what the agent produced, even
				// if the bytes haven't reached hub yet.
			}
		}
	}

	mt := guessMime(cleanName, sniff)

	return MessageAttachment{
		Path: uri,
		Name: cleanName,
		Size: obj.Size,
		Mime: mt,
	}, true
}

// attachScanCeiling caps the entire per-turn scan + forward
// pipeline. It must be larger than the per-file forward timeout
// (forwardTimeoutFor) summed across a worst-case batch — agents
// typically attach 1-5 files per turn, so 30 minutes covers even
// a 5×10 GiB pathological case while still bounding a hung scan
// against shutdown.
const attachScanCeiling = 30 * time.Minute

// attachForwardBaseTimeout is the floor on a hub PUT before the
// per-MiB allowance kicks in. Picked to cover TLS handshake + a
// few seconds of slow-network jitter without burning chat time
// on a confirmed-dead hub.
const attachForwardBaseTimeout = 30 * time.Second

// attachForwardPerMiB is the additional ceiling we grant per MiB
// of body. 200 ms/MiB ≈ 5 MiB/s minimum throughput, which is
// pessimistic for Tailscale on LAN/Wi-Fi (typically 50-500 MiB/s)
// but generous enough to survive the slowest cellular tether
// scenarios the operator might hit in the field. Total cap is
// floor(attachForwardBaseTimeout + size/MiB * attachForwardPerMiB)
// — a 1 GiB attachment gets ~3.5 minutes, a 10 GiB gets ~35.
const attachForwardPerMiB = 200 * time.Millisecond

// forwardTimeoutFor returns the bounded context timeout for a
// single hub PUT given the body size. Streamed reads on a hung
// connection still trip the context deadline so an offline hub
// can't pin the chat goroutine forever, but large valid uploads
// over slow links are no longer dropped by a one-size-fits-all
// 2-minute cap.
func forwardTimeoutFor(size int64) time.Duration {
	if size <= 0 {
		return attachForwardBaseTimeout
	}
	mib := size >> 20 // floor — sub-MiB rounds to 0, fine
	d := attachForwardBaseTimeout + time.Duration(mib)*attachForwardPerMiB
	return d
}

// safeStageDir resolves <agentDir>/.kojo/attach to a real path and
// verifies it sits inside agentDir. Refuses any of:
//   - the path does not exist (no work to do)
//   - the path lstat's to a non-directory (symlink / FIFO / file)
//   - EvalSymlinks resolves it to a location OUTSIDE agentDir
//     (parent-directory symlink swap, e.g. agent did
//     `ln -s /etc agentDir/.kojo` or moved an attach symlink in
//     place after a prior turn)
//   - agentDir itself fails to resolve (catastrophic FS state)
//
// On non-dir we drop the symlink itself so the agent isn't stuck.
// On out-of-tree resolution we leave the path alone — RemoveAll
// would walk the resolved target and that's exactly the
// exfiltration risk we are refusing.
func safeStageDir(agentID string, logger *slog.Logger) (string, bool) {
	stageDir := filepath.Join(agentDir(agentID), attachStagingSubpath)

	st, err := os.Lstat(stageDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("attach scan: lstat stage dir", "agent", agentID, "err", err)
		}
		return "", false
	}
	if !st.Mode().IsDir() {
		logger.Warn("attach scan: stage path is not a real directory; refusing to follow",
			"agent", agentID, "mode", st.Mode().String())
		_ = os.Remove(stageDir)
		return "", false
	}

	// Containment check. EvalSymlinks resolves the WHOLE path,
	// so a parent like agentDir/.kojo → /etc would surface as
	// resolved /etc/attach and the prefix check below catches
	// it. We compare both stageDir's resolved form AND the
	// agentDir's resolved form so a configdir that itself sits
	// behind a symlink (Tailscale-on-NixOS, custom $HOME) is
	// not a false positive.
	resolvedStage, err := filepath.EvalSymlinks(stageDir)
	if err != nil {
		logger.Warn("attach scan: evalsymlinks stage dir", "agent", agentID, "err", err)
		return "", false
	}
	resolvedAgent, err := filepath.EvalSymlinks(agentDir(agentID))
	if err != nil {
		logger.Warn("attach scan: evalsymlinks agent dir", "agent", agentID, "err", err)
		return "", false
	}
	// Use Rel so a string-prefix oddity (`/foo/bar` vs `/foo/barbaz`)
	// can't escape the check. A Rel that starts with ".." or is "."
	// means stage is not strictly inside agentDir.
	rel, err := filepath.Rel(resolvedAgent, resolvedStage)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		logger.Warn("attach scan: stage dir resolves outside agent dir; refusing",
			"agent", agentID, "resolved_stage", resolvedStage,
			"resolved_agent", resolvedAgent)
		return "", false
	}
	return stageDir, true
}

// buildAttachBlobPath assembles the canonical scope-relative path
// for an agent-attached file. Layout:
//
//	agents/{agentID}/attach/{messageID}/{filename}
//
// {messageID} is the assistant message's ID so attachments belong to
// exactly one turn and can be GC'd together with the message if it
// is ever deleted. Adding a sub-tier under attach/ keeps the
// directory listing under blob_refs scannable (no thousand-entry
// flat dir if an agent attaches once per turn for weeks).
func buildAttachBlobPath(agentID, messageID, filename string) string {
	if messageID == "" {
		// Fall back to a timestamp tier so the path is still
		// unique even when the caller forgot to seed messageID
		// (e.g. an abort-path persist that built the message on
		// the fly without going through newAssistantMessage).
		messageID = "ts_" + time.Now().UTC().Format("20060102T150405.000")
	}
	// agentID is trusted (we minted it). messageID is trusted
	// (same source). filename is sanitized by sanitizeAttachBasename
	// before we get here.
	return "agents/" + agentID + "/attach/" + messageID + "/" + filename
}

// sanitizeAttachBasename keeps only the basename, rejects empty /
// dotfile / path-traversal forms, and trims to a reasonable length.
// Returns (cleaned, true) on accept; (_, false) on reject.
//
// We are strict here because the result is concatenated into a blob
// path and an HTTP URL: anything weird (NUL, control chars, '..')
// would either round-trip through blob.BuildURI as an opaque escape
// or fail the storerefs validator. Rejecting up front gives a clear
// error path with the original name in the log.
func sanitizeAttachBasename(name string) (string, bool) {
	// Use both separators so we strip whatever an agent on Windows
	// might emit, even though the daemon's stageDir is POSIX-shaped.
	for _, sep := range []rune{'/', '\\'} {
		if idx := strings.LastIndexByte(name, byte(sep)); idx >= 0 {
			name = name[idx+1:]
		}
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", false
	}
	if strings.HasPrefix(name, ".") {
		// Dotfiles are rejected: an agent dropping `.env` or
		// `.bashrc` into the attach dir is almost always either
		// an accident or a smuggle attempt. The SKILL.md tells the
		// agent to use plain filenames.
		return "", false
	}
	if strings.ContainsAny(name, "\x00\n\r\t") {
		return "", false
	}
	// Cap at 200 chars. Most filesystems accept 255; leaving 55
	// for the numeric-suffix collision handling below.
	const maxLen = 200
	if len(name) > maxLen {
		ext := filepath.Ext(name)
		if len(ext) > 16 { // suspicious extension, drop it
			ext = ""
		}
		stem := strings.TrimSuffix(name, ext)
		if len(stem) > maxLen-len(ext) {
			stem = stem[:maxLen-len(ext)]
		}
		name = stem + ext
	}
	return name, true
}

// reserveUniqueName returns `name` unchanged if it has not been
// used in this batch; otherwise appends a `-N` suffix before the
// extension until a free slot is found. N starts at 1 and is
// bounded by the batch size so we never loop.
func reserveUniqueName(name string, used map[string]struct{}) string {
	if _, taken := used[name]; !taken {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 1<<16; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, taken := used[candidate]; !taken {
			return candidate
		}
	}
	// Pathological: 65k collisions in one turn. Append a sha-ish
	// suffix and give up — the blob layer will accept it.
	h := sha256.Sum256([]byte(name))
	return stem + "-" + hex.EncodeToString(h[:4]) + ext
}

// guessMime picks a content-type for the staged file. Prefer the
// extension-based mapping (mime.TypeByExtension reads /etc/mime.types
// on POSIX and the registry on Windows); fall back to a content
// sniff via net/http.DetectContentType which recognises the common
// magic numbers (PNG / JPEG / GIF / PDF / ZIP / ...). Final
// fallback is application/octet-stream.
func guessMime(name string, body []byte) string {
	if mt := mime.TypeByExtension(filepath.Ext(name)); mt != "" {
		// TypeByExtension may return "text/plain; charset=utf-8"
		// — keep the parameter, callers (UI image-vs-other gate)
		// only look at the prefix.
		return mt
	}
	if len(body) > 0 {
		// DetectContentType reads at most 512 bytes and never
		// returns "" (falls through to application/octet-stream
		// on unknown magic). Cheap and dependency-free.
		return httpDetectContentType(body)
	}
	return "application/octet-stream"
}

// attachDebounce is the delay after the last fs event for a file
// before the watcher ingests it. Covers non-atomic writes (cp) that
// emit CREATE then a burst of WRITEs; 150 ms is long enough for a
// small-to-medium file cp to finish, short enough to feel instant.
const attachDebounce = 150 * time.Millisecond

// watchAndStreamAttachments uses fsnotify to watch the
// <agentDir>/.kojo/attach/ directory for new files. When a file
// is created (or renamed into place), the watcher debounces
// briefly to let non-atomic writes settle, then ingests the file
// into the blob store and emits a MessageAttachment on the channel.
//
// Lifecycle is driven by a single context: Stop() cancels it,
// which exits the goroutine, cancels in-flight ingest/forward,
// and unblocks debounce callbacks. StopAndDrain() is the only
// correct way to obtain the definitive attachment list — the
// channel is for real-time UI notification only.
func (m *Manager) watchAndStreamAttachments(ctx context.Context, agentID string, messageID string) *attachWatcher {
	wCtx, wCancel := context.WithCancel(ctx)

	w := &attachWatcher{
		out:     make(chan MessageAttachment, 16),
		cancel:  wCancel,
		quit:    wCtx.Done(),
		used:    map[string]struct{}{},
		exited:  make(chan struct{}),
		timers:  map[string]*time.Timer{},
		retries: map[string]int{},
	}

	go w.run(m, wCtx, agentID, messageID)
	return w
}

// attachWatcher is the handle returned by watchAndStreamAttachments.
type attachWatcher struct {
	out    chan MessageAttachment
	cancel context.CancelFunc
	quit   <-chan struct{} // closed when the watcher should exit
	used   map[string]struct{}
	exited chan struct{} // closed when the goroutine exits

	timerMu sync.Mutex
	timers  map[string]*time.Timer

	retryMu sync.Mutex
	retries map[string]int // mtime re-debounce count per filename

	mu      sync.Mutex
	pending []MessageAttachment
}

// C returns the channel on which streamed attachments arrive.
func (w *attachWatcher) C() <-chan MessageAttachment { return w.out }

// Stop signals the watcher to exit. Idempotent (context cancel).
func (w *attachWatcher) Stop() { w.cancel() }

// StopAndDrain stops the watcher, waits for exit, and returns
// the definitive list of all successfully ingested attachments.
func (w *attachWatcher) StopAndDrain() []MessageAttachment {
	w.cancel()
	<-w.exited
	for range w.out {
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]MessageAttachment(nil), w.pending...)
}

func (w *attachWatcher) run(m *Manager, ctx context.Context, agentID, messageID string) {
	defer close(w.out)
	defer close(w.exited)
	defer w.stopTimers()

	if m.blobStore == nil {
		return
	}
	logger := m.logger
	if logger == nil {
		logger = slog.Default()
	}

	// Try safeStageDir first — if the directory already exists
	// and passes symlink / containment checks, use it directly.
	// When absent, validate the parent (.kojo) is not a symlink
	// before MkdirAll so we never create attach/ inside a
	// symlink target.
	stageDir, ok := safeStageDir(agentID, logger)
	if !ok {
		kojoDir := filepath.Join(agentDir(agentID), ".kojo")
		if st, err := os.Lstat(kojoDir); err == nil && st.Mode()&os.ModeSymlink != 0 {
			logger.Warn("attach stream: .kojo is a symlink, refusing", "agent", agentID)
			return
		}
		raw := filepath.Join(kojoDir, "attach")
		if err := os.MkdirAll(raw, 0o755); err != nil {
			logger.Warn("attach stream: mkdir stage dir", "err", err)
			return
		}
		stageDir, ok = safeStageDir(agentID, logger)
		if !ok {
			return
		}
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Warn("attach stream: create watcher", "err", err)
		return
	}
	defer fsw.Close()

	if err := fsw.Add(stageDir); err != nil {
		logger.Warn("attach stream: watch dir", "err", err)
		return
	}

	forwarder := m.attachmentForwarder()

	// ingestCh receives filenames whose debounce timer fired.
	ingestCh := make(chan string, 16)

	// Initial scan: files may already be present before
	// fsw.Add. Run them through debounce so partially-
	// written files get the same stability check as
	// fsnotify-triggered ones.
	if entries, err := os.ReadDir(stageDir); err == nil {
		for _, e := range entries {
			if _, done := w.used[e.Name()]; !done {
				w.debounce(e.Name(), ingestCh)
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-fsw.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}
			name := filepath.Base(ev.Name)
			if _, done := w.used[name]; done {
				continue
			}
			w.debounce(name, ingestCh)
		case name := <-ingestCh:
			w.ingestOne(m, ctx, logger, agentID, messageID, stageDir, name, ingestCh, forwarder)
		case err, ok := <-fsw.Errors:
			if !ok {
				return
			}
			logger.Debug("attach stream: watcher error", "err", err)
		}
	}
}

// debounce (re)arms a per-file timer. When the timer fires, the
// filename is sent to ingestCh.
func (w *attachWatcher) debounce(name string, ingestCh chan<- string) {
	w.timerMu.Lock()
	defer w.timerMu.Unlock()
	if t, ok := w.timers[name]; ok {
		t.Reset(attachDebounce)
		return
	}
	w.timers[name] = time.AfterFunc(attachDebounce, func() {
		w.timerMu.Lock()
		delete(w.timers, name)
		w.timerMu.Unlock()
		select {
		case ingestCh <- name:
		case <-w.quit:
		}
	})
}

func (w *attachWatcher) stopTimers() {
	w.timerMu.Lock()
	defer w.timerMu.Unlock()
	for name, t := range w.timers {
		t.Stop()
		delete(w.timers, name)
	}
}

// attachMaxDebounceRetries caps re-debounce cycles so a file
// whose mtime keeps advancing (external touch loop, clock skew)
// doesn't spin forever. After this many retries the file is
// ingested as-is — ingestOneAttachment's grow-detection rejects
// it if the body is still changing.
const attachMaxDebounceRetries = 10

// ingestOne processes a single file: check mtime stability →
// ingest → record → remove → notify. If the file's mtime is
// within the debounce window it is re-armed instead of ingested,
// preventing partial reads of non-atomic writes.
func (w *attachWatcher) ingestOne(
	m *Manager, ctx context.Context, logger *slog.Logger,
	agentID, messageID, stageDir, name string,
	ingestCh chan<- string,
	forwarder AttachmentForwarder,
) {
	if _, done := w.used[name]; done {
		return
	}
	full := filepath.Join(stageDir, name)

	// Mtime stability: if the file was modified within the
	// debounce window it may still be open for writing.
	// Re-debounce so we check again after the next quiet period.
	// Cap retries so a continuously-touched file doesn't spin.
	if info, err := os.Stat(full); err == nil {
		if time.Since(info.ModTime()) < attachDebounce {
			w.retryMu.Lock()
			n := w.retries[name] + 1
			w.retries[name] = n
			w.retryMu.Unlock()
			if n < attachMaxDebounceRetries {
				w.debounce(name, ingestCh)
				return
			}
			logger.Debug("attach stream: max debounce retries, ingesting anyway", "name", name)
		}
	}

	att, ok := m.ingestOneAttachment(ctx, logger, agentID, messageID, full, name, w.used, forwarder)
	if !ok {
		return
	}
	w.mu.Lock()
	w.pending = append(w.pending, att)
	w.mu.Unlock()

	if err := os.Remove(full); err != nil {
		logger.Warn("attach stream: remove after ingest", "path", full, "err", err)
	}
	select {
	case w.out <- att:
	case <-w.quit:
	}
}

// strSliceSortInPlace is a tiny insertion-sort used in place of
// pulling sort.Strings into this file (so the test file can stub
// the comparator without overriding stdlib). Batches are usually
// 1-5 items; insertion sort is fine for that scale.
func strSliceSortInPlace(s []string) {
	for i := 1; i < len(s); i++ {
		v := s[i]
		j := i - 1
		for j >= 0 && s[j] > v {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = v
	}
}
