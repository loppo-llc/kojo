package importers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/loppo-llc/kojo/internal/migrate"
	"github.com/loppo-llc/kojo/internal/store"
)

// pushSubscriptionsImporter walks <v0>/push_subscriptions.json and
// inserts the rows into the v1 push_subscriptions table. Domain key:
// "push_subscriptions".
//
// v0 schema (from internal/notify/webpush.go: webpush.Subscription):
//
//	[
//	  {
//	    "endpoint": "https://...",
//	    "keys": { "auth": "...", "p256dh": "..." }
//	  },
//	  ...
//	]
//
// v1 schema (0001_initial.sql) requires vapid_public_key alongside the
// p256dh/auth pair: a key rotation (regenerated VAPID pair) would
// otherwise leave existing rows unable to authenticate the next /push
// request. v0 stores the VAPID pair separately in vapid.json; this
// importer reads it directly to fill the column.
//
// Why v0/vapid.json rather than the v1 kv table:
//
//   - self-contained: no dependency on a future vapid importer's
//     ordering. Keeping the public-key source in v0 means this slice
//     can ship before the kv vapid envelope work (which needs KEK
//     plumbing through migrate.Options).
//   - read-once: parsed exactly once for this domain.
//   - matches notify_cursors's pattern (it reads agents.json directly
//     for the type lookup rather than the v1 agents table).
//
// A missing / empty / publicKey-less vapid.json is fatal: a row without
// a vapid_public_key would be unrecoverable on the next rotation, and
// v0 always materializes a pair on first boot (loadOrGenerateVAPID
// creates one if absent), so reaching this code with no public key
// means v0 was never started or the file was hand-deleted — both
// states warrant operator attention rather than silent migration of
// uninstallable rows.
type pushSubscriptionsImporter struct{}

func (pushSubscriptionsImporter) Domain() string { return "push_subscriptions" }

// v0WebpushSubscription mirrors webpush-go's wire shape exactly. The v0
// notify.Manager dropped any row missing endpoint/auth/p256dh on load
// (webpush.go:213-220); we apply the same skip rule below so a malformed
// row in the file doesn't poison the importer.
type v0WebpushSubscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		Auth   string `json:"auth"`
		P256dh string `json:"p256dh"`
	} `json:"keys"`
}

type v0VAPIDKeys struct {
	PrivateKey string `json:"privateKey"`
	PublicKey  string `json:"publicKey"`
}

func (pushSubscriptionsImporter) Run(ctx context.Context, st *store.Store, opts migrate.Options) error {
	if done, err := alreadyImported(ctx, st, "push_subscriptions"); err != nil {
		return err
	} else if done {
		return nil
	}

	logger := slog.Default().With("importer", "push_subscriptions")

	srcPaths, err := collectPushSubscriptionsSourcePaths(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("collect source paths: %w", err)
	}
	checksum, err := domainChecksum(opts.V0Dir, srcPaths)
	if err != nil {
		return fmt.Errorf("checksum push_subscriptions sources: %w", err)
	}

	subPath := filepath.Join(opts.V0Dir, "push_subscriptions.json")
	data, err := readV0(opts.V0Dir, subPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return markImported(ctx, st, "push_subscriptions", 0, checksum)
		}
		return err
	}
	if len(data) == 0 {
		return markImported(ctx, st, "push_subscriptions", 0, checksum)
	}

	var subs []v0WebpushSubscription
	if err := json.Unmarshal(data, &subs); err != nil {
		// Malformed file: log and mark imported with 0 rows so a re-run
		// doesn't repeatedly fail on the same parse. Matches the posture
		// in sessions / notify_cursors importers — a corrupt v0 file is
		// signalled in the migration log but doesn't block the rest of
		// the migration. Subscriptions are recoverable by re-permitting
		// the browser on first v1 boot, unlike notify_cursors where a
		// loss means a re-poll storm.
		logger.Warn("push_subscriptions: skipping malformed file",
			"path", subPath, "err", err)
		return markImported(ctx, st, "push_subscriptions", 0, checksum)
	}
	if len(subs) == 0 {
		return markImported(ctx, st, "push_subscriptions", 0, checksum)
	}

	mtime := fileMTimeMillis(subPath)
	// Filter the v0 entries first; we need to know whether any survive
	// the malformed-entry skip BEFORE deciding to require vapid.json.
	// A push_subscriptions.json full of garbage (every row missing at
	// least one of endpoint / auth / p256dh) is recoverable on the
	// browser side (re-permit re-creates the row), and demanding a
	// well-formed vapid.json in that case would block migration on a
	// dataset with nothing to actually migrate. v0's load-time cleanup
	// (webpush.go:213-220) applies the same filter, so what we drop
	// here is precisely what v0 itself ignored at boot.
	staged := make([]v0WebpushSubscription, 0, len(subs))
	skipped := 0
	for i, s := range subs {
		if s.Endpoint == "" || s.Keys.Auth == "" || s.Keys.P256dh == "" {
			logger.Warn("push_subscriptions: skipping malformed entry",
				"index", i,
				"has_endpoint", s.Endpoint != "",
				"has_auth", s.Keys.Auth != "",
				"has_p256dh", s.Keys.P256dh != "")
			skipped++
			continue
		}
		staged = append(staged, s)
	}
	if len(staged) == 0 {
		if skipped > 0 {
			logger.Info("push_subscriptions: all rows skipped (malformed)",
				"skipped", skipped)
		}
		return markImported(ctx, st, "push_subscriptions", 0, checksum)
	}

	// Resolve VAPID public key from v0/vapid.json. Fatal on miss because
	// the column is NOT NULL and no other v0 source carries it (the
	// v0 webpush.Subscription struct does not include the originating
	// public key — it's a server-side concern). Only consulted when at
	// least one v0 row survived the malformed filter, so a deployment
	// that legitimately has no subscriptions can migrate without ever
	// having booted v0's VAPID generator.
	pub, err := loadVAPIDPublicKey(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("load vapid public key: %w", err)
	}

	recs := make([]*store.PushSubscriptionRecord, 0, len(staged))
	for _, s := range staged {
		recs = append(recs, &store.PushSubscriptionRecord{
			Endpoint:       s.Endpoint,
			VAPIDPublicKey: pub,
			P256dh:         s.Keys.P256dh,
			Auth:           s.Keys.Auth,
			// DeviceID / UserAgent stay nil: v0 never recorded them.
			// ExpiredAt stays nil: v0 dropped expired rows from the file
			// rather than tombstoning, so anything still present is
			// considered active for migration purposes.
			CreatedAt: mtime,
			UpdatedAt: mtime,
		})
	}

	n, err := st.BulkInsertPushSubscriptions(ctx, recs, store.PushSubscriptionInsertOptions{})
	if err != nil {
		return fmt.Errorf("bulk insert push_subscriptions: %w", err)
	}
	if skipped > 0 {
		logger.Info("push_subscriptions: import complete with skips",
			"inserted", n, "skipped", skipped)
	}
	return markImported(ctx, st, "push_subscriptions", n, checksum)
}

// loadVAPIDPublicKey reads <v0>/vapid.json and returns the publicKey.
//
// Skip rules:
//   - missing vapid.json (os.ErrNotExist) → fatal: every imported row
//     would lack a usable vapid_public_key.
//   - empty file (len(data)==0) → fatal: a zero-byte file on a disk
//     that *has* the path is a truncation signal, not a v0 contract.
//   - malformed JSON → fatal: same reasoning as agents.json in
//     loadNotifySourceTypes.
//   - empty publicKey field → fatal: the column is NOT NULL.
//
// If push_subscriptions.json is empty / absent the importer never calls
// this function (early-return on the subs file), so the fatal posture
// here only fires when there are subscriptions to migrate but no
// public key to attach.
func loadVAPIDPublicKey(v0Dir string) (string, error) {
	path := filepath.Join(v0Dir, "vapid.json")
	data, err := readV0(v0Dir, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("vapid.json missing — push_subscriptions cannot migrate without it")
		}
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("vapid.json is empty")
	}
	var keys v0VAPIDKeys
	if err := json.Unmarshal(data, &keys); err != nil {
		return "", fmt.Errorf("vapid.json malformed: %w", err)
	}
	if keys.PublicKey == "" {
		return "", fmt.Errorf("vapid.json has empty publicKey")
	}
	return keys.PublicKey, nil
}

// collectPushSubscriptionsSourcePaths returns the v0 files this importer
// hashes for source_checksum. Includes push_subscriptions.json (the
// actual data) AND vapid.json (the public-key source): a change in
// either file alters what gets imported, so both must be in the
// checksum. Re-VAPIDing while leaving the same subscription file would
// produce different vapid_public_key values on a second run — flagging
// that drift is a feature, not a bug.
func collectPushSubscriptionsSourcePaths(v0Dir string) ([]string, error) {
	var paths []string
	for _, p := range []string{
		filepath.Join(v0Dir, "push_subscriptions.json"),
		filepath.Join(v0Dir, "vapid.json"),
	} {
		updated, err := addLeafIfRegular(v0Dir, p, paths)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		paths = updated
	}
	return paths, nil
}
