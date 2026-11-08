package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedAgyConversation(t *testing.T, id string) string {
	t.Helper()
	path := filepath.Join(agyHome(), "conversations", id+".db")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAgyClearTracksAllOwnedConversations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	id := "ag_agyclear"
	dir := agentDir(id)
	writeAgyConversationID(dir, "", agyIDA, silentLogger())
	writeAgyConversationID(dir, "slack:C:1", agyIDB, silentLogger())
	ledgerOnly := "12927f4d-240b-4ec7-9639-c75a39e1b1f0"
	recordAgyConversation(dir, ledgerOnly, silentLogger())
	for _, conv := range []string{agyIDA, agyIDB, ledgerOnly} {
		seedAgyConversation(t, conv)
	}
	unrelated := seedAgyConversation(t, "c072a152-be1f-4afa-a095-2f598649c640")
	if _, err := clearAgySessionCounted(id); err != nil {
		t.Fatal(err)
	}
	for _, conv := range []string{agyIDA, agyIDB, ledgerOnly} {
		if _, err := os.Stat(filepath.Join(agyHome(), "conversations", conv+".db")); !os.IsNotExist(err) {
			t.Errorf("owned conversation survived reset: %s, %v", conv, err)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated agent's conversation removed: %v", err)
	}
	if got := readAgyConversationID(dir, ""); got != "" {
		t.Errorf("ref survived: %s", got)
	}
}

func TestAgyClearAbortsOnUnreadableRef(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	id := "ag_agybadref"
	dir := agentDir(id)
	writeAgyConversationID(dir, "slack:C:1", agyIDA, silentLogger())
	path := seedAgyConversation(t, agyIDA)
	// ReadFile fails deterministically even when tests run as root.
	if err := os.Mkdir(agyConversationRefPath(dir, ""), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := clearAgySessionCounted(id); err == nil {
		t.Fatal("unreadable ref accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("clear deleted data before validating refs")
	}
	if got := readAgyConversationID(dir, "slack:C:1"); got != agyIDA {
		t.Fatal("clear lost the only ref")
	}
}

func TestAgyManagerResetAndRewindClearNativeSessions(t *testing.T) {
	for _, action := range []string{"session", "data", "truncate", "rewind"} {
		t.Run(action, func(t *testing.T) {
			mgr := newSealTestManager(t)
			a, err := mgr.Create(AgentConfig{Name: "agyreset", Tool: ToolGeminiAgy})
			if err != nil {
				t.Fatal(err)
			}
			dir := agentDir(a.ID)
			writeAgyConversationID(dir, "", agyIDA, silentLogger())
			writeAgyConversationID(dir, "slack:C:1", agyIDB, silentLogger())
			path := seedAgyConversation(t, agyIDA)
			seedAgyConversation(t, agyIDB)
			switch action {
			case "session":
				err = mgr.ResetSession(a.ID)
			case "data":
				err = mgr.ResetData(a.ID)
			case "truncate":
				_, err = mgr.TruncateMemoryAt(a.ID, time.Now())
			case "rewind":
				msg := &Message{ID: "msg_agy_rewind", Role: "user", Content: "rewind me", Timestamp: time.Now().Format(time.RFC3339)}
				if e := appendMessage(a.ID, msg); e != nil {
					t.Fatal(e)
				}
				_, err = mgr.RewindToMessage(a.ID, msg.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("native conversation survived %s: %v", action, err)
			}
			if got := readAgyConversationID(dir, ""); got != "" {
				t.Errorf("main ref survived %s: %s", action, got)
			}
			if agyCanResumeSession(a.ID, "slack:C:1") {
				t.Errorf("thread can resume after %s", action)
			}
		})
	}
}

func TestAgyOneShotCleansDataOnSuccessAndFailure(t *testing.T) {
	for _, status := range []string{"SUCCESS", "ERROR"} {
		t.Run(status, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			bin := t.TempDir()
			path := filepath.Join(agyHome(), "conversations", agyIDA+".db")
			script := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nmkdir -p '%s'\nprintf fixture > '%s'\nprintf '%%s\\n' '{\"event\":\"init\",\"conversation_id\":\"%s\"}' '{\"event\":\"result\",\"result\":{\"conversation_id\":\"%s\",\"status\":\"%s\",\"error\":\"fixture error\"}}'\n", filepath.Dir(path), path, agyIDA, agyIDA, status)
			if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			a := &Agent{ID: "ag_agyoneshot"}
			ch, err := NewAgyBackend(silentLogger()).Chat(context.Background(), a, "hello", "system", ChatOptions{OneShot: true})
			if err != nil {
				t.Fatal(err)
			}
			for range ch {
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("OneShot DB survived %s: %v", status, err)
			}
			if got := readAgyConversationID(agentDir(a.ID), ""); got != "" {
				t.Errorf("OneShot persisted ref: %s", got)
			}
			if b, _ := os.ReadFile(agyKnownFile(agentDir(a.ID))); strings.Contains(string(b), agyIDA) {
				t.Error("successfully deleted OneShot was retained in ledger")
			}
		})
	}
}

func TestAgyReadsRejectSymlinkedRefsAndLedger(t *testing.T) {
	for _, which := range []string{"ref", "ledger"} {
		t.Run(which, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			id := "ag_agyreadlink"
			dir := agentDir(id)
			if err := os.MkdirAll(agyStateDir(dir), 0755); err != nil {
				t.Fatal(err)
			}
			foreignDB := seedAgyConversation(t, agyIDA)
			victim := filepath.Join(t.TempDir(), "foreign-ref")
			if err := os.WriteFile(victim, []byte(agyIDA), 0600); err != nil {
				t.Fatal(err)
			}
			link := agyConversationRefPath(dir, "")
			if which == "ledger" {
				link = agyKnownFile(dir)
			}
			if err := os.Symlink(victim, link); err != nil {
				t.Fatal(err)
			}
			if which == "ref" && readAgyConversationID(dir, "") != "" {
				t.Fatal("followed a symlink while reading a conversation ref")
			}
			if _, err := clearAgySessionCounted(id); err == nil {
				t.Fatal("reset accepted symlinked state")
			}
			if _, err := os.Stat(foreignDB); err != nil {
				t.Fatal("reset deleted the symlink target's conversation")
			}
		})
	}
}

func TestAgyResetDataSurfacesCleanupFailure(t *testing.T) {
	mgr := newSealTestManager(t)
	a, err := mgr.Create(AgentConfig{Name: "agybadreset", Tool: ToolGeminiAgy})
	if err != nil {
		t.Fatal(err)
	}
	dir := agentDir(a.ID)
	if err := os.MkdirAll(agyStateDir(dir), 0755); err != nil {
		t.Fatal(err)
	}
	// An unreadable state entry must not turn a failed reset into success.
	if err := os.Mkdir(agyConversationRefPath(dir, ""), 0755); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ResetData(a.ID); err == nil || !strings.Contains(err.Error(), "clear agy sessions") {
		t.Fatalf("cleanup failure swallowed: %v", err)
	}
}

func TestAgyClearRetainsLedgerWhenHomeUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	id := "ag_agynoHome"
	dir := agentDir(id)
	recordAgyConversation(dir, agyIDA, silentLogger())
	path := seedAgyConversation(t, agyIDA)
	home := os.Getenv("HOME")
	t.Setenv("HOME", "")
	if _, err := clearAgySessionCounted(id); err == nil {
		t.Fatal("unknown home treated as a successful deletion")
	}
	if b, err := os.ReadFile(agyKnownFile(dir)); err != nil || !strings.Contains(string(b), agyIDA) {
		t.Fatalf("failed deletion lost retry ledger: %q, %v", b, err)
	}
	t.Setenv("HOME", home)
	if _, err := clearAgySessionCounted(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("retry did not delete the conversation")
	}
}

func TestAgyPromptRulesRefreshAndPruneOnlyManagedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(agyRulesDir(dir), 0755); err != nil {
		t.Fatal(err)
	}
	userRule := filepath.Join(agyRulesDir(dir), "user.md")
	if err := os.WriteFile(userRule, []byte("user rule"), 0600); err != nil {
		t.Fatal(err)
	}
	// Shares the managed prefix but is not a numbered chunk.
	prefixedUserRule := filepath.Join(agyRulesDir(dir), "kojo-system-local.md")
	if err := os.WriteFile(prefixedUserRule, []byte("user rule"), 0600); err != nil {
		t.Fatal(err)
	}
	body := agyAgentDefBody("")
	defPath := agyAgentDefPath(dir, agyAgentName(body))
	hold := func(prompt string) func() {
		t.Helper()
		files := map[string]string{defPath: renderAgyAgentDef(agyAgentName(body), body)}
		for name, body := range renderAgyRules(prompt) {
			files[filepath.Join(agyRulesDir(dir), name)] = body
		}
		release, err := holdAgyWorkspace(context.Background(), dir, 0, files)
		if err != nil {
			t.Fatal(err)
		}
		return release
	}
	hold(strings.Repeat("old prompt\n", 5000))()
	firstAgent, _ := os.ReadFile(defPath)
	hold("new prompt")()
	secondAgent, _ := os.ReadFile(defPath)
	if string(firstAgent) != string(secondAgent) {
		t.Fatal("custom agent body should remain stable across resumed turns")
	}
	rules, _ := os.ReadFile(filepath.Join(agyRulesDir(dir), "kojo-system-01.md"))
	if !strings.Contains(string(rules), "new prompt") || strings.Contains(string(rules), "old prompt") {
		t.Fatalf("stale rules: %s", rules)
	}
	if _, err := os.Stat(filepath.Join(agyRulesDir(dir), "kojo-system-02.md")); !os.IsNotExist(err) {
		t.Fatal("stale prompt chunk survived")
	}
	hold("")()
	if _, err := os.Stat(filepath.Join(agyRulesDir(dir), "kojo-system-01.md")); !os.IsNotExist(err) {
		t.Fatal("empty prompt retained rules")
	}
	for _, p := range []string{userRule, prefixedUserRule} {
		if b, err := os.ReadFile(p); err != nil || string(b) != "user rule" {
			t.Fatalf("removed unrelated rule %s", p)
		}
	}
}

func TestParseAgyStreamParity(t *testing.T) {
	input := "error: model unavailable\n"
	res := parseAgyStream(strings.NewReader(input), silentLogger(), func(ChatEvent) bool { return true }, nil, nil)
	if got := classifyAgyError(res, nil, ""); got != "error: model unavailable" {
		t.Fatalf("fatal line lost: %s", got)
	}
	input = `{"status":"ERROR","error":"bare error"}` + "\n"
	res = parseAgyStream(strings.NewReader(input), silentLogger(), func(ChatEvent) bool { return true }, nil, nil)
	if got := classifyAgyError(res, nil, ""); got != "bare error" {
		t.Fatalf("bare error lost: %s", got)
	}
	input = `{"event":"step_update","step_update":{"step_index":1,"step_type":"agent_response","text_delta":"Checking"}}
{"event":"step_update","step_update":{"step_index":2,"step_type":"tool","state":"ERROR","tool_info":{"name":"run_command","error":{"message":"boom"}}}}
{"event":"step_update","step_update":{"step_index":3,"step_type":"agent_response","text_delta":"See [a.go](file:///x/a.go)"}}
{"event":"result","result":{"status":"SUCCESS"}}
`
	var events []ChatEvent
	res = parseAgyStream(strings.NewReader(input), silentLogger(), func(e ChatEvent) bool { events = append(events, e); return true }, nil, nil)
	if got := cleanAgyContent(res.text); got != "Checking\n\nSee a.go" {
		t.Fatalf("response formatting: %q", got)
	}
	if len(res.toolUses) != 1 || res.toolUses[0].Output != "boom" {
		t.Fatalf("tool ERROR was not completed: %+v", res.toolUses)
	}
	input = `{"event":"result","result":{"status":"SUCCESS","denied_actions":[{"action":"run_command"}]}}` + "\n"
	res = parseAgyStream(strings.NewReader(input), silentLogger(), func(ChatEvent) bool { return true }, nil, nil)
	if got := classifyAgyError(res, nil, ""); !strings.Contains(got, "denied tool permission: run_command") {
		t.Fatalf("denial lost: %q", got)
	}
}

func TestClassifyAgyErrorKeepsDeniedActionsOnFailedStatus(t *testing.T) {
	res := &agyStreamResult{gotResult: true, status: "ERROR", errorMessage: "boom", deniedActions: []string{"run_command"}}
	got := classifyAgyError(res, nil, "")
	if !strings.Contains(got, "boom") || !strings.Contains(got, "run_command") {
		t.Fatalf("denied actions lost: %q", got)
	}
}
