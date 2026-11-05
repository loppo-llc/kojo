//go:build !windows

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// installCaptureHookForTest writes the hook + runner into a temp claude dir
// with captures under a temp base, and returns a function turning a Bash
// command into the wrapper the hook would hand back to claude.
func installCaptureHookForTest(t *testing.T, agentID string) (wrap func(cmd string) string, claudeDir string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available")
	}
	capBase := t.TempDir()
	origBase := bashCaptureBaseDir
	bashCaptureBaseDir = func() string { return capBase }
	t.Cleanup(func() { bashCaptureBaseDir = origBase })

	claudeDir = filepath.Join(t.TempDir(), ".claude")
	hookPath, err := writeBashCaptureHook(claudeDir, bashCaptureDir(agentID))
	if err != nil {
		t.Fatal(err)
	}
	wrap = func(cmd string) string {
		t.Helper()
		in, _ := json.Marshal(map[string]any{
			"tool_name":  "Bash",
			"tool_input": map[string]any{"command": cmd, "description": "d", "timeout": 5000},
		})
		c := exec.Command(hookPath)
		c.Stdin = bytes.NewReader(in)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("hook failed: %v", err)
		}
		var res struct {
			HookSpecificOutput struct {
				UpdatedInput map[string]any `json:"updatedInput"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("hook output not JSON: %v\n%s", err, out)
		}
		ui := res.HookSpecificOutput.UpdatedInput
		if ui["description"] != "d" || ui["timeout"] != float64(5000) {
			t.Errorf("other tool_input fields not preserved: %v", ui)
		}
		w, _ := ui["command"].(string)
		if w == "" {
			t.Fatalf("hook returned no wrapper command: %s", out)
		}
		return w
	}
	return wrap, claudeDir
}

func runCaptureWrapper(t *testing.T, wrapper string, timeout time.Duration) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "bash", "-c", wrapper)
	out, err := c.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("wrapper did not finish within %v; output head: %.200q", timeout, out)
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func TestBashCapture_WrapperBasics(t *testing.T) {
	if hostOS == "windows" {
		t.Skip("POSIX only")
	}
	wrap, _ := installCaptureHookForTest(t, "ag_capture_basics")

	out, code := runCaptureWrapper(t, wrap("echo out; echo err >&2; exit 3"), 10*time.Second)
	if code != 3 || !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Errorf("got code=%d out=%q, want code 3 with both streams", code, out)
	}

	// A background child holding the capture log open must not keep the
	// wrapper waiting (the reason the size limit is a watchdog, not a pipe).
	start := time.Now()
	out, code = runCaptureWrapper(t, wrap("sleep 5 & echo started"), 10*time.Second)
	if code != 0 || !strings.Contains(out, "started") {
		t.Errorf("got code=%d out=%q", code, out)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("wrapper waited %v for a background child", el)
	}

	out, _ = runCaptureWrapper(t, wrap("seq 1 100000"), 10*time.Second)
	if !strings.Contains(out, "KOJO_TRUNCATED") {
		t.Errorf("large output not truncated: %.200q", out)
	}
}

// Regression for the 644GB capture log: an agent recursively grepping its
// own directory for a string present in its files, piped into a second
// grep. With the capture log inside the agent dir this never terminated.
func TestBashCapture_SelfSearchOfAgentDirTerminates(t *testing.T) {
	if hostOS == "windows" {
		t.Skip("POSIX only")
	}
	if _, err := exec.LookPath("grep"); err != nil {
		t.Skip("grep not available")
	}
	wrap, claudeDir := installCaptureHookForTest(t, "ag_capture_selfgrep")
	agentRoot := filepath.Dir(claudeDir)
	if err := os.MkdirAll(filepath.Join(agentRoot, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	var notes strings.Builder
	for i := 0; i < 3000; i++ {
		notes.WriteString("U0SELFGREP line\n")
	}
	if err := os.WriteFile(filepath.Join(agentRoot, "memory", "notes.md"), []byte(notes.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := `grep -rn "U0SELFGREP" ` + agentRoot + `/ 2>/dev/null | grep -v "\.git"`
	out, code := runCaptureWrapper(t, wrap(cmd), 30*time.Second)
	if code != 0 {
		t.Errorf("exit code %d, out head %.200q", code, out)
	}
	logs, _ := filepath.Glob(filepath.Join(bashCaptureDir("ag_capture_selfgrep"), "out-*.log"))
	for _, l := range logs {
		if st, err := os.Stat(l); err == nil && st.Size() > 1<<20 {
			t.Errorf("capture log %s grew to %d bytes", l, st.Size())
		}
	}
}

// writeRunnerForTest writes the runner and a command script into a temp
// dir and returns (runner, script, log) paths.
func writeRunnerForTest(t *testing.T, cmd string) (run, script, log string) {
	t.Helper()
	if hostOS == "windows" {
		t.Skip("POSIX only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	run = filepath.Join(dir, "run.sh")
	if err := os.WriteFile(run, []byte(bashCaptureRunScript), 0o755); err != nil {
		t.Fatal(err)
	}
	script = filepath.Join(dir, "cmd")
	if err := os.WriteFile(script, []byte(cmd+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return run, script, filepath.Join(dir, "out.log")
}

// waitLogStable waits until log stops growing, failing after timeout.
func waitLogStable(t *testing.T, log string, timeout time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var prev int64 = -1
	for time.Now().Before(deadline) {
		st, err := os.Stat(log)
		if err == nil && st.Size() == prev {
			return prev
		}
		if err == nil {
			prev = st.Size()
		}
		time.Sleep(700 * time.Millisecond)
	}
	t.Fatalf("log %s still growing after %v (size %d)", log, timeout, prev)
	return 0
}

func TestBashCaptureRunner_KillsRunawayOutput(t *testing.T) {
	// Pipeline so the kill has to reach more than the direct child.
	run, script, log := writeRunnerForTest(t, "yes 0123456789abcdef | cat | cat")
	const limit = 1 << 20

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "bash", run, script, log, "1048576")
	stderr, err := c.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("runner did not stop the runaway command")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Errorf("want non-zero exit for killed command, got %v", err)
	}
	if len(stderr) != 0 {
		t.Errorf("runner wrote to its own output: %q", stderr)
	}
	st, err := os.Stat(log)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() <= limit {
		t.Errorf("log size %d not past limit; watchdog fired without cause?", st.Size())
	}
	f, err := os.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tail := make([]byte, 256)
	if _, err := f.ReadAt(tail, st.Size()-int64(len(tail))); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(tail, []byte("KOJO_CAPTURE_LIMIT")) {
		t.Errorf("limit marker missing from log tail: %q", tail)
	}
	// Size must stop growing: every process in the pipeline is dead.
	time.Sleep(300 * time.Millisecond)
	if st2, _ := os.Stat(log); st2.Size() != st.Size() {
		t.Errorf("log still growing after kill: %d -> %d", st.Size(), st2.Size())
	}
}

func TestPruneLegacyBashCaptures(t *testing.T) {
	claudeDir := t.TempDir()
	legacy := filepath.Join(claudeDir, "captures")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(legacy, "out-old.log")
	fresh := filepath.Join(legacy, "out-fresh.log")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-legacyBashCaptureTTL - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	pruneLegacyBashCaptures(claudeDir)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("expired legacy capture not removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh legacy capture (possibly in-flight) removed: %v", err)
	}

	if err := os.Chtimes(fresh, past, past); err != nil {
		t.Fatal(err)
	}
	pruneLegacyBashCaptures(claudeDir)
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("empty legacy dir not removed: %v", err)
	}
}

// A runaway left behind as a background child must still be capped after
// the runner itself has returned.
func TestBashCaptureRunner_KillsRunawayBackgroundChild(t *testing.T) {
	run, script, log := writeRunnerForTest(t, "yes 0123456789abcdef > /dev/stdout & echo started")
	start := time.Now()
	out, err := exec.Command("bash", run, script, log, "1048576").CombinedOutput()
	if err != nil {
		t.Fatalf("runner: %v %q", err, out)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("runner waited %v for a background child", el)
	}
	size := waitLogStable(t, log, 20*time.Second)
	if size > 1<<30 {
		t.Errorf("background runaway not capped: %d bytes", size)
	}
	b, _ := os.ReadFile(log)
	if !bytes.Contains(b[len(b)-256:], []byte("KOJO_CAPTURE_LIMIT")) {
		t.Errorf("limit marker missing")
	}
}

// An interrupt (SIGTERM to the runner, as Claude Code sends) must take the
// command down with it even though it runs in its own process group.
func TestBashCaptureRunner_TermKillsCommand(t *testing.T) {
	run, script, log := writeRunnerForTest(t, "while :; do echo tick; sleep 0.1; done | cat")
	c := exec.Command("bash", run, script, log, "1048576")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	_ = c.Process.Signal(syscall.SIGTERM)
	err := c.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 143 {
		t.Errorf("want exit 143, got %v", err)
	}
	waitLogStable(t, log, 5*time.Second)
}

// Reset/delete (or the hook's TTL prune) can unlink the log while a
// leftover child still writes to it; the limit must still apply.
func TestBashCaptureRunner_CapsUnlinkedLog(t *testing.T) {
	if _, err := os.Stat("/dev/fd/0"); err != nil {
		t.Skip("/dev/fd not available")
	}
	pidFile := filepath.Join(t.TempDir(), "writer.pid")
	run, script, log := writeRunnerForTest(t,
		"(sleep 1; echo $BASHPID > "+pidFile+"; exec yes 0123456789abcdef) & echo started")
	if out, err := exec.Command("bash", run, script, log, "1048576").CombinedOutput(); err != nil {
		t.Fatalf("runner: %v %q", err, out)
	}
	// Unlink before the writer starts producing (it sleeps 1s first).
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(pidFile); err == nil {
			if _, err := fmt.Sscan(string(b), &pid); err == nil && pid > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("writer never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	deadline = time.Now().Add(20 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("writer to unlinked capture log was never killed")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
