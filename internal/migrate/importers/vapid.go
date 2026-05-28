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
	"github.com/loppo-llc/kojo/internal/notify"
	"github.com/loppo-llc/kojo/internal/store"
	"github.com/loppo-llc/kojo/internal/store/secretcrypto"
)

// vapidImporter copies <v0>/vapid.json into the v1 kv table under
// namespace="notify". The public key lands plaintext (scope=global) so
// browser clients can subscribe (PushManager.subscribe needs the
// applicationServerKey) and any peer can read it; the private key —
// which the server actually uses to sign the JWT in the Authorization
// header on each /push request — is envelope-sealed
// with the host-bound KEK at <v1>/auth/kek.bin (see design doc §3.4)
// and lands secret=true scope=machine. There is no cluster KMS in v1;
// cluster-bound KEK is a v2 plan (see design doc §3.4).
//
// Layout (matches cmd/kojo/vapid_kv.go's runtime contract — keep these
// in lockstep, diverging keys/AAD would orphan every secret row):
//
//   - notify/vapid_public  → plaintext string, scope=global
//   - notify/vapid_private → envelope-encrypted binary, scope=machine,
//     secret=true. AAD = "notify/vapid_private".
//
// KEK is loaded from <v1>/auth/kek.bin via secretcrypto.LoadOrCreateKEK
// (creates a fresh 32-byte key on first call). Loaded only when the v0
// file is well-formed enough to need it; an absent / empty / malformed
// vapid.json is treated as "nothing to migrate" — the runtime will
// regenerate a fresh pair on first notify use, and any orphan rows in
// push_subscriptions are caught by that importer's own fatal posture.
//
// Idempotency: the alreadyImported gate short-circuits a re-run without
// touching the kv table. PutKV uses IfMatchAny (create-only), so even a
// crash-and-resume between the two PutKVs and the markImported call
// converges cleanly — a second run that finds the row already present
// returns ErrETagMismatch, which we treat as benign.
type vapidImporter struct{}

func (vapidImporter) Domain() string { return "vapid" }

// kv layout — pinned to internal/notify so the importer and runtime
// share a single source of truth on namespace / key / AAD. Drift would
// orphan every secret row, since the AAD participates in the GCM
// authentication tag.
const (
	vapidKVNamespace  = notify.KVNamespace
	vapidKVPublicKey  = notify.KVKeyVAPIDPublic
	vapidKVPrivateKey = notify.KVKeyVAPIDPrivate
)

// vapidPrivateAAD returns a fresh []byte clone on each call so call
// sites can pass it to secretcrypto.Seal/Open without risk of mutating
// the shared wire constant.
func vapidPrivateAAD() []byte { return notify.VAPIDPrivateAAD() }

// v0VAPIDFile mirrors v0's vapid.json shape. Only the two fields kojo's
// notify.Manager actually consults — the file may carry additional keys
// from a future v0 build, but we ignore them rather than fail closed.
type v0VAPIDFile struct {
	PrivateKey string `json:"privateKey"`
	PublicKey  string `json:"publicKey"`
}

func (vapidImporter) Run(ctx context.Context, st *store.Store, opts migrate.Options) error {
	if done, err := alreadyImported(ctx, st, "vapid"); err != nil {
		return err
	} else if done {
		return nil
	}

	logger := slog.Default().With("importer", "vapid")

	srcPaths, err := collectVAPIDSourcePaths(opts.V0Dir)
	if err != nil {
		return fmt.Errorf("collect source paths: %w", err)
	}
	checksum, err := domainChecksum(opts.V0Dir, srcPaths)
	if err != nil {
		return fmt.Errorf("checksum vapid sources: %w", err)
	}

	vapidPath := filepath.Join(opts.V0Dir, "vapid.json")
	data, err := readV0(opts.V0Dir, vapidPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No v0 VAPID. push_subscriptions importer's own fatal-
			// on-missing-vapid rule guards the case where there ARE
			// well-formed rows that need a key, so this importer is
			// free to mark imported(0) — runtime regenerates on first
			// notify use, harmless when no rows reference the prior
			// pair. The cross-check below is unnecessary here because
			// push_subscriptions runs after this importer and would
			// already abort the migration on the same vapid.json
			// missing condition with live rows.
			return markImported(ctx, st, "vapid", 0, checksum)
		}
		return err
	}
	if len(data) == 0 {
		// Zero-byte file is a truncation signal. Same cross-check as
		// the malformed/half-key branches: with live subscriptions,
		// silently regenerating on next boot would orphan every row's
		// vapid_public_key. Without live subscriptions, the empty file
		// is recoverable and we mark imported(0).
		if hasSubs, hsErr := hasWellFormedV0PushSubscriptions(opts.V0Dir); hsErr != nil {
			return fmt.Errorf("probe push_subscriptions for vapid orphan check: %w", hsErr)
		} else if hasSubs {
			return fmt.Errorf("vapid.json is empty but push_subscriptions has live rows: refusing to migrate")
		}
		logger.Warn("vapid.json is empty; treating as no v0 VAPID configured", "path", vapidPath)
		return markImported(ctx, st, "vapid", 0, checksum)
	}

	var keys v0VAPIDFile
	if err := json.Unmarshal(data, &keys); err != nil {
		// Malformed JSON: cross-check whether push_subscriptions has any
		// well-formed row that would lose its VAPID anchor on a runtime
		// regenerate. If so, fail loudly — silently markImported(0)
		// here would let the migration finish with subscriptions whose
		// vapid_public_key column points at a key the runtime can no
		// longer reproduce. With no live subscriptions, the regenerate
		// path is harmless and we mark imported(0) so the rerun loop
		// converges. Matches the posture push_subscriptions itself
		// applies to malformed v0 input.
		if hasSubs, hsErr := hasWellFormedV0PushSubscriptions(opts.V0Dir); hsErr != nil {
			return fmt.Errorf("probe push_subscriptions for vapid orphan check: %w", hsErr)
		} else if hasSubs {
			return fmt.Errorf("vapid.json malformed but push_subscriptions has live rows that would orphan: %w", err)
		}
		logger.Warn("vapid.json malformed; skipping (no live push subscriptions)", "path", vapidPath, "err", err)
		return markImported(ctx, st, "vapid", 0, checksum)
	}
	if keys.PublicKey == "" || keys.PrivateKey == "" {
		// Half-installed v0 state. v0's loadOrGenerateVAPID always
		// materializes a full pair at first boot, so reaching this
		// branch means the file was hand-edited or partially written.
		// Same orphan-protection cross-check as the malformed branch:
		// if push_subscriptions has live rows, we cannot silently leave
		// kv unset (the runtime would regenerate a new pair and orphan
		// every subscription's vapid_public_key column).
		if hasSubs, hsErr := hasWellFormedV0PushSubscriptions(opts.V0Dir); hsErr != nil {
			return fmt.Errorf("probe push_subscriptions for vapid orphan check: %w", hsErr)
		} else if hasSubs {
			return fmt.Errorf("vapid.json missing publicKey or privateKey but push_subscriptions has live rows: refusing to migrate (has_public=%t has_private=%t)",
				keys.PublicKey != "", keys.PrivateKey != "")
		}
		logger.Warn("vapid.json missing publicKey or privateKey; skipping (no live push subscriptions)",
			"path", vapidPath,
			"has_public", keys.PublicKey != "",
			"has_private", keys.PrivateKey != "")
		return markImported(ctx, st, "vapid", 0, checksum)
	}

	// KEK is host-bound and lives in <v1>/auth/kek.bin. We materialize
	// it here (LoadOrCreateKEK creates a fresh 32-byte key with mode
	// 0600 if absent) so the importer is self-contained — no Options
	// plumbing required. The runtime cmd/kojo/vapid_kv.go path also
	// calls LoadOrCreateKEK with the same auth dir, so the secret row
	// we seal here round-trips cleanly on the next boot.
	//
	// On --migrate-restart the wipeIncompleteV1 allowlist removes the
	// auth/ tree alongside kojo.db (see migrate.go), so the next
	// run starts with a fresh KEK paired with a fresh kojo.db — no
	// orphaned ciphertext.
	authDir := filepath.Join(opts.V1Dir, "auth")
	kek, err := secretcrypto.LoadOrCreateKEK(authDir)
	if err != nil {
		return fmt.Errorf("load/create KEK: %w", err)
	}

	// Public row first, private row second. Order is documented but
	// not load-bearing — vapidKVStore.LoadVAPID treats "public present
	// but private missing" as a half-installed error, so a crash
	// between the two PutKVs surfaces loudly on next boot rather than
	// silently regenerating.
	pubRec := &store.KVRecord{
		Namespace: vapidKVNamespace, Key: vapidKVPublicKey,
		Value: keys.PublicKey, Type: store.KVTypeString,
		Scope: store.KVScopeGlobal,
	}
	if _, err := st.PutKV(ctx, pubRec, store.KVPutOptions{IfMatchETag: store.IfMatchAny}); err != nil {
		if !errors.Is(err, store.ErrETagMismatch) {
			return fmt.Errorf("put vapid_public: %w", err)
		}
		// Row already exists. alreadyImported() should have short-
		// circuited at the top, so reaching here means a crash
		// between PutKV and markImported on a prior run, OR a hand-
		// bootstrapped v1 with mismatched contents. Verify the
		// existing value matches the v0 input before treating as
		// benign; mismatch means a different KEK or a different v0
		// produced the row, and silently markImported(2) over it
		// would lock in a broken pair.
		existing, getErr := st.GetKV(ctx, vapidKVNamespace, vapidKVPublicKey)
		if getErr != nil {
			return fmt.Errorf("verify existing vapid_public: %w", getErr)
		}
		// Compare every metadata field that defines this row's
		// contract — value, type, scope, secret — so a row that
		// landed via a different writer (e.g. a hand-edited DB or a
		// future schema migration that flipped scope/secret) is
		// surfaced loudly rather than silently overlaid as "imported".
		if existing.Value != keys.PublicKey {
			return fmt.Errorf("vapid_public already present with different value: refusing to migrate (use --migrate-restart to wipe v1)")
		}
		if existing.Type != store.KVTypeString || existing.Scope != store.KVScopeGlobal || existing.Secret || len(existing.ValueEncrypted) > 0 {
			return fmt.Errorf("vapid_public already present with mismatched metadata (type=%q scope=%q secret=%t encrypted=%t): refusing to migrate",
				existing.Type, existing.Scope, existing.Secret, len(existing.ValueEncrypted) > 0)
		}
		logger.Info("vapid_public row already present and matches v0; leaving untouched")
	}

	sealed, err := secretcrypto.Seal(kek, []byte(keys.PrivateKey), vapidPrivateAAD())
	if err != nil {
		return fmt.Errorf("seal vapid_private: %w", err)
	}
	privRec := &store.KVRecord{
		Namespace: vapidKVNamespace, Key: vapidKVPrivateKey,
		ValueEncrypted: sealed, Type: store.KVTypeBinary,
		Scope: store.KVScopeMachine, Secret: true,
	}
	if _, err := st.PutKV(ctx, privRec, store.KVPutOptions{IfMatchETag: store.IfMatchAny}); err != nil {
		if !errors.Is(err, store.ErrETagMismatch) {
			return fmt.Errorf("put vapid_private: %w", err)
		}
		// Existing private row — verify it decrypts to the same
		// plaintext as keys.PrivateKey under the current KEK + AAD.
		// A row sealed under a DIFFERENT KEK (e.g. the operator
		// rotated kek.bin between runs) would surface as an Open
		// error here, which is the correct fail-loud signal: the
		// resumed migration cannot reconcile two KEKs and the
		// operator must --migrate-restart. Same goes for a row sealed
		// under a different private key.
		existing, getErr := st.GetKV(ctx, vapidKVNamespace, vapidKVPrivateKey)
		if getErr != nil {
			return fmt.Errorf("verify existing vapid_private: %w", getErr)
		}
		// Same metadata symmetry as the public row — secret rows must
		// stay binary/secret/machine-scope, and the plaintext field
		// must be empty (PutKV enforces the value/value_encrypted
		// XOR but a hand-edited DB could violate it).
		if existing.Type != store.KVTypeBinary || existing.Scope != store.KVScopeMachine || !existing.Secret || existing.Value != "" || len(existing.ValueEncrypted) == 0 {
			return fmt.Errorf("vapid_private already present with mismatched metadata (type=%q scope=%q secret=%t plain_len=%d encrypted_len=%d): refusing to migrate",
				existing.Type, existing.Scope, existing.Secret, len(existing.Value), len(existing.ValueEncrypted))
		}
		plain, openErr := secretcrypto.Open(kek, existing.ValueEncrypted, vapidPrivateAAD())
		if openErr != nil {
			return fmt.Errorf("vapid_private already present but cannot decrypt under current KEK: %w (use --migrate-restart to wipe v1)", openErr)
		}
		if string(plain) != keys.PrivateKey {
			return fmt.Errorf("vapid_private already present with different plaintext: refusing to migrate (use --migrate-restart to wipe v1)")
		}
		logger.Info("vapid_private row already present and matches v0; leaving untouched")
	}

	// imported_count = 2 reflects the two kv rows produced from one
	// v0 file. Operators reading migration_status see "2" and can
	// cross-check via SELECT * FROM kv WHERE namespace='notify'.
	return markImported(ctx, st, "vapid", 2, checksum)
}

// hasWellFormedV0PushSubscriptions returns true if v0/push_subscriptions.json
// has at least one entry that would survive the push_subscriptions
// importer's malformed-row filter (endpoint+auth+p256dh all non-empty).
// Used by the vapid importer's orphan-protection cross-check: if
// vapid.json is broken (malformed/empty/half-key) but live subscriptions
// reference the v0 public key via their own vapid_public_key column,
// silently markImported(0) here would let the migration finish with a
// pair the runtime can no longer reproduce — every browser subscription
// orphans on the next /push attempt.
//
// Mirrors the filter rule in pushSubscriptionsImporter.Run (and v0's
// load-time cleanup in webpush.go:213-220) so what we count here is
// precisely what will end up in the v1 push_subscriptions table.
//
// Tolerant errors:
//   - missing file → false (no rows to orphan)
//   - empty file → false (same)
//   - malformed JSON → false (push_subscriptions importer logs and
//     markImported(0)s; matching that is what we want)
//
// Hard errors:
//   - read failure (non-NotExist) — surfaces upward so the operator
//     can investigate before we make a fatal-vs-skip decision on
//     incomplete data.
func hasWellFormedV0PushSubscriptions(v0Dir string) (bool, error) {
	subPath := filepath.Join(v0Dir, "push_subscriptions.json")
	data, err := readV0(v0Dir, subPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if len(data) == 0 {
		return false, nil
	}
	var subs []v0WebpushSubscription
	if err := json.Unmarshal(data, &subs); err != nil {
		// Same posture as push_subscriptions importer: malformed file
		// is treated as "no rows", not a hard error.
		return false, nil
	}
	for _, s := range subs {
		if s.Endpoint != "" && s.Keys.Auth != "" && s.Keys.P256dh != "" {
			return true, nil
		}
	}
	return false, nil
}

// collectVAPIDSourcePaths returns the v0 files this importer hashes for
// source_checksum. Includes:
//   - vapid.json: the primary source.
//   - push_subscriptions.json: consulted by the orphan-protection
//     cross-check (hasWellFormedV0PushSubscriptions). A v0 mutation
//     that adds well-formed rows would flip the importer's posture
//     from "markImported(0)" to "fatal", so the file must be in the
//     checksum to make that drift detectable on a re-run.
func collectVAPIDSourcePaths(v0Dir string) ([]string, error) {
	var paths []string
	for _, p := range []string{
		filepath.Join(v0Dir, "vapid.json"),
		filepath.Join(v0Dir, "push_subscriptions.json"),
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
