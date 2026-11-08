package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSplitAgySystemPrompt(t *testing.T) {
	cases := []struct {
		prompt, applied, shared, extra string
	}{
		{"BASE", "", "BASE", ""},
		{"BASE\n\nEXTRA", "EXTRA", "BASE", "EXTRA"},
		{"EXTRA", "EXTRA", "", "EXTRA"},
		// Not a suffix: keep everything shared rather than lose text.
		{"BASE\n\nOTHER", "EXTRA", "BASE\n\nOTHER", ""},
	}
	for _, c := range cases {
		shared, extra := splitAgySystemPrompt(c.prompt, c.applied)
		if shared != c.shared || extra != c.extra {
			t.Errorf("split(%q, %q) = %q, %q; want %q, %q", c.prompt, c.applied, shared, extra, c.shared, c.extra)
		}
	}
}

// Turns carrying different extras run side by side, and so do turns whose
// rules differ (the agent edited its memory in between): the newest rules
// win, stale chunks are pruned, and nothing waits on a running turn.
// Only a differing held file such as mcp_config.json is waited for.
func TestHoldAgyWorkspace_RulesAndExtras(t *testing.T) {
	dir := t.TempDir()
	rule := func(n int) string {
		return filepath.Join(agyRulesDir(dir), agyRulesFilePrefix+"0"+string(rune('0'+n))+".md")
	}
	def := func(extra string) (string, string) {
		body := agyAgentDefBody(extra)
		name := agyAgentName(body)
		return agyAgentDefPath(dir, name), renderAgyAgentDef(name, body)
	}
	mcp := agyMCPConfigPath(dir)
	defA, bodyA := def("thread A")
	defB, bodyB := def("thread B")
	relA, err := holdAgyWorkspace(context.Background(), dir, 0, map[string]string{mcp: "m", rule(1): "r1", rule(2): "r2", defA: bodyA}, defA)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	relB, err := holdAgyWorkspace(ctx, dir, 0, map[string]string{mcp: "m", rule(1): "r1", rule(2): "r2", defB: bodyB}, defB)
	if err != nil {
		t.Fatalf("same rules, other extra waited: %v", err)
	}
	relB()

	// Shorter, newer prompt while A still runs: written at once.
	relC, err := holdAgyWorkspace(ctx, dir, 0, map[string]string{mcp: "m", rule(1): "r1 new"})
	if err != nil {
		t.Fatalf("changed rules waited for a running turn: %v", err)
	}
	if b, _ := os.ReadFile(rule(1)); string(b) != "r1 new" {
		t.Errorf("newest rules not written: %q", b)
	}
	if _, err := os.Stat(rule(2)); !os.IsNotExist(err) {
		t.Errorf("stale rule chunk survived: %v", err)
	}
	// Releasing must not undo or remove the newer rules.
	relA()
	relC()
	if b, _ := os.ReadFile(rule(1)); string(b) != "r1 new" {
		t.Errorf("rules changed on release: %q", b)
	}
	if _, err := os.Stat(defA); !os.IsNotExist(err) {
		t.Errorf("released agent def survived: %v", err)
	}

	// A held mcp_config.json is still waited for.
	relD, err := holdAgyWorkspace(context.Background(), dir, 0, map[string]string{mcp: "m", rule(1): "r1"})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	var relE func()
	go func() {
		var err error
		relE, err = holdAgyWorkspace(context.Background(), dir, 0, map[string]string{mcp: "m2", rule(1): "r1 stale"})
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("a differing mcp_config.json did not wait")
	case <-time.After(50 * time.Millisecond):
	}
	// A newer turn writes its rules while the waiting one is blocked; the
	// waiter must not put its older rules back once it gets through.
	relF, err := holdAgyWorkspace(context.Background(), dir, 0, map[string]string{mcp: "m", rule(1): "r1 newest"})
	if err != nil {
		t.Fatal(err)
	}
	relD()
	relF()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(rule(1)); string(b) != "r1 newest" {
		t.Errorf("a turn that waited restored older rules: %q", b)
	}
	if b, _ := os.ReadFile(mcp); string(b) != "m2" {
		t.Errorf("waiter's mcp_config.json not written: %q", b)
	}
	relE()

	// Ordered by the generation the prompt was built under, not arrival.
	newer := nextAgyPromptGeneration() + 10
	relG, err := holdAgyWorkspace(context.Background(), dir, newer, map[string]string{mcp: "m2", rule(1): "built later"})
	if err != nil {
		t.Fatal(err)
	}
	relG()
	relH, err := holdAgyWorkspace(context.Background(), dir, newer-5, map[string]string{mcp: "m2", rule(1): "built earlier"})
	if err != nil {
		t.Fatal(err)
	}
	relH()
	if b, _ := os.ReadFile(rule(1)); string(b) != "built later" {
		t.Errorf("an earlier-built prompt arriving late overwrote a newer one: %q", b)
	}
}

// A keyed (thread / Slack) conversation gets its extra in the custom
// agent when it is created, and again in the message only when a resumed
// turn's extra differs from the one it last saw.
func TestAgyChat_ExtraPlacement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir()
	argvLog := filepath.Join(bin, "argv")
	stdinLog := filepath.Join(bin, "stdin")
	defLog := filepath.Join(bin, "agentdef")
	rulesLog := filepath.Join(bin, "rules")
	stream := filepath.Join(bin, "stream")
	if err := os.WriteFile(stream, []byte(agyTestStream), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvLog + "\ncat > " + stdinLog +
		"\ncat .agents/agents/*/agent.md > " + defLog + "\ncat .agents/rules/*.md > " + rulesLog +
		"\ncat " + stream + "\n"
	if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	b := NewAgyBackend(silentLogger())
	agent := &Agent{ID: "ag_agyextra"}
	const key = "ag_agyextra:slack:C1:1"
	run := func(extra string) {
		t.Helper()
		ch, err := b.Chat(context.Background(), agent, "hello", "SHARED\n\n"+extra, ChatOptions{SessionKey: key, SystemPromptExtraApplied: extra})
		if err != nil {
			t.Fatal(err)
		}
		for e := range ch {
			if e.Type == "error" {
				t.Fatalf("turn failed: %s", e.ErrorMessage)
			}
		}
	}
	read := func(p string) string {
		b, _ := os.ReadFile(p)
		return string(b)
	}

	run("posted by alice")
	if d := read(defLog); !strings.Contains(d, "posted by alice") || strings.Contains(d, "SHARED") {
		t.Errorf("first turn agent def = %q", d)
	}
	if r := read(rulesLog); !strings.Contains(r, "SHARED") || strings.Contains(r, "alice") {
		t.Errorf("first turn rules = %q", r)
	}
	if strings.Contains(read(stdinLog), "kojo-conversation-instructions") {
		t.Errorf("fresh conversation got the extra twice")
	}

	run("posted by alice")
	if !strings.Contains(read(argvLog), "--conversation\n"+agyIDA+"\n") {
		t.Fatalf("keyed turn did not resume: %q", read(argvLog))
	}
	if strings.Contains(read(stdinLog), "kojo-conversation-instructions") {
		t.Errorf("unchanged extra re-sent: %q", read(stdinLog))
	}

	run("posted by bob")
	if in := read(stdinLog); !strings.Contains(in, "kojo-conversation-instructions") || !strings.Contains(in, "posted by bob") {
		t.Errorf("changed extra not delivered to the resumed conversation: %q", in)
	}
	run("posted by bob")
	if strings.Contains(read(stdinLog), "kojo-conversation-instructions") {
		t.Errorf("delivered extra re-sent: %q", read(stdinLog))
	}

	run("")
	if in := read(stdinLog); !strings.Contains(in, "no longer apply") {
		t.Errorf("dropped extra not withdrawn: %q", in)
	}
	run("")
	if strings.Contains(read(stdinLog), "kojo-conversation-instructions") {
		t.Errorf("withdrawal re-sent: %q", read(stdinLog))
	}
}

func TestAgyResumeExtraChanged(t *testing.T) {
	for _, c := range []struct {
		stored, extra string
		want          bool
	}{
		{"", "", false},
		{"", "x", true},
		{agyExtraDigest(""), "", false},
		{agyExtraDigest("x"), "x", false},
		{agyExtraDigest("x"), "y", true},
		{agyExtraDigest("x"), "", true},
	} {
		if got := agyResumeExtraChanged(c.stored, c.extra); got != c.want {
			t.Errorf("changed(%q, %q) = %v, want %v", c.stored, c.extra, got, c.want)
		}
	}
}

// After a turn-boundary trim of the main conversation, truncate drops
// only the keyed conversations; the main one stays resumable and listed
// in the ledger for a later reset.
func TestClearAgyKeyedSessionsKeepsMain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	id := "ag_agykeep"
	dir := agentDir(id)
	writeAgyConversationID(dir, "", agyIDA, silentLogger())
	writeAgyConversationID(dir, "slack:C:1", agyIDB, silentLogger())
	writeAgyConversationExtra(dir, agyIDA, "", silentLogger())
	writeAgyConversationExtra(dir, agyIDB, "x", silentLogger())
	mainDB := seedAgyConversation(t, agyIDA)
	keyedDB := seedAgyConversation(t, agyIDB)

	if _, err := clearAgyKeyedSessionsCounted(id); err != nil {
		t.Fatal(err)
	}
	if got := readAgyConversationID(dir, ""); got != agyIDA {
		t.Errorf("main ref = %q, want kept", got)
	}
	if _, err := os.Stat(mainDB); err != nil {
		t.Errorf("main conversation removed: %v", err)
	}
	if readAgyConversationExtra(dir, agyIDA) == "" {
		t.Errorf("main extra record removed")
	}
	if agyCanResumeSession(id, "slack:C:1") {
		t.Errorf("keyed ref kept")
	}
	if _, err := os.Stat(keyedDB); !os.IsNotExist(err) {
		t.Errorf("keyed conversation kept: %v", err)
	}
	if readAgyConversationExtra(dir, agyIDB) != "" {
		t.Errorf("keyed extra record kept")
	}

	// The main conversation is still owned: a full reset removes it.
	if _, err := clearAgySessionCounted(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mainDB); !os.IsNotExist(err) {
		t.Errorf("main conversation survived reset: %v", err)
	}
	if _, err := os.Stat(agyExtrasDir(dir)); !os.IsNotExist(err) {
		t.Errorf("extra records survived reset: %v", err)
	}
}
