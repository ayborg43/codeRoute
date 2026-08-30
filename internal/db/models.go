package db

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"github.com/lib/pq"

	"github.com/coderouter/coderouter/internal/provider"
)

// ModelChange is what one refresh did to a provider's list.
type ModelChange struct {
	Provider string   `json:"provider"`
	Added    []string `json:"added"`
	Removed  []string `json:"removed"`
}

// ReplaceDiscoveredModels swaps in a provider's model list atomically, so a
// concurrent read sees either the previous list or the new one and never a
// half-written catalogue. It reports what changed, so a newly released model
// is something an operator can be told about rather than having to notice.
func ReplaceDiscoveredModels(ctx context.Context, database *sql.DB, providerName string, models []provider.DiscoveredModel) (ModelChange, error) {
	change := ModelChange{Provider: providerName, Added: []string{}, Removed: []string{}}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return change, err
	}
	defer tx.Rollback()

	previous, err := existingModels(ctx, tx, providerName)
	if err != nil {
		return change, err
	}

	current := make(map[string]bool, len(models))
	for _, m := range models {
		current[m.Model] = true
		if !previous[m.Model] {
			change.Added = append(change.Added, m.Model)
		}
	}
	for m := range previous {
		if !current[m] {
			change.Removed = append(change.Removed, m)
		}
	}
	sort.Strings(change.Added)
	sort.Strings(change.Removed)

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM discovered_models WHERE provider = $1 AND model <> ALL($2)`,
		providerName, pq.Array(modelNames(models))); err != nil {
		return change, err
	}

	// first_seen survives an update, so a model that has been available since
	// last month does not look new every time discovery runs.
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO discovered_models
		   (provider, model, input_cost_per_1m, output_cost_per_1m, price_known, context_length)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (provider, model) DO UPDATE SET
		   input_cost_per_1m = EXCLUDED.input_cost_per_1m,
		   output_cost_per_1m = EXCLUDED.output_cost_per_1m,
		   price_known = EXCLUDED.price_known,
		   context_length = EXCLUDED.context_length,
		   discovered_at = NOW(),
		   last_seen = NOW()`)
	if err != nil {
		return change, err
	}
	defer stmt.Close()

	for _, m := range models {
		if _, err := stmt.ExecContext(ctx,
			providerName, m.Model, m.InputCostPer1M, m.OutputCostPer1M, m.PriceKnown, m.ContextLength,
		); err != nil {
			return change, err
		}
	}

	return change, tx.Commit()
}

// existingModels reads a provider's current list, so a refresh can report the
// difference rather than only the result.
func existingModels(ctx context.Context, tx *sql.Tx, providerName string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT model FROM discovered_models WHERE provider = $1`, providerName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out[m] = true
	}
	return out, rows.Err()
}

func modelNames(models []provider.DiscoveredModel) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.Model)
	}
	return out
}

// NewModel is a model that appeared recently.
type NewModel struct {
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	Free      bool      `json:"free"`
	FirstSeen time.Time `json:"first_seen"`
}

// RecentlyAddedModels lists what has appeared within the window, newest first.
func RecentlyAddedModels(ctx context.Context, database *sql.DB, since time.Duration, limit int) ([]NewModel, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := database.QueryContext(ctx,
		`SELECT provider, model,
		        price_known AND input_cost_per_1m = 0 AND output_cost_per_1m = 0,
		        first_seen
		 FROM discovered_models
		 WHERE first_seen > NOW() - $1::interval
		 ORDER BY first_seen DESC
		 LIMIT $2`,
		intervalArg(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []NewModel{}
	for rows.Next() {
		var m NewModel
		if err := rows.Scan(&m.Provider, &m.Model, &m.Free, &m.FirstSeen); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DiscoveredModels returns the cached model lists, grouped by provider.
func DiscoveredModels(ctx context.Context, database *sql.DB) (map[string][]provider.DiscoveredModel, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT provider, model, input_cost_per_1m, output_cost_per_1m, price_known, context_length
		 FROM discovered_models ORDER BY provider, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]provider.DiscoveredModel{}
	for rows.Next() {
		var m provider.DiscoveredModel
		if err := rows.Scan(&m.Provider, &m.Model, &m.InputCostPer1M, &m.OutputCostPer1M,
			&m.PriceKnown, &m.ContextLength); err != nil {
			return nil, err
		}
		out[m.Provider] = append(out[m.Provider], m)
	}
	return out, rows.Err()
}

// ForgetDiscoveredModels drops a provider's cached list, for when its key goes.
func ForgetDiscoveredModels(ctx context.Context, database *sql.DB, providerName string) error {
	_, err := database.ExecContext(ctx,
		`DELETE FROM discovered_models WHERE provider = $1`, providerName)
	return err
}
