package blob

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loppo-llc/kojo/internal/store"
)

// scrubFixture builds a blob store + a scrubber rooted at the same
// configdir. The store handle is closed on cleanup; the scrubber's
// background goroutine is NOT started (tests drive ScrubOnce
// directly so the timing is deterministic).
func scrubFixture(t *testing.T) (*Store, *store.Store, *Scrubber) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{ConfigDir: root})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	bs := New(root,
		WithRefs(NewStoreRefs(st, "peer-test")),
		WithHomePeer("peer-test"),
	)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sc := NewScrubber(st, bs, logger, ScrubberOpts{})
	return bs, st, sc
}

// TestScrubber_HappyPathTouchesLastSeenOK pins the "all rows verify"
// path: a freshly-written blob is hashed, matches its row's
// sha256, and the row's last_seen_ok column is stamped.
func TestScrubber_HappyPathTouchesLastSeenOK(t *testing.T) {
	bs, st, sc := scrubFixture(t)
	body := "happy body"
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader(body), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	res := sc.ScrubOnce(context.Background())
	if res.Total != 1 || res.OK != 1 || res.Mismatch != 0 {
		t.Errorf("ScrubResult mismatch: %+v", res)
	}
	ref, err := st.GetBlobRef(context.Background(), "kojo://global/agents/ag_1/avatar.png")
	if err != nil {
		t.Fatalf("GetBlobRef: %v", err)
	}
	if ref.LastSeenOK == 0 {
		t.Errorf("last_seen_ok not stamped after a successful scrub")
	}
}

// TestScrubber_MissingFileSurfacesAsMissingCount covers the
// "row alive, body gone" path: the scrubber must NOT auto-delete
// the row (operator-recoverable in v1); it just counts Missing.
func TestScrubber_MissingFileSurfacesAsMissingCount(t *testing.T) {
	bs, st, sc := scrubFixture(t)
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader("body"), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Delete the file out from under the row (bypass the store
	// API so the blob_refs row survives).
	onDisk := filepath.Join(bs.BaseDir(), "global", "agents", "ag_1", "avatar.png")
	if err := os.Remove(onDisk); err != nil {
		t.Fatalf("rm file: %v", err)
	}
	res := sc.ScrubOnce(context.Background())
	if res.Total != 1 || res.Missing != 1 {
		t.Errorf("expected one Missing: %+v", res)
	}
	// Row must still be present — auto-delete would lose the
	// repair lead.
	if _, err := st.GetBlobRef(context.Background(), "kojo://global/agents/ag_1/avatar.png"); err != nil {
		t.Errorf("row deleted despite Missing posture: %v", err)
	}
}

// TestScrubber_HashMismatchQuarantinesFile pins the
// CORRECTNESS-CRITICAL path: an on-disk body whose sha256 doesn't
// match the row gets renamed under a `.quarantine/` sibling so a
// future Read can't hand out a body that disagrees with its etag.
func TestScrubber_HashMismatchQuarantinesFile(t *testing.T) {
	bs, st, sc := scrubFixture(t)
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader("original"), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	onDisk := filepath.Join(bs.BaseDir(), "global", "agents", "ag_1", "avatar.png")
	// Tamper: overwrite the body so its sha256 no longer matches
	// the row. This simulates silent bitrot.
	if err := os.WriteFile(onDisk, []byte("tampered"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	res := sc.ScrubOnce(context.Background())
	if res.Total != 1 || res.Mismatch != 1 {
		t.Errorf("expected one Mismatch: %+v", res)
	}
	// Canonical path must be gone (rename), and a quarantine
	// sibling must exist outside the scope subtree so Get/List
	// can't serve it.
	if _, err := os.Stat(onDisk); !os.IsNotExist(err) {
		t.Errorf("tampered file still at canonical path: err=%v", err)
	}
	quarRoot := filepath.Join(bs.BaseDir(), ".quarantine")
	entries, _ := os.ReadDir(quarRoot)
	if len(entries) == 0 {
		t.Fatalf(".quarantine dir empty after mismatch")
	}
	// Row should still exist — operator inspects the quarantine
	// and decides whether to auto-repair or delete.
	row, err := st.GetBlobRef(context.Background(), "kojo://global/agents/ag_1/avatar.png")
	if err != nil {
		t.Fatalf("row vanished: %v", err)
	}
	// LastSeenOK must NOT advance on a mismatch — the row is no
	// longer "seen healthy".
	if row.LastSeenOK != 0 {
		t.Errorf("last_seen_ok advanced despite mismatch: %d", row.LastSeenOK)
	}
}

// TestScrubber_ConcurrentPutRaceDoesNotQuarantineFreshBody covers
// the race-safety guarantee: while the scrubber is hashing a body,
// a concurrent Put can replace the file + bump blob_refs.sha256.
// The scrubber must re-check the row's sha256 after its hash and
// skip quarantine when the row has advanced.
//
// Simulated by calling scrubOne directly with a stale BlobRefRecord
// (sha256 = OLD value) while the actual blob_refs row already
// reflects the new value. The on-disk body is rewritten to match
// the new row. This is exactly the pattern that production fires:
// scrubOne captures rec at list time, then a concurrent Put writes
// a new body AND advances the row, and scrubOne's post-hash
// re-read should detect the advance.
func TestScrubber_ConcurrentPutRaceDoesNotQuarantineFreshBody(t *testing.T) {
	bs, st, sc := scrubFixture(t)
	uri := "kojo://global/agents/ag_1/avatar.png"
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader("first"), PutOptions{}); err != nil {
		t.Fatalf("Put first: %v", err)
	}
	// Capture the row as the scrubber's list-time snapshot would
	// have seen it — sha256 of "first".
	rowSnapshot, err := st.GetBlobRef(context.Background(), uri)
	if err != nil {
		t.Fatalf("GetBlobRef: %v", err)
	}
	// Now simulate a concurrent Put: rewrite the on-disk body to
	// "second" AND advance the live blob_refs row's sha256 to
	// match. The scrubber's snapshot still claims "first".
	if _, err := bs.Put(ScopeGlobal, "agents/ag_1/avatar.png",
		strings.NewReader("second"), PutOptions{}); err != nil {
		t.Fatalf("Put second: %v", err)
	}
	// Sanity: the live row now has the new sha256.
	live, _ := st.GetBlobRef(context.Background(), uri)
	if live.SHA256 == rowSnapshot.SHA256 {
		t.Fatal("row sha256 did not advance after second Put")
	}
	// Drive scrubOne with the STALE rec — what ListBlobRefs would
	// have produced before the concurrent Put landed. The mismatch
	// branch fires, sees the row has advanced under it, and skips
	// quarantine.
	outcome := sc.scrubOne(context.Background(), rowSnapshot)
	if outcome != scrubOK {
		t.Errorf("expected scrubOK (race-skipped), got %v", outcome)
	}
	// Canonical path must NOT have been quarantined — the file
	// holds the legitimate "second" body.
	onDisk := filepath.Join(bs.BaseDir(), "global", "agents", "ag_1", "avatar.png")
	body, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("canonical path missing after race-skip: %v", err)
	}
	if string(body) != "second" {
		t.Errorf("canonical body lost: %q", body)
	}
}

// TestScrubber_EmptyStoreCleanlyExits exercises the "no rows" path
// — a freshly-installed Hub has an empty blob_refs table and the
// scrubber must report an all-zero ScrubResult without error.
func TestScrubber_EmptyStoreCleanlyExits(t *testing.T) {
	_, _, sc := scrubFixture(t)
	res := sc.ScrubOnce(context.Background())
	if res.Total != 0 || res.OK != 0 || res.Missing != 0 || res.Mismatch != 0 || res.Errors != 0 {
		t.Errorf("non-zero result on empty store: %+v", res)
	}
}
