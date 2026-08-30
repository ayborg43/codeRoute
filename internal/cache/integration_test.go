package cache

import (
	"context"
	"database/sql"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/provider"
	"github.com/coderouter/coderouter/migrations"
)

// angleEmbedder places each known prompt on the unit circle spanned by the
// first two dimensions, so the cosine similarity between any two prompts is
// exactly the cosine of the angle between them. That makes threshold behaviour
// testable to the decimal rather than by hoping a real model cooperates.
type angleEmbedder struct {
	angles map[string]float64
}

func (a *angleEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, provider.EmbeddingDimensions)
	theta := a.angles[text]
	vec[0] = float32(math.Cos(theta))
	vec[1] = float32(math.Sin(theta))
	return vec, nil
}

func newCacheDB(t *testing.T) *sql.DB {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping cache integration tests")
	}

	database, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := database.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	lockTestDB(t, database)

	if err := db.Migrate(database, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !tableExists(database) {
		t.Skip("TEST_DATABASE_URL has no pgvector; skipping cache integration tests")
	}

	truncateAll(t, database)

	return database
}

// degrees is a readable way to express an angle in the tests below.
func degrees(d float64) float64 { return d * math.Pi / 180 }

func TestThresholdIsActuallyApplied(t *testing.T) {
	database := newCacheDB(t)

	// "stored" at 0°, "near" ~8.1° away (cos ≈ 0.990), "far" 60° (cos = 0.5).
	emb := &angleEmbedder{angles: map[string]float64{
		"stored": 0,
		"near":   degrees(8),
		"far":    degrees(60),
	}}

	c := New(database, Options{Embedder: emb, Threshold: 0.95, TTL: time.Hour})
	if !c.Enabled() {
		t.Fatal("cache did not enable against a pgvector database")
	}

	ctx := context.Background()
	if err := c.Store(ctx, "gpt-4o-mini", "stored", "the answer"); err != nil {
		t.Fatal(err)
	}

	// Near enough: served.
	got, hit, err := c.Lookup(ctx, "gpt-4o-mini", "near")
	if err != nil {
		t.Fatal(err)
	}
	if !hit || got != "the answer" {
		t.Errorf("a 0.99-similar prompt missed: (%q, %v)", got, hit)
	}

	// Far away: this is the bug the previous implementation had — it accepted
	// the nearest row whatever its distance, so any prompt got some answer.
	if got, hit, err := c.Lookup(ctx, "gpt-4o-mini", "far"); err != nil {
		t.Fatal(err)
	} else if hit {
		t.Errorf("an unrelated prompt was answered from cache with %q", got)
	}
}

func TestEntriesAreScopedToOneModel(t *testing.T) {
	database := newCacheDB(t)

	emb := &angleEmbedder{angles: map[string]float64{"q": 0}}
	c := New(database, Options{Embedder: emb, Threshold: 0.95, TTL: time.Hour})
	ctx := context.Background()

	if err := c.Store(ctx, "gpt-4o-mini", "q", "cheap answer"); err != nil {
		t.Fatal(err)
	}
	if _, hit, _ := c.Lookup(ctx, "gpt-4o", "q"); hit {
		t.Error("an entry stored for one model answered a request for another")
	}
}

func TestExpiredEntriesAreNotServed(t *testing.T) {
	database := newCacheDB(t)

	emb := &angleEmbedder{angles: map[string]float64{"q": 0}}
	c := New(database, Options{Embedder: emb, Threshold: 0.95, TTL: time.Hour})
	ctx := context.Background()

	if err := c.Store(ctx, "m", "q", "stale"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE cache_entries SET expires_at = NOW() - INTERVAL '1 minute'`); err != nil {
		t.Fatal(err)
	}

	if got, hit, err := c.Lookup(ctx, "m", "q"); err != nil {
		t.Fatal(err)
	} else if hit {
		t.Errorf("an expired entry was served: %q", got)
	}
}

func TestHitsAreCounted(t *testing.T) {
	database := newCacheDB(t)

	emb := &angleEmbedder{angles: map[string]float64{"q": 0}}
	c := New(database, Options{Embedder: emb, Threshold: 0.95, TTL: time.Hour})
	ctx := context.Background()

	if err := c.Store(ctx, "m", "q", "answer"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, hit, _ := c.Lookup(ctx, "m", "q"); !hit {
			t.Fatalf("lookup %d missed", i)
		}
	}

	var hits int
	if err := database.QueryRow(`SELECT hits FROM cache_entries LIMIT 1`).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 3 {
		t.Errorf("stored hit count = %d, want 3", hits)
	}

	stats := c.Stats()
	if stats.Hits != 3 || stats.Misses != 0 {
		t.Errorf("stats = %+v", stats)
	}
	if stats.HitRate() != 1 {
		t.Errorf("hit rate = %v", stats.HitRate())
	}
}

func TestMissesAreCounted(t *testing.T) {
	database := newCacheDB(t)

	emb := &angleEmbedder{angles: map[string]float64{"a": 0, "b": degrees(90)}}
	c := New(database, Options{Embedder: emb, Threshold: 0.95, TTL: time.Hour})
	ctx := context.Background()

	// A miss against an empty table, then a miss against a distant neighbour.
	c.Lookup(ctx, "m", "a")
	c.Store(ctx, "m", "a", "answer")
	c.Lookup(ctx, "m", "b")

	if stats := c.Stats(); stats.Misses != 2 || stats.Hits != 0 {
		t.Errorf("stats = %+v, want 2 misses", stats)
	}
}

func TestEmptyCompletionsAreNotStored(t *testing.T) {
	database := newCacheDB(t)

	emb := &angleEmbedder{angles: map[string]float64{"q": 0}}
	c := New(database, Options{Embedder: emb, Threshold: 0.95, TTL: time.Hour})

	if err := c.Store(context.Background(), "m", "q", "   "); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM cache_entries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("an empty completion was cached (%d rows)", n)
	}
}

// testDBLock serialises the packages that share TEST_DATABASE_URL.
//
// `go test ./...` runs packages in parallel, and these suites all truncate the
// same tables. Without a lock they clobber each other's fixtures and fail at
// random — which is worse than having no suite at all, because the failures
// look like real regressions.
const testDBLock = 8021975

// lockTestDB takes a Postgres advisory lock for the duration of one test, on a
// dedicated connection because the lock is session-scoped and the pool would
// otherwise hand it back mid-test.
func lockTestDB(t *testing.T, database *sql.DB) {
	t.Helper()

	conn, err := database.Conn(context.Background())
	if err != nil {
		t.Fatalf("acquiring a connection for the test lock: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, testDBLock); err != nil {
		conn.Close()
		t.Fatalf("taking the test lock: %v", err)
	}

	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, testDBLock)
		conn.Close()
	})
}

// truncateAll empties every table except the migration ledger.
//
// This was a hand-written list, and it went stale three times as tables were
// added — each time producing failures that looked like real regressions but
// were one suite treading on another's fixtures. Asking the database what
// exists cannot go stale.
func truncateAll(t *testing.T, database *sql.DB) {
	t.Helper()

	tables, err := publicTables(database)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	if len(tables) == 0 {
		return
	}

	// One statement with CASCADE, so foreign keys between them do not dictate
	// an order this helper would then have to know about.
	stmt := "TRUNCATE " + strings.Join(tables, ", ") + " RESTART IDENTITY CASCADE"
	if _, err := database.Exec(stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// publicTables lists what truncateAll should empty.
func publicTables(database *sql.DB) ([]string, error) {
	rows, err := database.Query(
		`SELECT tablename FROM pg_tables
		 WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, `"`+name+`"`)
	}
	return tables, rows.Err()
}
