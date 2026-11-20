package db

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const encryptedPrefix = "enc:v1:"

// ErrMissingCredentialKey is returned when an encrypted credential record is read
// without CREDENTIAL_ENCRYPTION_KEY configured in the environment.
var ErrMissingCredentialKey = errors.New("CREDENTIAL_ENCRYPTION_KEY is required to decrypt stored credentials")

var secretConnectionKeys = map[string]bool{
	"apiKey":       true,
	"accessToken":  true,
	"refreshToken": true,
	"idToken":      true,
	"cookie":       true,
	"pat":          true,
	"token":        true,
}

// HasCredentialEncryptionKey reports whether CREDENTIAL_ENCRYPTION_KEY is configured.
func HasCredentialEncryptionKey() bool {
	return strings.TrimSpace(os.Getenv("CREDENTIAL_ENCRYPTION_KEY")) != ""
}

func credentialKeyBytes() ([]byte, bool) {
	raw := strings.TrimSpace(os.Getenv("CREDENTIAL_ENCRYPTION_KEY"))
	if raw == "" {
		return nil, false
	}
	if len(raw) == 64 {
		if key, err := hex.DecodeString(raw); err == nil && len(key) == 32 {
			return key, true
		}
	}
	if key, err := base64.StdEncoding.DecodeString(raw); err == nil && len(key) == 32 {
		return key, true
	}
	sum := sha256.Sum256([]byte("9router-go:credential-key:v1:" + raw))
	return sum[:], true
}

// EncryptSecret encrypts a plaintext secret using AES-256-GCM when CREDENTIAL_ENCRYPTION_KEY
// is configured. When unset, returns plain unchanged for shared SQLite compatibility.
func EncryptSecret(plain string) (string, error) {
	if plain == "" || strings.HasPrefix(plain, encryptedPrefix) {
		return plain, nil
	}
	key, ok := credentialKeyBytes()
	if !ok {
		return plain, nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("init cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("init gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return encryptedPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptSecret decrypts an "enc:v1:" value. Plaintext values without the prefix are
// returned unchanged so legacy/shared-dashboard rows remain readable.
func DecryptSecret(stored string) (string, error) {
	if !strings.HasPrefix(stored, encryptedPrefix) {
		return stored, nil
	}
	key, ok := credentialKeyBytes()
	if !ok {
		return "", ErrMissingCredentialKey
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encryptedPrefix))
	if err != nil {
		return "", fmt.Errorf("decode encrypted secret: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("init cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("init gcm: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(payload) < nonceSize {
		return "", fmt.Errorf("encrypted secret truncated")
	}
	plain, err := gcm.Open(nil, payload[:nonceSize], payload[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	return string(plain), nil
}

// ProtectConnectionData encrypts sensitive string fields in a providerConnections.data JSON blob
// while keeping non-secret fields (e.g. modelLock_*, backoffLevel, projectId) plaintext so
// SQLite json_set lock operations continue working without full-row rewrites.
func ProtectConnectionData(rawJSON string) (string, error) {
	if rawJSON == "" || !HasCredentialEncryptionKey() {
		return rawJSON, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &m); err != nil || m == nil {
		return rawJSON, nil
	}
	changed := false
	for k := range secretConnectionKeys {
		if val, ok := m[k].(string); ok && val != "" && !strings.HasPrefix(val, encryptedPrefix) {
			enc, err := EncryptSecret(val)
			if err != nil {
				return "", err
			}
			m[k] = enc
			changed = true
		}
	}
	if !changed {
		return rawJSON, nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal protected connection data: %w", err)
	}
	return string(out), nil
}

// UnprotectConnectionData decrypts any "enc:v1:" sensitive fields in providerConnections.data.
func UnprotectConnectionData(rawJSON string) (string, error) {
	if rawJSON == "" || !strings.Contains(rawJSON, encryptedPrefix) {
		return rawJSON, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &m); err != nil || m == nil {
		return rawJSON, nil
	}
	changed := false
	for k, rawVal := range m {
		val, ok := rawVal.(string)
		if !ok || !strings.HasPrefix(val, encryptedPrefix) {
			continue
		}
		plain, err := DecryptSecret(val)
		if err != nil {
			return "", err
		}
		m[k] = plain
		changed = true
	}
	if !changed {
		return rawJSON, nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal unprotected connection data: %w", err)
	}
	return string(out), nil
}

// MigrateEncryptConnections encrypts any plaintext secret fields in existing
// providerConnections rows when CREDENTIAL_ENCRYPTION_KEY is set.
func MigrateEncryptConnections(database *sql.DB) (int, error) {
	if database == nil || !HasCredentialEncryptionKey() {
		return 0, nil
	}
	ctx := context.Background()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin migration tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, "SELECT id, data FROM providerConnections")
	if err != nil {
		return 0, fmt.Errorf("scan providerConnections: %w", err)
	}
	type rowUpdate struct {
		id   string
		data string
	}
	var updates []rowUpdate
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan row: %w", err)
		}
		protected, err := ProtectConnectionData(data)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("protect connection %s: %w", id, err)
		}
		if protected != data {
			updates = append(updates, rowUpdate{id: id, data: protected})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate providerConnections: %w", err)
	}
	rows.Close()

	for _, u := range updates {
		if _, err := tx.ExecContext(ctx, "UPDATE providerConnections SET data = ? WHERE id = ?", u.data, u.id); err != nil {
			return 0, fmt.Errorf("update encrypted connection %s: %w", u.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit migration tx: %w", err)
	}
	return len(updates), nil
}
