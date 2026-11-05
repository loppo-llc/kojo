package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFitPushPayload_ShrinksEscapeInflatedPreview(t *testing.T) {
	// 200 '<' bytes marshal to 1200 bytes (< each); with a name and
	// thread name of the same shape the raw payload exceeds the budget.
	fields := map[string]any{
		"type":       "agent_chat_done",
		"agentId":    "ag_0123456789abcdef",
		"groupId":    "gd_0123456789abcdef",
		"name":       truncateUTF8(strings.Repeat("<", 80), 80),
		"threadName": truncateUTF8(strings.Repeat("<", 40), 40),
		"preview":    truncateUTF8(strings.Repeat("<", 300), 200),
	}
	raw, _ := json.Marshal(fields)
	if len(raw) <= pushPayloadBudget {
		t.Fatalf("test setup: raw payload %d bytes is already within budget", len(raw))
	}
	got := fitPushPayload(fields)
	if len(got) > pushPayloadBudget {
		t.Fatalf("payload = %d bytes, want <= %d", len(got), pushPayloadBudget)
	}
	var out map[string]string
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	if out["groupId"] != "gd_0123456789abcdef" || out["preview"] == "" {
		t.Errorf("payload lost required fields: %+v", out)
	}
}

func TestFitPushPayload_LeavesSmallPayloadAlone(t *testing.T) {
	fields := map[string]any{"type": "agent_chat_done", "preview": "こんにちは"}
	got := fitPushPayload(fields)
	want, _ := json.Marshal(map[string]any{"type": "agent_chat_done", "preview": "こんにちは"})
	if string(got) != string(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}
