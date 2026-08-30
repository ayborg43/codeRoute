package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coderouter/coderouter/internal/provider"
	"github.com/coderouter/coderouter/migrations"
)

// newTestDB connects to the database named by TEST_DATABASE_URL, applies the
// migrations, and clears the tables these tests touch. Without that variable
// the whole file skips, so `go test ./...` stays runnable with no services.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping database integration tests")
	}

	database, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := database.Ping(); err != nil {
		t.Fatalf("ping %s: %v", url, err)
	}
	t.Cleanup(func() { database.Close() })

	lockTestDB(t, database)

	if err := Migrate(database, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	truncate(t, database)
	return database
}

func truncate(t *testing.T, database *sql.DB) {
	truncateAll(t, database)
}

// logUsage writes a usage row directly, standing in for a completed request.
func logUsage(t *testing.T, database *sql.DB, keyID, model, provider, status string, in, out, latency int, cost float64, cacheHit bool) {
	t.Helper()

	var key any
	if keyID != "" {
		key = keyID
	}
	_, err := database.Exec(
		`INSERT INTO usage_logs (api_key_id, model, tokens_in, tokens_out, latency_ms, cost_usd, provider, status, task, cache_hit)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'conversation', $9)`,
		key, model, in, out, latency, cost, provider, status, cacheHit)
	if err != nil {
		t.Fatalf("insert usage: %v", err)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	database := newTestDB(t)

	// A second pass must be a no-op; migrations already applied are skipped
	// and the files themselves are written to be re-runnable.
	if err := Migrate(database, migrations.FS); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 7 {
		t.Errorf("only %d migrations recorded; expected every file", n)
	}
}

func TestValidateClientKeyRejects(t *testing.T) {
	database := newTestDB(t)
	raw, _ := CreateClientKey(database, "k")

	for _, header := range []string{"", "Bearer ", "Bearer nonsense", "Bearer cr_wrong", raw + "x"} {
		if _, err := ValidateClientKey(database, header); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ValidateClientKey(%q) = %v, want ErrInvalidKey", header, err)
		}
	}

	// The header parses with or without the Bearer prefix.
	if _, err := ValidateClientKey(database, raw); err != nil {
		t.Errorf("a bare key was rejected: %v", err)
	}
}

func TestRevokedKeyStopsWorkingButKeepsItsHistory(t *testing.T) {
	database := newTestDB(t)
	raw, _ := CreateClientKey(database, "k")

	key, err := ValidateClientKey(database, raw)
	if err != nil {
		t.Fatal(err)
	}
	logUsage(t, database, key.ID, "gpt-4o-mini", "openai", "success", 10, 20, 100, 0.001, false)

	if err := RevokeClientKey(database, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateClientKey(database, raw); !errors.Is(err, ErrKeyDisabled) {
		t.Errorf("a revoked key returned %v, want ErrKeyDisabled", err)
	}

	// Revoking twice is not a silent success; the second call has nothing to do.
	if err := RevokeClientKey(database, key.ID); err == nil {
		t.Error("revoking an already-revoked key reported success")
	}

	// The usage row outlives the key, which is the point of revoking rather
	// than deleting.
	var rows int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM usage_logs WHERE api_key_id = $1`, key.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("usage history was lost on revocation: %d rows", rows)
	}
}

func TestSummarizeSeparatesCacheHits(t *testing.T) {
	database := newTestDB(t)

	logUsage(t, database, "", "gpt-4o-mini", "openai", "success", 100, 100, 1000, 0.02, false)
	logUsage(t, database, "", "gpt-4o-mini", "openai", "success", 100, 100, 2000, 0.02, false)
	logUsage(t, database, "", "gpt-4o-mini", "cache", "success", 0, 0, 3, 0, true)
	logUsage(t, database, "", "gpt-4o-mini", "openai", "error", 0, 0, 500, 0, false)

	s, err := Summarize(context.Background(), database, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if s.Requests != 4 {
		t.Errorf("requests = %d, want 4", s.Requests)
	}
	if s.CacheHits != 1 {
		t.Errorf("cache hits = %d, want 1", s.CacheHits)
	}
	if s.Errors != 1 {
		t.Errorf("errors = %d, want 1", s.Errors)
	}
	// A 3ms replay must not drag the upstream latency average down.
	if s.AvgLatencyMs != 1500 {
		t.Errorf("avg latency = %v, want 1500 (upstream successes only)", s.AvgLatencyMs)
	}
	if s.TokensIn != 200 || s.TokensOut != 200 {
		t.Errorf("tokens = %d/%d", s.TokensIn, s.TokensOut)
	}
	if s.CacheHitRate != 0.25 {
		t.Errorf("cache hit rate = %v, want 0.25", s.CacheHitRate)
	}
}

func TestSummarizeRespectsTheWindow(t *testing.T) {
	database := newTestDB(t)

	logUsage(t, database, "", "m", "openai", "success", 1, 1, 10, 0, false)
	if _, err := database.Exec(
		`UPDATE usage_logs SET created_at = NOW() - INTERVAL '3 hours'`); err != nil {
		t.Fatal(err)
	}

	recent, err := Summarize(context.Background(), database, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if recent.Requests != 0 {
		t.Errorf("a 3-hour-old row appeared in a 1-hour window")
	}

	wide, err := Summarize(context.Background(), database, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if wide.Requests != 1 {
		t.Errorf("the row is missing from a 24-hour window")
	}
}

func TestRecentUsageNamesTheKey(t *testing.T) {
	database := newTestDB(t)
	raw, _ := CreateClientKey(database, "laptop")
	key, _ := ValidateClientKey(database, raw)

	logUsage(t, database, key.ID, "gpt-4o", "openai", "success", 5, 6, 900, 0.03, false)

	entries, err := RecentUsage(context.Background(), database, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}

	e := entries[0]
	if e.KeyName != "laptop" {
		t.Errorf("key name not resolved: %+v", e)
	}
	if e.CostUSD != 0.03 || e.TokensIn != 5 || e.TokensOut != 6 {
		t.Errorf("row = %+v", e)
	}
}

// Usage rows outlive the key that made them, so the join must not drop them.
func TestRecentUsageSurvivesKeyRevocation(t *testing.T) {
	database := newTestDB(t)
	raw, _ := CreateClientKey(database, "laptop")
	key, _ := ValidateClientKey(database, raw)

	logUsage(t, database, key.ID, "gpt-4o", "openai", "success", 5, 6, 900, 0.03, false)
	if err := RevokeClientKey(database, key.ID); err != nil {
		t.Fatal(err)
	}

	entries, err := RecentUsage(context.Background(), database, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("history vanished after revocation: %d entries", len(entries))
	}
}

func TestModelUsageBreakdown(t *testing.T) {
	database := newTestDB(t)

	logUsage(t, database, "", "gpt-4o-mini", "openai", "success", 10, 10, 100, 0.01, false)
	logUsage(t, database, "", "gpt-4o-mini", "openai", "success", 10, 10, 300, 0.01, false)
	logUsage(t, database, "", "claude-3-haiku-20240307", "anthropic", "success", 5, 5, 50, 0.005, false)

	rows, err := ModelUsage(context.Background(), database, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d models, want 2", len(rows))
	}
	// Busiest first.
	if rows[0].Model != "gpt-4o-mini" || rows[0].Requests != 2 {
		t.Errorf("first row = %+v", rows[0])
	}
	if rows[0].Tokens != 40 || rows[0].LatencyMs != 200 {
		t.Errorf("aggregate wrong: %+v", rows[0])
	}
}

func TestProviderKeyStorageRoundTrip(t *testing.T) {
	database := newTestDB(t)
	key := []byte("0123456789abcdef")

	if err := StoreProviderKey(database, key, "openai", "sk-secret"); err != nil {
		t.Fatal(err)
	}

	got, err := ProviderKey(database, key, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-secret" {
		t.Errorf("round-tripped as %q", got)
	}

	if _, err := ProviderKey(database, key, "anthropic"); !errors.Is(err, ErrNoProviderKey) {
		t.Errorf("missing provider = %v, want ErrNoProviderKey", err)
	}
}

// The list is what the dashboard renders, so it must carry a fingerprint and
// never the key.
func TestListProviderKeysFingerprintsWithoutRevealing(t *testing.T) {
	database := newTestDB(t)
	key := []byte("0123456789abcdef")

	if err := StoreProviderKey(database, key, "openai", "sk-secret"); err != nil {
		t.Fatal(err)
	}

	listed, err := ListProviderKeys(database, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("got %d entries", len(listed))
	}
	if listed[0].Fingerprint != FingerprintKey("sk-secret") {
		t.Errorf("fingerprint = %q", listed[0].Fingerprint)
	}
	if listed[0].CreatedAt.IsZero() || listed[0].UpdatedAt.IsZero() {
		t.Errorf("timestamps missing: %+v", listed[0])
	}
}

// If ENCRYPTION_KEY changes, stored keys stop decrypting. That must be visible
// on the dashboard rather than the row silently vanishing — every request
// through that provider is already failing.
func TestUndecryptableKeyIsSurfacedNotHidden(t *testing.T) {
	database := newTestDB(t)

	if err := StoreProviderKey(database, []byte("0123456789abcdef"), "openai", "sk-secret"); err != nil {
		t.Fatal(err)
	}

	listed, err := ListProviderKeys(database, []byte("fedcba9876543210"))
	if err != nil {
		t.Fatalf("a wrong encryption key made the whole list fail: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("the row disappeared instead of being flagged: %+v", listed)
	}
	if listed[0].Fingerprint != "undecryptable" {
		t.Errorf("fingerprint = %q, want it flagged", listed[0].Fingerprint)
	}
}

func TestStoreProviderKeyReplacesInPlace(t *testing.T) {
	database := newTestDB(t)
	key := []byte("0123456789abcdef")

	if err := StoreProviderKey(database, key, "openai", "sk-first"); err != nil {
		t.Fatal(err)
	}
	if err := StoreProviderKey(database, key, "openai", "sk-second"); err != nil {
		t.Fatal(err)
	}

	listed, err := ListProviderKeys(database, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("rotation created %d rows, want 1", len(listed))
	}

	got, err := ProviderKey(database, key, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if got != "sk-second" {
		t.Errorf("after rotation the key is %q", got)
	}
}

func TestDeleteProviderKey(t *testing.T) {
	database := newTestDB(t)
	key := []byte("0123456789abcdef")

	if err := StoreProviderKey(database, key, "openai", "sk-secret"); err != nil {
		t.Fatal(err)
	}

	existed, err := DeleteProviderKey(database, "openai")
	if err != nil || !existed {
		t.Fatalf("delete = (%v, %v)", existed, err)
	}

	existed, err = DeleteProviderKey(database, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Error("deleting a second time reported that a key was there")
	}
}

// With tenants gone, a client key is a bare credential: it authenticates and
// carries nothing else.
func TestClientKeyIsJustACredential(t *testing.T) {
	database := newTestDB(t)

	raw, err := CreateClientKey(database, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 10 || raw[:3] != "cr_" {
		t.Fatalf("key %q does not look like a client key", raw)
	}

	key, err := ValidateClientKey(database, "Bearer "+raw)
	if err != nil {
		t.Fatal(err)
	}
	if key.Name != "laptop" {
		t.Errorf("name = %q", key.Name)
	}
	if key.Disabled() {
		t.Error("a fresh key reported itself revoked")
	}
	if key.CreatedAt.IsZero() {
		t.Error("created_at not populated")
	}
}

func TestListClientKeysHidesHashes(t *testing.T) {
	database := newTestDB(t)

	if _, err := CreateClientKey(database, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateClientKey(database, "b"); err != nil {
		t.Fatal(err)
	}

	keys, err := ListClientKeys(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2", len(keys))
	}

	names := map[string]bool{}
	for _, k := range keys {
		names[k.Name] = true
		if k.ID == "" {
			t.Error("key listed without an id; it could not be revoked")
		}
	}
	if !names["a"] || !names["b"] {
		t.Errorf("names = %+v", names)
	}
}

// A revoked key still appears in the listing, flagged, so the dashboard can
// show what was cut off rather than the row silently vanishing.
func TestRevokedKeysStayListed(t *testing.T) {
	database := newTestDB(t)

	raw, _ := CreateClientKey(database, "leaked")
	key, err := ValidateClientKey(database, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := RevokeClientKey(database, key.ID); err != nil {
		t.Fatal(err)
	}

	keys, err := ListClientKeys(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys", len(keys))
	}
	if !keys[0].Disabled() {
		t.Error("the revoked key is not flagged as such")
	}
}

// Nothing in the schema should still reference tenancy.
func TestTenancyIsGoneFromTheSchema(t *testing.T) {
	database := newTestDB(t)

	var present bool
	if err := database.QueryRow(
		`SELECT to_regclass('public.tenants') IS NOT NULL`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Error("the tenants table still exists")
	}

	for _, table := range []string{"api_keys", "usage_logs"} {
		var n int
		if err := database.QueryRow(
			`SELECT COUNT(*) FROM information_schema.columns
			 WHERE table_name = $1 AND column_name = 'tenant_id'`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s still has a tenant_id column", table)
		}
	}
}

// sql.NullTime marshals as {"Time":...,"Valid":false}, which is a truthy
// object in every JSON consumer that tests the field for presence — the
// dashboard rendered every key as revoked because of exactly that. Nullable
// timestamps must serialise as a timestamp or null.
func TestNullableTimestampsMarshalAsNull(t *testing.T) {
	database := newTestDB(t)

	raw, err := CreateClientKey(database, "fresh")
	if err != nil {
		t.Fatal(err)
	}

	keys, err := ListClientKeys(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys", len(keys))
	}

	encoded, err := json.Marshal(keys[0])
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)

	if strings.Contains(body, `"Valid"`) || strings.Contains(body, `"Time"`) {
		t.Errorf("nullable timestamps leaked their sql.NullTime shape: %s", body)
	}
	if !strings.Contains(body, `"disabled_at":null`) {
		t.Errorf("an active key does not report disabled_at as null: %s", body)
	}
	if !strings.Contains(body, `"last_used_at":null`) {
		t.Errorf("an unused key does not report last_used_at as null: %s", body)
	}

	// Once used and revoked, both become real timestamps.
	if _, err := ValidateClientKey(database, raw); err != nil {
		t.Fatal(err)
	}
	if err := RevokeClientKey(database, keys[0].ID); err != nil {
		t.Fatal(err)
	}

	keys, err = ListClientKeys(database)
	if err != nil {
		t.Fatal(err)
	}
	if keys[0].DisabledAt == nil || keys[0].LastUsedAt == nil {
		t.Fatalf("timestamps not populated after use and revocation: %+v", keys[0])
	}
	if !keys[0].Disabled() {
		t.Error("Disabled() disagrees with a populated disabled_at")
	}
}

func TestDiscoveryReportsWhatChanged(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	first := []provider.DiscoveredModel{
		{Provider: "p", Model: "a"}, {Provider: "p", Model: "b"},
	}
	change, err := ReplaceDiscoveredModels(ctx, database, "p", first)
	if err != nil {
		t.Fatal(err)
	}
	if len(change.Added) != 2 || len(change.Removed) != 0 {
		t.Fatalf("first refresh = %+v", change)
	}

	// b withdrawn, c released.
	second := []provider.DiscoveredModel{
		{Provider: "p", Model: "a"}, {Provider: "p", Model: "c"},
	}
	change, err = ReplaceDiscoveredModels(ctx, database, "p", second)
	if err != nil {
		t.Fatal(err)
	}
	if len(change.Added) != 1 || change.Added[0] != "c" {
		t.Errorf("added = %v, want [c]", change.Added)
	}
	if len(change.Removed) != 1 || change.Removed[0] != "b" {
		t.Errorf("removed = %v, want [b]", change.Removed)
	}

	// An unchanged refresh reports nothing, which is what makes a real
	// arrival worth noticing.
	change, err = ReplaceDiscoveredModels(ctx, database, "p", second)
	if err != nil {
		t.Fatal(err)
	}
	if len(change.Added) != 0 || len(change.Removed) != 0 {
		t.Errorf("an unchanged refresh reported %+v", change)
	}
}

// A model available since last month must not look new every time discovery
// runs, or a genuine arrival is impossible to spot.
func TestFirstSeenSurvivesRefreshes(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	models := []provider.DiscoveredModel{{Provider: "p", Model: "steady"}}
	if _, err := ReplaceDiscoveredModels(ctx, database, "p", models); err != nil {
		t.Fatal(err)
	}

	// Backdate it, as if it had been there a while.
	if _, err := database.Exec(
		`UPDATE discovered_models SET first_seen = NOW() - INTERVAL '30 days'`); err != nil {
		t.Fatal(err)
	}

	if _, err := ReplaceDiscoveredModels(ctx, database, "p", models); err != nil {
		t.Fatal(err)
	}

	recent, err := RecentlyAddedModels(ctx, database, 24*time.Hour, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 0 {
		t.Errorf("a month-old model was reported as new after a refresh: %+v", recent)
	}

	// A genuinely new one does show up.
	models = append(models, provider.DiscoveredModel{Provider: "p", Model: "fresh"})
	if _, err := ReplaceDiscoveredModels(ctx, database, "p", models); err != nil {
		t.Fatal(err)
	}
	recent, err = RecentlyAddedModels(ctx, database, 24*time.Hour, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Model != "fresh" {
		t.Errorf("recent = %+v, want just the new model", recent)
	}
}

func TestObserveModelsSummarisesTraffic(t *testing.T) {
	database := newTestDB(t)

	for i := 0; i < 3; i++ {
		logUsage(t, database, "", "m", "p", "success", 10, 10, 1000, 0, false)
	}
	logUsage(t, database, "", "m", "p", "error", 0, 0, 50, 0, false)
	// A cache hit never reached an upstream and must not flatter the model.
	logUsage(t, database, "", "m", "cache", "success", 0, 0, 2, 0, true)

	observed, err := ObserveModels(context.Background(), database, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, o := range observed {
		if o.Provider != "p" {
			continue
		}
		found = true
		if o.Attempts != 4 || o.Successes != 3 {
			t.Errorf("attempts/successes = %d/%d, want 4/3", o.Attempts, o.Successes)
		}
		// Median of successful calls only, so the fast failure is excluded.
		if o.MedianLatencyMs != 1000 {
			t.Errorf("median = %dms, want 1000 (successes only)", o.MedianLatencyMs)
		}
	}
	if !found {
		t.Error("the model was not summarised at all")
	}
	for _, o := range observed {
		if o.Provider == "cache" {
			t.Error("cache hits were counted as upstream traffic")
		}
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

func TestUserPasswordRoundTrip(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	u, err := CreateUser(ctx, database, "  Alice@Example.COM ", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	// Addresses are normalised, so case and spacing cannot create two accounts
	// for the same person.
	if u.Email != "alice@example.com" {
		t.Errorf("email stored as %q", u.Email)
	}

	if _, err := Authenticate(ctx, database, "ALICE@example.com", "correct horse battery"); err != nil {
		t.Errorf("could not sign in with a differently-cased address: %v", err)
	}
	if _, err := Authenticate(ctx, database, "alice@example.com", "wrong password!!"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong password gave %v", err)
	}
}

// A wrong password and an unknown address must be indistinguishable, or anyone
// can discover who has an account.
func TestUnknownEmailAndWrongPasswordAreIndistinguishable(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	if _, err := CreateUser(ctx, database, "real@example.com", "correct horse battery"); err != nil {
		t.Fatal(err)
	}

	_, wrongPassword := Authenticate(ctx, database, "real@example.com", "not the password")
	_, unknownEmail := Authenticate(ctx, database, "ghost@example.com", "not the password")

	if !errors.Is(wrongPassword, ErrInvalidCredentials) || !errors.Is(unknownEmail, ErrInvalidCredentials) {
		t.Fatalf("errors differ: %v vs %v", wrongPassword, unknownEmail)
	}
	if wrongPassword.Error() != unknownEmail.Error() {
		t.Errorf("messages differ and reveal which addresses exist: %q vs %q",
			wrongPassword, unknownEmail)
	}
}

// The password is hashed, never stored as given.
func TestPasswordIsNeverStoredInTheClear(t *testing.T) {
	database := newTestDB(t)
	const password = "a-very-memorable-passphrase"

	if _, err := CreateUser(context.Background(), database, "a@example.com", password); err != nil {
		t.Fatal(err)
	}

	var stored []byte
	if err := database.QueryRow(`SELECT password_hash FROM users`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), password) {
		t.Fatal("the password appears in the stored hash")
	}
	if !strings.HasPrefix(string(stored), "$2") {
		t.Errorf("stored value is not a bcrypt hash: %q", string(stored)[:8])
	}
}

func TestPasswordAndEmailValidation(t *testing.T) {
	for _, weak := range []string{"", "short", "elevenchars"} {
		if err := ValidatePassword(weak); err == nil {
			t.Errorf("password %q was accepted", weak)
		}
	}
	if err := ValidatePassword(strings.Repeat("x", 73)); err == nil {
		t.Error("a password past bcrypt's 72-byte limit was accepted; it would be silently truncated")
	}
	if err := ValidatePassword("twelvechars!"); err != nil {
		t.Errorf("a reasonable password was rejected: %v", err)
	}

	for _, bad := range []string{"", "nobody", "@example.com", "trailing@", "has space@example.com"} {
		if err := ValidateEmail(bad); err == nil {
			t.Errorf("email %q was accepted", bad)
		}
	}
	if err := ValidateEmail("Someone@Example.com"); err != nil {
		t.Errorf("a normal address was rejected: %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	u, err := CreateUser(ctx, database, "a@example.com", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}

	token, expires, err := CreateSession(ctx, database, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "crs_") {
		t.Errorf("session token %q is not recognisable as ours", token)
	}
	if time.Until(expires) > SessionLifetime+time.Minute {
		t.Errorf("session outlives its stated lifetime")
	}

	// Only the hash is stored, so a database dump does not hand over sessions.
	var stored []byte
	if err := database.QueryRow(`SELECT token_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), token) {
		t.Fatal("the raw session token is in the database")
	}

	s, err := ValidateSession(ctx, database, token)
	if err != nil {
		t.Fatal(err)
	}
	if s.Email != "a@example.com" {
		t.Errorf("session resolved to %q", s.Email)
	}

	if err := DeleteSession(ctx, database, token); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateSession(ctx, database, token); !errors.Is(err, ErrNoSession) {
		t.Errorf("a deleted session still validated: %v", err)
	}
}

func TestExpiredSessionsAreRefusedAndPruned(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	u, _ := CreateUser(ctx, database, "a@example.com", "correct horse battery")
	token, _, err := CreateSession(ctx, database, u.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := database.Exec(`UPDATE sessions SET expires_at = NOW() - INTERVAL '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateSession(ctx, database, token); !errors.Is(err, ErrNoSession) {
		t.Errorf("an expired session validated: %v", err)
	}

	n, err := PruneSessions(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	_ = n // the failed validation may already have removed it
}

// Disabling an account must end its sessions, or the person stays signed in.
func TestDisablingAUserEndsItsSessions(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	u, _ := CreateUser(ctx, database, "a@example.com", "correct horse battery")
	token, _, err := CreateSession(ctx, database, u.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := SetUserDisabled(ctx, database, u.ID, true); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateSession(ctx, database, token); err == nil {
		t.Error("a disabled user's session still validated")
	}
	if _, err := Authenticate(ctx, database, "a@example.com", "correct horse battery"); !errors.Is(err, ErrUserDisabled) {
		t.Errorf("a disabled user could still sign in: %v", err)
	}

	// Re-enabling restores sign-in.
	if err := SetUserDisabled(ctx, database, u.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Authenticate(ctx, database, "a@example.com", "correct horse battery"); err != nil {
		t.Errorf("a re-enabled user could not sign in: %v", err)
	}
}

func TestDuplicateEmailIsRefused(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	if _, err := CreateUser(ctx, database, "a@example.com", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateUser(ctx, database, "A@Example.com", "another good passphrase"); err == nil {
		t.Error("the same address was accepted twice with different casing")
	}
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
