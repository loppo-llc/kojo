package notify

// kv table layout for the notify domain. Single source of truth shared
// by cmd/kojo (runtime VAPID load/save) and internal/migrate/importers
// (v0→v1 migration). Keeping the constants in one place lets the
// compiler catch drift — diverging namespace / key / AAD between the
// importer and the runtime would orphan every secret row, since the
// AAD is part of the GCM authentication tag.
const (
	// KVNamespace is the kv.namespace value used for every notify
	// domain row.
	KVNamespace = "notify"

	// KVKeyVAPIDPublic holds the VAPID public key as plaintext.
	KVKeyVAPIDPublic = "vapid_public"

	// KVKeyVAPIDPrivate holds the VAPID private key, envelope-sealed
	// with the host-bound KEK at <v1>/auth/kek.bin (see design doc
	// §3.4). AAD is derived from the (namespace, key) pair below —
	// see VAPIDPrivateAAD.
	KVKeyVAPIDPrivate = "vapid_private"
)

// VAPIDPrivateAADString binds the encrypted private-key bytes to their
// (namespace, key) slot. Must round-trip between Seal (importer or
// SaveVAPID) and Open (LoadVAPID) — bumping it would orphan every
// existing secret row, so treat it as a wire constant. Exposed as
// `const string` rather than `var []byte` so a consumer can't
// accidentally mutate the shared backing array; call sites convert via
// []byte(notify.VAPIDPrivateAADString) at use.
const VAPIDPrivateAADString = KVNamespace + "/" + KVKeyVAPIDPrivate

// VAPIDPrivateAAD returns a fresh []byte for the AAD on each call.
// Helper for call sites that don't want to repeat the cast; the
// underlying bytes are independent per call so mutation by one
// consumer cannot leak into another.
func VAPIDPrivateAAD() []byte { return []byte(VAPIDPrivateAADString) }
