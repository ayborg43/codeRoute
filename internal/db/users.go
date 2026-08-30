package db

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is deliberately above the library default. Hashing a password
// should be slow enough to make guessing expensive; ~100ms per attempt costs a
// legitimate sign-in nothing and costs an attacker a great deal.
//
// It is a variable so tests can lower it. A suite that creates a few dozen
// accounts otherwise spends minutes doing nothing but key stretching, and a
// slow suite is one people stop running.
var bcryptCost = 12

// SetBcryptCostForTests lowers the work factor. It refuses anything a real
// deployment might use, so it cannot be called by mistake outside a test.
func SetBcryptCostForTests(cost int) {
	if cost >= 10 {
		panic("SetBcryptCostForTests is for tests only; use the default cost in production")
	}
	bcryptCost = cost
}

// ErrInvalidCredentials is returned for a wrong password and for an email that
// does not exist alike. Distinguishing them would let anyone enumerate who has
// an account.
var ErrInvalidCredentials = errors.New("invalid email or password")

// ErrUserDisabled marks an account that exists but may no longer sign in.
var ErrUserDisabled = errors.New("this account has been disabled")

// User is a dashboard operator.
type User struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
	DisabledAt  *time.Time `json:"disabled_at"`
}

// Disabled reports whether the account has been switched off.
func (u *User) Disabled() bool { return u.DisabledAt != nil }

// NormalizeEmail lowercases and trims an address, so that Alice@Example.com
// and alice@example.com are the same account.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidatePassword rejects passwords too weak to be worth hashing.
//
// The floor is length rather than a composition rule: requiring a digit and a
// symbol reliably produces "Password1!" and little else, while length is what
// actually costs an attacker time.
func ValidatePassword(password string) error {
	const minLength = 12

	if len([]rune(password)) < minLength {
		return fmt.Errorf("password must be at least %d characters", minLength)
	}
	// bcrypt silently truncates beyond 72 bytes, which would make a long
	// passphrase weaker than it looks.
	if len(password) > 72 {
		return errors.New("password must be 72 bytes or fewer")
	}
	return nil
}

// ValidateEmail does the minimum worth doing: an address is only really
// validated by sending something to it, and this gateway sends nothing.
func ValidateEmail(email string) error {
	email = NormalizeEmail(email)
	at := strings.IndexByte(email, '@')

	switch {
	case email == "":
		return errors.New("email is required")
	case len(email) > 320:
		return errors.New("email is too long")
	case at <= 0 || at == len(email)-1:
		return errors.New("email must look like name@example.com")
	case strings.ContainsAny(email, " \t\n"):
		return errors.New("email must not contain spaces")
	}
	return nil
}

// CreateUser adds an operator. The password is hashed before it touches the
// database and is never stored, logged or returned.
func CreateUser(ctx context.Context, database *sql.DB, email, password string) (*User, error) {
	if err := ValidateEmail(email); err != nil {
		return nil, err
	}
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return nil, err
	}

	var u User
	err = database.QueryRowContext(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2)
		 RETURNING id, email, created_at, last_login_at, disabled_at`,
		NormalizeEmail(email), hash,
	).Scan(&u.ID, &u.Email, &u.CreatedAt, &u.LastLoginAt, &u.DisabledAt)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return nil, errors.New("an account with that email already exists")
		}
		return nil, err
	}
	return &u, nil
}

// dummyHash is compared against when no account matches, so that a wrong email
// and a wrong password take the same time to reject. Without it the difference
// is measurable and reveals which addresses have accounts.
//
// Generated on demand rather than once at init, so that lowering the cost for
// tests keeps the two paths comparable — a dummy hash at cost 12 against a real
// one at cost 4 would reintroduce exactly the timing difference it exists to
// remove.
func dummyHashForCost() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("no-such-account"), bcryptCost)
	return h
}

// Authenticate checks an email and password, returning the user on success.
func Authenticate(ctx context.Context, database *sql.DB, email, password string) (*User, error) {
	var u User
	var hash []byte

	err := database.QueryRowContext(ctx,
		`SELECT id, email, password_hash, created_at, last_login_at, disabled_at
		 FROM users WHERE email = $1`,
		NormalizeEmail(email),
	).Scan(&u.ID, &u.Email, &hash, &u.CreatedAt, &u.LastLoginAt, &u.DisabledAt)

	if errors.Is(err, sql.ErrNoRows) {
		// Hash anyway, so the reply takes as long as a real rejection.
		_ = bcrypt.CompareHashAndPassword(dummyHashForCost(), []byte(password))
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}

	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		return nil, ErrInvalidCredentials
	}
	// Checked after the password, so a disabled account cannot be discovered
	// by anyone who does not already know its password.
	if u.Disabled() {
		return nil, ErrUserDisabled
	}

	_, _ = database.ExecContext(ctx, `UPDATE users SET last_login_at = NOW() WHERE id = $1`, u.ID)
	return &u, nil
}

// SetPassword replaces a user's password.
func SetPassword(ctx context.Context, database *sql.DB, userID, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return err
	}

	res, err := database.ExecContext(ctx,
		`UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such user")
	}
	return nil
}

// ListUsers returns every operator. Hashes are never included.
func ListUsers(ctx context.Context, database *sql.DB) ([]User, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT id, email, created_at, last_login_at, disabled_at FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.CreatedAt, &u.LastLoginAt, &u.DisabledAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUserDisabled switches an account off or back on. Disabling also ends that
// user's sessions, since leaving them signed in would make it pointless.
func SetUserDisabled(ctx context.Context, database *sql.DB, userID string, disabled bool) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var when any
	if disabled {
		when = time.Now()
	}
	res, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = $2 WHERE id = $1`, userID, when)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such user")
	}

	if disabled {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CountUsers reports how many accounts exist, for the startup bootstrap.
func CountUsers(ctx context.Context, database *sql.DB) (int, error) {
	var n int
	err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// constantTimeEqual is used where a comparison must not leak by timing.
func constantTimeEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
