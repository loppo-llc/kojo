package agent

import "testing"

// TestBackendLoadsClaudeSkills locks the gating contract for the
// kojo-attach SKILL.md install site. Adding a new backend that does
// NOT read `.claude/skills/` MUST keep returning false; conversely
// a new Claude-Code-compatible backend MUST be added here AND to
// the SyncAttachSkill call site in manager.go (otherwise the skill
// file never appears in its agentDir).
func TestBackendLoadsClaudeSkills(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tool string
		want bool
	}{
		// Claude Code itself: native loader.
		{"claude", true},
		// "custom" delegates to ClaudeBackend with a relocated config
		// dir; skill discovery still walks up from cwd.
		{"custom", true},
		// Grok Build: `grok inspect` from an agentDir lists kojo-*
		// skills as `project` scope, confirming the .claude/skills/
		// compatibility path is honored.
		{"grok", true},
		// codex / llama.cpp have no skill loader; installing files
		// would just leave dead bytes on disk.
		{"codex", false},
		{"llama.cpp", false},
		// Unknown / empty values must fail closed.
		{"", false},
		{"unknown-future-cli", false},
	}
	for _, tc := range cases {
		if got := backendLoadsClaudeSkills(tc.tool); got != tc.want {
			t.Errorf("backendLoadsClaudeSkills(%q) = %v, want %v", tc.tool, got, tc.want)
		}
	}
}

// TestBackendSupportsDeviceSwitch locks the gating contract for the
// kojo-switch-device SKILL.md install sites. A backend qualifies
// only when the handoff orchestrator knows how to migrate its
// session state to the target peer: claude / custom transfer the
// ~/.claude/projects/<...>/<uuid>.jsonl files; grok transfers
// `<agentDir>/.grok/session_id` plus the
// $GROK_HOME/sessions/<encoded(absAgentDir)>/<uuid>/ subtree (see
// grok_session_transfer.go). codex / llama.cpp have no session
// transfer wired up and must stay false until they do.
func TestBackendSupportsDeviceSwitch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		tool string
		want bool
	}{
		{"claude", true},
		{"custom", true},
		{"grok", true},
		{"codex", false},
		{"llama.cpp", false},
		{"", false},
		{"unknown-future-cli", false},
	}
	for _, tc := range cases {
		if got := backendSupportsDeviceSwitch(tc.tool); got != tc.want {
			t.Errorf("backendSupportsDeviceSwitch(%q) = %v, want %v", tc.tool, got, tc.want)
		}
	}
}

// TestDeviceSwitchSubsetOfClaudeSkills enforces the invariant that
// every device-switch-capable backend must also load Claude skills.
// The SKILL.md is the delivery mechanism, so promoting a backend in
// backendSupportsDeviceSwitch without also having it appear in
// backendLoadsClaudeSkills would install a file the backend cannot
// read. The check is cheap; running it pins the relationship in
// case a future refactor splits the two helpers further.
func TestDeviceSwitchSubsetOfClaudeSkills(t *testing.T) {
	t.Parallel()

	for _, tool := range []string{"claude", "custom", "grok", "codex", "llama.cpp", ""} {
		if backendSupportsDeviceSwitch(tool) && !backendLoadsClaudeSkills(tool) {
			t.Errorf("backendSupportsDeviceSwitch(%q) is true but backendLoadsClaudeSkills(%q) is false — every device-switch backend must also load .claude/skills/", tool, tool)
		}
	}
}
