package agent

import (
	"strings"
	"testing"
)

// TestBuildSystemPrompt_TmpRuleAllowsDisposableFiles locks the /tmp rule's
// shape: it keeps the intent of d0d9925d (nothing worth keeping lives only
// in /tmp) but must not be an absolute ban, because tools such as the Slack
// extension's slack_upload_file only accept files staged under
// {tmp}/kojo/upload and strict models refused that staging under the old
// "NEVER save files to /tmp" wording.
func TestBuildSystemPrompt_TmpRuleAllowsDisposableFiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	a := &Agent{ID: "ag_test_tmp_rule", Tool: ToolClaude, Persona: "You are terse."}
	seedToollessAgentFiles(t, a.ID)

	prompt := buildSystemPrompt(a, newQuietLogger(), "http://127.0.0.1:8080", nil, true)

	if strings.Contains(prompt, "NEVER save files to /tmp") {
		t.Error("prompt must not carry the absolute /tmp ban")
	}
	for _, s := range []string{
		"unless they are truly disposable",
		"kojo upload directory",
		"goes under this directory",
		// The workspace temp/ bullet stays the default for scratch outputs;
		// the /tmp exception must not displace it.
		"default there when unsure",
	} {
		if !strings.Contains(prompt, s) {
			t.Errorf("prompt must contain %q", s)
		}
	}
}
