package sandbox

import (
	"context"
	"testing"
)

func TestWrapCommandDisabled(t *testing.T) {
	cfg := Config{Enabled: false}
	cmd := WrapCommand(context.Background(), "/bin/echo", []string{"hello"}, cfg)
	// When Enabled=false, WrapCommand MUST passthrough to the original
	// binary on every platform. A regression that still re-execs through
	// "kojo sandbox" while disabled would silently change agent semantics
	// (extra fork, different argv exposure, etc.), so this is a hard
	// invariant — not a log statement.
	if cmd.Path != "/bin/echo" {
		t.Errorf("Enabled=false must passthrough: cmd.Path = %q, want %q", cmd.Path, "/bin/echo")
	}
	if len(cmd.Args) != 2 || cmd.Args[0] != "/bin/echo" || cmd.Args[1] != "hello" {
		t.Errorf("unexpected args: %v", cmd.Args)
	}
}

func TestParseSandboxArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantRW  []string
		wantCmd []string
		wantErr bool
	}{
		{
			name:    "basic",
			args:    []string{"--rw", "/tmp", "--rw", "/home/user/.cache", "--", "claude", "-p", "hello"},
			wantRW:  []string{"/tmp", "/home/user/.cache"},
			wantCmd: []string{"claude", "-p", "hello"},
		},
		{
			name:    "equals syntax",
			args:    []string{"--rw=/tmp", "--", "cmd"},
			wantRW:  []string{"/tmp"},
			wantCmd: []string{"cmd"},
		},
		{
			name:    "no rw paths",
			args:    []string{"--", "cmd", "arg1"},
			wantRW:  nil,
			wantCmd: []string{"cmd", "arg1"},
		},
		{
			name:    "missing separator",
			args:    []string{"--rw", "/tmp"},
			wantErr: true,
		},
		{
			name:    "missing rw value",
			args:    []string{"--rw"},
			wantErr: true,
		},
		{
			name:    "unknown flag",
			args:    []string{"--foo", "--", "cmd"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rw, cmd, err := parseSandboxArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(rw) != len(tt.wantRW) {
				t.Fatalf("rw paths: got %v, want %v", rw, tt.wantRW)
			}
			for i := range rw {
				if rw[i] != tt.wantRW[i] {
					t.Errorf("rw[%d]: got %q, want %q", i, rw[i], tt.wantRW[i])
				}
			}
			if len(cmd) != len(tt.wantCmd) {
				t.Fatalf("cmd args: got %v, want %v", cmd, tt.wantCmd)
			}
			for i := range cmd {
				if cmd[i] != tt.wantCmd[i] {
					t.Errorf("cmd[%d]: got %q, want %q", i, cmd[i], tt.wantCmd[i])
				}
			}
		})
	}
}
