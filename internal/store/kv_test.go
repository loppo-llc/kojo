package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func openKVStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), Options{ConfigDir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestKVPutGetRoundTripPlain(t *testing.T) {
	s := openKVStore(t)
	ctx := context.Background()

	rec, err := s.PutKV(ctx, &KVRecord{
		Namespace: "agent_settings", Key: "default_model",
		Value: "gpt-5", Type: KVTypeString, Scope: KVScopeGlobal,
	}, KVPutOptions{})
	if err != nil {
		t.Fatalf("PutKV: %v", err)
	}
	if rec.Version != 1 || rec.ETag == "" {
		t.Errorf("rec = %+v", rec)
	}
	got, err := s.GetKV(ctx, "agent_settings", "default_model")
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if got.Value != "gpt-5" || got.ValueEncrypted != nil {
		t.Errorf("got = %+v", got)
	}
}

func TestKVPutSecretRequiresEncryptedAndForbidsPlain(t *testing.T) {
	s := openKVStore(t)
	ctx := context.Background()

	if _, err := s.PutKV(ctx, &KVRecord{
		Namespace: "vapid", Key: "private", Value: "hidden",
		Type: KVTypeBinary, Scope: KVScopeMachine, Secret: true,
	}, KVPutOptions{}); err == nil {
		t.Error("Secret row with plaintext Value accepted")
	}
	if _, err := s.PutKV(ctx, &KVRecord{
		Namespace: "vapid", Key: "private",
		Type: KVTypeBinary, Scope: KVScopeMachine, Secret: true,
	}, KVPutOptions{}); err == nil {
		t.Error("Secret row with empty ValueEncrypted accepted")
	}
}

func TestKVPutNonSecretForbidsEncrypted(t *testing.T) {
	s := openKVStore(t)
	ctx := context.Background()

	if _, err := s.PutKV(ctx, &KVRecord{
		Namespace: "x", Key: "y", Value: "v", ValueEncrypted: []byte{1, 2},
		Type: KVTypeString, Scope: KVScopeGlobal,
	}, KVPutOptions{}); err == nil {
		t.Error("non-secret row with ValueEncrypted accepted")
	}
}

func TestKVPutSecretRoundTrip(t *testing.T) {
	s := openKVStore(t)
	ctx := context.Background()

	ciphertext := []byte{0xde, 0xad, 0xbe, 0xef}
	rec, err := s.PutKV(ctx, &KVRecord{
		Namespace: "vapid", Key: "private", ValueEncrypted: ciphertext,
		Type: KVTypeBinary, Scope: KVScopeMachine, Secret: true,
	}, KVPutOptions{})
	if err != nil {
		t.Fatalf("PutKV: %v", err)
	}
	got, err := s.GetKV(ctx, "vapid", "private")
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	if !got.Secret || got.Value != "" {
		t.Errorf("flags wrong: %+v", got)
	}
	if !bytes.Equal(got.ValueEncrypted, ciphertext) {
		t.Errorf("encrypted bytes mismatch: %x vs %x", got.ValueEncrypted, ciphertext)
	}
	if got.ETag != rec.ETag {
		t.Errorf("ETag mismatch")
	}
}

func TestKVPutIfMatch(t *testing.T) {
	s := openKVStore(t)
	ctx := context.Background()

	// IfMatch=* on missing row → succeeds.
	rec, err := s.PutKV(ctx, &KVRecord{
		Namespace: "n", Key: "k", Value: "v1",
		Type: KVTypeString, Scope: KVScopeGlobal,
	}, KVPutOptions{IfMatchETag: IfMatchAny})
	if err != nil {
		t.Fatalf("Put with *: %v", err)
	}
	// IfMatch=* on existing row → mismatch.
	if _, err := s.PutKV(ctx, &KVRecord{
		Namespace: "n", Key: "k", Value: "v2",
		Type: KVTypeString, Scope: KVScopeGlobal,
	}, KVPutOptions{IfMatchETag: IfMatchAny}); !errors.Is(err, ErrETagMismatch) {
		t.Errorf("err = %v, want ErrETagMismatch", err)
	}
	// IfMatch=<correct etag> → succeeds, version advances.
	rec2, err := s.PutKV(ctx, &KVRecord{
		Namespace: "n", Key: "k", Value: "v2",
		Type: KVTypeString, Scope: KVScopeGlobal,
	}, KVPutOptions{IfMatchETag: rec.ETag})
	if err != nil {
		t.Fatalf("conditional Put: %v", err)
	}
	if rec2.Version != rec.Version+1 {
		t.Errorf("Version = %d, want %d", rec2.Version, rec.Version+1)
	}
	// IfMatch=<wrong etag> → mismatch.
	if _, err := s.PutKV(ctx, &KVRecord{
		Namespace: "n", Key: "k", Value: "v3",
		Type: KVTypeString, Scope: KVScopeGlobal,
	}, KVPutOptions{IfMatchETag: "stale"}); !errors.Is(err, ErrETagMismatch) {
		t.Errorf("stale-etag err = %v, want ErrETagMismatch", err)
	}
}

func TestKVDeleteIdempotent(t *testing.T) {
	s := openKVStore(t)
	ctx := context.Background()

	if err := s.DeleteKV(ctx, "n", "missing", ""); err != nil {
		t.Errorf("Delete missing: %v", err)
	}
	if _, err := s.PutKV(ctx, &KVRecord{
		Namespace: "n", Key: "k", Value: "v", Type: KVTypeString, Scope: KVScopeGlobal,
	}, KVPutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.DeleteKV(ctx, "n", "k", ""); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if _, err := s.GetKV(ctx, "n", "k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-delete Get: %v", err)
	}
}

func TestKVListNamespace(t *testing.T) {
	s := openKVStore(t)
	ctx := context.Background()

	for _, k := range []string{"b", "a", "c"} {
		if _, err := s.PutKV(ctx, &KVRecord{
			Namespace: "ns", Key: k, Value: "x", Type: KVTypeString, Scope: KVScopeLocal,
		}, KVPutOptions{}); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	// Different namespace ignored.
	if _, err := s.PutKV(ctx, &KVRecord{
		Namespace: "other", Key: "z", Value: "y", Type: KVTypeString, Scope: KVScopeLocal,
	}, KVPutOptions{}); err != nil {
		t.Fatalf("Put other: %v", err)
	}
	got, err := s.ListKV(ctx, "ns")
	if err != nil {
		t.Fatalf("ListKV: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Key != "a" || got[1].Key != "b" || got[2].Key != "c" {
		t.Errorf("keys = %v, want a,b,c", []string{got[0].Key, got[1].Key, got[2].Key})
	}
}

func TestKVEmitsEvent(t *testing.T) {
	s := openKVStore(t)
	ctx := context.Background()

	if _, err := s.PutKV(ctx, &KVRecord{
		Namespace: "n", Key: "k", Value: "v", Type: KVTypeString, Scope: KVScopeGlobal,
	}, KVPutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	res, err := s.ListEventsSince(ctx, 0, ListEventsSinceOptions{Table: "kv"})
	if err != nil {
		t.Fatalf("ListEventsSince: %v", err)
	}
	if len(res.Events) != 1 || res.Events[0].Op != EventOpInsert || res.Events[0].ID != "n/k" {
		t.Errorf("events = %+v", res.Events)
	}

	if err := s.DeleteKV(ctx, "n", "k", ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	res, err = s.ListEventsSince(ctx, 0, ListEventsSinceOptions{Table: "kv"})
	if err != nil {
		t.Fatalf("ListEventsSince: %v", err)
	}
	if len(res.Events) != 2 || res.Events[1].Op != EventOpDelete {
		t.Errorf("after delete, events = %+v", res.Events)
	}
}
