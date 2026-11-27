package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// markerKVTestSetup opens kojo.db at a temp HOME, registers the
// global store handle (so deleteMarker routes to kv), and
// seeds an agent row so any future agent_messages FK is satisfied
// (not used by the marker tests but harmless).
func markerKVTestSetup(t *testing.T, agentID string) *agentStore {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", "")
	st, err := newStore(testLogger())
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if agentID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rec := &store.AgentRecord{ID: agentID, Name: agentID}
		if _, err := st.db.InsertAgent(ctx, rec, store.AgentInsertOptions{}); err != nil {
			t.Fatalf("seed agent: %v", err)
		}
	}
	return st
}

// TestDeleteMarkerWipesLeftovers: reset / delete still clear the summary
// cursor older builds left in kv and on disk.
func TestDeleteMarkerWipesLeftovers(t *testing.T) {
	st := markerKVTestSetup(t, "ag")
	if err := os.MkdirAll(agentDir("ag"), 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := &store.KVRecord{
		Namespace: autosummaryKVNamespace,
		Key:       "ag",
		Value:     `{"lastN":1}`,
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeGlobal,
	}
	if _, err := st.db.PutKV(ctx, rec, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed kv row: %v", err)
	}
	legacy := filepath.Join(agentDir("ag"), autoSummaryMarkerFile)
	if err := os.WriteFile(legacy, []byte("{}"), 0o644); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	deleteMarker("ag", testLogger())

	if _, err := st.db.GetKV(ctx, autosummaryKVNamespace, "ag"); err == nil {
		t.Errorf("kv row still present after deleteMarker")
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy file still present after deleteMarker: err=%v", err)
	}
	// Idempotent on a clean agent.
	deleteMarker("ag", testLogger())
}
