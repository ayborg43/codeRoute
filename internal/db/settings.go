package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Settings that an operator can change at runtime live in the config table
// introduced by migration 001, which has been unused until now. They are
// deliberately few: anything that belongs in a deploy's environment stays
// there, and only what an operator needs to flip mid-session lands here.
const settingFreeOnly = "free_only"

// ErrNoSetting means the key has never been written, so the deployment's
// configured default still stands.
var ErrNoSetting = errors.New("setting not stored")

// GetBoolSetting reads a stored boolean override.
func GetBoolSetting(ctx context.Context, database *sql.DB, key string) (bool, error) {
	var raw []byte
	err := database.QueryRowContext(ctx,
		`SELECT value FROM config WHERE key = $1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNoSetting
	}
	if err != nil {
		return false, err
	}

	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		// A value written by hand in the wrong shape should not wedge the
		// gateway; treat it as absent and let the default stand.
		return false, ErrNoSetting
	}
	return v, nil
}

// SetBoolSetting stores an override, replacing any previous value.
func SetBoolSetting(ctx context.Context, database *sql.DB, key string, value bool) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}

	_, err = database.ExecContext(ctx,
		`INSERT INTO config (key, value) VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
		key, raw)
	return err
}

// FreeOnlySetting reads the stored free-only override.
func FreeOnlySetting(ctx context.Context, database *sql.DB) (bool, error) {
	return GetBoolSetting(ctx, database, settingFreeOnly)
}

// SetFreeOnlySetting persists the free-only override.
func SetFreeOnlySetting(ctx context.Context, database *sql.DB, value bool) error {
	return SetBoolSetting(ctx, database, settingFreeOnly, value)
}
