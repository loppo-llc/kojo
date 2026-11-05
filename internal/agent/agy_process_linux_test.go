//go:build linux

package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/proctree"
)

func TestAgyStopsDetachedChildren(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid unavailable")
	}
	for _, mode := range []string{"cancel", "result-grace", "bare-result"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			bin := t.TempDir()
			pidFile := filepath.Join(bin, "child.pid")
			data := filepath.Join(agyHome(), "conversations", agyIDA+".db")
			script := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nmkdir -p '%s'\nprintf fixture > '%s'\nsetsid sleep 60 &\necho $! > '%s'\nprintf '%%s\\n' '{\"event\":\"init\",\"conversation_id\":\"%s\"}'\n", filepath.Dir(data), data, pidFile, agyIDA)
			if mode != "cancel" {
				if mode == "bare-result" {
					script += `printf '%s\n' '{"status":"SUCCESS"}'` + "\n"
				} else {
					script += `printf '%s\n' '{"event":"result","result":{"status":"SUCCESS"}}'` + "\n"
				}
				previous := agyExitGrace
				agyExitGrace = 100 * time.Millisecond
				t.Cleanup(func() { agyExitGrace = previous })
			}
			script += "wait\n"
			if err := os.WriteFile(filepath.Join(bin, "agy"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			ch, err := NewAgyBackend(silentLogger()).Chat(ctx, &Agent{ID: "ag_agychild"}, "hello", "system", ChatOptions{OneShot: true})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			var childPID int
			for time.Now().Before(deadline) {
				if b, e := os.ReadFile(pidFile); e == nil {
					childPID, _ = strconv.Atoi(strings.TrimSpace(string(b)))
					if childPID > 0 {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			if childPID <= 0 {
				t.Fatal("fake agy failed to spawn child")
			}
			childIdent := proctree.Identify(childPID)
			t.Cleanup(func() { proctree.KillTreeIdent(childPID, childIdent, 100*time.Millisecond) })
			if mode == "cancel" {
				cancel()
			}
			done := make(chan ChatEvent, 1)
			go func() {
				var last ChatEvent
				for e := range ch {
					last = e
				}
				done <- last
			}()
			select {
			case last := <-done:
				if last.Type != "done" || (mode != "cancel" && last.ErrorMessage != "") {
					t.Errorf("unexpected final event: %+v", last)
				}
			case <-time.After(12 * time.Second):
				t.Fatal("agy shutdown hung")
			}
			// kill(0) also succeeds on zombies. A killed, unreaped child cannot
			// do more work, so both absence and zombie state count as stopped.
			if b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", childPID)); e == nil {
				tail := string(b)[strings.LastIndex(string(b), ")")+2:]
				if !strings.HasPrefix(tail, "Z ") {
					t.Fatalf("detached child still running: %s", b)
				}
			}
			if _, e := os.Stat(data); !os.IsNotExist(e) {
				t.Errorf("cancelled/terminated OneShot left native DB: %v", e)
			}
		})
	}
}
