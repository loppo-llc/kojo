// Package blob is the native blob store described in
// docs/multi-device-storage.md §2.4 and §4.2. v1 phase 3 slice 1 ships a
// filesystem-only implementation: Get / Head / Put / Delete / List with
// atomic publish (temp + fsync + rename + parent dir fsync) plus content
// digest verification on Put. The blob_refs DB cache and the HTTP
// transport are deliberately not wired here — slice 2 (DB integration)
// and slice 3 (HTTP handler) layer on top of this package.
package blob

import (
	"errors"
	"path/filepath"
)

// Scope partitions blobs by replication policy. The three values map
// 1-1 to the on-disk subtrees of the kojo config dir:
//
//	<root>/global/   — replicated across peers (avatars, books, outbox, MEMORY.md)
//	<root>/local/    — peer-local only (FTS index, transient temp)
//	<root>/machine/  — never leaves the host (credentials, machine secrets)
//
// `cas` (content-addressed storage, see blob_refs CHECK) is reserved for
// a future slice and not exposed yet — accepting it here would let
// callers create paths the rest of the stack does not understand.
type Scope string

const (
	ScopeGlobal  Scope = "global"
	ScopeLocal   Scope = "local"
	ScopeMachine Scope = "machine"
)

// ErrInvalidScope is returned when a caller passes a Scope outside the
// three valid values.
var ErrInvalidScope = errors.New("blob: invalid scope")

// Valid reports whether s is one of the three accepted scopes.
func (s Scope) Valid() bool {
	switch s {
	case ScopeGlobal, ScopeLocal, ScopeMachine:
		return true
	}
	return false
}

// resolveDir joins root with the scope's on-disk subdir name. Callers
// are expected to have validated `s` first; an invalid scope here
// returns "" so a downstream filepath.Join can't accidentally produce a
// path under root and write outside the scope sandbox.
func resolveDir(root string, s Scope) string {
	if !s.Valid() {
		return ""
	}
	return filepath.Join(root, string(s))
}
