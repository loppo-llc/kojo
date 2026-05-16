// Package externalcli implements docs/multi-device-storage.md §5.5.1's
// continuity layer for path-hashed external-CLI transcript stores
// (claude, codex) and the mapping-file store (gemini).
//
// Forward (`--migrate-external-cli`): when v0 → v1 changes the agent
// working directory from <v0>/agents/<id> to <v1>/agents/<id>, the
// external CLIs would otherwise lose their prior chat history. We
// create a symlink (or, on Windows, a junction) at the v1-hash
// directory pointing at the v0-hash directory so each CLI's internal
// path-hash lookup still resolves to the same on-disk state. For
// gemini we append a (v1_abs_dir → existing project_name) row to
// projects.json so the same chats dir is reachable under both paths.
//
// Reverse (`--rollback-external-cli`): every forward operation is
// recorded into a manifest under the v1 dir. Rollback walks the
// manifest in reverse and undoes each entry. Importantly the
// v0-hash target is NEVER touched — symlink removal uses os.Remove
// (not os.RemoveAll) so the underlying transcript store stays
// intact for an operator who is rolling back to v0.
//
// All forward operations are best-effort: any individual failure is
// logged as a warning and the migration continues. The v1 install
// continues to work; the affected agent simply starts with a fresh
// chat session.
package externalcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestFileName is the on-disk record of forward operations,
// written under the v1 dir. Its presence is the trigger that
// `kojo --rollback-external-cli` looks for.
const ManifestFileName = "external_cli_migration.json"

// OpKind enumerates reversible operations we know how to perform.
type OpKind string

const (
	// OpSymlink: a path-hash directory symlink was created. Path =
	// the new (v1) symlink. Target = the v0 hash directory it points
	// at. Reverse with os.Remove(Path) (do not touch Target).
	OpSymlink OpKind = "symlink"

	// OpGeminiProjectAdd: a new map entry was appended to
	// gemini's projects.json. Path = absolute path of projects.json.
	// Target = the v1 absolute directory key that we added. Reverse
	// by reading the file, removing the key, atomic-rewrite.
	OpGeminiProjectAdd OpKind = "gemini_project_add"
)

// Op is one reversible forward action recorded for rollback.
type Op struct {
	Kind   OpKind `json:"kind"`
	Path   string `json:"path"`
	Target string `json:"target,omitempty"`
	// AgentID is informational — included to help an operator who is
	// auditing the manifest understand why each entry exists.
	AgentID string `json:"agent_id,omitempty"`
}

// Manifest is the JSON file written under the v1 dir.
type Manifest struct {
	Version int  `json:"version"`
	Ops     []Op `json:"ops"`
}

const manifestVersion = 1

// LoadManifest reads the manifest from v1Dir. Returns (nil, nil) when
// the file is absent — rollback then has nothing to undo.
func LoadManifest(v1Dir string) (*Manifest, error) {
	path := filepath.Join(v1Dir, ManifestFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("externalcli: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("externalcli: parse manifest: %w", err)
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("externalcli: manifest version %d not supported", m.Version)
	}
	return &m, nil
}

// SaveManifest writes the manifest atomically. An empty Ops slice
// still writes the file so a subsequent rollback knows the forward
// pass ran (vs "we forgot to record anything").
func SaveManifest(v1Dir string, m *Manifest) error {
	if m == nil {
		return errors.New("externalcli.SaveManifest: nil manifest")
	}
	m.Version = manifestVersion
	if err := os.MkdirAll(v1Dir, 0o755); err != nil {
		return fmt.Errorf("externalcli: mkdir v1: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(v1Dir, ManifestFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// PlanInput is the per-agent data the planner needs to decide which
// operations to record. Caller (cmd/kojo/migrate.go) converts the
// migration's agent list into a slice of these.
type PlanInput struct {
	AgentID  string
	V0Dir    string // <v0>/agents/<id>
	V1Dir    string // <v1>/agents/<id>
}

// Hasher is the path-hash function used by claude / codex. The forward
// pass reuses internal/agent's claudeEncodePath; tests can substitute
// a stub.
type Hasher func(absDir string) string

// CLISpec describes one path-hash CLI's transcript layout for the
// forward pass. ProjectsRoot is the directory under which each CLI
// keeps a per-path subdir (e.g. ~/.claude/projects/).
type CLISpec struct {
	Name         string
	ProjectsRoot string
	Hash         Hasher
}

// PlanSymlinks builds the list of OpSymlink entries for the given
// CLIs and agents. Skips any (cli, agent) pair where:
//
//   - the v0 hash directory does not exist (CLI never ran for that agent
//     in v0; nothing to alias)
//   - the v1 hash directory already exists (collision; do not clobber)
//
// PlanSymlinks does NOT touch the filesystem; it only computes what
// Apply would do. Used by tests to assert plan correctness.
func PlanSymlinks(specs []CLISpec, agents []PlanInput) []Op {
	var ops []Op
	for _, sp := range specs {
		for _, ag := range agents {
			v0Hash := sp.Hash(ag.V0Dir)
			v1Hash := sp.Hash(ag.V1Dir)
			target := filepath.Join(sp.ProjectsRoot, v0Hash)
			link := filepath.Join(sp.ProjectsRoot, v1Hash)
			if !isExistingDir(target) {
				continue
			}
			if existsAny(link) {
				continue
			}
			ops = append(ops, Op{
				Kind:    OpSymlink,
				Path:    link,
				Target:  target,
				AgentID: ag.AgentID,
			})
		}
	}
	// Stable order so manifest diffs are reviewable.
	sort.Slice(ops, func(i, j int) bool { return ops[i].Path < ops[j].Path })
	return ops
}

// ApplyForward executes the planned operations and records each
// successful one into the returned Manifest. Failures are turned
// into warnings (returned as the second value); a partial manifest is
// still returned so rollback can undo the operations that did succeed.
func ApplyForward(v1Dir string, ops []Op) (*Manifest, []string) {
	m := &Manifest{Version: manifestVersion}
	var warnings []string
	for _, op := range ops {
		switch op.Kind {
		case OpSymlink:
			if err := createSymlink(op.Target, op.Path); err != nil {
				warnings = append(warnings, fmt.Sprintf("symlink %s -> %s: %v", op.Path, op.Target, err))
				continue
			}
		case OpGeminiProjectAdd:
			if err := addGeminiProject(op.Path, op.Target, op.AgentID); err != nil {
				warnings = append(warnings, fmt.Sprintf("gemini projects.json: %v", err))
				continue
			}
		default:
			warnings = append(warnings, fmt.Sprintf("unknown op kind %q (skipped)", op.Kind))
			continue
		}
		m.Ops = append(m.Ops, op)
	}
	if err := SaveManifest(v1Dir, m); err != nil {
		warnings = append(warnings, fmt.Sprintf("save manifest: %v", err))
	}
	return m, warnings
}

// Rollback walks the manifest in reverse and undoes each operation.
// Errors are returned as warnings — rollback is best-effort by design;
// an entry the operator already cleaned up by hand should not block
// the remaining entries.
func Rollback(v1Dir string) ([]string, error) {
	m, err := LoadManifest(v1Dir)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return []string{"externalcli: no manifest at " + v1Dir + " — nothing to roll back"}, nil
	}
	var warnings []string
	// Reverse order: gemini-project-add entries should be undone in the
	// reverse of their insertion so a hand-edited projects.json
	// preserves any other appends that happened after migration.
	for i := len(m.Ops) - 1; i >= 0; i-- {
		op := m.Ops[i]
		switch op.Kind {
		case OpSymlink:
			if err := removeSymlink(op.Path, op.Target); err != nil {
				warnings = append(warnings, fmt.Sprintf("remove %s: %v", op.Path, err))
			}
		case OpGeminiProjectAdd:
			if err := removeGeminiProject(op.Path, op.Target); err != nil {
				warnings = append(warnings, fmt.Sprintf("revert %s: %v", op.Path, err))
			}
		default:
			warnings = append(warnings, fmt.Sprintf("unknown op kind %q in manifest (skipped)", op.Kind))
		}
	}
	// Manifest stays on disk after rollback as a record. Subsequent
	// forward run will overwrite it.
	return warnings, nil
}

// --- internals -------------------------------------------------------

// createSymlink: refuse to clobber, fail loudly if the target is not a
// directory (junctions on Windows are dir-only too).
func createSymlink(target, link string) error {
	if !isExistingDir(target) {
		return fmt.Errorf("target %s missing or not a directory", target)
	}
	if existsAny(link) {
		return fmt.Errorf("link path %s already exists", link)
	}
	return os.Symlink(target, link)
}

// removeSymlink: only delete if the link still points at our target —
// otherwise an operator may have replaced it with a real directory and
// we must not destroy that. Symlink-or-junction removal uses os.Remove
// (NOT os.RemoveAll, which would follow into the target).
func removeSymlink(link, expectedTarget string) error {
	got, err := os.Readlink(link)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // already removed; treat as success
		}
		return err
	}
	if got != expectedTarget {
		return fmt.Errorf("link %s points at %s, not %s; refusing to remove", link, got, expectedTarget)
	}
	return os.Remove(link)
}

// geminiProjects is the on-disk shape of ~/.gemini/projects.json. We
// keep the full Projects map so a re-marshal preserves keys we don't
// care about.
type geminiProjects struct {
	Projects map[string]string `json:"projects"`
	// Other fields are passed through via the raw map below.
}

// addGeminiProject reads/writes projects.json. `key` is the v1 abs dir
// to add; `agentID` is informational. The mapping value is taken from
// the existing v0 key for the same agent: we look for a key whose
// suffix matches "/agents/<id>" and copy its value. Without that
// fallback we cannot know what projectName gemini gave the v0 dir.
func addGeminiProject(path, v1Key, agentID string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("projects.json missing")
		}
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("projects.json parse: %w", err)
	}
	projsRaw, ok := doc["projects"].(map[string]any)
	if !ok {
		// older / different format — refuse, leave the file intact.
		return errors.New("projects.json: 'projects' field absent or wrong type")
	}
	if _, exists := projsRaw[v1Key]; exists {
		return nil // already present (idempotent)
	}
	suffix := "/agents/" + agentID
	var v0Value string
	for k, v := range projsRaw {
		if strings.HasSuffix(k, suffix) {
			if s, ok := v.(string); ok && s != "" {
				v0Value = s
				break
			}
		}
	}
	if v0Value == "" {
		return errors.New("no v0 entry to mirror")
	}
	projsRaw[v1Key] = v0Value
	doc["projects"] = projsRaw

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removeGeminiProject(path, v1Key string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	projsRaw, ok := doc["projects"].(map[string]any)
	if !ok {
		return nil
	}
	if _, exists := projsRaw[v1Key]; !exists {
		return nil
	}
	delete(projsRaw, v1Key)
	doc["projects"] = projsRaw
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func isExistingDir(p string) bool {
	info, err := os.Lstat(p)
	if err != nil {
		return false
	}
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

// existsAny returns true for any directory entry — file, dir, symlink.
// Used to refuse clobbering at link creation time.
func existsAny(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
