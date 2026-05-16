package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// withPeerCount swaps peerCountLookup for the duration of a test.
// Restores the original (typically nil) on cleanup.
func withPeerCount(t *testing.T, n int) {
	t.Helper()
	prev := peerCountLookup
	peerCountLookup = func() int { return n }
	t.Cleanup(func() { peerCountLookup = prev })
}

func skillPath(agentID string) string {
	return filepath.Join(agentDir(agentID), ".claude", "skills", deviceSwitchSkillDirName, "SKILL.md")
}

// TestSyncDeviceSwitchSkill_InstallsWhenEnabledWithPeers verifies the
// install path: enabled=true + peer count > 0 → SKILL.md written
// under the per-agent .claude/skills/ tree.
func TestSyncDeviceSwitchSkill_InstallsWhenEnabledWithPeers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	withPeerCount(t, 2)

	const agentID = "ag_devswitch_install"
	if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
		t.Fatalf("mkdir agentDir: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	SyncDeviceSwitchSkill(agentID, true, logger)

	body, err := os.ReadFile(skillPath(agentID))
	if err != nil {
		t.Fatalf("SKILL.md not written: %v", err)
	}
	// Frontmatter must declare the canonical name and pre-approve
	// curl so claude can drive the begin → complete chain without
	// per-call permission prompts.
	if !strings.Contains(string(body), "name: kojo-switch-device") {
		t.Errorf("SKILL.md missing name frontmatter; got:\n%s", body)
	}
	if !strings.Contains(string(body), "allowed-tools: Bash(curl:*)") {
		t.Errorf("SKILL.md missing allowed-tools frontmatter; got:\n%s", body)
	}
	if !strings.Contains(string(body), "/api/v1/peers") || !strings.Contains(string(body), "/handoff/switch") {
		t.Errorf("SKILL.md missing required API references; got:\n%s", body)
	}
}

// TestSyncDeviceSwitchSkill_RemovesWhenDisabled verifies the cleanup
// path when the toggle is flipped off: a pre-existing SKILL.md is
// removed on the next sync. The surrounding .claude/ tree must stay
// intact so settings.local.json and other skills survive.
func TestSyncDeviceSwitchSkill_RemovesWhenDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	withPeerCount(t, 1)

	const agentID = "ag_devswitch_remove"
	if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
		t.Fatalf("mkdir agentDir: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	// Install first.
	SyncDeviceSwitchSkill(agentID, true, logger)
	if _, err := os.Stat(skillPath(agentID)); err != nil {
		t.Fatalf("pre-condition: install failed: %v", err)
	}

	// Plant a sibling artefact to confirm cleanup is surgical —
	// only the kojo-switch-device subdir is wiped.
	siblingDir := filepath.Join(agentDir(agentID), ".claude", "skills", "some-other-skill")
	if err := os.MkdirAll(siblingDir, 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	siblingFile := filepath.Join(siblingDir, "SKILL.md")
	if err := os.WriteFile(siblingFile, []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}

	// Flip the toggle.
	SyncDeviceSwitchSkill(agentID, false, logger)

	if _, err := os.Stat(skillPath(agentID)); !os.IsNotExist(err) {
		t.Fatalf("SKILL.md not removed: err=%v", err)
	}
	if _, err := os.Stat(siblingFile); err != nil {
		t.Errorf("sibling skill collateral-damaged: %v", err)
	}
}

// TestSyncDeviceSwitchSkill_SuppressedOnSingleNode verifies that
// enabled=true + 0 peers leaves no SKILL.md. Single-node installs
// have no target, so exposing the skill would tease the agent into
// calling a handoff endpoint that would 4xx every time.
func TestSyncDeviceSwitchSkill_SuppressedOnSingleNode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	withPeerCount(t, 0)

	const agentID = "ag_devswitch_single"
	if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
		t.Fatalf("mkdir agentDir: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	SyncDeviceSwitchSkill(agentID, true, logger)

	if _, err := os.Stat(skillPath(agentID)); !os.IsNotExist(err) {
		t.Fatalf("SKILL.md unexpectedly installed on single-node setup: err=%v", err)
	}
}

// TestSyncDeviceSwitchSkill_NoLookupTreatedAsZeroPeers covers the
// boot-order edge case: SetPeerCountLookup hasn't been wired yet
// (e.g. peer identity load failed) → LookupPeerCount returns 0 →
// skill is suppressed. Fail-closed: never spam the skill into an
// install where we can't even count peers.
func TestSyncDeviceSwitchSkill_NoLookupTreatedAsZeroPeers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	prev := peerCountLookup
	peerCountLookup = nil
	t.Cleanup(func() { peerCountLookup = prev })

	const agentID = "ag_devswitch_nolookup"
	if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
		t.Fatalf("mkdir agentDir: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	SyncDeviceSwitchSkill(agentID, true, logger)

	if _, err := os.Stat(skillPath(agentID)); !os.IsNotExist(err) {
		t.Fatalf("SKILL.md installed without a peer-count lookup wired: err=%v", err)
	}
}

// TestDeviceSwitchSkillBody_FrontmatterShape sanity-checks the
// SKILL.md frontmatter — well-formed `---` delimiters, every key
// the official skills docs document for our usage is present, and
// description + when_to_use stay under claude's 1,536-char cap
// (https://code.claude.com/docs/en/skills.md). YAML structure is
// validated by line-by-line key presence; bringing in a YAML
// library just for this test is overkill.
func TestDeviceSwitchSkillBody_FrontmatterShape(t *testing.T) {
	body := deviceSwitchSkillBody
	if !strings.HasPrefix(body, "---\n") {
		t.Fatalf("body must start with --- delimiter; got first 16 bytes: %q", body[:16])
	}
	// Find the closing delimiter.
	rest := body[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("body missing closing --- delimiter")
	}
	frontmatter := rest[:end]

	wantKeys := []string{"name:", "description:", "argument-hint:", "allowed-tools:"}
	for _, k := range wantKeys {
		if !strings.Contains(frontmatter, k) {
			t.Errorf("frontmatter missing key %q", k)
		}
	}
	// Description length cap per claude docs (l.262-293):
	// description + when_to_use combined is truncated at 1,536
	// chars. We only emit description, so a clean cap on the
	// rendered value catches accidental over-budget edits.
	for _, line := range strings.Split(frontmatter, "\n") {
		if !strings.HasPrefix(line, "description:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(line, "description:"))
		if len(val) > 1536 {
			t.Errorf("description exceeds 1536-char claude cap: %d bytes", len(val))
		}
	}
}

// TestSyncDeviceSwitchSkill_ConcurrentSync exercises the per-agent
// mutex by hammering Sync from many goroutines with mixed enable/
// disable + peer-count states. No panic should fire (the lock map
// and atomicfile.WriteBytes together must keep the directory state
// consistent), and the eventual SKILL.md (if any) must be the full
// body — never a half-written truncation.
func TestSyncDeviceSwitchSkill_ConcurrentSync(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")

	// 1 peer for the full test run — concurrent flippers toggle
	// the enable flag, not the peer count.
	withPeerCount(t, 1)

	const agentID = "ag_devswitch_concurrent"
	if err := os.MkdirAll(agentDir(agentID), 0o755); err != nil {
		t.Fatalf("mkdir agentDir: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Reader goroutines run alongside the writers and assert
	// that every snapshot they observe is either ENOENT (cleanup
	// just won) or the FULL body (write just won) — never a
	// partial/truncated payload. atomicfile.WriteBytes is
	// tmp-rename, so a reader catching the file mid-call sees
	// either the prior inode or the new one but not a write in
	// progress. A regression to plain os.WriteFile would surface
	// here as a short read.
	stop := make(chan struct{})
	var readerErr error
	var readerMu sync.Mutex
	var readerWG sync.WaitGroup
	for r := 0; r < 4; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(skillPath(agentID))
				if err != nil {
					if !os.IsNotExist(err) {
						readerMu.Lock()
						if readerErr == nil {
							readerErr = err
						}
						readerMu.Unlock()
					}
					continue
				}
				if string(data) != deviceSwitchSkillBody {
					readerMu.Lock()
					if readerErr == nil {
						readerErr = errShortRead{got: len(data), want: len(deviceSwitchSkillBody)}
					}
					readerMu.Unlock()
				}
			}
		}()
	}

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			enabled := seed%2 == 0
			SyncDeviceSwitchSkill(agentID, enabled, logger)
		}(i)
	}
	wg.Wait()
	close(stop)
	readerWG.Wait()

	if readerErr != nil {
		t.Fatalf("concurrent reader saw partial SKILL.md: %v", readerErr)
	}

	// Final state: call once with enabled=true so the file
	// SHOULD exist, and verify it's the full body.
	SyncDeviceSwitchSkill(agentID, true, logger)
	got, err := os.ReadFile(skillPath(agentID))
	if err != nil {
		t.Fatalf("post-concurrent install missing: %v", err)
	}
	if string(got) != deviceSwitchSkillBody {
		t.Fatalf("post-concurrent SKILL.md content mismatch (truncated write?); got %d bytes want %d",
			len(got), len(deviceSwitchSkillBody))
	}
}

type errShortRead struct{ got, want int }

func (e errShortRead) Error() string {
	return fmt.Sprintf("short read: got=%d want=%d", e.got, e.want)
}

// TestIsDeviceSwitchEnabled_DefaultsToTrue locks in the documented
// default: a nil pointer means "use the default" which is true so
// agents predating the field still get the skill.
func TestIsDeviceSwitchEnabled_DefaultsToTrue(t *testing.T) {
	var nilAgent *Agent
	if !nilAgent.IsDeviceSwitchEnabled() {
		t.Errorf("nil receiver: want true, got false")
	}
	a := &Agent{DeviceSwitchEnabled: nil}
	if !a.IsDeviceSwitchEnabled() {
		t.Errorf("nil pointer: want true, got false")
	}
	tru, fls := true, false
	if !(&Agent{DeviceSwitchEnabled: &tru}).IsDeviceSwitchEnabled() {
		t.Errorf("explicit true: want true, got false")
	}
	if (&Agent{DeviceSwitchEnabled: &fls}).IsDeviceSwitchEnabled() {
		t.Errorf("explicit false: want false, got true")
	}
}
