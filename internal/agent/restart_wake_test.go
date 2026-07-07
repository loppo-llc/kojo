package agent

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/loppo-llc/kojo/internal/store"
)

// TestRestartWake_ArmAndConsume: the marker round-trips through kv and
// is consumed at-most-once — a second ConsumeRestartWake is a no-op.
func TestRestartWake_ArmAndConsume(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if err := m.ArmRestartWake("ag_wake_missing", ""); err != nil {
		t.Fatalf("arm: %v", err)
	}
	rec, err := m.Store().GetKV(context.Background(), "system", "restart_wake")
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if rec.Value != "ag_wake_missing" {
		t.Fatalf("marker = %q", rec.Value)
	}

	// Consume: the agent doesn't exist, so the chat fails (logged), but
	// the marker MUST be cleared regardless — at-most-once.
	m.ConsumeRestartWake("vtest", time.Now().Add(time.Minute))
	if _, err := m.Store().GetKV(context.Background(), "system", "restart_wake"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("marker not cleared after consume: %v", err)
	}

	// Second consume with no marker: no panic, still nothing.
	m.ConsumeRestartWake("vtest", time.Now().Add(time.Minute))
}

// TestRestartWake_BootFence: a marker written AFTER the consumer's boot
// time belongs to the next process and must be left in place.
func TestRestartWake_BootFence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if err := m.ArmRestartWake("ag_future", ""); err != nil {
		t.Fatalf("arm: %v", err)
	}
	// Boot time in the past → marker is newer → must survive.
	m.ConsumeRestartWake("vtest", time.Now().Add(-time.Minute))
	rec, err := m.Store().GetKV(context.Background(), "system", "restart_wake")
	if err != nil {
		t.Fatalf("marker was consumed despite being newer than boot: %v", err)
	}
	if rec.Value != "ag_future" {
		t.Fatalf("marker = %q", rec.Value)
	}
}

// TestRestartWake_MarkerEncoding: the marker value stays byte-identical
// to the legacy bare-agentID form when no thread is targeted, and becomes
// a JSON object (decoding back to both fields) when a thread is.
func TestRestartWake_MarkerEncoding(t *testing.T) {
	// No sessionKey → bare agentID, backward-compatible with old readers.
	if got := encodeRestartWake("ag_1", ""); got != "ag_1" {
		t.Fatalf("encode(no thread) = %q, want %q", got, "ag_1")
	}
	id, sk := decodeRestartWake("ag_1")
	if id != "ag_1" || sk != "" {
		t.Fatalf("decode(bare) = (%q,%q), want (ag_1,\"\")", id, sk)
	}

	// With sessionKey → JSON, round-trips both fields.
	enc := encodeRestartWake("ag_2", "groupdm:g_9")
	if enc == "ag_2" {
		t.Fatalf("encode(thread) did not include the session key: %q", enc)
	}
	id, sk = decodeRestartWake(enc)
	if id != "ag_2" || sk != "groupdm:g_9" {
		t.Fatalf("decode(json) = (%q,%q), want (ag_2,groupdm:g_9)", id, sk)
	}

	// A malformed JSON marker degrades to a main-conversation wake on the
	// whole raw value rather than panicking or dropping the wake.
	id, sk = decodeRestartWake("{not json")
	if id != "{not json" || sk != "" {
		t.Fatalf("decode(malformed) = (%q,%q)", id, sk)
	}
}

// TestRestartWake_ArmThreadMarker: arming with a session key persists the
// JSON marker, and ConsumeRestartWake still clears it at-most-once even
// when the target thread/agent is gone (delivery falls back to the main
// conversation, which also fails for a missing agent — logged, not fatal).
func TestRestartWake_ArmThreadMarker(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if err := m.ArmRestartWake("ag_thread_missing", "groupdm:g_gone"); err != nil {
		t.Fatalf("arm: %v", err)
	}
	rec, err := m.Store().GetKV(context.Background(), "system", "restart_wake")
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	id, sk := decodeRestartWake(rec.Value)
	if id != "ag_thread_missing" || sk != "groupdm:g_gone" {
		t.Fatalf("marker decoded to (%q,%q)", id, sk)
	}

	m.ConsumeRestartWake("vtest", time.Now().Add(time.Minute))
	if _, err := m.Store().GetKV(context.Background(), "system", "restart_wake"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("marker not cleared after consume: %v", err)
	}
}

// TestInFlightOneShotSessionKey: the reverse lookup returns the running
// thread turn's key, "" when none/ambiguous.
func TestInFlightOneShotSessionKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if got := m.InFlightOneShotSessionKey("ag_x"); got != "" {
		t.Fatalf("no turn: got %q", got)
	}

	// One thread turn in flight → its key.
	id1 := m.trackOneShot("ag_x", func() {}, "groupdm:g_1")
	if got := m.InFlightOneShotSessionKey("ag_x"); got != "groupdm:g_1" {
		t.Fatalf("single thread: got %q", got)
	}

	// A concurrent keyless one-shot must not shadow the thread key.
	id2 := m.trackOneShot("ag_x", func() {}, "")
	if got := m.InFlightOneShotSessionKey("ag_x"); got != "groupdm:g_1" {
		t.Fatalf("thread + keyless: got %q", got)
	}

	// Two distinct thread keys → ambiguous → "".
	id3 := m.trackOneShot("ag_x", func() {}, "groupdm:g_2")
	if got := m.InFlightOneShotSessionKey("ag_x"); got != "" {
		t.Fatalf("ambiguous: got %q", got)
	}

	m.untrackOneShot("ag_x", id1)
	m.untrackOneShot("ag_x", id2)
	m.untrackOneShot("ag_x", id3)
	if got := m.InFlightOneShotSessionKey("ag_x"); got != "" {
		t.Fatalf("after untrack: got %q", got)
	}
}

// TestWakeChat_Rejections mirrors Checkin's entry contract.
func TestWakeChat_Rejections(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, err := NewManager(slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if err := m.WakeChat("ag_nope", "hi"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("missing agent: err = %v, want ErrAgentNotFound", err)
	}
}
