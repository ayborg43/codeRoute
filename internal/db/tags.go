package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// ModelTag records that an operator considers one model suited to one kind of
// work. It is a statement rather than the inference routing otherwise falls
// back on, and it is treated as authoritative.
type ModelTag struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Task     string `json:"task"`
}

// ListModelTags returns every tag, grouped by provider and model.
func ListModelTags(ctx context.Context, database *sql.DB) (map[string][]string, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT provider, model, task FROM model_tags ORDER BY provider, model, task`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var t ModelTag
		if err := rows.Scan(&t.Provider, &t.Model, &t.Task); err != nil {
			return nil, err
		}
		out[TagKey(t.Provider, t.Model)] = append(out[TagKey(t.Provider, t.Model)], t.Task)
	}
	return out, rows.Err()
}

// TagKey identifies one model at one provider.
func TagKey(providerName, model string) string { return providerName + "/" + model }

// SetModelTags replaces the tags on one model. An empty list clears them,
// which returns that model to being judged by inference.
//
// Replacing rather than adding keeps the dashboard's checkboxes honest: what
// is shown is exactly what is stored.
func SetModelTags(ctx context.Context, database *sql.DB, providerName, model string, tasks []string) error {
	if strings.TrimSpace(providerName) == "" || strings.TrimSpace(model) == "" {
		return fmt.Errorf("provider and model are required")
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM model_tags WHERE provider = $1 AND model = $2`, providerName, model); err != nil {
		return err
	}

	seen := map[string]bool{}
	for _, task := range tasks {
		task = strings.TrimSpace(task)
		if task == "" || seen[task] {
			continue
		}
		seen[task] = true

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO model_tags (provider, model, task) VALUES ($1, $2, $3)`,
			providerName, model, task); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// TaggedModelsFor lists the models an operator has marked for a task.
func TaggedModelsFor(ctx context.Context, database *sql.DB, task string) ([]ModelTag, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT provider, model, task FROM model_tags WHERE task = $1 ORDER BY provider, model`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ModelTag{}
	for rows.Next() {
		var t ModelTag
		if err := rows.Scan(&t.Provider, &t.Model, &t.Task); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SortedTasks renders a tag list deterministically, for display and tests.
func SortedTasks(tasks []string) []string {
	out := append([]string{}, tasks...)
	sort.Strings(out)
	return out
}
