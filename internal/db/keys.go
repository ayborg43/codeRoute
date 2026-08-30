package db

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidKey is returned for any client key that does not resolve, so
// callers cannot distinguish "unknown" from "malformed".
var ErrInvalidKey = errors.New("invalid API key")

const clientKeyPrefix = "cr_"

// ErrKeyDisabled marks a revoked key, so callers can say why it failed.
var ErrKeyDisabled = errors.New("API key has been revoked")

// ClientKey is a caller-facing CodeRouter key, as sent by an editor. It is a
// credential and nothing more: it carries no allowance, and the only thing
// that can be done to it is revocation.
type ClientKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`

	// Nullable timestamps are pointers rather than sql.NullTime so they
	// marshal as a timestamp or null. sql.NullTime encodes as
	// {"Time":...,"Valid":false}, which is a truthy object in every consumer
	// that checks the field for presence.
	LastUsedAt *time.Time `json:"last_used_at"`
	DisabledAt *time.Time `json:"disabled_at"`
}

// Disabled reports whether the key has been revoked.
func (k *ClientKey) Disabled() bool { return k.DisabledAt != nil }

// timePtr converts a scanned nullable timestamp to a pointer.
func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// CreateClientKey mints a client key, storing only its hash. The raw key is
// returned once and is unrecoverable afterwards.
func CreateClientKey(database *sql.DB, name string) (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}

	rawKey := clientKeyPrefix + base64.RawURLEncoding.EncodeToString(buf)
	hash := sha256.Sum256([]byte(rawKey))
	id := uuid.New().String()

	_, err := database.Exec(
		`INSERT INTO api_keys (id, key_hash, name) VALUES ($1, $2, $3)`,
		id, hash[:], name,
	)
	if err != nil {
		return "", err
	}

	return rawKey, nil
}

// ValidateClientKey resolves an Authorization header to a stored key.
func ValidateClientKey(database *sql.DB, header string) (*ClientKey, error) {
	provided := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if provided == "" {
		return nil, ErrInvalidKey
	}

	hash := sha256.Sum256([]byte(provided))

	var key ClientKey
	var name sql.NullString
	var lastUsed, disabled sql.NullTime
	var storedHash []byte
	err := database.QueryRow(
		`SELECT id, name, created_at, last_used_at, disabled_at, key_hash
		 FROM api_keys WHERE key_hash = $1`,
		hash[:],
	).Scan(&key.ID, &name, &key.CreatedAt, &lastUsed, &disabled, &storedHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrInvalidKey
		}
		return nil, err
	}
	if subtle.ConstantTimeCompare(storedHash, hash[:]) != 1 {
		return nil, ErrInvalidKey
	}
	if disabled.Valid {
		return nil, ErrKeyDisabled
	}
	key.LastUsedAt, key.DisabledAt = timePtr(lastUsed), timePtr(disabled)
	key.Name = name.String

	_, _ = database.Exec(`UPDATE api_keys SET last_used_at = NOW() WHERE id = $1`, key.ID)

	return &key, nil
}

// RevokeClientKey disables a key without deleting it, so its usage history
// stays intact. This is the only way to cut a caller off.
func RevokeClientKey(database *sql.DB, keyID string) error {
	res, err := database.Exec(
		`UPDATE api_keys SET disabled_at = NOW() WHERE id = $1 AND disabled_at IS NULL`,
		keyID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no active key with that id")
	}
	return nil
}

// ListClientKeys returns every client key. Hashes are never returned.
func ListClientKeys(database *sql.DB) ([]ClientKey, error) {
	rows, err := database.Query(
		`SELECT id, name, created_at, last_used_at, disabled_at
		 FROM api_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []ClientKey{}
	for rows.Next() {
		var k ClientKey
		var name sql.NullString
		var lastUsed, disabled sql.NullTime
		if err := rows.Scan(&k.ID, &name, &k.CreatedAt, &lastUsed, &disabled); err != nil {
			return nil, err
		}
		k.Name = name.String
		k.LastUsedAt, k.DisabledAt = timePtr(lastUsed), timePtr(disabled)
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// CountClientKeys reports how many client keys exist, for startup bootstrap.
func CountClientKeys(database *sql.DB) (int, error) {
	var n int
	err := database.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n)
	return n, err
}

// StoreProviderKey encrypts and upserts an upstream provider key (BYOK).
func StoreProviderKey(database *sql.DB, encryptionKey []byte, provider, apiKey string) error {
	encrypted, err := encrypt([]byte(apiKey), encryptionKey)
	if err != nil {
		return err
	}

	_, err = database.Exec(
		`INSERT INTO provider_keys (provider, encrypted_key) VALUES ($1, $2)
		 ON CONFLICT (provider) DO UPDATE SET encrypted_key = EXCLUDED.encrypted_key, updated_at = NOW()`,
		provider, encrypted,
	)
	return err
}

// ProviderKeys returns every configured upstream key, decrypted, by provider.
func ProviderKeys(database *sql.DB, encryptionKey []byte) (map[string]string, error) {
	rows, err := database.Query(`SELECT provider, encrypted_key FROM provider_keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make(map[string]string)
	for rows.Next() {
		var provider string
		var encrypted []byte
		if err := rows.Scan(&provider, &encrypted); err != nil {
			return nil, err
		}
		decrypted, err := decrypt(encrypted, encryptionKey)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt key for %s: %w", provider, err)
		}
		keys[provider] = string(decrypted)
	}

	return keys, rows.Err()
}

// FingerprintKey renders a short, non-reversible identifier for logs.
func FingerprintKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:4])
}

func encrypt(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return []byte(base64.StdEncoding.EncodeToString(ciphertext)), nil
}

func decrypt(ciphertext, key []byte) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(string(ciphertext))
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}

	nonce, data := data[:nonceSize], data[nonceSize:]
	return gcm.Open(nil, nonce, data, nil)
}

// ProviderKeyInfo describes a stored upstream key without revealing it. The
// fingerprint is enough to tell two keys apart, or to confirm which one is
// deployed, and reverses to nothing.
type ProviderKeyInfo struct {
	Provider    string    `json:"provider"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ListProviderKeys reports which upstream keys are stored. It decrypts each one
// only to fingerprint it; the plaintext never leaves this function.
func ListProviderKeys(database *sql.DB, encryptionKey []byte) ([]ProviderKeyInfo, error) {
	rows, err := database.Query(
		`SELECT provider, encrypted_key, created_at, updated_at FROM provider_keys ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProviderKeyInfo{}
	for rows.Next() {
		var info ProviderKeyInfo
		var encrypted []byte
		if err := rows.Scan(&info.Provider, &encrypted, &info.CreatedAt, &info.UpdatedAt); err != nil {
			return nil, err
		}

		decrypted, err := decrypt(encrypted, encryptionKey)
		if err != nil {
			// A key that will not decrypt is worth surfacing rather than
			// hiding: it means ENCRYPTION_KEY changed, and every request
			// through that provider is already failing.
			info.Fingerprint = "undecryptable"
		} else {
			info.Fingerprint = FingerprintKey(string(decrypted))
		}

		out = append(out, info)
	}
	return out, rows.Err()
}

// DeleteProviderKey removes an upstream key. Reports whether one was there.
func DeleteProviderKey(database *sql.DB, provider string) (bool, error) {
	res, err := database.Exec(`DELETE FROM provider_keys WHERE provider = $1`, provider)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ProviderKey returns one decrypted upstream key, or ErrNoProviderKey when
// none is stored. It is the read path for anything that needs a single key
// rather than the whole set.
func ProviderKey(database *sql.DB, encryptionKey []byte, provider string) (string, error) {
	var encrypted []byte
	err := database.QueryRow(
		`SELECT encrypted_key FROM provider_keys WHERE provider = $1`, provider).Scan(&encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoProviderKey
	}
	if err != nil {
		return "", err
	}

	decrypted, err := decrypt(encrypted, encryptionKey)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt key for %s: %w", provider, err)
	}
	return string(decrypted), nil
}

// ErrNoProviderKey means no upstream key is stored for that provider.
var ErrNoProviderKey = errors.New("no provider key stored")
