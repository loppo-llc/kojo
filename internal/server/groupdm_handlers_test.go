package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
)

func newGroupDMHandlerTestServer(t *testing.T) (*Server, *agent.GroupDMManager, *agent.GroupDM, *agent.Agent) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", "")

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr, err := agent.NewManager(logger)
	if err != nil {
		t.Fatalf("agent.NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	disableCron := ""
	alice, err := mgr.Create(agent.AgentConfig{Name: "Alice", Tool: "claude", CronExpr: &disableCron})
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := mgr.Create(agent.AgentConfig{Name: "Bob", Tool: "claude", CronExpr: &disableCron})
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	outsider, err := mgr.Create(agent.AgentConfig{Name: "Outsider", Tool: "claude", CronExpr: &disableCron})
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}

	gdm := agent.NewGroupDMManager(mgr, logger)
	mgr.SetGroupDMManager(gdm)
	group, err := gdm.Create("Team", []string{alice.ID, bob.ID}, 0, "", "")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	srv := &Server{agents: mgr, groupdms: gdm, logger: logger}
	return srv, gdm, group, outsider
}

func deleteGroupMessagesRequest(groupID string, p auth.Principal) *http.Request {
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/groupdms/"+groupID+"/messages", nil)
	req.SetPathValue("id", groupID)
	return authedRequest(req, p)
}

func TestHandleClearGroupMessages_Owner(t *testing.T) {
	srv, gdm, group, _ := newGroupDMHandlerTestServer(t)
	if _, err := gdm.PostUserMessage(context.Background(), group.ID, "one", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := gdm.PostUserMessage(context.Background(), group.ID, "two", nil, false); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	srv.handleClearGroupMessages(rr, deleteGroupMessagesRequest(group.ID, auth.Principal{Role: auth.RoleOwner}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK      bool  `json:"ok"`
		Deleted int64 `json:"deleted"`
	}
	readJSONResponse(t, rr, &resp)
	if !resp.OK || resp.Deleted != 2 {
		t.Fatalf("response = %+v, want ok/deleted=2", resp)
	}

	msgs, hasMore, latest, err := gdm.Messages(group.ID, 50, "")
	if err != nil {
		t.Fatalf("messages after clear: %v", err)
	}
	if len(msgs) != 0 || hasMore || latest != "" {
		t.Fatalf("messages after clear = (%d, %v, %q), want empty", len(msgs), hasMore, latest)
	}
}

func TestHandleClearGroupMessages_RejectsNonMemberAgent(t *testing.T) {
	srv, gdm, group, outsider := newGroupDMHandlerTestServer(t)
	if _, err := gdm.PostUserMessage(context.Background(), group.ID, "keep", nil, false); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	srv.handleClearGroupMessages(rr, deleteGroupMessagesRequest(group.ID, auth.Principal{
		Role:    auth.RoleAgent,
		AgentID: outsider.ID,
	}))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	msgs, _, _, err := gdm.Messages(group.ID, 50, "")
	if err != nil {
		t.Fatalf("messages after forbidden clear: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("message count after forbidden clear = %d, want 1", len(msgs))
	}
}
