package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loppo-llc/kojo/internal/auth"
)

// TestHandleSteerAgent_NotBusy verifies a 409 with the "not_busy" reason
// when the agent has no turn currently running.
func TestHandleSteerAgent_NotBusy(t *testing.T) {
	srv, _, _, outsider := newGroupDMHandlerTestServer(t)

	body := bytes.NewBufferString(`{"content":"steer this"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+outsider.ID+"/steer", body)
	req.SetPathValue("id", outsider.ID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})

	rr := httptest.NewRecorder()
	srv.handleSteerAgent(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestHandleSteerAgent_EmptyContent verifies a 400 for an empty/blank body.
func TestHandleSteerAgent_EmptyContent(t *testing.T) {
	srv, _, _, outsider := newGroupDMHandlerTestServer(t)

	body := bytes.NewBufferString(`{"content":"  "}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+outsider.ID+"/steer", body)
	req.SetPathValue("id", outsider.ID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})

	rr := httptest.NewRecorder()
	srv.handleSteerAgent(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestHandleSteerGroupDM_NotBusy verifies a 409 when no thread turn is in
// flight for the room.
func TestHandleSteerGroupDM_NotBusy(t *testing.T) {
	srv, _, group, _ := newGroupDMHandlerTestServer(t)

	body := bytes.NewBufferString(`{"content":"steer this"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/groupdms/"+group.ID+"/steer", body)
	req.SetPathValue("id", group.ID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})

	rr := httptest.NewRecorder()
	srv.handleSteerGroupDM(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestHandleSteerGroupDM_EmptyContent verifies a 400 for blank content.
func TestHandleSteerGroupDM_EmptyContent(t *testing.T) {
	srv, _, group, _ := newGroupDMHandlerTestServer(t)

	body := bytes.NewBufferString(`{"content":""}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/groupdms/"+group.ID+"/steer", body)
	req.SetPathValue("id", group.ID)
	req = authedRequest(req, auth.Principal{Role: auth.RoleOwner})

	rr := httptest.NewRecorder()
	srv.handleSteerGroupDM(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}
