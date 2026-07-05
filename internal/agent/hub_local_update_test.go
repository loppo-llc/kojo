package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

func seedHubLocalAgent(t *testing.T, m *Manager, id string) {
	t.Helper()
	a := &Agent{ID: id, Name: "before", Tool: "claude"}
	m.mu.Lock()
	m.agents[id] = a
	m.mu.Unlock()
	if err := m.store.Upsert(a); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
}

// A hub-local-safe PATCH (name / notes / effort-class fields) must be
// applied even while a §3.7 device switch is in flight; a holder-only
// field must keep the ErrAgentBusy refusal.
func TestUpdateDuringSwitching(t *testing.T) {
	m := newTestManager(t)
	seedHubLocalAgent(t, m, "ag_sw")

	if err := m.SetSwitching("ag_sw", true); err != nil {
		t.Fatalf("SetSwitching: %v", err)
	}
	defer func() { _ = m.SetSwitching("ag_sw", false) }()

	name := "mid-switch-name"
	a, err := m.Update("ag_sw", AgentUpdateConfig{Name: &name})
	if err != nil {
		t.Fatalf("hub-local update during switching: %v", err)
	}
	if a.Name != name {
		t.Fatalf("name = %q, want %q", a.Name, name)
	}

	// The mid-switch write may have missed the sync payload snapshot →
	// it must be recorded as a pending override for the next ingest.
	rec, err := m.Store().GetKV(context.Background(),
		"device_switch", "hub_local_overrides/ag_sw")
	if err != nil {
		t.Fatalf("mid-switch override kv row missing: %v", err)
	}
	if rec.Value == "" {
		t.Fatal("mid-switch override kv row empty")
	}

	model := "opus"
	if _, err := m.Update("ag_sw", AgentUpdateConfig{Model: &model}); !errors.Is(err, ErrAgentBusy) {
		t.Fatalf("holder-only update during switching: err = %v, want ErrAgentBusy", err)
	}
}

// UpdateRemoteHubRow writes the hub row for an agent that is NOT in
// the in-memory map, rejects holder-only fields, and refuses when the
// agent is actually local.
func TestUpdateRemoteHubRow(t *testing.T) {
	m := newTestManager(t)
	seedHubLocalAgent(t, m, "ag_rr")

	// Remote = row exists, not in memory.
	m.mu.Lock()
	delete(m.agents, "ag_rr")
	m.mu.Unlock()

	name := "remote-name"
	notes := "remote notes"
	auto := true
	a, err := m.UpdateRemoteHubRow("ag_rr", AgentUpdateConfig{
		Name:          &name,
		PublicProfile: &notes,
		AutoEffort:    &auto,
	}, "")
	if err != nil {
		t.Fatalf("UpdateRemoteHubRow: %v", err)
	}
	if a.Name != name || a.PublicProfile != notes {
		t.Fatalf("agent = %+v", a)
	}
	// Round-trip through the store.
	got, err := m.store.LoadByID("ag_rr")
	if err != nil {
		t.Fatalf("LoadByID: %v", err)
	}
	if got.Name != name || got.PublicProfile != notes {
		t.Fatalf("stored agent = name %q profile %q", got.Name, got.PublicProfile)
	}
	if got.AutoEffort == nil || !*got.AutoEffort {
		t.Fatalf("stored autoEffort = %v", got.AutoEffort)
	}

	// Holder-only field → refused.
	model := "opus"
	if _, err := m.UpdateRemoteHubRow("ag_rr", AgentUpdateConfig{Model: &model}, ""); err == nil {
		t.Fatal("holder-only field accepted by UpdateRemoteHubRow")
	}

	// Local agent → refused (in-memory runtime is the authority).
	seedHubLocalAgent(t, m, "ag_loc")
	if _, err := m.UpdateRemoteHubRow("ag_loc", AgentUpdateConfig{Name: &name}, ""); err == nil {
		t.Fatal("UpdateRemoteHubRow accepted a local agent")
	}
}

// A hub-local write while remote records a pending override; a
// source-wins sync ingest with an OLDER incoming row gets the edit
// re-applied, and the kv row is cleared.
func TestReapplyHubLocalOverridesAfterSourceWinsIngest(t *testing.T) {
	m := newTestManager(t)
	seedHubLocalAgent(t, m, "ag_ov")
	m.mu.Lock()
	delete(m.agents, "ag_ov")
	m.mu.Unlock()

	name := "edited-while-remote"
	if _, err := m.UpdateRemoteHubRow("ag_ov", AgentUpdateConfig{Name: &name}, ""); err != nil {
		t.Fatalf("UpdateRemoteHubRow: %v", err)
	}

	// Simulate the source-wins ingest clobbering the row with the
	// holder's copy, which still carries the pre-edit base value
	// ("before") — the holder never touched the field.
	stale := &Agent{ID: "ag_ov", Name: "before", Tool: "claude"}
	if err := m.store.Upsert(stale); err != nil {
		t.Fatalf("simulate ingest: %v", err)
	}

	applied, dropped, err := m.ReapplyHubLocalOverrides(context.Background(), "ag_ov")
	if err != nil || len(applied) != 1 || applied[0] != "name" || len(dropped) != 0 {
		t.Fatalf("reapply = (%v, %v, %v)", applied, dropped, err)
	}
	got, err := m.store.LoadByID("ag_ov")
	if err != nil || got.Name != name {
		t.Fatalf("row after reapply = %+v, err %v", got, err)
	}
	// kv row cleared → second call is a no-op.
	applied, dropped, err = m.ReapplyHubLocalOverrides(context.Background(), "ag_ov")
	if err != nil || len(applied) != 0 || len(dropped) != 0 {
		t.Fatalf("second reapply = (%v, %v, %v)", applied, dropped, err)
	}
}

// If the ingested row's field differs from the recorded base, the
// holder changed it after handoff: the holder edit wins for that
// field (dropped list, never silent) and the ingested value stays.
// Fields the holder did NOT touch are still re-applied.
func TestReapplyHubLocalOverridesFieldLevelMerge(t *testing.T) {
	m := newTestManager(t)
	seedHubLocalAgent(t, m, "ag_nw")
	m.mu.Lock()
	delete(m.agents, "ag_nw")
	m.mu.Unlock()

	name := "hub-edit"
	notes := "hub-notes"
	if _, err := m.UpdateRemoteHubRow("ag_nw", AgentUpdateConfig{
		Name:          &name,
		PublicProfile: &notes,
	}, ""); err != nil {
		t.Fatalf("UpdateRemoteHubRow: %v", err)
	}
	// Holder changed name (≠ base "before") but never touched
	// publicProfile (== base "").
	newer := &Agent{ID: "ag_nw", Name: "holder-newer", Tool: "claude"}
	if err := m.store.Upsert(newer); err != nil {
		t.Fatalf("simulate ingest: %v", err)
	}

	applied, dropped, err := m.ReapplyHubLocalOverrides(context.Background(), "ag_nw")
	if err != nil || len(applied) != 1 || applied[0] != "publicProfile" ||
		len(dropped) != 1 || dropped[0] != "name" {
		t.Fatalf("reapply = (%v, %v, %v)", applied, dropped, err)
	}
	got, err := m.store.LoadByID("ag_nw")
	if err != nil || got.Name != "holder-newer" || got.PublicProfile != notes {
		t.Fatalf("row = %+v, err %v", got, err)
	}
	// kv row cleared afterwards.
	if _, err := m.Store().GetKV(context.Background(),
		"device_switch", "hub_local_overrides/ag_nw"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("override kv row not cleared: %v", err)
	}
}

// Reapply with the agent already attached in-memory must merge into
// the FRESH DB row and reload memory from it — never flush the stale
// in-memory copy over fields the sync just ingested.
func TestReapplyHubLocalOverridesInMemoryNoRollback(t *testing.T) {
	m := newTestManager(t)
	seedHubLocalAgent(t, m, "ag_im")
	m.mu.Lock()
	delete(m.agents, "ag_im")
	m.mu.Unlock()

	name := "hub-edit"
	if _, err := m.UpdateRemoteHubRow("ag_im", AgentUpdateConfig{Name: &name}, ""); err != nil {
		t.Fatalf("UpdateRemoteHubRow: %v", err)
	}

	// Simulated ingest: row carries base name ("before") plus a field
	// the source changed (effort=high).
	ingested := &Agent{ID: "ag_im", Name: "before", Tool: "claude", Effort: "high"}
	if err := m.store.Upsert(ingested); err != nil {
		t.Fatalf("simulate ingest: %v", err)
	}
	// Agent re-attached with a STALE in-memory copy (pre-sync values).
	m.mu.Lock()
	m.agents["ag_im"] = &Agent{ID: "ag_im", Name: "stale-mem", Tool: "claude"}
	m.mu.Unlock()

	applied, dropped, err := m.ReapplyHubLocalOverrides(context.Background(), "ag_im")
	if err != nil || len(applied) != 1 || applied[0] != "name" || len(dropped) != 0 {
		t.Fatalf("reapply = (%v, %v, %v)", applied, dropped, err)
	}
	row, err := m.store.LoadByID("ag_im")
	if err != nil || row.Name != name || row.Effort != "high" {
		t.Fatalf("row = name %q effort %q, err %v (synced effort must survive)",
			row.Name, row.Effort, err)
	}
	// In-memory copy refreshed from the merged row, not the stale one.
	m.mu.Lock()
	mem := m.agents["ag_im"]
	m.mu.Unlock()
	if mem == nil || mem.Name != name || mem.Effort != "high" {
		t.Fatalf("in-memory agent = %+v", mem)
	}
}

// A failed row CAS (stale If-Match) must revert the pre-recorded
// override — no ghost override survives a failed PATCH.
func TestUpdateRemoteHubRowFailedCASRevertsOverride(t *testing.T) {
	m := newTestManager(t)
	seedHubLocalAgent(t, m, "ag_rv")
	m.mu.Lock()
	delete(m.agents, "ag_rv")
	m.mu.Unlock()

	name := "never-lands"
	if _, err := m.UpdateRemoteHubRow("ag_rv", AgentUpdateConfig{Name: &name}, "bogus-etag"); err == nil {
		t.Fatal("stale If-Match accepted")
	}
	if _, err := m.Store().GetKV(context.Background(),
		"device_switch", "hub_local_overrides/ag_rv"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ghost override survived failed PATCH: %v", err)
	}
	// Row unchanged.
	row, err := m.store.LoadByID("ag_rv")
	if err != nil || row.Name != "before" {
		t.Fatalf("row = %+v, err %v", row, err)
	}
}

// An unconfirmed Pending section (crashed / failed PATCH) must never
// be applied at ingest — only the confirmed override merges.
func TestReapplyHubLocalOverridesIgnoresPending(t *testing.T) {
	m := newTestManager(t)
	seedHubLocalAgent(t, m, "ag_pd")
	m.mu.Lock()
	delete(m.agents, "ag_pd")
	m.mu.Unlock()

	// Confirmed: name before→hub-edit. Pending (ghost): publicProfile.
	before := "before"
	hubEdit := "hub-edit"
	ghost := "ghost-notes"
	rec := hubLocalOverrideRecord{
		Cfg:  AgentUpdateConfig{Name: &hubEdit},
		Base: AgentUpdateConfig{Name: &before},
		Pending: &hubLocalOverridePending{
			Cfg:  AgentUpdateConfig{PublicProfile: &ghost},
			Base: AgentUpdateConfig{PublicProfile: &before},
		},
	}
	buf, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store().PutKV(context.Background(), &store.KVRecord{
		Namespace: "device_switch",
		Key:       "hub_local_overrides/ag_pd",
		Value:     string(buf),
		Type:      store.KVTypeJSON,
		Scope:     store.KVScopeMachine,
	}, store.KVPutOptions{}); err != nil {
		t.Fatalf("seed kv: %v", err)
	}
	// Ingested row still carries the base name.
	if err := m.store.Upsert(&Agent{ID: "ag_pd", Name: "before", Tool: "claude"}); err != nil {
		t.Fatalf("simulate ingest: %v", err)
	}

	applied, dropped, err := m.ReapplyHubLocalOverrides(context.Background(), "ag_pd")
	if err != nil || len(applied) != 1 || applied[0] != "name" || len(dropped) != 0 {
		t.Fatalf("reapply = (%v, %v, %v)", applied, dropped, err)
	}
	row, err := m.store.LoadByID("ag_pd")
	if err != nil || row.Name != hubEdit {
		t.Fatalf("row = %+v, err %v", row, err)
	}
	if row.PublicProfile == ghost {
		t.Fatal("ghost pending override was applied at ingest")
	}
	if _, err := m.Store().GetKV(context.Background(),
		"device_switch", "hub_local_overrides/ag_pd"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("override kv row not cleared: %v", err)
	}
}

// HubLocalOnly classification: every holder-bound pointer flips it.
func TestHubLocalOnlyClassification(t *testing.T) {
	s := "x"
	b := true
	if !(&AgentUpdateConfig{Name: &s, Effort: &s, AutoEffort: &b}).HubLocalOnly() {
		t.Fatal("hub-safe cfg classified holder-only")
	}
	holderOnly := []AgentUpdateConfig{
		{Persona: &s}, {CronMessage: &s}, {Model: &s}, {Tool: &s}, {WorkDir: &s},
		{CronExpr: &s}, {CustomBaseURL: &s}, {ThinkingMode: &s},
		{DeviceSwitchEnabled: &b}, {TTS: &TTSConfig{}},
		{AllowedTools: []string{"Bash"}}, {AllowProtectedPaths: &[]string{"/x"}},
	}
	for i, cfg := range holderOnly {
		if cfg.HubLocalOnly() {
			t.Fatalf("case %d classified hub-safe: %+v", i, cfg)
		}
	}
}
