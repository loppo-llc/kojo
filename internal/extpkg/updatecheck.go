package extpkg

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Update checking.
//
// An installed package follows the ref it was installed from (a branch,
// a tag, or the remote's default branch when the ref is empty). Update
// re-fetches that ref; this file answers the cheaper question "would
// Update change anything?" with `git ls-remote`, which transfers ref
// names only, and — for packages the operator opted into — applies the
// update without being asked.
//
// Auto-update never widens what a package may do: a commit asking for a
// different scope set is left for the operator to consent to, exactly as
// the manual Update button would. A commit that needs a newer kojo is
// held back too. Both are remembered per commit so the periodic pass
// does not re-download the same refused code every time.

// UpdateStatus is what the last check learned about one package. The
// zero value means "not checked yet".
type UpdateStatus struct {
	// Latest is the commit the package's ref pointed at on the remote
	// at the last check. Empty when the check failed or the ref is
	// pinned to a commit.
	Latest string `json:"latest,omitempty"`
	// Available is true when Latest differs from the installed commit.
	Available bool `json:"available"`
	// Pinned means the ref names a fixed commit, so there is nothing to
	// follow.
	Pinned bool `json:"pinned,omitempty"`
	// CheckedAt is when the remote was last asked (RFC 3339, UTC).
	CheckedAt string `json:"checkedAt,omitempty"`
	// Error is why the last check failed.
	Error string `json:"error,omitempty"`
	// AutoError is why auto-update did not apply Latest. Only set while
	// that commit is still the one available.
	AutoError string `json:"autoError,omitempty"`
	// NeedsConsent means Latest asks for a different scope set, so it
	// has to go through the manual update and its consent dialog.
	NeedsConsent bool `json:"needsConsent,omitempty"`
}

// updateState is the in-memory record behind UpdateStatus.
type updateState struct {
	// ref is the row's Ref the check ran against. A status whose ref no
	// longer matches the row (an update moved it to another branch)
	// describes something else and is ignored.
	ref       string
	latest    string
	pinned    bool
	checkedAt time.Time
	err       string
	// autoCommit is the commit an auto-update attempt failed for, and
	// autoErr why. autoFinal marks a failure that retrying cannot fix
	// (consent or kojo version), so the pass skips that commit until
	// the remote moves on or kojo restarts — a restart is also how a
	// self-update to a newer kojo arrives.
	autoCommit   string
	autoErr      string
	autoFinal    bool
	needsConsent bool
}

var (
	// fullCommitRe matches a complete SHA-1 or SHA-256 object name.
	fullCommitRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	// shortCommitRe matches what could be an abbreviated object name.
	shortCommitRe = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)
)

// resolveRemoteRef asks url what ref points at right now, without
// fetching any objects. pinned is true when ref names a commit rather
// than something that moves. The lookup order mirrors the rules git
// fetch uses to expand a short ref, so the answer is the commit Update
// would check out.
func resolveRemoteRef(ctx context.Context, dir, url, ref string) (commit string, pinned bool, err error) {
	ref = strings.TrimSpace(ref)
	if fullCommitRe.MatchString(strings.ToLower(ref)) {
		return "", true, nil
	}
	target := ref
	if target == "" {
		target = "HEAD"
	}
	// ls-remote matches patterns against ref-name tails, so a peeled
	// tag line ("refs/tags/v1^{}") needs its own pattern to show up.
	out, err := runGit(ctx, dir, "ls-remote", "--", url, target, target+"^{}")
	if err != nil {
		return "", false, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		sha, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		refs[strings.TrimSpace(name)] = strings.TrimSpace(sha)
	}
	candidates := []string{target}
	if target != "HEAD" {
		candidates = append(candidates,
			"refs/"+target,
			"refs/tags/"+target,
			"refs/heads/"+target,
			"refs/remotes/"+target,
			"refs/remotes/"+target+"/HEAD",
		)
	}
	for _, c := range candidates {
		// An annotated tag points at a tag object; the commit it
		// names is on the peeled line, and that is what a checkout
		// records as Commit.
		if sha, ok := refs[c+"^{}"]; ok {
			return sha, false, nil
		}
		if sha, ok := refs[c]; ok {
			return sha, false, nil
		}
	}
	if shortCommitRe.MatchString(ref) {
		// No ref by that name, and it looks like an abbreviated
		// commit: treat it as pinned rather than as an error.
		return "", true, nil
	}
	return "", false, fmt.Errorf("ref %q not found on the remote", target)
}

// statusLocked renders id's update status against its current row.
// Caller holds m.mu.
func (m *Manager) statusLocked(row *Installed) (UpdateStatus, bool) {
	st := m.updates[row.ID]
	if st == nil || st.ref != row.Ref {
		return UpdateStatus{}, false
	}
	out := UpdateStatus{
		Latest:    st.latest,
		Pinned:    st.pinned,
		CheckedAt: st.checkedAt.UTC().Format(time.RFC3339),
		Error:     st.err,
	}
	out.Available = st.latest != "" && st.latest != row.Commit
	if out.Available && st.autoCommit == st.latest {
		out.AutoError = st.autoErr
		out.NeedsConsent = st.needsConsent
	}
	return out, true
}

// UpdateStatus returns the last check result for id. ok is false when
// the package has not been checked since it was installed (or since
// kojo started).
func (m *Manager) UpdateStatus(id string) (UpdateStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.st.Extensions[id]
	if !ok {
		return UpdateStatus{}, false
	}
	return m.statusLocked(row)
}

// UpdateStatuses returns the last check result for every package that
// has one, keyed by ID.
func (m *Manager) UpdateStatuses() map[string]UpdateStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]UpdateStatus{}
	for id, row := range m.st.Extensions {
		if st, ok := m.statusLocked(row); ok {
			out[id] = st
		}
	}
	return out
}

// noteUpdatedLocked refreshes id's status after a successful Update:
// the commit just fetched is the freshest answer to "where does the ref
// point", and whatever auto-update was waiting on is settled. A status
// for a different ref is dropped. Caller holds m.mu.
func (m *Manager) noteUpdatedLocked(id, ref, commit string) {
	st := m.updates[id]
	if st == nil {
		return
	}
	if st.ref != ref {
		delete(m.updates, id)
		return
	}
	if !st.pinned {
		st.latest = commit
	}
	st.err = ""
	st.checkedAt = m.now()
	st.autoCommit, st.autoErr, st.autoFinal, st.needsConsent = "", "", false, false
}

// checkOne asks id's remote whether its ref has moved and records the
// answer. Nothing on disk changes.
func (m *Manager) checkOne(ctx context.Context, id string) error {
	m.mu.Lock()
	row, ok := m.st.Extensions[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	source, ref := strings.TrimSpace(row.Source), row.Ref
	startGen := m.gen[id]
	m.mu.Unlock()

	var (
		latest string
		pinned bool
		err    error
	)
	if err = ValidateSourceURL(source); err == nil {
		if err = ValidateRef(ref); err == nil {
			latest, pinned, err = resolveRemoteRef(ctx, m.root, source, ref)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok = m.st.Extensions[id]
	if !ok {
		return ErrNotFound
	}
	if m.gen[id] != startGen {
		// Updated, reinstalled or re-pointed while the remote was
		// being asked. The answer may predate the commit now on disk
		// (an update that finished meanwhile already refreshed the
		// status), so it is dropped; the next pass asks again.
		return nil
	}
	if m.updates == nil {
		m.updates = map[string]*updateState{}
	}
	st := m.updates[id]
	if st == nil || st.ref != ref {
		st = &updateState{ref: ref}
		m.updates[id] = st
	}
	st.checkedAt = m.now()
	st.pinned = pinned
	if err != nil {
		err = redactURLErr(err)
		st.err = err.Error()
		st.latest = ""
		return err
	}
	st.err = ""
	st.latest = latest
	return nil
}

// ErrCheckBusy is returned by CheckUpdates when another pass (usually
// the periodic one, which may be applying updates) holds the slot.
var ErrCheckBusy = errors.New("an update check is already running")

// CheckUpdates asks every installed package's remote whether its ref
// has moved. timeout bounds each package's query. Per-package failures
// are recorded in the package's status rather than returned; the error
// is ErrCheckBusy when a pass is already running, or ctx's error when
// the pass was cut short.
func (m *Manager) CheckUpdates(ctx context.Context, timeout time.Duration) error {
	// Never queue behind a running pass: it may be fetching updates for
	// minutes, and it refreshes the same statuses anyway.
	select {
	case m.checkSem <- struct{}{}:
	default:
		return ErrCheckBusy
	}
	defer m.releaseCheck()
	m.checkAllLocked(ctx, timeout)
	return ctx.Err()
}

// acquireCheck takes the pass slot, or gives up when ctx ends.
func (m *Manager) acquireCheck(ctx context.Context) error {
	select {
	case m.checkSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) releaseCheck() { <-m.checkSem }

func (m *Manager) checkAllLocked(ctx context.Context, timeout time.Duration) {
	for _, row := range m.List() {
		if ctx.Err() != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		if err := m.checkOne(cctx, row.ID); err != nil && !errors.Is(err, ErrNotFound) {
			m.logger.Warn("extension update check failed", "id", row.ID, "error", err)
		}
		cancel()
	}
}

// CheckAndAutoUpdate runs CheckUpdates and then applies the update for
// every package that opted into AutoUpdate and has one available.
// checkTimeout bounds each remote query and updateTimeout each update.
// It returns the IDs it updated, so the caller can restart their
// services.
func (m *Manager) CheckAndAutoUpdate(ctx context.Context, checkTimeout, updateTimeout time.Duration) []string {
	if err := m.acquireCheck(ctx); err != nil {
		return nil
	}
	defer m.releaseCheck()
	m.checkAllLocked(ctx, checkTimeout)

	var updated []string
	for _, id := range m.autoUpdateCandidates() {
		if ctx.Err() != nil {
			break
		}
		uctx, cancel := context.WithTimeout(ctx, updateTimeout)
		// An empty request re-fetches the stored ref and carries no
		// scope acknowledgement, so a commit that changes the scope
		// set comes back as ScopeMismatchError instead of installing.
		row, err := m.updateIf(uctx, id, UpdateRequest{}, func(row *Installed) error {
			if !row.AutoUpdate {
				return errAutoUpdateOff
			}
			return nil
		})
		cancel()
		if errors.Is(err, errAutoUpdateOff) {
			continue
		}
		if err != nil {
			m.noteAutoFailure(id, err)
			continue
		}
		m.logger.Info("extension auto-updated", "id", id, "version", row.Manifest.Version, "commit", row.Commit)
		updated = append(updated, id)
	}
	return updated
}

// autoUpdateCandidates lists the packages the pass should update.
func (m *Manager) autoUpdateCandidates() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, row := range m.st.Extensions {
		if !row.AutoUpdate {
			continue
		}
		st, ok := m.statusLocked(row)
		if !ok || !st.Available {
			continue
		}
		if s := m.updates[id]; s.autoFinal && s.autoCommit == s.latest {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// errAutoUpdateOff aborts an auto-update whose package was opted out
// while its fetch was running.
var errAutoUpdateOff = errors.New("auto-update was turned off")

// credentialRe matches the userinfo part of a URL ("https://user:token@").
var credentialRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)

// redactURLErr strips URL credentials from a git error before it is
// logged or shown: git echoes the remote URL in its messages, and an
// https remote may carry a token.
func redactURLErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	clean := credentialRe.ReplaceAllString(msg, "${1}***@")
	if clean == msg {
		return err
	}
	return errors.New(clean)
}

// noteAutoFailure records why an auto-update did not happen.
func (m *Manager) noteAutoFailure(id string, err error) {
	var mismatch *ScopeMismatchError
	var incompatible *IncompatibleError
	consent := errors.As(err, &mismatch)
	final := consent || errors.As(err, &incompatible)
	err = redactURLErr(err)
	if consent {
		m.logger.Info("extension update needs scope consent; not applied automatically", "id", id)
	} else {
		m.logger.Warn("extension auto-update failed", "id", id, "error", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.updates[id]
	if st == nil {
		return
	}
	st.autoCommit = st.latest
	st.autoErr = err.Error()
	st.autoFinal = final
	st.needsConsent = consent
}
