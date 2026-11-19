package extpkg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/loppo-llc/kojo/internal/atomicfile"
)

// stateVersion is bumped when the on-disk registry shape changes in a
// way that needs migrating. Reading a newer version is a hard error:
// silently ignoring unknown fields would drop an operator's config on
// the next write.
const stateVersion = 1

const (
	stateFilename = "state.json"
	pkgDirName    = "pkg"
	tmpDirName    = "tmp"
	// dataDirName holds per-extension writable state. It sits beside
	// the checkouts rather than inside them so an update, which
	// replaces a checkout wholesale, never destroys package data.
	dataDirName = "data"
)

// AgentBinding is an extension's per-agent enablement and settings.
type AgentBinding struct {
	Enabled bool           `json:"enabled"`
	Config  map[string]any `json:"config,omitempty"`
	// Secrets names the secret settings stored for this agent. Only
	// the names live here; the values are in the credential store
	// (see secrets.go).
	Secrets []string `json:"secrets,omitempty"`
}

// Installed is one registry row: where the package came from, which
// commit is checked out, what the operator granted it, and how it is
// configured globally and per agent.
type Installed struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	// Ref is the branch/tag/SHA the operator asked for; empty means
	// the remote's default branch. Commit is what it resolved to.
	Ref         string `json:"ref,omitempty"`
	Commit      string `json:"commit"`
	InstalledAt string `json:"installedAt"`
	UpdatedAt   string `json:"updatedAt"`
	// Enabled gates the whole package. Per-agent contributions need
	// both this and the agent's own binding.
	Enabled bool `json:"enabled"`
	// AutoUpdate opts the package into unattended updates: when the
	// periodic check sees its ref move on the remote, kojo applies the
	// new commit itself — unless it asks for a different scope set or
	// a newer kojo, which still need the operator. Off by default.
	AutoUpdate    bool     `json:"autoUpdate,omitempty"`
	GrantedScopes []string `json:"grantedScopes,omitempty"`
	// Token is the package's bearer token (see token.go). It is
	// persisted but never leaves the process through clone(), so the
	// HTTP handlers — which serialize clones — cannot leak it into a
	// list response. Read it deliberately via Manager.Token.
	Token  string         `json:"token,omitempty"`
	Config map[string]any `json:"config,omitempty"`
	// Secrets names the global secret settings that are set. Values
	// are kept in the credential store, never here (see secrets.go).
	Secrets  []string                `json:"secrets,omitempty"`
	Agents   map[string]AgentBinding `json:"agents,omitempty"`
	Manifest Manifest                `json:"manifest"`
}

// clone deep-copies the row so callers can never mutate registry state
// through a returned pointer. The bearer token is stripped: every
// value that leaves the Manager goes through here, and an extension's
// token has no business appearing in an API response body.
func (i *Installed) clone() Installed {
	out := i.snapshot()
	out.Token = ""
	return out
}

// snapshot is clone's in-process twin: the same deep copy, token
// included. Rollback paths use it, because restoring a token-stripped
// copy over a live row would silently disable the package.
func (i *Installed) snapshot() Installed {
	out := *i
	out.GrantedScopes = append([]string(nil), i.GrantedScopes...)
	out.Config = cloneMap(i.Config)
	out.Secrets = append([]string(nil), i.Secrets...)
	if i.Agents != nil {
		out.Agents = make(map[string]AgentBinding, len(i.Agents))
		for k, v := range i.Agents {
			out.Agents[k] = AgentBinding{
				Enabled: v.Enabled,
				Config:  cloneMap(v.Config),
				Secrets: append([]string(nil), v.Secrets...),
			}
		}
	}
	out.Manifest = i.Manifest.clone()
	return out
}

// clone deep-copies a manifest. The registry hands manifests out by
// value, and a value copy still shares every slice and map inside it —
// so a caller appending to Contributes.Skills or writing to a
// Service.Env it was handed would be editing live registry state.
func (m Manifest) clone() Manifest {
	out := m
	out.Scopes = append([]string(nil), m.Scopes...)
	out.Contributes.Skills = append([]string(nil), m.Contributes.Skills...)
	if m.Contributes.MCPServers != nil {
		servers := make([]MCPServer, 0, len(m.Contributes.MCPServers))
		for _, srv := range m.Contributes.MCPServers {
			srv.Args = append([]string(nil), srv.Args...)
			srv.Env = cloneStringMap(srv.Env)
			srv.Exec = cloneStringMap(srv.Exec)
			servers = append(servers, srv)
		}
		out.Contributes.MCPServers = servers
	}
	if svc := m.Contributes.Service; svc != nil {
		cp := *svc
		cp.Args = append([]string(nil), svc.Args...)
		cp.Env = cloneStringMap(svc.Env)
		cp.Exec = cloneStringMap(svc.Exec)
		out.Contributes.Service = &cp
	}
	if st := m.Contributes.Settings; st != nil {
		cp := *st
		out.Contributes.Settings = &cp
	}
	// LocaleFile is all strings, so copying the slice is the whole
	// deep copy.
	out.Contributes.Locales = append([]LocaleFile(nil), m.Contributes.Locales...)
	return out
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	// A JSON round trip is the only deep copy that is correct for
	// arbitrary decoded values (nested maps and slices).
	data, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

type registryState struct {
	Version    int                   `json:"version"`
	Extensions map[string]*Installed `json:"extensions"`
}

func loadState(path string) (*registryState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &registryState{Version: stateVersion, Extensions: map[string]*Installed{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var st registryState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Version > stateVersion {
		return nil, fmt.Errorf("extension registry version %d is newer than this kojo supports (%d)", st.Version, stateVersion)
	}
	if st.Extensions == nil {
		st.Extensions = map[string]*Installed{}
	}
	st.Version = stateVersion
	return &st, nil
}

func saveState(path string, st *registryState) error {
	return atomicfile.WriteJSON(path, st, 0o600)
}

// sortedIDs gives List a stable order regardless of map iteration.
func (st *registryState) sortedIDs() []string {
	ids := make([]string, 0, len(st.Extensions))
	for id := range st.Extensions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func pkgPath(root, id string) string { return filepath.Join(root, pkgDirName, id) }
