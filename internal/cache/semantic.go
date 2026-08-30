// Package cache serves a completion from a previous, near-identical request
// instead of paying an upstream provider for it again.
//
// A hit must clear an explicit similarity threshold, and requests whose output
// is meant to vary are never cached at all.
//
// The cache is global: any client key may be served a completion produced for
// any other. That was not always so — entries used to be scoped per tenant —
// and it is the deliberate consequence of withdrawing multi-tenancy.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coderouter/coderouter/internal/provider"
)

// ErrDisabled means the cache cannot run: pgvector is absent, or no embedder
// was configured. Callers treat it as a miss.
var ErrDisabled = errors.New("semantic cache is disabled")

// pruneInterval bounds how often expired rows are swept, so a busy gateway
// does not issue a DELETE alongside every lookup.
const pruneInterval = 10 * time.Minute

// Stats is a snapshot of cache effectiveness since process start.
type Stats struct {
	Hits    uint64 `json:"hits"`
	Misses  uint64 `json:"misses"`
	Errors  uint64 `json:"errors"`
	Enabled bool   `json:"enabled"`
}

// HitRate is the share of lookups served from cache, 0 when nothing was asked.
func (s Stats) HitRate() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

type SemanticCache struct {
	db        *sql.DB
	embedder  provider.Embedder
	threshold float64
	ttl       time.Duration
	enabled   bool

	hits, misses, errs atomic.Uint64

	mu        sync.Mutex
	lastPrune time.Time
}

// Options configures a cache. An unset Embedder or a database without pgvector
// yields a cache that reports itself disabled and always misses.
type Options struct {
	Embedder provider.Embedder

	// Threshold is the minimum cosine similarity, 0..1, for a stored entry to
	// answer a new prompt. 0.95 is close paraphrase; 1.0 is near-exact.
	Threshold float64

	// TTL bounds how long an entry may answer. Zero means entries never
	// expire, which is rarely what a deployment wants.
	TTL time.Duration
}

// New builds a cache, probing the database for the pgvector-backed table. It
// never fails: a cache that cannot run degrades to always missing.
func New(database *sql.DB, opts Options) *SemanticCache {
	c := &SemanticCache{
		db:        database,
		embedder:  opts.Embedder,
		threshold: opts.Threshold,
		ttl:       opts.TTL,
	}

	switch {
	case opts.Embedder == nil:
		log.Print("cache: no embedder configured; semantic cache disabled")
	case database == nil:
		log.Print("cache: no database; semantic cache disabled")
	case !tableExists(database):
		log.Print("cache: cache_entries is absent (pgvector not installed); semantic cache disabled")
	case opts.Threshold <= 0 || opts.Threshold > 1:
		log.Printf("cache: threshold %v is outside (0,1]; semantic cache disabled", opts.Threshold)
	default:
		c.enabled = true
		log.Printf("cache: semantic cache enabled (threshold %.2f, ttl %s)", opts.Threshold, opts.TTL)
	}

	return c
}

func tableExists(database *sql.DB) bool {
	var present bool
	err := database.QueryRow(`SELECT to_regclass('public.cache_entries') IS NOT NULL`).Scan(&present)
	return err == nil && present
}

func (c *SemanticCache) Enabled() bool { return c != nil && c.enabled }

func (c *SemanticCache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	return Stats{
		Hits:    c.hits.Load(),
		Misses:  c.misses.Load(),
		Errors:  c.errs.Load(),
		Enabled: c.enabled,
	}
}

// Lookup returns a stored completion for a semantically equivalent prompt. A
// miss, a disabled cache, and a failed lookup are all reported as a miss, so
// the caller can simply carry on upstream.
func (c *SemanticCache) Lookup(ctx context.Context, model, prompt string) (string, bool, error) {
	if !c.Enabled() {
		return "", false, ErrDisabled
	}

	embedding, err := c.embedder.Embed(ctx, prompt)
	if err != nil {
		// No embedding key yet is an ordinary state on a fresh deployment, not
		// a fault: miss quietly rather than logging on every request.
		if errors.Is(err, provider.ErrNoKey) {
			c.misses.Add(1)
			return "", false, nil
		}
		c.errs.Add(1)
		return "", false, fmt.Errorf("failed to embed prompt: %w", err)
	}

	var (
		id         string
		response   string
		similarity float64
	)
	err = c.db.QueryRowContext(ctx,
		`SELECT id, response, 1 - (embedding <=> $1::vector) AS similarity
		 FROM cache_entries
		 WHERE model = $2
		   AND (expires_at IS NULL OR expires_at > NOW())
		 ORDER BY embedding <=> $1::vector
		 LIMIT 1`,
		vectorLiteral(embedding), model,
	).Scan(&id, &response, &similarity)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		c.misses.Add(1)
		return "", false, nil
	case err != nil:
		c.errs.Add(1)
		return "", false, err
	}

	// The nearest neighbour is only an answer if it is actually near. Without
	// this the query returns the least-unrelated row for any prompt at all.
	if similarity < c.threshold {
		c.misses.Add(1)
		return "", false, nil
	}

	c.hits.Add(1)
	c.recordHit(id)
	return response, true, nil
}

// Store saves a completion for reuse. Failures are returned but are never
// worth failing a request that already succeeded.
func (c *SemanticCache) Store(ctx context.Context, model, prompt, response string) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	if strings.TrimSpace(response) == "" {
		// An empty completion is not worth replaying.
		return nil
	}

	embedding, err := c.embedder.Embed(ctx, prompt)
	if err != nil {
		if errors.Is(err, provider.ErrNoKey) {
			return nil
		}
		c.errs.Add(1)
		return fmt.Errorf("failed to embed prompt: %w", err)
	}

	var expires any
	if c.ttl > 0 {
		expires = time.Now().Add(c.ttl)
	}

	_, err = c.db.ExecContext(ctx,
		`INSERT INTO cache_entries (embedding, prompt, response, model, expires_at)
		 VALUES ($1::vector, $2, $3, $4, $5)`,
		vectorLiteral(embedding), truncate(prompt, 8000), response, model, expires,
	)
	if err != nil {
		c.errs.Add(1)
		return err
	}

	c.maybePrune(ctx)
	return nil
}

// recordHit updates usage counters. It is best-effort bookkeeping: a failure
// here must not turn a served hit into an error.
func (c *SemanticCache) recordHit(id string) {
	_, err := c.db.Exec(
		`UPDATE cache_entries SET hits = hits + 1, last_hit_at = NOW() WHERE id = $1`, id)
	if err != nil {
		log.Printf("cache: failed to record hit on %s: %v", id, err)
	}
}

// maybePrune drops expired entries, at most once per pruneInterval.
func (c *SemanticCache) maybePrune(ctx context.Context) {
	c.mu.Lock()
	if time.Since(c.lastPrune) < pruneInterval {
		c.mu.Unlock()
		return
	}
	c.lastPrune = time.Now()
	c.mu.Unlock()

	if _, err := c.db.ExecContext(ctx,
		`DELETE FROM cache_entries WHERE expires_at IS NOT NULL AND expires_at <= NOW()`); err != nil {
		log.Printf("cache: prune failed: %v", err)
	}
}

// vectorLiteral renders a vector in pgvector's text input format.
func vectorLiteral(v []float32) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
