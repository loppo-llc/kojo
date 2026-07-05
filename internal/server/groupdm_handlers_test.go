package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// muteGroupMembers flips every member to muted so handler-driven posts
// (notify=true) never spawn a real Chat — the test TempDir would otherwise
// race the background guide sync at cleanup.
func muteGroupMembers(t *testing.T, gdm *agent.GroupDMManager, g *agent.GroupDM) {
	t.Helper()
	for _, m := range g.Members {
		if _, err := gdm.SetMemberNotifyMode(g.ID, m.AgentID, agent.NotifyMuted, 0, ""); err != nil {
			t.Fatalf("mute %s: %v", m.AgentID, err)
		}
	}
}

func postGroupMessageRequest(groupID, body string, p auth.Principal) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/groupdms/"+groupID+"/messages",
		strings.NewReader(body))
	req.SetPathValue("id", groupID)
	return authedRequest(req, p)
}

// Agent posts must carry expectedLatestMessageId once the room has a head.
func TestHandlePostGroupMessage_AgentEmptyCASRejected(t *testing.T) {
	srv, gdm, group, _ := newGroupDMHandlerTestServer(t)
	muteGroupMembers(t, gdm, group)
	aliceID := group.Members[0].AgentID
	if _, err := gdm.PostUserMessage(context.Background(), group.ID, "seed", nil, false); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	srv.handlePostGroupMessage(rr, postGroupMessageRequest(group.ID,
		`{"content":"hello"}`, auth.Principal{Role: auth.RoleAgent, AgentID: aliceID}))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error           string `json:"error"`
		LatestMessageID string `json:"latestMessageId"`
	}
	readJSONResponse(t, rr, &resp)
	if resp.Error != "expected_latest_message_id_required" || resp.LatestMessageID == "" {
		t.Fatalf("response = %+v", resp)
	}
}

// A brand-new room has no head, so "" is the only expressible cursor and
// the first agent post passes.
func TestHandlePostGroupMessage_AgentEmptyCASAllowedOnEmptyRoom(t *testing.T) {
	srv, gdm, group, _ := newGroupDMHandlerTestServer(t)
	muteGroupMembers(t, gdm, group)
	aliceID := group.Members[0].AgentID

	rr := httptest.NewRecorder()
	srv.handlePostGroupMessage(rr, postGroupMessageRequest(group.ID,
		`{"content":"first"}`, auth.Principal{Role: auth.RoleAgent, AgentID: aliceID}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// Owner posts keep the legacy skip-when-empty behavior.
func TestHandlePostGroupMessage_OwnerEmptyCASAllowed(t *testing.T) {
	srv, gdm, group, _ := newGroupDMHandlerTestServer(t)
	muteGroupMembers(t, gdm, group)
	aliceID := group.Members[0].AgentID
	if _, err := gdm.PostUserMessage(context.Background(), group.ID, "seed", nil, false); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	srv.handlePostGroupMessage(rr, postGroupMessageRequest(group.ID,
		`{"agentId":"`+aliceID+`","content":"admin"}`, auth.Principal{Role: auth.RoleOwner}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// Agent post with a valid (current head) cursor succeeds.
func TestHandlePostGroupMessage_AgentWithCurrentCAS(t *testing.T) {
	srv, gdm, group, _ := newGroupDMHandlerTestServer(t)
	muteGroupMembers(t, gdm, group)
	aliceID := group.Members[0].AgentID
	seed, err := gdm.PostUserMessage(context.Background(), group.ID, "seed", nil, false)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	srv.handlePostGroupMessage(rr, postGroupMessageRequest(group.ID,
		`{"content":"reply","expectedLatestMessageId":"`+seed.ID+`"}`,
		auth.Principal{Role: auth.RoleAgent, AgentID: aliceID}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleFindOrCreateDM(t *testing.T) {
	srv, _, group, _ := newGroupDMHandlerTestServer(t)
	aliceID := group.Members[0].AgentID

	mk := func(body string, p auth.Principal) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/dms", strings.NewReader(body))
		rr := httptest.NewRecorder()
		srv.handleFindOrCreateDM(rr, authedRequest(req, p))
		return rr
	}

	// Owner creates a human↔agent DM.
	rr := mk(`{"agentId":"`+aliceID+`"}`, auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var dm agent.GroupDM
	readJSONResponse(t, rr, &dm)
	if dm.Kind != agent.GroupDMKindDM || len(dm.Members) != 1 {
		t.Fatalf("dm = kind %q members %d", dm.Kind, len(dm.Members))
	}

	// Second call finds the same room (200).
	rr = mk(`{"agentId":"`+aliceID+`"}`, auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusOK {
		t.Fatalf("find status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var dm2 agent.GroupDM
	readJSONResponse(t, rr, &dm2)
	if dm2.ID != dm.ID {
		t.Errorf("find returned %s, want %s", dm2.ID, dm.ID)
	}

	// Agent may not open a DM it is not a member of.
	bobID := group.Members[1].AgentID
	rr = mk(`{"agentId":"`+aliceID+`"}`, auth.Principal{Role: auth.RoleAgent, AgentID: bobID})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("foreign dm status = %d, body = %s", rr.Code, rr.Body.String())
	}

	// Agent↔agent DM by a participant.
	rr = mk(`{"memberIds":["`+aliceID+`","`+bobID+`"]}`,
		auth.Principal{Role: auth.RoleAgent, AgentID: aliceID})
	if rr.Code != http.StatusCreated {
		t.Fatalf("pair dm status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestHandleGetGroupUnread(t *testing.T) {
	srv, gdm, group, _ := newGroupDMHandlerTestServer(t)
	m1, err := gdm.PostUserMessage(context.Background(), group.ID, "one", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	aliceID := group.Members[0].AgentID
	if _, err := gdm.PostMessage(context.Background(), group.ID, aliceID, "ping @user", m1.ID, false); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/groupdms/"+group.ID+"/unread?after="+m1.ID, nil)
	req.SetPathValue("id", group.ID)
	rr := httptest.NewRecorder()
	srv.handleGetGroupUnread(rr, authedRequest(req, auth.Principal{Role: auth.RoleOwner}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Count        int  `json:"count"`
		MentionsUser bool `json:"mentionsUser"`
	}
	readJSONResponse(t, rr, &resp)
	if resp.Count != 1 || !resp.MentionsUser {
		t.Fatalf("unread = %+v, want count=1 mentionsUser=true", resp)
	}
}
