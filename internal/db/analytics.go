package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Window is the reporting period the dashboard summarises over.
type Window struct {
	Label string
	Since time.Duration
}

// Summary aggregates usage_logs over a window. Cache hits are separated out
// throughout: counting a replay as a 12ms upstream call would make the gateway
// look faster than it is.
type Summary struct {
	Window string `json:"window"`

	Requests  int64 `json:"requests"`
	CacheHits int64 `json:"cache_hits"`
	Errors    int64 `json:"errors"`

	// AvgLatencyMs covers requests that actually reached a provider.
	AvgLatencyMs float64 `json:"avg_latency_ms"`

	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`

	// CacheHitRate is the share of requests served without an upstream call.
	CacheHitRate float64 `json:"cache_hit_rate"`
}

// Summarize aggregates usage over a window.
func Summarize(ctx context.Context, database *sql.DB, since time.Duration) (*Summary, error) {
	s := &Summary{Window: since.String()}

	err := database.QueryRowContext(ctx,
		`SELECT
		   COUNT(*),
		   COUNT(*) FILTER (WHERE cache_hit),
		   COUNT(*) FILTER (WHERE status <> 'success'),
		   COALESCE(AVG(latency_ms) FILTER (WHERE NOT cache_hit AND status = 'success'), 0),
		   COALESCE(SUM(tokens_in), 0),
		   COALESCE(SUM(tokens_out), 0),
		   COALESCE(SUM(cost_usd), 0)
		 FROM usage_logs
		 WHERE created_at > NOW() - $1::interval`,
		intervalArg(since),
	).Scan(&s.Requests, &s.CacheHits, &s.Errors, &s.AvgLatencyMs, &s.TokensIn, &s.TokensOut, &s.CostUSD)
	if err != nil {
		return nil, err
	}

	if s.Requests > 0 {
		s.CacheHitRate = float64(s.CacheHits) / float64(s.Requests)
	}
	return s, nil
}

// UsageEntry is one row of the recent-activity table.
type UsageEntry struct {
	CreatedAt time.Time `json:"created_at"`
	KeyName   string    `json:"key_name"`
	Model     string    `json:"model"`
	Provider  string    `json:"provider"`
	Task      string    `json:"task"`
	TokensIn  int       `json:"tokens_in"`
	TokensOut int       `json:"tokens_out"`
	LatencyMs int       `json:"latency_ms"`
	CostUSD   float64   `json:"cost_usd"`
	Status    string    `json:"status"`
	CacheHit  bool      `json:"cache_hit"`

	// Failure is the upstream's own words when the call did not succeed.
	Failure string `json:"failure,omitempty"`
}

// RecentUsage lists the latest calls, newest first.
func RecentUsage(ctx context.Context, database *sql.DB, limit int) ([]UsageEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	rows, err := database.QueryContext(ctx,
		`SELECT u.created_at, COALESCE(k.name, ''),
		        u.model, u.provider, COALESCE(u.task, ''),
		        u.tokens_in, u.tokens_out, u.latency_ms,
		        COALESCE(u.cost_usd, 0), u.status, u.cache_hit, COALESCE(u.failure, '')
		 FROM usage_logs u
		 LEFT JOIN api_keys k ON k.id = u.api_key_id
		 ORDER BY u.created_at DESC
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := []UsageEntry{}
	for rows.Next() {
		var e UsageEntry
		if err := rows.Scan(
			&e.CreatedAt, &e.KeyName, &e.Model, &e.Provider, &e.Task,
			&e.TokensIn, &e.TokensOut, &e.LatencyMs, &e.CostUSD, &e.Status, &e.CacheHit, &e.Failure,
		); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ModelBreakdown is per-model activity, for seeing where the spend goes.
type ModelBreakdown struct {
	Model     string  `json:"model"`
	Provider  string  `json:"provider"`
	Requests  int64   `json:"requests"`
	Tokens    int64   `json:"tokens"`
	CostUSD   float64 `json:"cost_usd"`
	LatencyMs float64 `json:"latency_ms"`
}

// ModelUsage breaks a window down by model, busiest first.
func ModelUsage(ctx context.Context, database *sql.DB, since time.Duration) ([]ModelBreakdown, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT model, provider, COUNT(*),
		        COALESCE(SUM(tokens_in + tokens_out), 0),
		        COALESCE(SUM(cost_usd), 0),
		        COALESCE(AVG(latency_ms) FILTER (WHERE status = 'success'), 0)
		 FROM usage_logs
		 WHERE created_at > NOW() - $1::interval
		 GROUP BY model, provider
		 ORDER BY COUNT(*) DESC`,
		intervalArg(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ModelBreakdown{}
	for rows.Next() {
		var m ModelBreakdown
		if err := rows.Scan(&m.Model, &m.Provider, &m.Requests, &m.Tokens, &m.CostUSD, &m.LatencyMs); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// intervalArg renders a duration as a Postgres interval literal. Passing it as
// a parameter rather than interpolating keeps the window uninjectable.
func intervalArg(d time.Duration) string {
	if d <= 0 {
		d = 24 * time.Hour
	}
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}

// ModelObservation is what one model at one provider has actually done.
type ModelObservation struct {
	Provider        string
	Model           string
	Attempts        int
	Successes       int
	MedianLatencyMs int
}

// ObserveModels summarises recent traffic per provider and model.
//
// Cache hits are excluded: they never reached an upstream, so counting them
// would make a well-cached model look flawlessly reliable and instant.
func ObserveModels(ctx context.Context, database *sql.DB, since time.Duration) ([]ModelObservation, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT provider, model,
		        COUNT(*),
		        COUNT(*) FILTER (WHERE status = 'success'),
		        COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms)
		                 FILTER (WHERE status = 'success'), 0)
		 FROM usage_logs
		 WHERE created_at > NOW() - $1::interval AND NOT cache_hit
		 GROUP BY provider, model`,
		intervalArg(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ModelObservation{}
	for rows.Next() {
		var o ModelObservation
		var median float64
		if err := rows.Scan(&o.Provider, &o.Model, &o.Attempts, &o.Successes, &median); err != nil {
			return nil, err
		}
		o.MedianLatencyMs = int(median)
		out = append(out, o)
	}
	return out, rows.Err()
}
