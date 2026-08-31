package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// BlacklistedModel is one model an operator has forbidden routing from
// choosing.
type BlacklistedModel struct {
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
}

// ListBlacklistedModels returns every blacklisted model, most recently added
// first.
func ListBlacklistedModels(ctx context.Context, database *sql.DB) ([]BlacklistedModel, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT provider, model, created_at FROM model_blacklist ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []BlacklistedModel{}
	for rows.Next() {
		var m BlacklistedModel
		if err := rows.Scan(&m.Provider, &m.Model, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// BlacklistModel forbids routing from choosing this model. Adding one already
// blacklisted is not an error — the operator got the outcome they asked for.
func BlacklistModel(ctx context.Context, database *sql.DB, providerName, model string) error {
	if strings.TrimSpace(providerName) == "" || strings.TrimSpace(model) == "" {
		return fmt.Errorf("provider and model are required")
	}

	_, err := database.ExecContext(ctx,
		`INSERT INTO model_blacklist (provider, model) VALUES ($1, $2)
		 ON CONFLICT (provider, model) DO NOTHING`,
		providerName, model)
	return err
}

// UnblacklistModel allows routing to choose this model again.
func UnblacklistModel(ctx context.Context, database *sql.DB, providerName, model string) error {
	_, err := database.ExecContext(ctx,
		`DELETE FROM model_blacklist WHERE provider = $1 AND model = $2`,
		providerName, model)
	return err
}
