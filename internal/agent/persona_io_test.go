package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

func personaFixture(t *testing.T, agentID string) *Manager {
	return memoryIOFixture(t, agentID)
}

func TestPutAgentPersona_RoundTrip(t *testing.T) {
	mgr := personaFixture(t, "ag_p1")
	ctx := context.Background()

	rec1, err := mgr.PutAgentPersona(ctx, "ag_p1", "snarky barista", "")
	if err != nil {
		t.Fatalf("first PUT: %v", err)
	}
	if rec1.ETag == "" {
		t.Error("expected non-empty etag")
	}

	// File on disk matches.
	body, err := os.ReadFile(agentPersonaFilePath("ag_p1"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(body) != "snarky barista" {
		t.Errorf("file body = %q", body)
	}

	// In-memory Persona reflected.
	mgr.mu.Lock()
	persona := mgr.agents["ag_p1"].Persona
	mgr.mu.Unlock()
	if persona != "snarky barista" {
		t.Errorf("in-memory persona = %q", persona)
	}

	// GET round-trips.
	got, err := mgr.GetAgentPersona(ctx, "ag_p1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got.Body != "snarky barista" || got.ETag != rec1.ETag {
		t.Errorf("GET drift: body=%q etag-eq=%v", got.Body, got.ETag == rec1.ETag)
	}

	// Conditional PUT advances etag.
	rec2, err := mgr.PutAgentPersona(ctx, "ag_p1", "perky barista", rec1.ETag)
	if err != nil {
		t.Fatalf("second PUT: %v", err)
	}
	if rec2.ETag == rec1.ETag {
		t.Error("etag did not advance")
	}

	// Stale If-Match → 412.
	_, err = mgr.PutAgentPersona(ctx, "ag_p1", "x", rec1.ETag)
	if !errors.Is(err, store.ErrETagMismatch) {
		t.Errorf("stale If-Match: want ErrETagMismatch, got %v", err)
	}
}

func TestPutAgentPersona_EmptyClearsFile(t *testing.T) {
	mgr := personaFixture(t, "ag_p2")
	ctx := context.Background()

	rec, err := mgr.PutAgentPersona(ctx, "ag_p2", "before", "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// PUT empty body → file removed, DB row updated to empty body.
	rec2, err := mgr.PutAgentPersona(ctx, "ag_p2", "", rec.ETag)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if rec2.Body != "" {
		t.Errorf("expected empty body, got %q", rec2.Body)
	}

	// File should not exist.
	if _, err := os.Stat(agentPersonaFilePath("ag_p2")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file should be gone, got err=%v", err)
	}
}

func TestPutAgentPersona_NotFound(t *testing.T) {
	mgr := newTestManager(t)
	_, err := mgr.PutAgentPersona(context.Background(), "ag_nope", "x", "")
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("want ErrAgentNotFound, got %v", err)
	}
}

func TestPutAgentPersona_Archived(t *testing.T) {
	mgr := personaFixture(t, "ag_p3")
	mgr.mu.Lock()
	mgr.agents["ag_p3"].Archived = true
	mgr.mu.Unlock()

	_, err := mgr.PutAgentPersona(context.Background(), "ag_p3", "x", "")
	if !errors.Is(err, ErrAgentArchived) {
		t.Errorf("want ErrAgentArchived, got %v", err)
	}
}

func TestPutAgentPersona_BodyCap(t *testing.T) {
	mgr := personaFixture(t, "ag_p4")
	huge := strings.Repeat("x", personaBodyCap+1)
	_, err := mgr.PutAgentPersona(context.Background(), "ag_p4", huge, "")
	if !errors.Is(err, ErrInvalidPersona) {
		t.Errorf("oversize body: want ErrInvalidPersona, got %v", err)
	}
}

func TestGetAgentPersona_NotSyncedYet(t *testing.T) {
	mgr := personaFixture(t, "ag_p5")
	_, err := mgr.GetAgentPersona(context.Background(), "ag_p5")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("brand-new agent: want ErrNotFound, got %v", err)
	}
}

// TestPutAgentPersona_PreSyncReflectsCLIEdit covers the disk-→DB
// pre-sync path: a CLI-side persona.md edit that hasn't been
// reflected in the DB row gets pulled in BEFORE the If-Match
// precondition runs, so a stale Web client can't clobber it.
func TestPutAgentPersona_PreSyncReflectsCLIEdit(t *testing.T) {
	mgr := personaFixture(t, "ag_p6")
	ctx := context.Background()

	// Seed via PUT so DB + disk are in sync.
	rec, err := mgr.PutAgentPersona(ctx, "ag_p6", "v1", "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Simulate a CLI-side edit by overwriting persona.md without
	// going through the API.
	if err := os.WriteFile(agentPersonaFilePath("ag_p6"), []byte("v2-cli"), 0o644); err != nil {
		t.Fatalf("CLI edit: %v", err)
	}

	// Web client PUTs with the OLD etag — pre-sync should pull
	// "v2-cli" into the DB first, making the OLD etag stale, so
	// the precondition fires.
	_, err = mgr.PutAgentPersona(ctx, "ag_p6", "v3-web", rec.ETag)
	if !errors.Is(err, store.ErrETagMismatch) {
		t.Errorf("stale If-Match after CLI edit: want ErrETagMismatch, got %v", err)
	}
}
