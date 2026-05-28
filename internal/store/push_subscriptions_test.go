package store

import (
	"context"
	"errors"
	"testing"
)

func TestBulkInsertPushSubscriptions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	dev := "dev-1"
	ua := "Mozilla/5.0"
	recs := []*PushSubscriptionRecord{
		{
			Endpoint:       "https://push.example.com/a",
			DeviceID:       &dev,
			UserAgent:      &ua,
			VAPIDPublicKey: "pub-1",
			P256dh:         "p1",
			Auth:           "a1",
		},
		{
			Endpoint:       "https://push.example.com/b",
			VAPIDPublicKey: "pub-1",
			P256dh:         "p2",
			Auth:           "a2",
		},
	}
	n, err := s.BulkInsertPushSubscriptions(ctx, recs, PushSubscriptionInsertOptions{})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 2 {
		t.Errorf("inserted = %d, want 2", n)
	}
	for _, r := range recs {
		if r.CreatedAt == 0 {
			t.Errorf("created_at not stamped: %+v", r)
		}
		if r.UpdatedAt == 0 {
			t.Errorf("updated_at not stamped: %+v", r)
		}
	}

	// Round-trip: nullable fields restore as expected.
	got, err := s.GetPushSubscription(ctx, "https://push.example.com/a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DeviceID == nil || *got.DeviceID != "dev-1" {
		t.Errorf("device_id = %v, want &dev-1", got.DeviceID)
	}
	if got.UserAgent == nil || *got.UserAgent != "Mozilla/5.0" {
		t.Errorf("user_agent = %v, want &Mozilla/5.0", got.UserAgent)
	}
	if got.VAPIDPublicKey != "pub-1" {
		t.Errorf("vapid_public_key = %q, want pub-1", got.VAPIDPublicKey)
	}
	if got.ExpiredAt != nil {
		t.Errorf("expired_at should be nil for fresh row, got %v", got.ExpiredAt)
	}

	// Detached row: NULL device_id / user_agent.
	got2, err := s.GetPushSubscription(ctx, "https://push.example.com/b")
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	if got2.DeviceID != nil || got2.UserAgent != nil {
		t.Errorf("nullable fields not nil: %+v", got2)
	}

	// ListActive returns both (ordered by created_at).
	active, err := s.ListActivePushSubscriptions(ctx)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("active = %d, want 2", len(active))
	}

	// Re-running silently skips (preload-hit).
	n2, err := s.BulkInsertPushSubscriptions(ctx, recs, PushSubscriptionInsertOptions{})
	if err != nil {
		t.Fatalf("bulk re-run: %v", err)
	}
	if n2 != 0 {
		t.Errorf("idempotent re-run inserted = %d, want 0", n2)
	}

	// Mixed: one new, one duplicate.
	mix := []*PushSubscriptionRecord{
		{Endpoint: "https://push.example.com/a", VAPIDPublicKey: "pub-1", P256dh: "different", Auth: "different"},
		{Endpoint: "https://push.example.com/c", VAPIDPublicKey: "pub-1", P256dh: "p3", Auth: "a3"},
	}
	n3, err := s.BulkInsertPushSubscriptions(ctx, mix, PushSubscriptionInsertOptions{})
	if err != nil {
		t.Fatalf("bulk mixed: %v", err)
	}
	if n3 != 1 {
		t.Errorf("mixed inserted = %d, want 1", n3)
	}
	if mix[0].CreatedAt != 0 {
		t.Errorf("skipped record was mutated: %+v", mix[0])
	}
	if mix[1].CreatedAt == 0 {
		t.Errorf("inserted record not stamped: %+v", mix[1])
	}
	got3, err := s.GetPushSubscription(ctx, "https://push.example.com/a")
	if err != nil {
		t.Fatalf("get after skip: %v", err)
	}
	if got3.P256dh != "p1" {
		t.Errorf("first-write-wins violated: p256dh = %q, want p1", got3.P256dh)
	}
}

func TestBulkInsertPushSubscriptionsRejectsEmptyFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		rec  *PushSubscriptionRecord
	}{
		{"empty endpoint", &PushSubscriptionRecord{Endpoint: "", VAPIDPublicKey: "pub", P256dh: "p", Auth: "a"}},
		{"empty vapid", &PushSubscriptionRecord{Endpoint: "x", VAPIDPublicKey: "", P256dh: "p", Auth: "a"}},
		{"empty p256dh", &PushSubscriptionRecord{Endpoint: "x", VAPIDPublicKey: "pub", P256dh: "", Auth: "a"}},
		{"empty auth", &PushSubscriptionRecord{Endpoint: "x", VAPIDPublicKey: "pub", P256dh: "p", Auth: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.BulkInsertPushSubscriptions(ctx, []*PushSubscriptionRecord{tc.rec}, PushSubscriptionInsertOptions{}); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestGetPushSubscriptionNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetPushSubscription(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestBulkInsertPushSubscriptionsInBatchDuplicate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	recs := []*PushSubscriptionRecord{
		{Endpoint: "dup", VAPIDPublicKey: "pub", P256dh: "first", Auth: "a1"},
		{Endpoint: "dup", VAPIDPublicKey: "pub", P256dh: "second", Auth: "a2"},
	}
	n, err := s.BulkInsertPushSubscriptions(ctx, recs, PushSubscriptionInsertOptions{})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 1 {
		t.Errorf("inserted = %d, want 1 (first-write-wins on dup)", n)
	}
	got, err := s.GetPushSubscription(ctx, "dup")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.P256dh != "first" {
		t.Errorf("first-write-wins violated: p256dh = %q, want first", got.P256dh)
	}
}

func TestBulkInsertPushSubscriptionsEmptyStringPtrNormalized(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	empty := ""
	recs := []*PushSubscriptionRecord{
		{
			Endpoint:       "x",
			DeviceID:       &empty,
			UserAgent:      &empty,
			VAPIDPublicKey: "pub",
			P256dh:         "p",
			Auth:           "a",
		},
	}
	n, err := s.BulkInsertPushSubscriptions(ctx, recs, PushSubscriptionInsertOptions{})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if n != 1 {
		t.Errorf("inserted = %d, want 1", n)
	}
	if recs[0].DeviceID != nil {
		t.Errorf("staged DeviceID should be nil after normalization, got %v", recs[0].DeviceID)
	}
	if recs[0].UserAgent != nil {
		t.Errorf("staged UserAgent should be nil after normalization, got %v", recs[0].UserAgent)
	}
	got, err := s.GetPushSubscription(ctx, "x")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DeviceID != nil {
		t.Errorf("device_id should be nil after &\"\" normalization, got %v", got.DeviceID)
	}
	if got.UserAgent != nil {
		t.Errorf("user_agent should be nil after &\"\" normalization, got %v", got.UserAgent)
	}
}
