package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"time"
)

// SessionLifetime is how long a sign-in lasts before it must be repeated.
// Long enough for a working day, short enough that a stolen cookie expires.
const SessionLifetime = 12 * time.Hour

// sessionTokenPrefix marks a cookie value as ours in logs and error messages,
// the same way client keys are prefixed.
const sessionTokenPrefix = "crs_"

// ErrNoSession means the cookie is unknown, expired, or belongs to an account
// that has since been disabled.
var ErrNoSession = errors.New("not signed in")

// Session is an authenticated browser session.
type Session struct {
	UserID    string
	Email     string
	ExpiresAt time.Time
}

// CreateSession issues a session token. Only its hash is stored, so a database
// dump does not hand over live sessions; the raw token is returned once and
// lives thereafter only in the caller's cookie.
func CreateSession(ctx context.Context, database *sql.DB, userID string) (string, time.Time, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", time.Time{}, err
	}

	token := sessionTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	hash := sha256.Sum256([]byte(token))
	expires := time.Now().Add(SessionLifetime)

	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		hash[:], userID, expires); err != nil {
		return "", time.Time{}, err
	}

	return token, expires, nil
}

// ValidateSession resolves a cookie value to the signed-in user.
func ValidateSession(ctx context.Context, database *sql.DB, token string) (*Session, error) {
	if token == "" {
		return nil, ErrNoSession
	}

	hash := sha256.Sum256([]byte(token))

	var s Session
	var storedHash []byte
	var disabledAt *time.Time

	err := database.QueryRowContext(ctx,
		`SELECT s.token_hash, s.user_id, s.expires_at, u.email, u.disabled_at
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = $1`,
		hash[:],
	).Scan(&storedHash, &s.UserID, &s.ExpiresAt, &s.Email, &disabledAt)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	if !constantTimeEqual(storedHash, hash[:]) {
		return nil, ErrNoSession
	}
	if time.Now().After(s.ExpiresAt) {
		// Clear it out rather than leaving it to the sweeper, so a replayed
		// expired cookie cannot be reused if the clock moves.
		_, _ = database.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash[:])
		return nil, ErrNoSession
	}
	if disabledAt != nil {
		return nil, ErrUserDisabled
	}

	_, _ = database.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = NOW() WHERE token_hash = $1`, hash[:])

	return &s, nil
}

// DeleteSession signs one browser out.
func DeleteSession(ctx context.Context, database *sql.DB, token string) error {
	if token == "" {
		return nil
	}
	hash := sha256.Sum256([]byte(token))
	_, err := database.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash[:])
	return err
}

// PruneSessions removes expired rows, so the table does not grow forever.
func PruneSessions(ctx context.Context, database *sql.DB) (int64, error) {
	res, err := database.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= NOW()`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
