package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedRemoteHeldAgent persists an agent row that is NOT in the local
// Manager's runtime map and, when holder != "", records an agent_locks row
// naming that holder — the Hub-side shape of an agent that has been moved to
// another peer via device switch.
func seedRemoteHeldAgent(t *testing.T, mgr *Manager, id, name, holder string) {
	t.Helper()
	if err := mgr.store.Upsert(&Agent{ID: id, Name: name, Tool: "claude"}); err != nil {
		t.Fatalf("seed agent %s: %v", id, err)
	}
	if holder == "" {
		return
	}
	now := time.Now().UnixMilli()
	if _, err := mgr.Store().AcquireAgentLock(context.Background(), id, holder, now, int64(time.Hour/time.Millisecond)); err != nil {
		t.Fatalf("acquire lock for %s: %v", id, err)
	}
}

func TestCreateThread_RemoteHeldAgentRequiresHolderRouter(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	seedRemoteHeldAgent(t, mgr, "ag_remote", "Remote", "peer-remote")

	// Without the holder-aware router (peer-only daemon / local oneShot),
	// the local Manager could not answer, so creation stays rejected.
	stub := &threadStub{reply: "pong"}
	gdm.SetOneShotForTesting(stub.fn)
	if _, err := gdm.CreateThread("ag_remote"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("CreateThread without router err = %v, want ErrAgentNotFound", err)
	}
	if _, _, err := gdm.FindOrCreateDM([]string{"ag_remote"}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("FindOrCreateDM without router err = %v, want ErrAgentNotFound", err)
	}
}

func TestCreateThread_RemoteHeldAgentViaHolderRouter(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	seedRemoteHeldAgent(t, mgr, "ag_remote", "Remote", "peer-remote")
	stub := &threadStub{reply: "pong from holder"}
	gdm.SetOneShotRouter(stub.fn)

	g, err := gdm.CreateThread("ag_remote")
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	if g.Kind != GroupDMKindThread || len(g.Members) != 1 || g.Members[0].AgentID != "ag_remote" {
		t.Fatalf("thread = %+v", g)
	}
	if g.Members[0].AgentName != "Remote" {
		t.Fatalf("member name = %q, want Remote", g.Members[0].AgentName)
	}

	// The thread turn is dispatched through the router for the remote agent
	// and the reply is attributed with the persisted display name.
	if _, err := gdm.PostUserMessage(context.Background(), g.ID, "ping", nil, true); err != nil {
		t.Fatalf("PostUserMessage: %v", err)
	}
	reply := waitForMessage(t, gdm, g.ID, "pong from holder")
	if reply.AgentID != "ag_remote" || reply.AgentName != "Remote" {
		t.Fatalf("reply author = %q/%q, want ag_remote/Remote", reply.AgentID, reply.AgentName)
	}
	if got := stub.lastOpts().SessionKey; got != "groupdm:"+g.ID {
		t.Fatalf("SessionKey = %q, want %q", got, "groupdm:"+g.ID)
	}

	// The legacy single-agent DM shape is also a thread room.
	dm, created, err := gdm.FindOrCreateDM([]string{"ag_remote"})
	if err != nil || !created || dm.Kind != GroupDMKindDM {
		t.Fatalf("FindOrCreateDM single remote = %+v created=%v err=%v", dm, created, err)
	}
}

func TestCreateRoom_RemoteHeldAgentStillRejectedOutsideThreads(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	seedRemoteHeldAgent(t, mgr, "ag_remote", "Remote", "peer-remote")
	gdm.SetOneShotRouter((&threadStub{}).fn)

	// Classic groups and two-agent DMs deliver through the local
	// Manager.Chat fan-out, which cannot reach a remote holder.
	if _, err := gdm.Create("mixed", []string{"ag_alice", "ag_remote"}, 0, "", ""); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("Create group err = %v, want ErrAgentNotFound", err)
	}
	if _, _, err := gdm.FindOrCreateDM([]string{"ag_alice", "ag_remote"}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("FindOrCreateDM pair err = %v, want ErrAgentNotFound", err)
	}
}

func TestCreateThread_UnheldOrMissingAgentRejected(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	// DB row without any holder lock: not a device-switched agent.
	seedRemoteHeldAgent(t, mgr, "ag_orphan", "Orphan", "")
	gdm.SetOneShotRouter((&threadStub{}).fn)

	if _, err := gdm.CreateThread("ag_orphan"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("CreateThread unheld err = %v, want ErrAgentNotFound", err)
	}
	if _, err := gdm.CreateThread("ag_missing"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("CreateThread missing err = %v, want ErrAgentNotFound", err)
	}
}

func TestCreateThread_RemoteHeldArchivedAgentRejected(t *testing.T) {
	gdm, mgr := setupGroupDMTest(t)
	if err := mgr.store.Upsert(&Agent{ID: "ag_arch", Name: "Arch", Tool: "claude", Archived: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := mgr.Store().AcquireAgentLock(context.Background(), "ag_arch", "peer-remote", now, int64(time.Hour/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	gdm.SetOneShotRouter((&threadStub{}).fn)
	if _, err := gdm.CreateThread("ag_arch"); !errors.Is(err, ErrAgentArchived) {
		t.Fatalf("CreateThread archived remote err = %v, want ErrAgentArchived", err)
	}
}
