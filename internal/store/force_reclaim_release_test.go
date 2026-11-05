package store

import (
	"context"
	"errors"
	"strconv"
	"testing"
)

func putHandoffKVForTest(t *testing.T, s *Store, key string) {
	t.Helper()
	if _, err := s.PutKV(context.Background(), &KVRecord{
		Namespace: "handoff", Key: key, Value: "x",
		Type: KVTypeString, Scope: KVScopeMachine,
	}, KVPutOptions{}); err != nil {
		t.Fatalf("put kv %s: %v", key, err)
	}
}

func hasHandoffKVForTest(t *testing.T, s *Store, key string) bool {
	t.Helper()
	_, err := s.GetKV(context.Background(), "handoff", key)
	if errors.Is(err, ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("get kv %s: %v", key, err)
	}
	return true
}

// The split-brain this guards: another device force-reclaimed the agent,
// but this peer still records holder==self plus its arrival markers.
func TestReleaseReclaimedAgentHereReleasesSelfHolder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	held, err := s.AcquireAgentLock(ctx, id, "mac", NowMillis(), 300000)
	if err != nil {
		t.Fatal(err)
	}
	putHandoffKVForTest(t, s, "arrived/"+id)
	putHandoffKVForTest(t, s, "arrived_proxy/"+id)
	putHandoffKVForTest(t, s, "pending/"+id+"/op1")
	putHandoffKVForTest(t, s, "arrived/ag_other")
	before, err := s.GetAgentLockVersion(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	released, err := s.ReleaseReclaimedAgentHere(ctx, id, "mac", "hub")
	if err != nil || !released {
		t.Fatalf("released=%v err=%v; want true", released, err)
	}
	if _, err := s.GetAgentLock(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lock row survived: %v", err)
	}
	for _, k := range []string{"arrived/" + id, "arrived_proxy/" + id, "pending/" + id + "/op1"} {
		if hasHandoffKVForTest(t, s, k) {
			t.Errorf("kv %s survived", k)
		}
	}
	if !hasHandoffKVForTest(t, s, "arrived/ag_other") {
		t.Error("another agent's marker was deleted")
	}
	// Durable with the release: startup eviction + full-sync next time.
	for _, k := range []string{"released/" + id, AgentFullSyncMarkerKey(id)} {
		if !hasHandoffKVForTest(t, s, k) {
			t.Errorf("kv %s not written", k)
		}
	}
	if rec, err := s.GetKV(ctx, "handoff", AgentFullSyncMarkerKey(id)); err != nil || rec.Value != "hub" || rec.ETag == "" {
		t.Fatalf("full-sync marker: %+v %v", rec, err)
	}
	after, err := s.GetAgentLockVersion(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Token <= before.Token || after.Holder != "" {
		t.Fatalf("version %+v -> %+v; want bumped token, empty holder", before, after)
	}
	// The old runtime's in-flight writes must fail fencing.
	if err := s.CheckFencing(ctx, id, "mac", held.FencingToken); err == nil {
		t.Fatal("old token still passes fencing")
	}
	claims, err := s.LocalPeerClaimsAgent(ctx, id, "mac")
	if err != nil || claims {
		t.Fatalf("still claims after release: %v %v", claims, err)
	}
}

// Graceful stop wiped the lock row but the arrival marker would re-seed
// AgentLockGuard on the next boot.
func TestReleaseReclaimedAgentHereReleasesArrivalMarkerOnly(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	putHandoffKVForTest(t, s, "arrived/"+id)
	released, err := s.ReleaseReclaimedAgentHere(ctx, id, "mac", "hub")
	if err != nil || !released {
		t.Fatalf("released=%v err=%v; want true", released, err)
	}
	if hasHandoffKVForTest(t, s, "arrived/"+id) {
		t.Fatal("arrival marker survived")
	}
}

// A peer that already handed the agent on (or never had it) must not be
// touched by an obsolete notice.
func TestReleaseReclaimedAgentHereIgnoresNonHolder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	if released, err := s.ReleaseReclaimedAgentHere(ctx, id, "mac", "hub"); err != nil || released {
		t.Fatalf("no row/no marker: released=%v err=%v", released, err)
	}
	if _, err := s.AcquireAgentLock(ctx, id, "hub", NowMillis(), 300000); err != nil {
		t.Fatal(err)
	}
	putHandoffKVForTest(t, s, "pending/"+id+"/op1")
	before, _ := s.GetAgentLockVersion(ctx, id)
	if released, err := s.ReleaseReclaimedAgentHere(ctx, id, "mac", "hub"); err != nil || released {
		t.Fatalf("other holder: released=%v err=%v", released, err)
	}
	after, _ := s.GetAgentLockVersion(ctx, id)
	if before != after {
		t.Fatalf("version changed %+v -> %+v", before, after)
	}
	if !hasHandoffKVForTest(t, s, "pending/"+id+"/op1") {
		t.Fatal("non-holder kv was modified")
	}
}

func TestReleaseReclaimedAgentHereKeepsForeignRowWithStaleArrival(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	if _, err := s.AcquireAgentLock(ctx, id, "hub", NowMillis(), 300000); err != nil {
		t.Fatal(err)
	}
	putHandoffKVForTest(t, s, "arrived/"+id)
	before, _ := s.GetAgentLockVersion(ctx, id)
	released, err := s.ReleaseReclaimedAgentHere(ctx, id, "mac", "hub")
	if err != nil || !released {
		t.Fatalf("arrival marker must count as a claim: released=%v err=%v", released, err)
	}
	if hasHandoffKVForTest(t, s, "arrived/"+id) {
		t.Fatal("arrival marker must be cleared")
	}
	after, _ := s.GetAgentLockVersion(ctx, id)
	if before != after {
		t.Fatalf("foreign row / counter changed %+v -> %+v", before, after)
	}
	lock, err := s.GetAgentLock(ctx, id)
	if err != nil || lock.HolderPeer != "hub" {
		t.Fatalf("foreign row must stay: %+v %v", lock, err)
	}
}

func TestSyncAgentFromPeerClearsFullSyncMarkerOnlyOnFullSync(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedAgent(t, s, "ag")
	putHandoffKVForTest(t, s, AgentFullSyncMarkerKey("ag"))
	agent, err := s.GetAgent(ctx, "ag")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncAgentFromPeer(ctx, AgentSyncPayload{Agent: agent, IncrementalMessages: true}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasAgentFullSyncMarker(ctx, "ag"); err != nil || !ok {
		t.Fatalf("incremental sync cleared marker: %v %v", ok, err)
	}
	agent, _ = s.GetAgent(ctx, "ag")
	if err := s.SyncAgentFromPeer(ctx, AgentSyncPayload{Agent: agent}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasAgentFullSyncMarker(ctx, "ag"); err != nil || ok {
		t.Fatalf("full sync kept marker: %v %v", ok, err)
	}
}

func TestForceReclaimStampedBindsReclaimTimeToToken(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := agentLockTestSetup(t, s)
	// The unstamped variant leaves no record.
	if _, err := s.ForceReclaimAgentToLocal(ctx, id, "hub", NowMillis(), 300000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetKV(ctx, ReclaimedAtNamespace, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unstamped reclaim wrote a stamp: %v", err)
	}
	rec, err := s.ForceReclaimAgentToLocalStamped(ctx, id, "hub", NowMillis(), 300000, 1234)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := s.GetKV(ctx, ReclaimedAtNamespace, id)
	if err != nil {
		t.Fatal(err)
	}
	if want := "1234:" + strconv.FormatInt(rec.FencingToken, 10); kv.Value != want {
		t.Fatalf("stamp = %q, want %q", kv.Value, want)
	}
}
