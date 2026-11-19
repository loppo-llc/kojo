package extpkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sentinel errors callers map onto HTTP status codes.
var (
	ErrNotFound         = errors.New("extension not found")
	ErrAlreadyInstalled = errors.New("extension already installed")
)

// ScopeMismatchError reports that the operator's acknowledgement did
// not cover exactly the scopes the manifest requests. The manifest is
// attached so the caller can re-render the consent dialog without a
// second network round trip.
type ScopeMismatchError struct {
	Manifest *Manifest
	Missing  []string
	Extra    []string
}

func (e *ScopeMismatchError) Error() string {
	return fmt.Sprintf("scope acknowledgement mismatch (missing: %v, unexpected: %v)", e.Missing, e.Extra)
}

// IncompatibleError reports a package whose manifest rules out the
// running kojo version. Typed so the auto-updater can tell "this
// commit will never install here" from a transient fetch failure.
type IncompatibleError struct {
	ID         string
	Constraint string
	Current    string
}

func (e *IncompatibleError) Error() string {
	return fmt.Sprintf("extension %s requires kojo %s, this instance is %s", e.ID, e.Constraint, e.Current)
}

// Manager owns the on-disk extension registry. Every mutation is
// serialised by mu and persisted before it is visible to readers, so a
// crash mid-install leaves either the old state or the new one.
type Manager struct {
	root        string
	kojoVersion string
	logger      *slog.Logger
	now         func() time.Time

	mu sync.Mutex
	st *registryState
	// gen stamps each registered package with a serial number. Update
	// does its git fetch with the lock released, so by the time it
	// swaps the checkout in, the row it read at the start may have been
	// removed, reinstalled, or updated by somebody else. Comparing the
	// stamp across that gap catches all three. Values come from a
	// single counter rather than a per-ID one so an entry can be
	// dropped on removal without a later reinstall reusing its number,
	// which keeps the map the size of the registry instead of the size
	// of its history. In memory only: a restart ends every in-flight
	// update anyway.
	gen     map[string]uint64
	nextGen uint64
	// apiBase is the loopback URL extensions call kojo back on. It is
	// only known once the listener is bound, so it arrives after
	// construction via SetAPIBase.
	apiBase string
	// secrets is the encrypted store secret settings are filed in
	// (see secrets.go). Nil until the server wires one in.
	secrets SecretStore
	// undoSecrets is set by a mutate callback that wrote to the
	// credential store; see mutate. Guarded by mu.
	undoSecrets func()
	// updates holds the result of the last upstream check per package
	// (see updatecheck.go). In memory only, guarded by mu: it is a
	// cache of remote state, refilled by the next check after a
	// restart.
	updates map[string]*updateState
	// checkSem serialises whole check/auto-update passes so the
	// periodic loop and an operator's "check now" never fetch the same
	// package twice at once. A one-slot channel rather than a mutex so
	// a waiter can give up when its context ends.
	checkSem chan struct{}
}

// bumpGen stamps a package ID with a fresh serial, marking every
// in-flight update over that ID stale. Caller holds m.mu.
func (m *Manager) bumpGen(id string) {
	if m.gen == nil {
		m.gen = map[string]uint64{}
	}
	m.nextGen++
	m.gen[id] = m.nextGen
}

// SetAPIBase records the URL handed to extension processes as
// KOJO_API_BASE. Idempotent; safe to call again after a rebind.
func (m *Manager) SetAPIBase(base string) {
	m.mu.Lock()
	m.apiBase = strings.TrimRight(base, "/")
	m.mu.Unlock()
}

// APIBase returns the URL extensions call back on, or "" before the
// listener is up.
func (m *Manager) APIBase() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.apiBase
}

// NewManager opens (or creates) the registry rooted at root.
// kojoVersion is the running daemon's version string, used to enforce
// each manifest's kojoVersion constraint.
func NewManager(root, kojoVersion string, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	for _, d := range []string{root, filepath.Join(root, pkgDirName), filepath.Join(root, tmpDirName), filepath.Join(root, dataDirName)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	st, err := loadState(filepath.Join(root, stateFilename))
	if err != nil {
		return nil, err
	}
	m := &Manager{
		root:        root,
		kojoVersion: kojoVersion,
		logger:      logger.With("component", "extpkg"),
		now:         time.Now,
		st:          st,
		checkSem:    make(chan struct{}, 1),
	}
	m.sweepTmp()
	return m, nil
}

// Root is the registry directory.
func (m *Manager) Root() string { return m.root }

// Dir returns the checkout directory for an installed extension.
func (m *Manager) Dir(id string) string { return pkgPath(m.root, id) }

// sweepTmp removes staging directories left behind by an install that
// was interrupted before its rename.
func (m *Manager) sweepTmp() {
	tmp := filepath.Join(m.root, tmpDirName)
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(tmp, e.Name())); err != nil {
			m.logger.Warn("sweep staging dir failed", "name", e.Name(), "error", err)
		}
	}
}

// List returns every installed extension, ordered by ID.
func (m *Manager) List() []Installed {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.st.sortedIDs()
	out := make([]Installed, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.st.Extensions[id].clone())
	}
	return out
}

// Get returns one extension by ID.
func (m *Manager) Get(id string) (Installed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.st.Extensions[id]
	if !ok {
		return Installed{}, ErrNotFound
	}
	return row.clone(), nil
}

// Preview fetches a package into a staging directory, validates it,
// and returns its manifest and resolved commit without installing
// anything. The web UI calls this to render the scope-consent dialog
// and passes the commit back to Install, which refuses to install
// anything else (see InstallRequest.Commit).
func (m *Manager) Preview(ctx context.Context, url, ref string) (*Manifest, string, error) {
	stage, mf, commit, err := m.stage(ctx, url, ref)
	if stage != "" {
		defer os.RemoveAll(stage)
	}
	if err != nil {
		return nil, "", err
	}
	return mf, commit, nil
}

// InstallRequest is the input to Install.
type InstallRequest struct {
	URL string
	Ref string
	// AckScopes is the scope list the operator approved. It must
	// match the manifest's scopes exactly.
	AckScopes []string
	// Enabled sets the initial enablement; installs default to on.
	Enabled bool
	// Commit, when set, is the commit Preview resolved. A branch is a
	// moving target: without this, the code that gets installed is
	// whatever the ref points at on the second fetch, not the one
	// whose manifest the operator just approved. Empty means "no
	// preview happened" (API clients that install blind).
	Commit string
}

// Install fetches, validates and registers a package.
func (m *Manager) Install(ctx context.Context, req InstallRequest) (Installed, error) {
	stage, mf, commit, err := m.stage(ctx, req.URL, req.Ref)
	if stage != "" {
		defer os.RemoveAll(stage)
	}
	if err != nil {
		return Installed{}, err
	}
	if want := strings.TrimSpace(req.Commit); want != "" && want != commit {
		return Installed{}, fmt.Errorf(
			"%s moved between preview and install (approved %s, now %s); review it again",
			strings.TrimSpace(req.Ref), shortCommit(want), shortCommit(commit))
	}
	if err := checkScopeAck(mf, req.AckScopes); err != nil {
		return Installed{}, err
	}
	// Mint before taking the lock: a failing CSPRNG must abort the
	// install rather than register a package that can never
	// authenticate.
	token, err := newExtensionToken()
	if err != nil {
		return Installed{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.st.Extensions[mf.ID]; exists {
		return Installed{}, fmt.Errorf("%w: %s", ErrAlreadyInstalled, mf.ID)
	}
	dst := pkgPath(m.root, mf.ID)
	// A directory with no registry row is debris from an earlier
	// failed install; the row check above already proved the ID is
	// free, so clearing it cannot destroy a live extension.
	if err := os.RemoveAll(dst); err != nil {
		return Installed{}, err
	}
	if err := os.Rename(stage, dst); err != nil {
		return Installed{}, fmt.Errorf("install %s: %w", mf.ID, err)
	}
	now := m.now().UTC().Format(time.RFC3339)
	row := &Installed{
		ID:            mf.ID,
		Source:        strings.TrimSpace(req.URL),
		Ref:           strings.TrimSpace(req.Ref),
		Commit:        commit,
		InstalledAt:   now,
		UpdatedAt:     now,
		Enabled:       req.Enabled,
		GrantedScopes: append([]string(nil), mf.Scopes...),
		Token:         token,
		Manifest:      *mf,
	}
	m.st.Extensions[mf.ID] = row
	m.bumpGen(mf.ID)
	if err := m.save(); err != nil {
		// Roll the checkout back so disk and registry agree. The
		// serial goes with it: nothing was registered, so leaving a
		// stamp behind would only grow the map.
		os.RemoveAll(dst)
		delete(m.st.Extensions, mf.ID)
		delete(m.gen, mf.ID)
		return Installed{}, err
	}
	// A status left over from an earlier registration of this ID
	// describes somebody else's source; start clean.
	delete(m.updates, mf.ID)
	m.logger.Info("extension installed", "id", mf.ID, "version", mf.Version, "commit", commit)
	return row.clone(), nil
}

// UpdateRequest is the input to Update.
type UpdateRequest struct {
	// Ref overrides the stored ref when non-empty.
	Ref string
	// AckScopes is required only when the new manifest asks for a
	// different scope set than the one already granted.
	AckScopes []string
}

// Update re-fetches an installed package at the given ref and swaps
// the checkout in place. Enablement and configuration survive.
func (m *Manager) Update(ctx context.Context, id string, req UpdateRequest) (Installed, error) {
	return m.updateIf(ctx, id, req, nil)
}

// updateIf is Update with a last-moment veto: accept runs under the
// lock just before the new checkout is swapped in, against the row as
// it is then, and a non-nil error aborts the swap. The auto-updater
// uses it so an operator who turns auto-update off while a fetch is
// running still wins.
func (m *Manager) updateIf(ctx context.Context, id string, req UpdateRequest, accept func(row *Installed) error) (Installed, error) {
	m.mu.Lock()
	row, ok := m.st.Extensions[id]
	if !ok {
		m.mu.Unlock()
		return Installed{}, ErrNotFound
	}
	source, ref := row.Source, row.Ref
	granted := append([]string(nil), row.GrantedScopes...)
	startGen := m.gen[id]
	m.mu.Unlock()

	if strings.TrimSpace(req.Ref) != "" {
		ref = strings.TrimSpace(req.Ref)
	}
	stage, mf, commit, err := m.stage(ctx, source, ref)
	if stage != "" {
		defer os.RemoveAll(stage)
	}
	if err != nil {
		return Installed{}, err
	}
	if mf.ID != id {
		return Installed{}, fmt.Errorf("update %s: remote package now declares id %q", id, mf.ID)
	}
	// Only re-prompt when the requested capabilities actually
	// changed; an unchanged scope set updates silently.
	if !sameScopeSet(granted, mf.Scopes) {
		if err := checkScopeAck(mf, req.AckScopes); err != nil {
			return Installed{}, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok = m.st.Extensions[id]
	if !ok {
		return Installed{}, ErrNotFound
	}
	// The registration changed while the fetch was running: either
	// somebody uninstalled and reinstalled this ID, or a second update
	// already finished. Both mean the checkout staged above describes a
	// package that is no longer the one on disk, so it must not be
	// swapped in.
	if m.gen[id] != startGen {
		return Installed{}, fmt.Errorf("update %s: the package changed while updating; try again", id)
	}
	if accept != nil {
		if err := accept(row); err != nil {
			return Installed{}, err
		}
	}
	dst := pkgPath(m.root, id)
	backup := dst + ".old"
	_ = os.RemoveAll(backup)
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, backup); err != nil {
			return Installed{}, err
		}
	}
	if err := os.Rename(stage, dst); err != nil {
		// Put the previous checkout back before giving up.
		_ = os.Rename(backup, dst)
		return Installed{}, err
	}
	prev := *row
	row.Ref = ref
	row.Commit = commit
	row.UpdatedAt = m.now().UTC().Format(time.RFC3339)
	row.GrantedScopes = append([]string(nil), mf.Scopes...)
	row.Manifest = *mf
	if err := m.save(); err != nil {
		*row = prev
		_ = os.RemoveAll(dst)
		_ = os.Rename(backup, dst)
		return Installed{}, err
	}
	// A completed update counts as a re-registration too. Two updates
	// racing over one ID both pass the check above with the same
	// starting value otherwise, and the slower fetch — carrying older
	// code — lands last and wins.
	m.bumpGen(id)
	_ = os.RemoveAll(backup)
	// The new schema may mark a setting secret that the old one kept
	// in the clear; move any such value out of the registry.
	if m.migrateSecretsLocked() {
		if err := m.save(); err != nil {
			m.logger.Warn("saving the registry after secret migration failed", "id", id, "error", err)
		}
	}
	m.noteUpdatedLocked(id, ref, commit)
	m.logger.Info("extension updated", "id", id, "version", mf.Version, "commit", commit)
	return row.clone(), nil
}

// Remove deletes the registry row and the checkout.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.st.Extensions[id]
	if !ok {
		return ErrNotFound
	}
	delete(m.st.Extensions, id)
	// Dropped even if the save below fails and the row comes back. The
	// serial never restarts, so an update that was in flight over this
	// ID still finds a value it does not recognise (0 against its own
	// non-zero stamp) and redoes its fetch — the safe direction to be
	// wrong in.
	delete(m.gen, id)
	if err := m.save(); err != nil {
		m.st.Extensions[id] = row
		return err
	}
	delete(m.updates, id)
	// The registry is the source of truth; a leftover directory is
	// harmless debris, so a failure here is logged, not returned.
	if err := os.RemoveAll(pkgPath(m.root, id)); err != nil {
		m.logger.Warn("remove extension directory failed", "id", id, "error", err)
	}
	if err := os.RemoveAll(filepath.Join(m.root, dataDirName, id)); err != nil {
		m.logger.Warn("remove extension data directory failed", "id", id, "error", err)
	}
	m.dropSecretsLocked(id, "")
	for agentID := range row.Agents {
		m.dropSecretsLocked(id, agentID)
	}
	m.logger.Info("extension removed", "id", id)
	return nil
}

// SetEnabled toggles the package as a whole.
func (m *Manager) SetEnabled(id string, enabled bool) (Installed, error) {
	return m.mutate(id, func(row *Installed) error {
		row.Enabled = enabled
		return nil
	})
}

// SetAutoUpdate opts the package in or out of unattended updates.
func (m *Manager) SetAutoUpdate(id string, on bool) (Installed, error) {
	return m.SetFlags(id, nil, &on)
}

// SetFlags changes the package-wide switches in one write. A nil
// pointer leaves that switch alone.
func (m *Manager) SetFlags(id string, enabled, autoUpdate *bool) (Installed, error) {
	return m.mutate(id, func(row *Installed) error {
		if enabled != nil {
			row.Enabled = *enabled
		}
		if autoUpdate != nil {
			row.AutoUpdate = *autoUpdate
		}
		return nil
	})
}

// SetConfig replaces the global settings object.
func (m *Manager) SetConfig(id string, cfg map[string]any) (Installed, error) {
	return m.mutate(id, func(row *Installed) error {
		if st := row.Manifest.Contributes.Settings; st == nil || st.Scope != ScopeGlobal {
			return fmt.Errorf("extension %s has no global settings", id)
		}
		names, undo, err := m.splitSecretsLocked(row, "", cfg, row.Secrets)
		if err != nil {
			return err
		}
		m.undoSecrets = undo
		row.Config = cfg
		row.Secrets = names
		return nil
	})
}

// SetAgentBinding enables/configures the extension for one agent.
func (m *Manager) SetAgentBinding(id, agentID string, b AgentBinding) (Installed, error) {
	return m.SetAgentBindingIf(id, agentID, b, nil)
}

// SetAgentBindingIf is SetAgentBinding with a condition on the package
// as it is at the moment of the write. accept sees the installed row
// and the settings its schema files as secrets, under the registry
// lock, and a non-nil error from it cancels the binding. It is for a
// caller that decided to hand a package secrets from an earlier read:
// the package may have been replaced, updated or unbound since, and the
// decision has to hold for the one actually written to.
func (m *Manager) SetAgentBindingIf(id, agentID string, b AgentBinding, accept func(row Installed, secretKeys []string) error) (Installed, error) {
	if err := validateAgentID(agentID); err != nil {
		return Installed{}, err
	}
	return m.mutate(id, func(row *Installed) error {
		if accept != nil {
			if err := accept(row.clone(), m.secretKeys(row)); err != nil {
				return err
			}
		}
		if !row.Manifest.hasPerAgentContribution() {
			return fmt.Errorf("extension %s has no per-agent contribution", id)
		}
		if row.Agents == nil {
			row.Agents = map[string]AgentBinding{}
		}
		// The caller cannot name secrets directly: the list is derived
		// from what the store actually holds for this agent.
		names, undo, err := m.splitSecretsLocked(row, agentID, b.Config, row.Agents[agentID].Secrets)
		if err != nil {
			return err
		}
		m.undoSecrets = undo
		b.Secrets = names
		row.Agents[agentID] = b
		return nil
	})
}

// ForgetAgent drops every binding for an agent. Called when an agent
// is deleted so the registry does not accumulate dead references.
func (m *Manager) ForgetAgent(agentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := map[string]AgentBinding{}
	for id, row := range m.st.Extensions {
		if b, ok := row.Agents[agentID]; ok {
			removed[id] = b
			delete(row.Agents, agentID)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	if err := m.save(); err != nil {
		// Memory and disk have to agree: an unbinding that only
		// happened in memory would come back on the next restart,
		// re-granting a deleted agent's binding.
		for id, b := range removed {
			if row, ok := m.st.Extensions[id]; ok {
				if row.Agents == nil {
					row.Agents = map[string]AgentBinding{}
				}
				row.Agents[agentID] = b
			}
		}
		return err
	}
	for id := range removed {
		m.dropSecretsLocked(id, agentID)
	}
	return nil
}

// SettingsSchema returns the raw JSON Schema an extension ships, for
// the generic settings form.
func (m *Manager) SettingsSchema(id string) ([]byte, error) {
	row, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	st := row.Manifest.Contributes.Settings
	if st == nil {
		return nil, fmt.Errorf("extension %s contributes no settings", id)
	}
	return os.ReadFile(filepath.Join(pkgPath(m.root, id), filepath.FromSlash(st.Schema)))
}

// SkillContribution is one installed skill directory.
type SkillContribution struct {
	ExtensionID string
	Name        string
	Dir         string
}

// SkillsForAgent lists the skill directories that should be present in
// the given agent's data directory: those from packages that are both
// globally enabled and bound to this agent.
func (m *Manager) SkillsForAgent(agentID string) []SkillContribution {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SkillContribution
	for _, id := range m.st.sortedIDs() {
		row := m.st.Extensions[id]
		if !row.Enabled {
			continue
		}
		if b, ok := row.Agents[agentID]; !ok || !b.Enabled {
			continue
		}
		for _, rel := range row.Manifest.Contributes.Skills {
			out = append(out, SkillContribution{
				ExtensionID: id,
				Name:        filepath.Base(filepath.FromSlash(rel)),
				Dir:         filepath.Join(pkgPath(m.root, id), filepath.FromSlash(rel)),
			})
		}
	}
	return out
}

// mutate applies fn to a row under the lock and persists the result,
// rolling back in memory if the write fails.
func (m *Manager) mutate(id string, fn func(*Installed) error) (Installed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.st.Extensions[id]
	if !ok {
		return Installed{}, ErrNotFound
	}
	// snapshot, not clone: clone blanks the token for the API, and
	// restoring one of those on a failed save would leave the package
	// unable to authenticate until it was reinstalled.
	prev := row.snapshot()
	// fn may have filed secrets in the credential store before the row
	// that names them is saved. It leaves the way back in undoSecrets,
	// so a failed save does not strand a value the registry denies.
	m.undoSecrets = nil
	defer func() { m.undoSecrets = nil }()
	if err := fn(row); err != nil {
		return Installed{}, err
	}
	if err := m.save(); err != nil {
		*row = prev
		if m.undoSecrets != nil {
			m.undoSecrets()
		}
		return Installed{}, err
	}
	return row.clone(), nil
}

func (m *Manager) save() error {
	return saveState(filepath.Join(m.root, stateFilename), m.st)
}

// stage fetches url@ref into a fresh staging directory and validates
// both the manifest and the files it points at. The caller owns the
// returned directory and must remove it unless it renames it into
// place.
func (m *Manager) stage(ctx context.Context, url, ref string) (string, *Manifest, string, error) {
	if err := ValidateSourceURL(url); err != nil {
		return "", nil, "", err
	}
	if err := ValidateRef(ref); err != nil {
		return "", nil, "", err
	}
	stage, err := os.MkdirTemp(filepath.Join(m.root, tmpDirName), "stage-")
	if err != nil {
		return "", nil, "", err
	}
	commit, err := fetchInto(ctx, stage, strings.TrimSpace(url), strings.TrimSpace(ref))
	if err != nil {
		return stage, nil, "", err
	}
	data, err := os.ReadFile(filepath.Join(stage, ManifestFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return stage, nil, "", fmt.Errorf("%s not found at the repository root", ManifestFilename)
		}
		return stage, nil, "", err
	}
	mf, err := ParseManifest(data)
	if err != nil {
		return stage, nil, "", err
	}
	ok, err := mf.SatisfiesKojoVersion(m.kojoVersion)
	if err != nil {
		return stage, nil, "", err
	}
	if !ok {
		return stage, nil, "", &IncompatibleError{ID: mf.ID, Constraint: mf.KojoVersion, Current: m.kojoVersion}
	}
	if err := verifyContributions(stage, mf); err != nil {
		return stage, nil, "", err
	}
	return stage, mf, commit, nil
}

// containedPath resolves a package-relative path and proves the result
// is still inside the checkout. checkRelPath (manifest validation) is
// lexical only, so on its own it stops "../../etc" but not a repository
// that ships `bin/srv` as a symlink to /usr/bin/env — the manifest
// looks clean and kojo executes, copies or reads whatever the link
// points at. Resolving the link before use is the only check that
// catches it.
func containedPath(root, rel string) (string, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	inside, err := filepath.Rel(realRoot, realFull)
	if err != nil {
		return "", err
	}
	if inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s resolves outside the package", rel)
	}
	return realFull, nil
}

// verifyContributions checks that every path the manifest names
// actually exists in the checkout and stays inside it once symlinks are
// resolved, so a broken or escaping package is rejected at install time
// rather than at first use.
func verifyContributions(dir string, m *Manifest) error {
	for _, rel := range m.Contributes.Skills {
		skillDir, err := containedPath(dir, rel)
		if err != nil {
			return fmt.Errorf("contributes.skills: %s is not a usable directory in the package: %w", rel, err)
		}
		info, err := os.Stat(skillDir)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("contributes.skills: %s is not a directory in the package", rel)
		}
		if _, err := containedPath(skillDir, "SKILL.md"); err != nil {
			return fmt.Errorf("contributes.skills: %s/SKILL.md is missing", rel)
		}
	}
	if st := m.Contributes.Settings; st != nil {
		schema, err := containedPath(dir, st.Schema)
		if err != nil {
			return fmt.Errorf("contributes.settings.schema: %s is missing from the package: %w", st.Schema, err)
		}
		data, err := os.ReadFile(schema)
		if err != nil {
			return fmt.Errorf("contributes.settings.schema: %s is missing from the package", st.Schema)
		}
		var probe map[string]any
		if err := json.Unmarshal(data, &probe); err != nil {
			return fmt.Errorf("contributes.settings.schema: %s is not a JSON object", st.Schema)
		}
	}
	// Locale files are parsed, not just stat'd: a catalogue that is
	// not a flat JSON object would leave the language installed and
	// permanently empty, and the operator would see a picker entry
	// that does nothing.
	for i, loc := range m.Contributes.Locales {
		file, err := containedPath(dir, loc.File)
		if err != nil {
			return fmt.Errorf("contributes.locales[%d].file: %s is missing from the package: %w", i, loc.File, err)
		}
		if _, err := readLocaleFile(file); err != nil {
			return fmt.Errorf("contributes.locales[%d].file: %s is not a usable message catalogue: %w", i, loc.File, err)
		}
	}
	// A relative MCP command is part of the package, so it has to be
	// there and be runnable now — the alternative is a server that
	// fails to spawn on the agent's next turn, where the operator
	// never sees the error.
	for i, srv := range m.Contributes.MCPServers {
		cmd := strings.TrimSpace(srv.Command)
		label := fmt.Sprintf("contributes.mcpServers[%d].command", i)
		if len(srv.Exec) > 0 {
			key := runtime.GOOS + "/" + runtime.GOARCH
			rel, ok := srv.Exec[key]
			if !ok {
				return fmt.Errorf("extension MCP server %q has no executable for %s", srv.Name, key)
			}
			cmd = rel
			label = fmt.Sprintf("contributes.mcpServers[%d].exec[%s]", i, key)
		} else if !strings.ContainsAny(cmd, `/\`) {
			// Bare name: resolved through the child's PATH at spawn
			// time, which is not kojo's to validate here.
			continue
		}
		bin, err := containedPath(dir, cmd)
		if err != nil {
			return fmt.Errorf("%s: %s is missing from the package: %w", label, cmd, err)
		}
		info, err := os.Stat(bin)
		if err != nil || info.IsDir() {
			return fmt.Errorf("%s: %s is not a file in the package", label, cmd)
		}
		if err := os.Chmod(bin, 0o755); err != nil {
			return err
		}
	}
	if svc := m.Contributes.Service; svc != nil {
		key := runtime.GOOS + "/" + runtime.GOARCH
		rel, ok := svc.Exec[key]
		if !ok {
			platforms := make([]string, 0, len(svc.Exec))
			for k := range svc.Exec {
				platforms = append(platforms, k)
			}
			sort.Strings(platforms)
			return fmt.Errorf("extension has no executable for %s (available: %s)", key, strings.Join(platforms, ", "))
		}
		bin, err := containedPath(dir, rel)
		if err != nil {
			return fmt.Errorf("contributes.service.exec[%s]: %s is missing from the package: %w", key, rel, err)
		}
		info, err := os.Stat(bin)
		if err != nil || info.IsDir() {
			return fmt.Errorf("contributes.service.exec[%s]: %s is missing from the package", key, rel)
		}
		// git preserves only the executable bit it recorded; make
		// the entry point runnable regardless of how it was packed.
		if err := os.Chmod(bin, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// hasPerAgentContribution reports whether anything in the manifest is
// scoped per agent, which is what makes a per-agent binding meaningful.
func (m *Manifest) hasPerAgentContribution() bool {
	if len(m.Contributes.Skills) > 0 || len(m.Contributes.MCPServers) > 0 {
		return true
	}
	if svc := m.Contributes.Service; svc != nil && svc.Scope == ScopePerAgent {
		return true
	}
	if st := m.Contributes.Settings; st != nil && st.Scope == ScopePerAgent {
		return true
	}
	return false
}

// checkScopeAck requires the operator's acknowledgement to match the
// manifest exactly. Approving a subset would leave the extension
// broken at runtime; approving a superset means the UI showed the
// operator something the package did not ask for.
func checkScopeAck(m *Manifest, ack []string) error {
	want := map[string]bool{}
	for _, s := range m.Scopes {
		want[s] = true
	}
	got := map[string]bool{}
	for _, s := range ack {
		got[s] = true
	}
	var missing, extra []string
	for s := range want {
		if !got[s] {
			missing = append(missing, s)
		}
	}
	for s := range got {
		if !want[s] {
			extra = append(extra, s)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return &ScopeMismatchError{Manifest: m, Missing: missing, Extra: extra}
}

func sameScopeSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// shortCommit abbreviates a SHA for an error message.
func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}
