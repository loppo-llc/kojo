package agent

import (
	"errors"
	"os"
	"testing"
)

// setupCredentialStore creates a CredentialStore backed by a temporary directory.
func setupCredentialStore(t *testing.T) *CredentialStore {
	t.Helper()
	tmp := t.TempDir()
	// Override HOME so kojoConfigDir() resolves to tmp
	t.Setenv("HOME", tmp)
	// Clear APPDATA to avoid Windows path on non-Windows
	t.Setenv("APPDATA", "")

	cs, err := NewCredentialStore()
	if err != nil {
		t.Fatal("NewCredentialStore:", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestCredentialStore_AddAndList(t *testing.T) {
	cs := setupCredentialStore(t)

	cred, err := cs.AddCredential("ag_test1", "GitHub", "user1", "secret123", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cred.ID == "" {
		t.Error("expected non-empty credential ID")
	}
	if cred.Label != "GitHub" {
		t.Errorf("label = %q, want %q", cred.Label, "GitHub")
	}
	if cred.Username != "user1" {
		t.Errorf("username = %q, want %q", cred.Username, "user1")
	}
	// Password should be masked in return value
	if cred.Password != maskedValue {
		t.Errorf("password should be masked, got %q", cred.Password)
	}

	// List should return the credential with masked password
	list, err := cs.ListCredentials("ag_test1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 credential, got %d", len(list))
	}
	if list[0].Password != maskedValue {
		t.Errorf("listed password should be masked")
	}
}

func TestCredentialStore_RevealPassword(t *testing.T) {
	cs := setupCredentialStore(t)

	cred, err := cs.AddCredential("ag_test1", "Test", "user", "myPassword!", nil)
	if err != nil {
		t.Fatal(err)
	}

	pw, err := cs.RevealPassword("ag_test1", cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pw != "myPassword!" {
		t.Errorf("revealed password = %q, want %q", pw, "myPassword!")
	}
}

func TestCredentialStore_Update(t *testing.T) {
	cs := setupCredentialStore(t)

	cred, err := cs.AddCredential("ag_test1", "Old", "user", "pass", nil)
	if err != nil {
		t.Fatal(err)
	}

	newLabel := "New"
	newPw := "newpass"
	updated, err := cs.UpdateCredential("ag_test1", cred.ID, &newLabel, nil, &newPw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Label != "New" {
		t.Errorf("label = %q, want %q", updated.Label, "New")
	}
	if updated.Username != "user" {
		t.Errorf("username should not change: %q", updated.Username)
	}

	pw, err := cs.RevealPassword("ag_test1", cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pw != "newpass" {
		t.Errorf("revealed password = %q, want %q", pw, "newpass")
	}
}

func TestCredentialStore_Delete(t *testing.T) {
	cs := setupCredentialStore(t)

	cred, err := cs.AddCredential("ag_test1", "Del", "user", "pass", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := cs.DeleteCredential("ag_test1", cred.ID); err != nil {
		t.Fatal(err)
	}

	list, err := cs.ListCredentials("ag_test1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("expected 0 credentials after delete, got %d", len(list))
	}
}

func TestCredentialStore_DeleteNotFound(t *testing.T) {
	cs := setupCredentialStore(t)

	err := cs.DeleteCredential("ag_test1", "cred_nonexistent")
	if !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("expected ErrCredentialNotFound, got %v", err)
	}
}

func TestCredentialStore_DeleteWrongAgent(t *testing.T) {
	cs := setupCredentialStore(t)

	cred, err := cs.AddCredential("ag_test1", "Test", "u", "p", nil)
	if err != nil {
		t.Fatal(err)
	}

	err = cs.DeleteCredential("ag_wrong", cred.ID)
	if !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("expected ErrCredentialNotFound for wrong agent, got %v", err)
	}
}

func TestCredentialStore_DeleteAllForAgent(t *testing.T) {
	cs := setupCredentialStore(t)

	if _, err := cs.AddCredential("ag_test1", "A", "u1", "p1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.AddCredential("ag_test1", "B", "u2", "p2", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.AddCredential("ag_test2", "C", "u3", "p3", nil); err != nil {
		t.Fatal(err)
	}

	if err := cs.DeleteAllForAgent("ag_test1"); err != nil {
		t.Fatal(err)
	}

	list1, _ := cs.ListCredentials("ag_test1")
	if len(list1) != 0 {
		t.Errorf("expected 0 for ag_test1, got %d", len(list1))
	}

	list2, _ := cs.ListCredentials("ag_test2")
	if len(list2) != 1 {
		t.Errorf("expected 1 for ag_test2, got %d", len(list2))
	}
}

func TestCredentialStore_EmptyPassword(t *testing.T) {
	cs := setupCredentialStore(t)

	cred, err := cs.AddCredential("ag_test1", "NoPass", "user", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	pw, err := cs.RevealPassword("ag_test1", cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pw != "" {
		t.Errorf("expected empty password, got %q", pw)
	}
}

func TestCredentialStore_AddWithTOTP(t *testing.T) {
	cs := setupCredentialStore(t)

	// JBSWY3DPEHPK3PXP is a well-known test secret
	totp := &TOTPParams{
		Secret:    "JBSWY3DPEHPK3PXP",
		Algorithm: "SHA1",
		Digits:    6,
		Period:    30,
	}
	cred, err := cs.AddCredential("ag_test1", "2FA Site", "user", "pass", totp)
	if err != nil {
		t.Fatal(err)
	}
	if cred.TOTPSecret != maskedValue {
		t.Errorf("TOTP secret should be masked, got %q", cred.TOTPSecret)
	}

	// List should show masked TOTP
	list, err := cs.ListCredentials("ag_test1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 credential, got %d", len(list))
	}
	if list[0].TOTPSecret != maskedValue {
		t.Errorf("listed TOTP secret should be masked")
	}
}

func TestCredentialStore_GetTOTPCode(t *testing.T) {
	cs := setupCredentialStore(t)

	totp := &TOTPParams{
		Secret:    "JBSWY3DPEHPK3PXP",
		Algorithm: "SHA1",
		Digits:    6,
		Period:    30,
	}
	cred, err := cs.AddCredential("ag_test1", "2FA", "user", "pass", totp)
	if err != nil {
		t.Fatal(err)
	}

	code, remaining, err := cs.GetTOTPCode("ag_test1", cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 6 {
		t.Errorf("expected 6-digit code, got %q", code)
	}
	if remaining <= 0 {
		t.Errorf("expected positive remaining seconds, got %d", remaining)
	}
}

func TestCredentialStore_GetTOTPCode_NoSecret(t *testing.T) {
	cs := setupCredentialStore(t)

	cred, err := cs.AddCredential("ag_test1", "NoTOTP", "user", "pass", nil)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = cs.GetTOTPCode("ag_test1", cred.ID)
	if !errors.Is(err, ErrNoTOTPSecret) {
		t.Errorf("expected ErrNoTOTPSecret, got %v", err)
	}
}

func TestCredentialStore_AddWithInvalidTOTP(t *testing.T) {
	cs := setupCredentialStore(t)

	totp := &TOTPParams{
		Secret: "not-valid-base32!!!",
	}
	_, err := cs.AddCredential("ag_test1", "Bad", "user", "pass", totp)
	if err == nil {
		t.Error("expected error for invalid TOTP secret")
	}
}

// TestCredentialStore_ExportDecryptsAll mirrors the §3.7 peer-sync
// source side: ExportCredentials must return decrypted password +
// TOTP secret, ordered by created_at (same surface ListCredentials
// uses, but with secrets revealed).
func TestCredentialStore_ExportDecryptsAll(t *testing.T) {
	cs := setupCredentialStore(t)
	totp := &TOTPParams{
		Secret:    "JBSWY3DPEHPK3PXP",
		Algorithm: "SHA1",
		Digits:    6,
		Period:    30,
	}
	if _, err := cs.AddCredential("ag_exp", "Site", "alice", "p4ss!", totp); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.AddCredential("ag_exp", "Other", "bob", "secret2", nil); err != nil {
		t.Fatal(err)
	}
	// Different agent — must NOT bleed across.
	if _, err := cs.AddCredential("ag_other", "Foo", "x", "y", nil); err != nil {
		t.Fatal(err)
	}

	out, err := cs.ExportCredentials("ag_exp")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 credentials, got %d", len(out))
	}
	// Plaintext must be revealed.
	var sawPass bool
	for _, c := range out {
		if c.Username == "alice" {
			sawPass = true
			if c.Password != "p4ss!" {
				t.Errorf("password not decrypted: %q", c.Password)
			}
			if c.TOTPSecret != "JBSWY3DPEHPK3PXP" {
				t.Errorf("totp not decrypted: %q", c.TOTPSecret)
			}
		}
	}
	if !sawPass {
		t.Error("expected alice's credential in export")
	}

	empty, err := cs.ExportCredentials("ag_none")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("expected empty slice for unknown agent, got %d", len(empty))
	}
}

// TestCredentialStore_ReplaceReencrypts mirrors the §3.7 peer-sync
// receiver side: ReplaceCredentials wipes the agent's prior rows and
// inserts the supplied set re-encrypted under the target's key, with
// stable id / createdAt preservation.
func TestCredentialStore_ReplaceReencrypts(t *testing.T) {
	cs := setupCredentialStore(t)

	// Stale prior row that must be discarded.
	if _, err := cs.AddCredential("ag_rep", "Stale", "old", "oldpw", nil); err != nil {
		t.Fatal(err)
	}
	// Another agent's rows must be left alone.
	if _, err := cs.AddCredential("ag_other", "Keep", "k", "kpw", nil); err != nil {
		t.Fatal(err)
	}

	incoming := []*Credential{
		{
			ID:        "cred_remote1",
			Label:     "Synced",
			Username:  "alice",
			Password:  "fresh!",
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-02T00:00:00Z",
		},
		{
			ID:            "cred_remote2",
			Label:         "WithTOTP",
			Username:      "bob",
			Password:      "pw2",
			TOTPSecret:    "JBSWY3DPEHPK3PXP",
			TOTPAlgorithm: "SHA1",
			TOTPDigits:    6,
			TOTPPeriod:    30,
			CreatedAt:     "2026-01-03T00:00:00Z",
			UpdatedAt:     "2026-01-03T00:00:00Z",
		},
	}
	if err := cs.ReplaceCredentials("ag_rep", incoming); err != nil {
		t.Fatal(err)
	}

	got, err := cs.ExportCredentials("ag_rep")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 credentials, got %d", len(got))
	}
	byID := map[string]*Credential{}
	for _, c := range got {
		byID[c.ID] = c
	}
	c1, ok := byID["cred_remote1"]
	if !ok {
		t.Fatal("cred_remote1 missing")
	}
	if c1.Password != "fresh!" {
		t.Errorf("password not re-encrypted/decrypted correctly: %q", c1.Password)
	}
	if c1.CreatedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("createdAt not preserved: %q", c1.CreatedAt)
	}
	c2, ok := byID["cred_remote2"]
	if !ok {
		t.Fatal("cred_remote2 missing")
	}
	if c2.TOTPSecret != "JBSWY3DPEHPK3PXP" {
		t.Errorf("totp secret not preserved: %q", c2.TOTPSecret)
	}

	// Stale row must be gone.
	for _, c := range got {
		if c.Username == "old" {
			t.Error("stale credential still present after replace")
		}
	}

	// Other agent's row must remain.
	other, err := cs.ListCredentials("ag_other")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 {
		t.Fatalf("ag_other rows tampered with: got %d", len(other))
	}

	// Replace with empty set clears the agent.
	if err := cs.ReplaceCredentials("ag_rep", nil); err != nil {
		t.Fatal(err)
	}
	cleared, err := cs.ExportCredentials("ag_rep")
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 0 {
		t.Errorf("expected cleared after empty replace, got %d", len(cleared))
	}
}

// TestCredentialStore_ReplaceRejectsMalformed defends the wire-side
// boundary: a peer-sync payload with a malformed credential record
// must fail loud (rolling back the whole tx) rather than silently
// dropping the bad row and applying the rest.
func TestCredentialStore_ReplaceRejectsMalformed(t *testing.T) {
	cs := setupCredentialStore(t)
	if _, err := cs.AddCredential("ag_rb", "Stay", "u", "p", nil); err != nil {
		t.Fatal(err)
	}

	err := cs.ReplaceCredentials("ag_rb", []*Credential{
		{ID: "cred_ok", Label: "ok", Username: "u", Password: "p"},
		{ID: "", Label: "broken"},
	})
	if err == nil {
		t.Fatal("expected error for missing credential id")
	}

	// Tx rolled back — original row must still be present.
	list, err := cs.ListCredentials("ag_rb")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Username != "u" {
		t.Errorf("rollback failed: list=%+v", list)
	}
}

func TestCredentialStore_KeyCorruption(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("APPDATA", "")

	// Create a store to generate key + DB
	cs1, err := NewCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs1.AddCredential("ag_test1", "Test", "u", "p", nil); err != nil {
		t.Fatal(err)
	}
	cs1.Close()

	// Corrupt the key
	keyPath := tmp + "/.config/kojo-v1/credentials.key"
	if err := os.WriteFile(keyPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = NewCredentialStore()
	if err == nil {
		t.Error("expected error with corrupted key and existing DB")
	}
}
