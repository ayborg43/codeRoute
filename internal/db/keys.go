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

	"github.com/google/uuid"
)

// ErrInvalidKey is returned for any client key that does not resolve, so
// callers cannot distinguish "unknown" from "malformed".
var ErrInvalidKey = errors.New("invalid API key")

const clientKeyPrefix = "cr_"

// ClientKey is a caller-facing CodeRouter key, as sent by an editor.
type ClientKey struct {
	ID         string
	Name       string
	CreatedAt  string
	LastUsedAt sql.NullTime
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

// ValidateClientKey resolves an Authorization header value to a stored key.
func ValidateClientKey(database *sql.DB, header string) (*ClientKey, error) {
	provided := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if provided == "" {
		return nil, ErrInvalidKey
	}

	hash := sha256.Sum256([]byte(provided))

	var key ClientKey
	var name sql.NullString
	var storedHash []byte
	err := database.QueryRow(
		`SELECT id, name, created_at, last_used_at, key_hash FROM api_keys WHERE key_hash = $1`,
		hash[:],
	).Scan(&key.ID, &name, &key.CreatedAt, &key.LastUsedAt, &storedHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrInvalidKey
		}
		return nil, err
	}
	if subtle.ConstantTimeCompare(storedHash, hash[:]) != 1 {
		return nil, ErrInvalidKey
	}
	key.Name = name.String

	_, _ = database.Exec(`UPDATE api_keys SET last_used_at = NOW() WHERE id = $1`, key.ID)

	return &key, nil
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
