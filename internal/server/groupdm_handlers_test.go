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

// TestHandleCreateThread verifies POST /api/v1/threads always creates a fresh
// kind="thread" room (no dedup), unlike POST /api/v1/dms.
func TestHandleCreateThread(t *testing.T) {
	srv, _, group, _ := newGroupDMHandlerTestServer(t)
	aliceID := group.Members[0].AgentID

	mk := func(body string, p auth.Principal) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/threads", strings.NewReader(body))
		rr := httptest.NewRecorder()
		srv.handleCreateThread(rr, authedRequest(req, p))
		return rr
	}

	rr := mk(`{"agentId":"`+aliceID+`"}`, auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusCreated {
		t.Fatalf("first status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var t1 agent.GroupDM
	readJSONResponse(t, rr, &t1)
	if t1.Kind != agent.GroupDMKindThread || len(t1.Members) != 1 {
		t.Fatalf("thread = kind %q members %d", t1.Kind, len(t1.Members))
	}

	// Second call must create a DISTINCT room (always new, no dedup).
	rr = mk(`{"agentId":"`+aliceID+`"}`, auth.Principal{Role: auth.RoleOwner})
	if rr.Code != http.StatusCreated {
		t.Fatalf("second status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var t2 agent.GroupDM
	readJSONResponse(t, rr, &t2)
	if t2.ID == t1.ID {
		t.Errorf("threads deduped: both have id %s", t1.ID)
	}

	// Agent may not open a thread as another agent.
	bobID := group.Members[1].AgentID
	rr = mk(`{"agentId":"`+aliceID+`"}`, auth.Principal{Role: auth.RoleAgent, AgentID: bobID})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("foreign thread status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestArchiveThenRecreateDM verifies that archiving (deleting) a thread room
// frees the partial-unique dm_member_key so POST /api/v1/dms creates a fresh
// room rather than resurrecting the tombstoned one.
func TestArchiveThenRecreateDM(t *testing.T) {
	srv, _, group, _ := newGroupDMHandlerTestServer(t)
	aliceID := group.Members[0].AgentID

	openDM := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/dms",
			strings.NewReader(`{"agentId":"`+aliceID+`"}`))
		rr := httptest.NewRecorder()
		srv.handleFindOrCreateDM(rr, authedRequest(req, auth.Principal{Role: auth.RoleOwner}))
		return rr
	}

	rr := openDM()
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var first agent.GroupDM
	readJSONResponse(t, rr, &first)

	// Archive it (DELETE = tombstone, the thread-room archive path).
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/groupdms/"+first.ID, nil)
	req.SetPathValue("id", first.ID)
	drr := httptest.NewRecorder()
	srv.handleDeleteGroupDM(drr, authedRequest(req, auth.Principal{Role: auth.RoleOwner}))
	if drr.Code != http.StatusOK {
		t.Fatalf("archive status = %d, body = %s", drr.Code, drr.Body.String())
	}

	// A new open must create a fresh room, not find the tombstoned one.
	rr = openDM()
	if rr.Code != http.StatusCreated {
		t.Fatalf("recreate status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var second agent.GroupDM
	readJSONResponse(t, rr, &second)
	if second.ID == first.ID {
		t.Errorf("recreated room reused archived id %s", first.ID)
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

// TestMarkGroupReadPersistsCursor verifies the server-side read cursor makes
// unread counts survive a daemon restart. The dashboard's unread poll omits
// ?after= when the browser-local cursor is gone (the exact state after a
// restart wipes localStorage); the persisted cursor must then drive the
// count to 0 instead of re-reporting every message as unread.
func TestMarkGroupReadPersistsCursor(t *testing.T) {
	srv, gdm, group, _ := newGroupDMHandlerTestServer(t)
	m1, err := gdm.PostUserMessage(context.Background(), group.ID, "one", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	aliceID := group.Members[0].AgentID
	m2, err := gdm.PostMessage(context.Background(), group.ID, aliceID, "reply", m1.ID, false)
	if err != nil {
		t.Fatal(err)
	}

	// Before marking read, an ?after=-less poll (restarted browser, no local
	// cursor) counts every message as unread.
	unread := func(after string) int {
		url := "/api/v1/groupdms/" + group.ID + "/unread"
		if after != "" {
			url += "?after=" + after
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.SetPathValue("id", group.ID)
		rr := httptest.NewRecorder()
		srv.handleGetGroupUnread(rr, authedRequest(req, auth.Principal{Role: auth.RoleOwner}))
		if rr.Code != http.StatusOK {
			t.Fatalf("unread status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Count int `json:"count"`
		}
		readJSONResponse(t, rr, &resp)
		return resp.Count
	}
	if got := unread(""); got != 2 {
		t.Fatalf("pre-mark unread = %d, want 2", got)
	}

	// Mark the room read at the head via the API.
	mreq := httptest.NewRequest(http.MethodPost, "/api/v1/groupdms/"+group.ID+"/read",
		strings.NewReader(`{"messageId":"`+m2.ID+`"}`))
	mreq.SetPathValue("id", group.ID)
	mrr := httptest.NewRecorder()
	srv.handleMarkGroupRead(mrr, authedRequest(mreq, auth.Principal{Role: auth.RoleOwner}))
	if mrr.Code != http.StatusOK {
		t.Fatalf("mark-read status = %d, body = %s", mrr.Code, mrr.Body.String())
	}

	// A restart-style poll (no ?after=) must now see 0 unread from the
	// persisted cursor.
	if got := unread(""); got != 0 {
		t.Fatalf("post-mark unread (no after) = %d, want 0", got)
	}

	// A device with a STALE localStorage cursor (?after=m1, one behind the
	// persisted cursor at m2) must not resurrect the unread badge: the
	// server takes the max of both cursors.
	if got := unread(m1.ID); got != 0 {
		t.Fatalf("post-mark unread (stale after) = %d, want 0", got)
	}

	// The persisted cursor is the HUMAN operator's. A member agent's unread
	// view must not be advanced by it: with no ?after= the agent still sees
	// both messages, and with ?after=m1 it sees one.
	agentUnread := func(after string) int {
		url := "/api/v1/groupdms/" + group.ID + "/unread"
		if after != "" {
			url += "?after=" + after
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.SetPathValue("id", group.ID)
		rr := httptest.NewRecorder()
		srv.handleGetGroupUnread(rr, authedRequest(req,
			auth.Principal{Role: auth.RoleAgent, AgentID: aliceID}))
		if rr.Code != http.StatusOK {
			t.Fatalf("agent unread status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Count int `json:"count"`
		}
		readJSONResponse(t, rr, &resp)
		return resp.Count
	}
	if got := agentUnread(""); got != 2 {
		t.Fatalf("agent unread (no after) = %d, want 2 (operator cursor must not apply)", got)
	}
	if got := agentUnread(m1.ID); got != 1 {
		t.Fatalf("agent unread (after=m1) = %d, want 1", got)
	}
}
