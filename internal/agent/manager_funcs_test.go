package agent

import (
	"testing"

	"github.com/loppo-llc/kojo/internal/notifysource"
)

func TestResolvePublicProfile(t *testing.T) {
	t.Run("persona emptied clears profile", func(t *testing.T) {
		a := &Agent{Persona: "", PublicProfile: "old profile"}
		empty := ""
		regen := resolvePublicProfile(a, AgentUpdateConfig{Persona: &empty}, "had persona", false)
		if regen {
			t.Error("expected no regen when persona emptied")
		}
		if a.PublicProfile != "" {
			t.Errorf("expected empty profile, got %q", a.PublicProfile)
		}
	})

	t.Run("persona changed triggers regen", func(t *testing.T) {
		a := &Agent{Persona: "new persona", PublicProfile: "old"}
		newP := "new persona"
		regen := resolvePublicProfile(a, AgentUpdateConfig{Persona: &newP}, "old persona", false)
		if !regen {
			t.Error("expected regen when persona changed")
		}
		if a.PublicProfile != "" {
			t.Errorf("expected profile cleared for regen, got %q", a.PublicProfile)
		}
	})

	t.Run("override on prevents regen", func(t *testing.T) {
		a := &Agent{Persona: "new", PublicProfile: "manual", PublicProfileOverride: true}
		newP := "new"
		regen := resolvePublicProfile(a, AgentUpdateConfig{Persona: &newP}, "old", true)
		if regen {
			t.Error("expected no regen with override on")
		}
		if a.PublicProfile != "manual" {
			t.Errorf("expected profile preserved, got %q", a.PublicProfile)
		}
	})

	t.Run("override turned off triggers regen", func(t *testing.T) {
		// override flips from true → false
		a := &Agent{Persona: "has persona", PublicProfile: "manual", PublicProfileOverride: false}
		off := false
		regen := resolvePublicProfile(a, AgentUpdateConfig{PublicProfileOverride: &off}, "has persona", true)
		if !regen {
			t.Error("expected regen when override turned off")
		}
	})

	t.Run("override stays off does not regen", func(t *testing.T) {
		// override stays false → false (form re-sent same value); must not regen
		a := &Agent{Persona: "has persona", PublicProfile: "kept", PublicProfileOverride: false}
		off := false
		regen := resolvePublicProfile(a, AgentUpdateConfig{PublicProfileOverride: &off}, "has persona", false)
		if regen {
			t.Error("expected no regen when override stays off")
		}
		if a.PublicProfile != "kept" {
			t.Errorf("expected profile preserved, got %q", a.PublicProfile)
		}
	})

	t.Run("no persona no override clears profile", func(t *testing.T) {
		a := &Agent{Persona: "", PublicProfile: "leftover"}
		regen := resolvePublicProfile(a, AgentUpdateConfig{}, "", false)
		if regen {
			t.Error("expected no regen")
		}
		if a.PublicProfile != "" {
			t.Errorf("expected empty profile, got %q", a.PublicProfile)
		}
	})
}

func TestIsRateLimitMessage(t *testing.T) {
	tests := []struct {
		name string
		msg  *Message
		want bool
	}{
		{"nil message", nil, false},
		{"empty content", &Message{Content: ""}, false},
		{"has tool uses", &Message{Content: "hit your limit", ToolUses: []ToolUse{{Name: "test"}}}, false},
		{"claude rate limit", &Message{Content: "You've hit your limit for the day."}, true},
		{"generic rate limit", &Message{Content: "Rate limit exceeded. Please wait."}, true},
		{"gemini exhausted", &Message{Content: "Resource exhausted, try again later"}, true},
		{"openai quota", &Message{Content: "You exceeded your current quota"}, true},
		{"usage limit", &Message{Content: "Usage limit exceeded for this month"}, true},
		{"normal message", &Message{Content: "Here is the code you requested."}, false},
		{"long message not rate limit", &Message{Content: string(make([]rune, 301))}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRateLimitMessage(tt.msg); got != tt.want {
				t.Errorf("isRateLimitMessage() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatMessageWithAttachments(t *testing.T) {
	atts := []MessageAttachment{
		{Path: "/tmp/file.txt", Name: "file.txt", Mime: "text/plain"},
		{Path: "/tmp/img.png", Name: "img.png", Mime: "image/png"},
	}
	result := formatMessageWithAttachments("Hello", atts)
	expected := "[Attached files — use your Read tool to view these files]\n" +
		"- /tmp/file.txt (file.txt, text/plain)\n" +
		"- /tmp/img.png (img.png, image/png)\n" +
		"\nHello"
	if result != expected {
		t.Errorf("got:\n%s\nwant:\n%s", result, expected)
	}
}

func TestTruncatePreview(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"this is too long", 5, "this …"},
		{"", 5, ""},
		{"日本語テスト", 3, "日本語…"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := truncatePreview(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncatePreview(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestCopyAgent(t *testing.T) {
	orig := &Agent{
		ID:   "ag_test",
		Name: "Test",
		LastMessage: &MessagePreview{
			Content: "hello", Role: "user", Timestamp: "2024-01-01T00:00:00Z",
		},
		NotifySources: []notifysource.Config{
			{Type: "gmail", Options: map[string]string{"key": "val"}},
		},
	}
	cp := copyAgent(orig)

	// Verify deep copy
	if cp == orig {
		t.Error("copy should be a different pointer")
	}
	if cp.LastMessage == orig.LastMessage {
		t.Error("LastMessage should be deep copied")
	}
	if cp.LastMessage.Content != "hello" {
		t.Errorf("expected content 'hello', got %q", cp.LastMessage.Content)
	}

	// Mutate copy, verify original is unchanged
	cp.Name = "Changed"
	cp.LastMessage.Content = "changed"
	cp.NotifySources[0].Options["key"] = "changed"

	if orig.Name != "Test" {
		t.Error("original Name should be unchanged")
	}
	if orig.LastMessage.Content != "hello" {
		t.Error("original LastMessage.Content should be unchanged")
	}
	if orig.NotifySources[0].Options["key"] != "val" {
		t.Error("original NotifySources options should be unchanged")
	}
}

func TestMatchToolOutput(t *testing.T) {
	t.Run("match by ID", func(t *testing.T) {
		tools := []ToolUse{
			{ID: "t1", Name: "read", Output: ""},
			{ID: "t2", Name: "write", Output: ""},
		}
		matchToolOutput(tools, "t1", "read", "file content")
		if tools[0].Output != "file content" {
			t.Errorf("expected t1 output set, got %q", tools[0].Output)
		}
		if tools[1].Output != "" {
			t.Error("t2 should be unchanged")
		}
	})

	t.Run("match by name when no ID", func(t *testing.T) {
		tools := []ToolUse{
			{Name: "read", Output: "already set"},
			{Name: "read", Output: ""},
		}
		matchToolOutput(tools, "", "read", "new output")
		if tools[1].Output != "new output" {
			t.Errorf("expected last unmatched 'read' output set, got %q", tools[1].Output)
		}
	})

	t.Run("ID provided but not found does not fallback", func(t *testing.T) {
		tools := []ToolUse{
			{Name: "read", Output: ""},
		}
		matchToolOutput(tools, "nonexistent", "read", "output")
		if tools[0].Output != "" {
			t.Error("should not fallback to name match when ID is provided")
		}
	})
}

func TestFilterEnv(t *testing.T) {
	result := filterEnv([]string{"HOME=", "PATH="}, "ag_123", "/data/dir")

	// Should contain AGENT_BROWSER vars
	found := map[string]bool{}
	for _, e := range result {
		if e == "AGENT_BROWSER_SESSION=ag_123" {
			found["session"] = true
		}
		if e == "AGENT_BROWSER_COOKIE_DIR=/data/dir" {
			found["cookie"] = true
		}
		if e == "AGENT_BROWSER_SESSION_NAME=ag_123" {
			found["name"] = true
		}
	}
	if !found["session"] || !found["cookie"] || !found["name"] {
		t.Error("expected AGENT_BROWSER env vars to be set")
	}
}
