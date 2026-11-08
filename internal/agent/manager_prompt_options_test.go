package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPrepareChatOptionsDisableKojoAttachmentInstructions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", "")

	a := Agent{ID: "ag_transport_prompt"}
	applyPrepareChatOptions(&a, prepareChatOptions{disableKojoAttachmentInstructions: true})

	prompt := buildSystemPrompt(&a,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		"", nil, false)
	for _, forbidden := range []string{
		"Sending file attachments to the user",
		"attachments.md",
		attachStagingSubpath,
	} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("transport-disabled prompt still contains %q", forbidden)
		}
	}
}

// A gemini-agy thread / external turn that hides the attachment contract
// writes the same rules as the main chat (which agy re-reads under every
// running turn) and carries the opt-out in its custom agent instead.
func TestAgyOneShotKeepsSharedRulesWhenHidingAttachments(t *testing.T) {
	mgr := newSealTestManager(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	script := "#!/bin/sh\ncat >/dev/null\n" +
		"name=''; prev=''; for a in \"$@\"; do [ \"$prev\" = --agent ] && name=$a; prev=$a; done\n" +
		"{ echo '=== RULES'; cat .agents/rules/kojo-system-*.md; echo '=== DEF'; cat \".agents/agents/$name/agent.md\"; } > \"" + log + ".$$\"\n" +
		"printf '%s\\n' '{\"event\":\"result\",\"result\":{\"status\":\"SUCCESS\",\"response\":\"ok\"}}'\n"
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	a, err := mgr.Create(AgentConfig{Name: "agyattach", Tool: ToolGeminiAgy})
	if err != nil {
		t.Fatal(err)
	}
	run := func(key string, hide bool) (rules, def string) {
		t.Helper()
		before, _ := filepath.Glob(log + ".*")
		ch, err := mgr.ChatOneShot(context.Background(), a.ID, "hi", OneShotOpts{
			SessionKey:                        key,
			SystemPromptExtra:                 "EXTRA " + key,
			DisableKojoAttachmentInstructions: hide,
		})
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
		}
		after, _ := filepath.Glob(log + ".*")
		if len(after) != len(before)+1 {
			t.Fatalf("agy ran %d times", len(after)-len(before))
		}
		var b []byte
		for _, p := range after {
			if !slices.Contains(before, p) {
				b, _ = os.ReadFile(p)
			}
		}
		r, d, ok := strings.Cut(string(b), "=== DEF")
		if !ok {
			t.Fatalf("fake agy log: %s", b)
		}
		return r, d
	}
	mainRules, mainDef := run("plain", false)
	threadRules, threadDef := run("groupdm:gd_x", true)
	if mainRules != threadRules {
		t.Errorf("hiding attachments changed the shared rules:\n%s\n---\n%s", mainRules, threadRules)
	}
	if !strings.Contains(threadRules, "Sending file attachments to the user") {
		t.Error("shared rules lost the attachment section")
	}
	optOut := agyAttachmentOptOutPrompt(a.ID)
	if !strings.Contains(threadDef, optOut) || !strings.Contains(threadDef, "EXTRA groupdm:gd_x") {
		t.Errorf("thread agent def lacks the opt-out or its extra:\n%s", threadDef)
	}
	if strings.Contains(mainDef, optOut) {
		t.Error("opt-out leaked into a turn that keeps attachments")
	}
}
