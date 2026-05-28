package agent

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// testLogger returns a slog.Logger that only emits errors, suitable for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// transcriptTestSetup opens kojo.db at the test's HOME-redirected
// configdir and seeds parent agents so the transcript helpers'
// FK-on-agents constraint is satisfied. Each agent ID listed becomes a
// minimal AgentRecord whose only purpose is to host downstream
// agent_messages rows.
//
// Tests that previously poked agentDir(...) and wrote messages.jsonl
// directly need to call this — appendMessage now goes through the DB
// and refuses to insert against a non-existent agent.
func transcriptTestSetup(t *testing.T, agentIDs ...string) {
	t.Helper()
	st, err := newStore(testLogger())
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range agentIDs {
		rec := &store.AgentRecord{ID: id, Name: id}
		if _, err := st.db.InsertAgent(ctx, rec, store.AgentInsertOptions{}); err != nil {
			t.Fatalf("seed agent %s: %v", id, err)
		}
	}
}
