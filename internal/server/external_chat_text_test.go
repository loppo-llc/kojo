package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
	"github.com/loppo-llc/kojo/internal/auth"
	"github.com/loppo-llc/kojo/internal/chathistory"
	"github.com/loppo-llc/kojo/internal/peer"
	"github.com/loppo-llc/kojo/internal/store"
)

func prepareRemoteExternalChat(t *testing.T, holderURL string) (*Server, *externalChatRouter, string) {
	t.Helper()
	srv := newChunkedSyncTestServer(t)
	srv.peerID = &peer.Identity{DeviceID: "hub", Name: "Hub"}
	a, err := srv.agents.Create(agent.AgentConfig{Name: "remote Slack"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := srv.agents.Store().UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: "holder", Name: "Holder", URL: holderURL, Status: store.PeerStatusOnline,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.agents.Store().AcquireAgentLock(ctx, a.ID, "holder",
		store.NowMillis(), int64((5*time.Minute)/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	router := newExternalChatRouter(srv)
	router.pollInterval = 5 * time.Millisecond
	router.probeTimeout = time.Second
	return srv, router, a.ID
}

func writeExternalChatTestStream(t *testing.T, w http.ResponseWriter, events ...agent.ChatEvent) {
	t.Helper()
	w.Header().Set("Content-Type", externalChatTextContentType)
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	for i := range events {
		evt := events[i]
		if err := enc.Encode(externalChatTextEnvelope{Kind: "event", Event: &evt}); err != nil {
			t.Errorf("encode event: %v", err)
			return
		}
	}
}

func TestTrackHandoffCapabilityStreamDropsUnusedCapability(t *testing.T) {
	srv := &Server{}
	capability := srv.mintHandoffArrivalCapability("ag_1", "slack:C:T",
		&fakeArrivalReservation{started: make(chan struct{}, 1)})
	router := &externalChatRouter{server: srv}
	source := make(chan agent.ChatEvent, 1)
	source <- agent.ChatEvent{Type: "text", Delta: "done"}
	close(source)

	var got []agent.ChatEvent
	for event := range router.trackHandoffCapabilityStream(context.Background(), capability, source, false) {
		got = append(got, event)
	}
	if len(got) != 1 || got[0].Delta != "done" {
		t.Fatalf("events = %#v", got)
	}
	srv.handoffArrivalMu.Lock()
	defer srv.handoffArrivalMu.Unlock()
	if _, ok := srv.handoffArrivalCaps[capability]; ok {
		t.Fatal("capability survived closed source stream")
	}
}

func TestTrackHandoffCapabilityStreamPreservesTerminalAfterCancel(t *testing.T) {
	srv := &Server{}
	capability := srv.mintHandoffArrivalCapability("ag_1", "slack:C:T",
		&fakeArrivalReservation{started: make(chan struct{}, 1)})
	router := &externalChatRouter{server: srv}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := make(chan agent.ChatEvent, 2)
	source <- agent.ChatEvent{Type: "text", Delta: "lossy partial"}
	source <- agent.ChatEvent{Type: "done", Message: &agent.Message{Content: "authoritative partial"}, ErrorMessage: agent.ErrMsgCancelled}
	close(source)

	var got []agent.ChatEvent
	for event := range router.trackHandoffCapabilityStream(ctx, capability, source, true) {
		got = append(got, event)
	}
	if len(got) == 0 {
		t.Fatal("terminal event was dropped")
	}
	last := got[len(got)-1]
	if last.Type != "done" || last.Message == nil || last.Message.Content != "authoritative partial" {
		t.Fatalf("events = %#v", got)
	}
}

func TestExternalChatRouterStreamsRemoteTextTurn(t *testing.T) {
	requestSeen := make(chan externalChatTextRequest, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
		case http.MethodPost:
			var req externalChatTextRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			requestSeen <- req
			writeExternalChatTestStream(t, w,
				agent.ChatEvent{Type: "text", Delta: "hel"},
				agent.ChatEvent{Type: "text", Delta: "lo"},
				agent.ChatEvent{Type: "done", Message: &agent.Message{Role: "assistant", Content: "hello"}})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	events, err := router.ChatOneShot(context.Background(), agentID, "full history", agent.OneShotOpts{
		SessionKey: "slack-thread", SystemPromptExtra: "slack context",
		History: []chathistory.HistoryMessage{
			{MessageID: "m1", UserID: "U1", Text: "earlier"},
		},
		HistorySelfUserID:                 "B1",
		DisableKojoAttachmentInstructions: true,
		ForceFreshSession:                 true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []agent.ChatEvent
	for evt := range events {
		got = append(got, evt)
	}
	if len(got) != 3 || got[0].Delta != "hel" || got[1].Delta != "lo" || got[2].Type != "done" ||
		got[2].Message == nil || got[2].Message.Content != "hello" {
		t.Fatalf("events = %#v", got)
	}
	select {
	case req := <-requestSeen:
		if req.Message != "full history" || req.SessionKey != "slack-thread" {
			t.Fatalf("request = %#v", req)
		}
		if req.FreshSessionContext == "" || req.ResumeSessionContext == "" ||
			!strings.Contains(req.FreshSessionContext, "earlier") ||
			!strings.Contains(req.ResumeSessionContext, "earlier") {
			t.Fatalf("request history contexts = %#v", req)
		}
		if !req.DisableAttachments || req.SystemPromptExtra != "slack context" || !req.ForceFreshSession {
			t.Fatalf("request options = %#v", req)
		}
	default:
		t.Fatal("holder did not receive request")
	}
}

func TestExternalChatRouterSteersRemoteThreadHolder(t *testing.T) {
	requestSeen := make(chan externalChatSteerRequest, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/external-chat/ready"):
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/external-chat/steer"):
			var req externalChatSteerRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			requestSeen <- req
			writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	if err := router.SteerOneShot(context.Background(), agentID, "groupdm:g_test", "additional detail"); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-requestSeen:
		if req.SessionKey != "groupdm:g_test" || req.Content != "additional detail" {
			t.Fatalf("request = %#v", req)
		}
	default:
		t.Fatal("holder did not receive steer")
	}
}

func TestExternalChatRouterReroutesSteerFromStaleHolderHint(t *testing.T) {
	var oldPosts atomic.Int32
	oldHolder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			oldPosts.Add(1)
			t.Error("steer must not be posted to stale holder")
		}
		_ = json.NewEncoder(w).Encode(externalChatReadyResponse{
			Ready: false, HolderPeer: "new-holder", Unavailable: "agent is held by another peer",
		})
	}))
	t.Cleanup(oldHolder.Close)

	requestSeen := make(chan externalChatSteerRequest, 1)
	newHolder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "new-holder"})
		case http.MethodPost:
			var req externalChatSteerRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			requestSeen <- req
			writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
		}
	}))
	t.Cleanup(newHolder.Close)

	srv, router, agentID := prepareRemoteExternalChat(t, oldHolder.URL)
	if _, err := srv.agents.Store().UpsertPeer(context.Background(), &store.PeerRecord{
		DeviceID: "new-holder", Name: "New Holder", URL: newHolder.URL, Status: store.PeerStatusOnline,
	}); err != nil {
		t.Fatal(err)
	}
	router.rememberRoute(agentID, "holder")
	if err := router.SteerOneShot(context.Background(), agentID, "groupdm:g_test", "follow handoff"); err != nil {
		t.Fatal(err)
	}
	if got := oldPosts.Load(); got != 0 {
		t.Fatalf("old holder received %d steer POSTs", got)
	}
	select {
	case req := <-requestSeen:
		if req.SessionKey != "groupdm:g_test" || req.Content != "follow handoff" {
			t.Fatalf("request = %#v", req)
		}
	default:
		t.Fatal("new holder did not receive steer")
	}
}

func TestExternalChatRouterMapsRemoteSteerNotBusy(t *testing.T) {
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		writeError(w, http.StatusConflict, "not_busy", "turn ended")
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	err := router.SteerOneShot(context.Background(), agentID, "groupdm:g_test", "late")
	if !errors.Is(err, agent.ErrAgentNotBusy) {
		t.Fatalf("err = %v, want ErrAgentNotBusy", err)
	}
}

func TestExternalChatRouterMarksLostSteerResponseUncertain(t *testing.T) {
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	err := router.SteerOneShot(context.Background(), agentID, "groupdm:g_test", "maybe delivered")
	if !errors.Is(err, agent.ErrSteerDeliveryUncertain) {
		t.Fatalf("err = %v, want ErrSteerDeliveryUncertain", err)
	}
}

func TestSteerOneShotWithContextReturnsUncertainWithoutWaitingForLocalBackend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- steerOneShotWithContext(ctx, make(chan struct{}, 1), time.Second, func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, agent.ErrSteerDeliveryUncertain) {
			t.Fatalf("err = %v, want ErrSteerDeliveryUncertain", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled local steer remained blocked")
	}
	close(release)
}

func TestSteerOneShotWithContextBoundsAbandonedLocalCalls(t *testing.T) {
	sem := make(chan struct{}, 1)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- steerOneShotWithContext(firstCtx, sem, time.Second, func() error {
			close(firstStarted)
			<-releaseFirst
			return nil
		})
	}()
	<-firstStarted
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, agent.ErrSteerDeliveryUncertain) {
		t.Fatalf("first err = %v, want ErrSteerDeliveryUncertain", err)
	}

	secondCalled := false
	err := steerOneShotWithContext(context.Background(), sem, 10*time.Millisecond, func() error {
		secondCalled = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "admission timed out") {
		t.Fatalf("second err = %v, want bounded admission timeout", err)
	}
	if secondCalled {
		t.Fatal("second local backend call started while abandoned call occupied limiter")
	}
	close(releaseFirst)
}

func TestExternalChatRouterDoesNotMarkDialFailureUncertain(t *testing.T) {
	holder := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	holder.Close()
	resp, attempted, err := router.postRemoteSteer(context.Background(), agentID, "holder", externalChatSteerRequest{
		SessionKey: "groupdm:g_test", Content: "not delivered",
	})
	if resp != nil || err == nil || attempted {
		t.Fatalf("resp = %v, attempted = %v, err = %v; want definite pre-write failure", resp, attempted, err)
	}
}

func TestExternalChatRouterUploadsAttachmentsToRemoteHolder(t *testing.T) {
	originalUploadDir := uploadDir
	uploadDir = t.TempDir()
	t.Cleanup(func() { uploadDir = originalUploadDir })
	sourcePath := filepath.Join(uploadDir, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("peer attachment"), 0o600); err != nil {
		t.Fatal(err)
	}

	requestSeen := make(chan externalChatTextRequest, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
		case r.URL.Path == "/api/v1/upload":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f, h, err := r.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer f.Close()
			raw, _ := io.ReadAll(f)
			if !bytes.Equal(raw, []byte("peer attachment")) || h.Filename != "source.txt" {
				t.Errorf("uploaded file = %q filename=%q", raw, h.Filename)
			}
			_ = json.NewEncoder(w).Encode(agent.MessageAttachment{
				Path: "/holder/upload/source.txt", Name: h.Filename, Mime: h.Header.Get("Content-Type"), Size: int64(len(raw)),
			})
		case r.Method == http.MethodPost:
			var req externalChatTextRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode external chat: %v", err)
				return
			}
			requestSeen <- req
			writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done"})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	events, err := router.ChatOneShot(context.Background(), agentID, "read this", agent.OneShotOpts{
		Attachments: []agent.MessageAttachment{{Path: sourcePath, Name: "source.txt", Mime: "text/plain", Size: 15}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	select {
	case req := <-requestSeen:
		if len(req.Attachments) != 1 || req.Attachments[0].Path != "/holder/upload/source.txt" {
			t.Fatalf("relayed attachments = %#v", req.Attachments)
		}
	case <-time.After(time.Second):
		t.Fatal("holder did not receive external chat request")
	}
}

func TestExternalChatFileRequiresActiveMatchingRelay(t *testing.T) {
	originalUploadDir := uploadDir
	uploadDir = t.TempDir()
	t.Cleanup(func() { uploadDir = originalUploadDir })
	path := filepath.Join(uploadDir, "result.txt")
	if err := os.WriteFile(path, []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{externalChatRelays: newExternalChatRelayRegistry()}
	release := s.externalChatRelays.acquire("ag_test", "hub")
	defer release()

	call := func(peerID string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"path": path})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/ag_test/external-chat/file", bytes.NewReader(body))
		req.SetPathValue("id", "ag_test")
		req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Role: auth.RolePeer, PeerID: peerID}))
		rr := httptest.NewRecorder()
		s.handleExternalChatFile(rr, req)
		return rr
	}
	if rr := call("other"); rr.Code != http.StatusForbidden {
		t.Fatalf("other peer status = %d", rr.Code)
	}
	if rr := call("hub"); rr.Code != http.StatusOK || rr.Body.String() != "result" {
		t.Fatalf("hub response status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestExternalChatHubAddressRequiresAllowedProxyPeer(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	a, err := s.agents.Create(agent.AgentConfig{Name: "remote MCP trust"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"hub", "other"} {
		if _, err := s.agents.Store().UpsertPeer(ctx, &store.PeerRecord{
			DeviceID: id, Name: id, URL: "https://" + id + ".example:443", Status: store.PeerStatusOnline,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.agents.Store().AcquireAgentLock(ctx, a.ID, "holder", store.NowMillis(), int64(time.Minute/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := s.agents.Store().UpdateAgentLockAllowedProxy(ctx, a.ID, "holder", "hub"); err != nil {
		t.Fatal(err)
	}

	resolve := func(peerID string) (string, error) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/external-chat/text", nil)
		req.SetPathValue("id", a.ID)
		req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Role: auth.RolePeer, PeerID: peerID}))
		_, addr, err := s.externalChatHubAddress(ctx, req, "")
		return addr, err
	}
	if _, err := resolve("other"); err == nil {
		t.Fatal("unrelated paired peer was accepted as the Hub")
	}
	addr, err := resolve("hub")
	if err != nil || addr != "https://hub.example:443" {
		t.Fatalf("allowed Hub addr=%q err=%v", addr, err)
	}
}

func TestAgentWebSocketRoutingRejectsUnrelatedPeer(t *testing.T) {
	s := newChunkedSyncTestServer(t)
	s.peerID = &peer.Identity{DeviceID: "holder", Name: "Holder"}
	a, err := s.agents.Create(agent.AgentConfig{Name: "remote WebUI trust"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.agents.Store().AcquireAgentLock(ctx, a.ID, "holder", store.NowMillis(), int64(time.Minute/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := s.agents.Store().UpdateAgentLockAllowedProxy(ctx, a.ID, "holder", "hub"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+a.ID+"/ws", nil)
	req.SetPathValue("id", a.ID)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Role: auth.RolePeer, PeerID: "other"}))
	rr := httptest.NewRecorder()
	s.handleAgentWebSocketRouting(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestExternalChatRouterBoundsHistoryBeforeRemoteRelay(t *testing.T) {
	requestSeen := make(chan externalChatTextRequest, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		if r.ContentLength >= externalChatTextBodyLimit {
			t.Errorf("relayed body = %d bytes, limit = %d", r.ContentLength, externalChatTextBodyLimit)
		}
		var req externalChatTextRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requestSeen <- req
		writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done"})
	}))
	t.Cleanup(holder.Close)

	history := make([]chathistory.HistoryMessage, 6000)
	for i := range history {
		history[i] = chathistory.HistoryMessage{
			MessageID: fmt.Sprintf("m-%04d", i),
			UserID:    "U1",
			UserName:  "User",
			Text:      fmt.Sprintf("message-%04d-%s", i, strings.Repeat("x", 1000)),
		}
	}
	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	events, err := router.ChatOneShot(context.Background(), agentID, "current", agent.OneShotOpts{
		History: history, HistorySelfUserID: "B1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	select {
	case req := <-requestSeen:
		if len(req.FreshSessionContext) > chathistory.DefaultMaxChars+len("\n---\n\n") {
			t.Fatalf("fresh context length = %d", len(req.FreshSessionContext))
		}
		if !strings.Contains(req.FreshSessionContext, "message-5999") ||
			strings.Contains(req.FreshSessionContext, "message-0000") {
			t.Fatal("fresh context did not preserve the bounded recent window")
		}
		if !strings.Contains(req.ResumeSessionContext, "message-0000") ||
			!strings.Contains(req.ResumeSessionContext, "message-5999") ||
			strings.Contains(req.ResumeSessionContext, "message-3000") {
			t.Fatal("resume context did not preserve bounded head and tail windows")
		}
	default:
		t.Fatal("holder did not receive request")
	}
}

func TestExternalChatRouterWaitsInMemoryDuringHandoff(t *testing.T) {
	var ready atomic.Bool
	var posts atomic.Int32
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if !ready.Load() {
				_ = json.NewEncoder(w).Encode(externalChatReadyResponse{
					Switching: true, HolderPeer: "holder", Unavailable: "device switch in progress",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		posts.Add(1)
		w.Header().Set("Content-Type", externalChatTextContentType)
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(30 * time.Millisecond)
		evt := agent.ChatEvent{Type: "done"}
		if err := json.NewEncoder(w).Encode(externalChatTextEnvelope{Kind: "event", Event: &evt}); err != nil {
			t.Errorf("encode done: %v", err)
		}
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	router.handoffWait = 500 * time.Millisecond
	go func() {
		time.Sleep(40 * time.Millisecond)
		ready.Store(true)
	}()
	events, err := router.ChatOneShot(context.Background(), agentID, "hello", agent.OneShotOpts{})
	if err != nil {
		t.Fatal(err)
	}
	seenDone := false
	for evt := range events {
		seenDone = seenDone || evt.Type == "done"
	}
	if posts.Load() != 1 {
		t.Fatalf("POST count = %d, want 1", posts.Load())
	}
	if !seenDone {
		t.Fatal("handoff-dispatched stream was cancelled before done")
	}
}

func TestExternalChatRouterWaitsForRuntimeAfterLockArrival(t *testing.T) {
	var ready atomic.Bool
	var posts atomic.Int32
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if !ready.Load() {
				// Lock already points here, but finalize has not activated the
				// runtime yet. No explicit Switching flag exists on the target.
				_ = json.NewEncoder(w).Encode(externalChatReadyResponse{
					HolderPeer: "holder", Unavailable: "agent runtime is not active on this peer",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		posts.Add(1)
		writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done"})
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	router.handoffWait = 500 * time.Millisecond
	go func() {
		time.Sleep(40 * time.Millisecond)
		ready.Store(true)
	}()
	events, err := router.ChatOneShot(context.Background(), agentID, "hello", agent.OneShotOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if posts.Load() != 1 {
		t.Fatalf("POST count = %d, want 1 after runtime activation", posts.Load())
	}
}

func TestExternalChatRouterDoesNotRetryAfterUncertainPOST(t *testing.T) {
	var posts atomic.Int32
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		posts.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer cannot hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	if _, err := router.ChatOneShot(context.Background(), agentID, "hello", agent.OneShotOpts{}); err == nil {
		t.Fatal("expected transport error")
	}
	if posts.Load() != 1 {
		t.Fatalf("POST count = %d, want exactly 1", posts.Load())
	}
}

func TestExternalChatRouterRediscoversBeforePOSTAttempt(t *testing.T) {
	var ready atomic.Bool
	var posts atomic.Int32
	newHolder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if !ready.Load() {
				// Discovery reached the new lock holder before its runtime was
				// activated. It must stay in the bounded handoff wait.
				_ = json.NewEncoder(w).Encode(externalChatReadyResponse{
					HolderPeer: "new-holder", Unavailable: "agent runtime is not active on this peer",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "new-holder"})
			return
		}
		posts.Add(1)
		writeExternalChatTestStream(t, w, agent.ChatEvent{Type: "done"})
	}))
	t.Cleanup(newHolder.Close)

	srv, router, agentID := prepareRemoteExternalChat(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if _, err := srv.agents.Store().UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: "holder", Name: "Old holder", URL: "http://127.0.0.1:1", Status: store.PeerStatusOffline,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.agents.Store().UpsertPeer(ctx, &store.PeerRecord{
		DeviceID: "new-holder", Name: "New holder", URL: newHolder.URL, Status: store.PeerStatusOnline,
	}); err != nil {
		t.Fatal(err)
	}
	router.handoffWait = 500 * time.Millisecond
	go func() {
		time.Sleep(40 * time.Millisecond)
		ready.Store(true)
	}()
	events, err := router.ChatOneShot(ctx, agentID, "hello", agent.OneShotOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if posts.Load() != 1 {
		t.Fatalf("new holder POST count = %d, want 1", posts.Load())
	}
}

func TestExternalChatRouterHandoffWaitTimesOutWithoutDispatch(t *testing.T) {
	var posts atomic.Int32
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{
				Switching: true, HolderPeer: "holder", Unavailable: "device switch in progress",
			})
			return
		}
		posts.Add(1)
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	router.handoffWait = 35 * time.Millisecond
	if _, err := router.ChatOneShot(context.Background(), agentID, "hello", agent.OneShotOpts{}); err == nil {
		t.Fatal("expected handoff timeout")
	}
	if posts.Load() != 0 {
		t.Fatalf("POST count = %d, want 0", posts.Load())
	}
}

func TestExternalChatRouterCancellationReachesHolder(t *testing.T) {
	cancelled := make(chan struct{})
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
			return
		}
		w.Header().Set("Content-Type", externalChatTextContentType)
		_ = json.NewEncoder(w).Encode(externalChatTextEnvelope{
			Kind: "event", Event: &agent.ChatEvent{Type: "text", Delta: "started"},
		})
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				close(cancelled)
				return
			case <-ticker.C:
				if _, err := w.Write([]byte("{\"kind\":\"heartbeat\"}\n")); err != nil {
					close(cancelled)
					return
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	ctx, cancel := context.WithCancel(context.Background())
	events, err := router.ChatOneShot(ctx, agentID, "hello", agent.OneShotOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if evt := <-events; evt.Delta != "started" {
		t.Fatalf("first event = %#v", evt)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("holder request context was not cancelled")
	}
}

func TestExternalChatHandlerRejectsNonPeer(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/ag_x/external-chat/ready", nil)
	req.SetPathValue("id", "ag_x")
	rr := httptest.NewRecorder()
	srv.handleExternalChatReady(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestExternalChatReadyAdvertisesOriginAwareArrival(t *testing.T) {
	srv := newChunkedSyncTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/ag_x/external-chat/ready", nil)
	req.SetPathValue("id", "ag_x")
	req = authedRequest(req, auth.Principal{Role: auth.RolePeer, PeerID: "peer-a"})
	rr := httptest.NewRecorder()
	srv.handleExternalChatReady(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got externalChatReadyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OriginAwareArrivalV1 {
		t.Fatal("origin-aware arrival capability was not advertised")
	}
}

func TestDispatchFinalizeSignalsDowngradeWhenOldTargetRejectsContinuation(t *testing.T) {
	var calls int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, has := body["continuation"]; has {
			http.Error(w, `json: unknown field "continuation"`, http.StatusBadRequest)
			return
		}
		writeJSONResponse(w, http.StatusOK, peerAgentSyncFinalizeResponse{AgentID: "ag_x"})
	}))
	defer target.Close()
	srv := &Server{peerID: &peer.Identity{DeviceID: "source"}, logger: slog.Default()}
	err := srv.dispatchPeerAgentSyncFinalize(context.Background(), target.URL, "target", "ag_x", "op_x", nil,
		&handoffContinuation{SessionKey: "groupdm:g", OriginPeerID: "hub", Capability: "cap"})
	if !errors.Is(err, errFinalizeContinuationDowngrade) {
		t.Fatalf("err = %v, want continuation downgrade", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want only capability finalize before caller applies barrier", calls)
	}
}

func TestExternalChatRouterAcknowledgesAttachmentAfterConsumerAccepts(t *testing.T) {
	ackSeen := make(chan struct{}, 1)
	ackCalled := make(chan struct{}, 1)
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
		case strings.HasSuffix(r.URL.Path, "/external-chat/attachment-ack"):
			var in externalChatAttachmentAckRequest
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Token != "ack-token" {
				t.Errorf("attachment ack = %#v err=%v", in, err)
				http.Error(w, "bad ack", http.StatusBadRequest)
				return
			}
			ackSeen <- struct{}{}
			ackCalled <- struct{}{}
			writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
		case strings.HasSuffix(r.URL.Path, "/external-chat"):
			w.Header().Set("Content-Type", externalChatTextContentType)
			w.WriteHeader(http.StatusOK)
			enc := json.NewEncoder(w)
			evt := agent.ChatEvent{Type: "attachment", Attachments: []agent.MessageAttachment{{Path: "blob:result"}}}
			if err := enc.Encode(externalChatTextEnvelope{Kind: "event", Event: &evt, AttachmentAckToken: "ack-token"}); err != nil {
				t.Error(err)
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			done := agent.ChatEvent{Type: "done"}
			if err := enc.Encode(externalChatTextEnvelope{Kind: "event", Event: &done}); err != nil {
				t.Error(err)
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case <-ackSeen:
			case <-time.After(time.Second):
				t.Error("Hub did not acknowledge accepted attachment")
				return
			}
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	events, err := router.ChatOneShot(context.Background(), agentID, "create file", agent.OneShotOpts{})
	if err != nil {
		t.Fatal(err)
	}
	seenDone := false
	var attachmentClaims []agent.ChatEvent
	for evt := range events {
		switch evt.Type {
		case "attachment":
			if !evt.BeginAttachmentOwnership() {
				t.Fatal("could not claim remote attachment")
			}
			attachmentClaims = append(attachmentClaims, evt)
		case "done":
			seenDone = true
		}
	}
	if !seenDone {
		t.Fatal("stream did not reach terminal event before persistence")
	}
	// The response adapter acknowledges only after its durable append succeeds.
	for i := range attachmentClaims {
		attachmentClaims[i].FinishAttachmentOwnership(true)
	}
	select {
	case <-ackCalled:
	case <-time.After(time.Second):
		t.Fatal("holder did not receive post-persistence acknowledgement")
	}
}

func TestExternalChatAttachmentAckIsBoundToAgentAndPeer(t *testing.T) {
	s := &Server{}
	token, entry, err := s.externalChatAttachmentAcks.register("ag_test", "hub")
	if err != nil {
		t.Fatal(err)
	}
	done := entry.done
	call := func(agentID, peerID string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(externalChatAttachmentAckRequest{Token: token})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/external-chat/attachment-ack", bytes.NewReader(body))
		req.SetPathValue("id", agentID)
		req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Role: auth.RolePeer, PeerID: peerID}))
		rr := httptest.NewRecorder()
		s.handleExternalChatAttachmentAck(rr, req)
		return rr
	}
	if rr := call("ag_other", "hub"); rr.Code != http.StatusConflict {
		t.Fatalf("wrong agent status = %d", rr.Code)
	}
	if rr := call("ag_test", "other"); rr.Code != http.StatusConflict {
		t.Fatalf("wrong peer status = %d", rr.Code)
	}
	select {
	case <-done:
		t.Fatal("invalid caller acknowledged attachment")
	default:
	}
	if rr := call("ag_test", "hub"); rr.Code != http.StatusOK {
		t.Fatalf("matching ack status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !s.externalChatAttachmentAcks.finish(token, entry) {
		t.Fatal("acknowledged entry was not accepted")
	}
	if rr := call("ag_test", "hub"); rr.Code != http.StatusOK {
		t.Fatalf("idempotent retry status = %d body=%s", rr.Code, rr.Body.String())
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("matching acknowledgement did not release holder")
	}
}

// Slack `!stop all` follows the thread holder like a steer: the holder gets a
// stopBackground request, and its "no session" answer maps back to
// agent.ErrBackgroundSessionNotFound.
func TestExternalChatRouterRelaysStopThreadBackgroundTasks(t *testing.T) {
	requestSeen := make(chan externalChatSteerRequest, 2)
	var missing atomic.Bool
	holder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/external-chat/ready"):
			_ = json.NewEncoder(w).Encode(externalChatReadyResponse{Ready: true, HolderPeer: "holder"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/external-chat/steer"):
			var req externalChatSteerRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			requestSeen <- req
			if missing.Load() {
				writeError(w, http.StatusNotFound, "background_session_not_found", "no background session")
				return
			}
			writeJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	}))
	t.Cleanup(holder.Close)

	_, router, agentID := prepareRemoteExternalChat(t, holder.URL)
	key := agentID + ":slack:C1:1.0"
	if err := router.StopThreadBackgroundTasks(context.Background(), agentID, key); err != nil {
		t.Fatal(err)
	}
	if req := <-requestSeen; !req.StopBackground || req.SessionKey != key || req.Content != "" || req.Question != nil {
		t.Fatalf("request = %#v", req)
	}
	missing.Store(true)
	if err := router.StopThreadBackgroundTasks(context.Background(), agentID, key); !errors.Is(err, agent.ErrBackgroundSessionNotFound) {
		t.Fatalf("err = %v, want ErrBackgroundSessionNotFound", err)
	}
}
