package agent

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Credential represents a stored ID/password pair for an agent.
type Credential struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	TOTPSecret    string `json:"totpSecret,omitempty"`
	TOTPAlgorithm string `json:"totpAlgorithm,omitempty"` // SHA1 (default), SHA256, SHA512
	TOTPDigits    int    `json:"totpDigits,omitempty"`     // 6 (default) or 8
	TOTPPeriod    int    `json:"totpPeriod,omitempty"`     // 30 (default)
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

// TOTPParams holds TOTP configuration for a credential.
type TOTPParams struct {
	Secret    string
	Algorithm string // SHA1, SHA256, SHA512
	Digits    int    // 6 or 8
	Period    int    // seconds
}

const maskedValue = "\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022"

func maskCredential(c *Credential) *Credential {
	cp := *c
	if cp.Password != "" {
		cp.Password = maskedValue
	}
	if cp.TOTPSecret != "" {
		cp.TOTPSecret = maskedValue
	}
	return &cp
}

// CredentialStore manages encrypted credential storage in SQLite.
type CredentialStore struct {
	mu  sync.Mutex
	db  *sql.DB
	gcm cipher.AEAD
}

// kojoConfigDir returns the kojo config root directory.
func kojoConfigDir() string {
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "kojo")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "kojo")
	}
	return filepath.Join(home, ".config", "kojo")
}

// NewCredentialStore opens or creates the encrypted credential store.
func NewCredentialStore() (*CredentialStore, error) {
	dir := kojoConfigDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}

	// Load or generate encryption key
	keyPath := filepath.Join(dir, "credentials.key")
	dbPath := filepath.Join(dir, "credentials.db")
	key, err := loadOrCreateKey(keyPath, dbPath)
	if err != nil {
		return nil, fmt.Errorf("encryption key: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := createCredentialTable(db); err != nil {
		db.Close()
		return nil, err
	}

	s := &CredentialStore{db: db, gcm: gcm}

	// Migrate legacy credentials.json files
	s.migrateLegacy()

	return s, nil
}

func createCredentialTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS credentials (
		id TEXT NOT NULL,
		agent_id TEXT NOT NULL,
		label TEXT NOT NULL,
		username TEXT NOT NULL,
		password_enc TEXT NOT NULL DEFAULT '',
		totp_secret_enc TEXT NOT NULL DEFAULT '',
		totp_algorithm TEXT NOT NULL DEFAULT '',
		totp_digits INTEGER NOT NULL DEFAULT 0,
		totp_period INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (agent_id, id)
	)`)
	return err
}

// loadOrCreateKey loads a 32-byte AES-256 key from file, or generates one.
// If the DB already exists but the key is missing/corrupt, it returns an error
// (fail closed) to prevent generating a new key that can't decrypt existing data.
func loadOrCreateKey(keyPath, dbPath string) ([]byte, error) {
	data, err := os.ReadFile(keyPath)
	if err == nil && len(data) == 32 {
		return data, nil
	}

	// Key is missing or corrupt — check if DB already has data
	dbExists := false
	if fi, statErr := os.Stat(dbPath); statErr == nil && fi.Size() > 0 {
		dbExists = true
	}
	if dbExists {
		if err != nil {
			return nil, fmt.Errorf("encryption key file missing/unreadable but credentials.db exists: %w", err)
		}
		return nil, fmt.Errorf("encryption key file corrupt (%d bytes, expected 32) but credentials.db exists", len(data))
	}

	// No existing DB — safe to generate a new key
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}
	return key, nil
}

func (s *CredentialStore) encryptChecked(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := s.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(sealed), nil
}

func (s *CredentialStore) decrypt(cipherHex string) (string, error) {
	if cipherHex == "" {
		return "", nil
	}
	data, err := hex.DecodeString(cipherHex)
	if err != nil {
		return "", err
	}
	nonceSize := s.gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := s.gcm.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func generateCredID() string {
	return generatePrefixedID("cred_")
}

// Close closes the database connection.
func (s *CredentialStore) Close() error {
	return s.db.Close()
}

// ListCredentials returns all credentials for an agent with secrets masked.
// Passwords and TOTP secrets are NOT decrypted — only metadata is returned.
func (s *CredentialStore) ListCredentials(agentID string) ([]*Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(
		`SELECT id, label, username, password_enc, totp_secret_enc,
		        totp_algorithm, totp_digits, totp_period, created_at, updated_at
		 FROM credentials WHERE agent_id = ? ORDER BY created_at`,
		agentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var creds []*Credential
	for rows.Next() {
		c, err := scanCredentialMasked(rows)
		if err != nil {
			return nil, err
		}
		creds = append(creds, c)
	}
	return creds, rows.Err()
}

// scanCredentialMasked scans a credential row without decrypting secrets.
// Password and TOTPSecret are replaced with presence indicators.
func scanCredentialMasked(sc scannable) (*Credential, error) {
	var c Credential
	var pwEnc, totpEnc string
	err := sc.Scan(&c.ID, &c.Label, &c.Username, &pwEnc, &totpEnc,
		&c.TOTPAlgorithm, &c.TOTPDigits, &c.TOTPPeriod, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if pwEnc != "" {
		c.Password = "••••••••"
	}
	if totpEnc != "" {
		c.TOTPSecret = "••••••••"
	}
	return &c, nil
}

// AddCredential adds a new credential for an agent.
func (s *CredentialStore) AddCredential(agentID, label, username, password string, totp *TOTPParams) (*Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	c := &Credential{
		ID:        generateCredID(),
		Label:     label,
		Username:  username,
		Password:  password,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if totp != nil && totp.Secret != "" {
		normalized, err := ValidateTOTPParams(totp.Secret, totp.Algorithm, totp.Digits, totp.Period)
		if err != nil {
			return nil, err
		}
		c.TOTPSecret = normalized
		c.TOTPAlgorithm = totp.Algorithm
		c.TOTPDigits = totp.Digits
		c.TOTPPeriod = totp.Period
	}

	pwEnc, err := s.encryptChecked(c.Password)
	if err != nil {
		return nil, err
	}
	totpEnc, err := s.encryptChecked(c.TOTPSecret)
	if err != nil {
		return nil, err
	}

	_, err = s.db.Exec(
		`INSERT INTO credentials (id, agent_id, label, username, password_enc, totp_secret_enc,
		 totp_algorithm, totp_digits, totp_period, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, agentID, c.Label, c.Username,
		pwEnc, totpEnc,
		c.TOTPAlgorithm, c.TOTPDigits, c.TOTPPeriod,
		c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	return maskCredential(c), nil
}

// UpdateCredential updates an existing credential. Only non-nil fields are applied.
func (s *CredentialStore) UpdateCredential(agentID, credID string, label, username, password *string, totp *TOTPParams) (*Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getCredentialLocked(agentID, credID)
	if err != nil {
		return nil, err
	}

	if label != nil {
		c.Label = *label
	}
	if username != nil {
		c.Username = *username
	}
	if password != nil {
		c.Password = *password
	}
	if totp != nil {
		if totp.Secret != "" {
			normalized, err := ValidateTOTPParams(totp.Secret, totp.Algorithm, totp.Digits, totp.Period)
			if err != nil {
				return nil, err
			}
			totp.Secret = normalized
		}
		c.TOTPSecret = totp.Secret
		c.TOTPAlgorithm = totp.Algorithm
		c.TOTPDigits = totp.Digits
		c.TOTPPeriod = totp.Period
	}
	c.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	pwEnc, err := s.encryptChecked(c.Password)
	if err != nil {
		return nil, err
	}
	totpEnc, err := s.encryptChecked(c.TOTPSecret)
	if err != nil {
		return nil, err
	}

	_, err = s.db.Exec(
		`UPDATE credentials SET label=?, username=?, password_enc=?, totp_secret_enc=?,
		 totp_algorithm=?, totp_digits=?, totp_period=?, updated_at=?
		 WHERE agent_id=? AND id=?`,
		c.Label, c.Username,
		pwEnc, totpEnc,
		c.TOTPAlgorithm, c.TOTPDigits, c.TOTPPeriod,
		c.UpdatedAt, agentID, credID,
	)
	if err != nil {
		return nil, err
	}

	return maskCredential(c), nil
}

// DeleteCredential removes a credential by ID.
func (s *CredentialStore) DeleteCredential(agentID, credID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(`DELETE FROM credentials WHERE agent_id=? AND id=?`, agentID, credID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("credential not found: %s", credID)
	}
	return nil
}

// DeleteAllForAgent removes all credentials for an agent.
func (s *CredentialStore) DeleteAllForAgent(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`DELETE FROM credentials WHERE agent_id=?`, agentID)
	return err
}

// RevealPassword returns the plaintext password for a credential.
func (s *CredentialStore) RevealPassword(agentID, credID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getCredentialLocked(agentID, credID)
	if err != nil {
		return "", err
	}
	return c.Password, nil
}

// GetTOTPCode generates the current TOTP code for a credential.
func (s *CredentialStore) GetTOTPCode(agentID, credID string) (string, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getCredentialLocked(agentID, credID)
	if err != nil {
		return "", 0, err
	}
	if c.TOTPSecret == "" {
		return "", 0, fmt.Errorf("no TOTP secret configured for credential: %s", credID)
	}
	return GenerateTOTPCode(c.TOTPSecret, c.TOTPAlgorithm, c.TOTPDigits, c.TOTPPeriod)
}

// getCredentialLocked fetches and decrypts a single credential. Caller must hold mu.
func (s *CredentialStore) getCredentialLocked(agentID, credID string) (*Credential, error) {
	row := s.db.QueryRow(
		`SELECT id, label, username, password_enc, totp_secret_enc,
		        totp_algorithm, totp_digits, totp_period, created_at, updated_at
		 FROM credentials WHERE agent_id=? AND id=?`,
		agentID, credID,
	)
	c, err := s.scanCredentialRow(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("credential not found: %s", credID)
	}
	return c, err
}

type scannable interface {
	Scan(dest ...any) error
}

func (s *CredentialStore) scanCredential(sc scannable) (*Credential, error) {
	return s.scanCredentialInner(sc)
}

func (s *CredentialStore) scanCredentialRow(row *sql.Row) (*Credential, error) {
	return s.scanCredentialInner(row)
}

func (s *CredentialStore) scanCredentialInner(sc scannable) (*Credential, error) {
	var c Credential
	var pwEnc, totpEnc string
	err := sc.Scan(&c.ID, &c.Label, &c.Username, &pwEnc, &totpEnc,
		&c.TOTPAlgorithm, &c.TOTPDigits, &c.TOTPPeriod, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.Password, err = s.decrypt(pwEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt password: %w", err)
	}
	c.TOTPSecret, err = s.decrypt(totpEnc)
	if err != nil {
		return nil, fmt.Errorf("decrypt totp secret: %w", err)
	}
	return &c, nil
}

// migrateLegacy migrates credentials.json files from agent directories to the database.
func (s *CredentialStore) migrateLegacy() {
	base := agentsDir()
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agentID := entry.Name()
		jsonPath := filepath.Join(base, agentID, "credentials.json")
		data, err := os.ReadFile(jsonPath)
		if err != nil {
			if !os.IsNotExist(err) {
				slog.Warn("credential migration: failed to read JSON file", "agent", agentID, "err", err)
			}
			continue
		}
		var creds []*Credential
		if err := json.Unmarshal(data, &creds); err != nil {
			slog.Warn("credential migration: failed to parse JSON", "agent", agentID, "err", err)
			continue
		}

		// Use a transaction so either all migrate or none
		tx, err := s.db.Begin()
		if err != nil {
			slog.Warn("credential migration: failed to begin transaction", "agent", agentID, "err", err)
			continue
		}
		allOK := true
		for _, c := range creds {
			if c == nil || c.ID == "" {
				continue
			}
			pwEnc, err := s.encryptChecked(c.Password)
			if err != nil {
				slog.Warn("credential migration: encryption failed", "agent", agentID, "cred", c.ID, "err", err)
				allOK = false
				break
			}
			totpEnc, err := s.encryptChecked(c.TOTPSecret)
			if err != nil {
				slog.Warn("credential migration: TOTP encryption failed", "agent", agentID, "cred", c.ID, "err", err)
				allOK = false
				break
			}
			_, err = tx.Exec(
				`INSERT OR IGNORE INTO credentials (id, agent_id, label, username, password_enc, totp_secret_enc,
				 totp_algorithm, totp_digits, totp_period, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				c.ID, agentID, c.Label, c.Username,
				pwEnc, totpEnc,
				c.TOTPAlgorithm, c.TOTPDigits, c.TOTPPeriod,
				c.CreatedAt, c.UpdatedAt,
			)
			if err != nil {
				slog.Warn("credential migration: insert failed", "agent", agentID, "cred", c.ID, "err", err)
				allOK = false
				break
			}
		}
		if allOK {
			if err := tx.Commit(); err != nil {
				slog.Warn("credential migration: commit failed", "agent", agentID, "err", err)
			} else {
				os.Remove(jsonPath)
				slog.Info("credential migration: migrated", "agent", agentID, "count", len(creds))
			}
		} else {
			tx.Rollback()
		}
	}
}
