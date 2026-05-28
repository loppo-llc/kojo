package agent

import (
	"context"
	"testing"
)

// MarkAgentArrivedHere → GetAgentArrivedProxy round-trip with a non-empty
// allowed_proxy_peer. Verifies the sibling kv row lands and reads back.
func TestArrivedProxyKVRoundTrip(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	const agentID = "ag_test_arrived_proxy"
	const proxy = "device_hub_xyz"

	if err := m.MarkAgentArrivedHere(ctx, agentID, proxy); err != nil {
		t.Fatalf("MarkAgentArrivedHere: %v", err)
	}
	got, err := m.GetAgentArrivedProxy(ctx, agentID)
	if err != nil {
		t.Fatalf("GetAgentArrivedProxy: %v", err)
	}
	if got != proxy {
		t.Fatalf("proxy mismatch: got %q want %q", got, proxy)
	}
}

// Empty allowedProxyPeer skips the sibling write — preserves the legacy
// caller contract (test fixtures that don't care about the proxy hint).
func TestArrivedProxyKVEmptySkipsSibling(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	const agentID = "ag_test_arrived_no_proxy"

	if err := m.MarkAgentArrivedHere(ctx, agentID, ""); err != nil {
		t.Fatalf("MarkAgentArrivedHere: %v", err)
	}
	got, err := m.GetAgentArrivedProxy(ctx, agentID)
	if err != nil {
		t.Fatalf("GetAgentArrivedProxy: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty proxy for legacy-style call, got %q", got)
	}
}

// ClearAgentArrivedHere drops both the arrival marker and the proxy
// sibling so a subsequent restore finds nothing.
func TestArrivedProxyKVClearedAlongside(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	const agentID = "ag_test_arrived_cleared"
	const proxy = "device_hub_xyz"

	if err := m.MarkAgentArrivedHere(ctx, agentID, proxy); err != nil {
		t.Fatalf("MarkAgentArrivedHere: %v", err)
	}
	if err := m.ClearAgentArrivedHere(ctx, agentID); err != nil {
		t.Fatalf("ClearAgentArrivedHere: %v", err)
	}
	got, err := m.GetAgentArrivedProxy(ctx, agentID)
	if err != nil {
		t.Fatalf("GetAgentArrivedProxy after clear: %v", err)
	}
	if got != "" {
		t.Fatalf("expected proxy sibling cleared, got %q", got)
	}
	arrived, err := m.ListArrivedAgents(ctx)
	if err != nil {
		t.Fatalf("ListArrivedAgents: %v", err)
	}
	for _, id := range arrived {
		if id == agentID {
			t.Fatalf("arrival marker not cleared: %q still listed", agentID)
		}
	}
}

// A second MarkAgentArrivedHere overwrites the proxy value — covers
// switch-back-to-this-peer where the orchestrator may differ between
// the prior and the new arrival.
func TestArrivedProxyKVOverwrite(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	const agentID = "ag_test_arrived_overwrite"
	const first = "device_hub_v1"
	const second = "device_hub_v2"

	if err := m.MarkAgentArrivedHere(ctx, agentID, first); err != nil {
		t.Fatalf("MarkAgentArrivedHere first: %v", err)
	}
	if err := m.MarkAgentArrivedHere(ctx, agentID, second); err != nil {
		t.Fatalf("MarkAgentArrivedHere second: %v", err)
	}
	got, err := m.GetAgentArrivedProxy(ctx, agentID)
	if err != nil {
		t.Fatalf("GetAgentArrivedProxy: %v", err)
	}
	if got != second {
		t.Fatalf("proxy not overwritten: got %q want %q", got, second)
	}
}

// GetAgentArrivedProxy returns ("", nil) when no row exists — boot
// path relies on this to treat legacy / pre-fix arrivals as "leave
// fresh-acquire default" instead of erroring.
func TestArrivedProxyKVMissingReturnsEmpty(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	got, err := m.GetAgentArrivedProxy(ctx, "ag_does_not_exist")
	if err != nil {
		t.Fatalf("GetAgentArrivedProxy missing: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty for missing row, got %q", got)
	}
}
