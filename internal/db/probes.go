package db

import (
	"context"
	"database/sql"
	"time"
)

// ProbeResult is what one trial completion established about a model.
type ProbeResult struct {
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	OK        bool      `json:"ok"`
	CheckedAt time.Time `json:"checked_at"`
	LatencyMs int       `json:"latency_ms"`
	Failure   string    `json:"failure,omitempty"`
}

// RecordProbe stores the outcome of trying a model.
func RecordProbe(ctx context.Context, database *sql.DB, r ProbeResult) error {
	var failure any
	if r.Failure != "" {
		failure = r.Failure
	}

	_, err := database.ExecContext(ctx,
		`INSERT INTO model_probes (provider, model, ok, latency_ms, failure, checked_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())
		 ON CONFLICT (provider, model) DO UPDATE SET
		   ok = EXCLUDED.ok,
		   latency_ms = EXCLUDED.latency_ms,
		   failure = EXCLUDED.failure,
		   checked_at = NOW()`,
		r.Provider, r.Model, r.OK, r.LatencyMs, failure)
	return err
}

// ListProbes returns every probe result, most recently checked first.
func ListProbes(ctx context.Context, database *sql.DB) ([]ProbeResult, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT provider, model, ok, checked_at, latency_ms, COALESCE(failure, '')
		 FROM model_probes ORDER BY ok DESC, latency_ms ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProbeResult{}
	for rows.Next() {
		var r ProbeResult
		if err := rows.Scan(&r.Provider, &r.Model, &r.OK, &r.CheckedAt, &r.LatencyMs, &r.Failure); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ConfirmedModels returns the models a probe found working within the window.
//
// Stale results are excluded rather than trusted: an account's entitlements
// and balance change, and a model that worked last week is not evidence about
// today.
func ConfirmedModels(ctx context.Context, database *sql.DB, within time.Duration) (map[string]bool, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT provider, model FROM model_probes
		 WHERE ok AND checked_at > NOW() - $1::interval`,
		intervalArg(within))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var p, m string
		if err := rows.Scan(&p, &m); err != nil {
			return nil, err
		}
		out[TagKey(p, m)] = true
	}
	return out, rows.Err()
}

// ForgetProbes drops a provider's results, for when its key changes and its
// entitlements may have changed with it.
func ForgetProbes(ctx context.Context, database *sql.DB, providerName string) error {
	_, err := database.ExecContext(ctx,
		`DELETE FROM model_probes WHERE provider = $1`, providerName)
	return err
}
