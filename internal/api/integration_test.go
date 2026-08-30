package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/gateway"
	"github.com/coderouter/coderouter/internal/iot"
	"github.com/coderouter/coderouter/migrations"
)

// liveHandler builds the real handler over the database named by
// TEST_DATABASE_URL. Without that variable every test here skips.
func liveHandler(t *testing.T) (http.Handler, *sql.DB, *config.Config) {
	t.Helper()
	return liveHandlerWith(t, nil)
}

// liveHandlerWith is liveHandler with a hook for tests that need to redirect
// the gateway at a stand-in upstream.
func liveHandlerWith(t *testing.T, adjust func(*config.Config)) (http.Handler, *sql.DB, *config.Config) {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping API integration tests")
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
	truncateAll(t, database)

	cfg := config.Load()
	cfg.AdminToken = testAdminToken
	cfg.Cache.Enabled = false

	if adjust != nil {
		adjust(cfg)
	}

	gw, err := gateway.New(database, gateway.Options{Config: cfg})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	bridge := iot.NewBridge(iot.Config{}, gw, iot.NewStore(database))

	return NewHandler(gw, database, cfg, bridge), database, cfg
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, target any) {
	t.Helper()

	if err := json.Unmarshal(rec.Body.Bytes(), target); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
}

func TestDashboardReportsRealNumbers(t *testing.T) {
	h, database, _ := liveHandler(t)

	if _, err := database.Exec(
		`INSERT INTO usage_logs (model, tokens_in, tokens_out, latency_ms, cost_usd, provider, status, task, cache_hit)
		 VALUES ('gpt-4o-mini',100,100,1000,0.02,'openai','success','conversation',FALSE),
		        ('gpt-4o-mini',0,0,4,0,'cache','success','conversation',TRUE)`); err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, http.MethodGet, "/api/stats", testAdminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats = %d: %s", rec.Code, rec.Body.String())
	}

	var stats struct {
		Summary db.Summary `json:"summary"`
	}
	decode(t, rec, &stats)

	if stats.Summary.Requests != 2 || stats.Summary.CacheHits != 1 {
		t.Errorf("summary = %+v", stats.Summary)
	}
	if stats.Summary.CacheHitRate != 0.5 {
		t.Errorf("cache hit rate = %v, want 0.5", stats.Summary.CacheHitRate)
	}
	// The 4ms replay must not be averaged into upstream latency.
	if stats.Summary.AvgLatencyMs != 1000 {
		t.Errorf("avg latency = %v, want 1000", stats.Summary.AvgLatencyMs)
	}
	if stats.Summary.CostUSD != 0.02 {
		t.Errorf("cost = %v", stats.Summary.CostUSD)
	}

	// Recent activity and the model breakdown both answer.
	for _, path := range []string{"/api/usage", "/api/models"} {
		if rec := do(t, h, http.MethodGet, path, testAdminToken, ""); rec.Code != http.StatusOK {
			t.Errorf("%s = %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestDashboardModelsPairsUsageWithTheCatalogue(t *testing.T) {
	h, _, _ := liveHandler(t)

	rec := do(t, h, http.MethodGet, "/api/models", testAdminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Usage     []db.ModelBreakdown `json:"usage"`
		Catalogue []struct {
			Model       string `json:"model"`
			EstimatedMs int    `json:"estimated_ms"`
			ObservedMs  int    `json:"observed_ms"`
		} `json:"catalogue"`
	}
	decode(t, rec, &body)

	if len(body.Catalogue) == 0 {
		t.Fatal("the catalogue came back empty")
	}
	for _, c := range body.Catalogue {
		if c.EstimatedMs <= 0 {
			t.Errorf("catalogue entry %q has no latency estimate", c.Model)
		}
	}
}

// The management surface is now flat: mint a key, list keys, revoke a key.
// There is nothing to attach a key to and nothing to configure about it.
func TestClientKeyLifecycle(t *testing.T) {
	h, _, _ := liveHandler(t)

	rec := do(t, h, http.MethodPost, "/v1/admin/keys", testAdminToken, `{"name":"vscode"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}

	var created struct {
		Key     string `json:"key"`
		Name    string `json:"name"`
		Warning string `json:"warning"`
	}
	decode(t, rec, &created)
	if !strings.HasPrefix(created.Key, "cr_") || created.Name != "vscode" {
		t.Fatalf("created = %+v", created)
	}
	if created.Warning == "" {
		t.Error("the one-shot key came with no warning that it cannot be retrieved")
	}
	// Nothing tenant-shaped should survive in the response.
	if strings.Contains(rec.Body.String(), "tenant") {
		t.Errorf("the create response still mentions a tenant: %s", rec.Body.String())
	}

	// It works immediately, with no limits applied.
	for i := 0; i < 5; i++ {
		if rec := do(t, h, http.MethodGet, "/v1/models", created.Key, ""); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d; nothing should be throttling it", i, rec.Code)
		}
	}

	// It appears in the listing, without its hash.
	rec = do(t, h, http.MethodGet, "/v1/admin/keys", testAdminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "key_hash") {
		t.Error("the key list leaked a hash")
	}

	var list struct {
		Data []db.ClientKey `json:"data"`
	}
	decode(t, rec, &list)
	if len(list.Data) != 1 || list.Data[0].Name != "vscode" {
		t.Fatalf("list = %+v", list.Data)
	}

	// Revoking is the only control left over a caller.
	rec = do(t, h, http.MethodDelete, "/v1/admin/keys/"+list.Data[0].ID, testAdminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/v1/models", created.Key, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key = %d, want 401", rec.Code)
	}
	if msg := errorMessage(t, rec); !strings.Contains(msg, "revoked") {
		t.Errorf("revoked key error said %q", msg)
	}
}

// Documents the consequence of the change: a key is now unlimited. If limiting
// ever comes back, this test should fail and be reconsidered deliberately.
func TestKeysAreUnlimited(t *testing.T) {
	h, database, _ := liveHandler(t)

	raw, err := db.CreateClientKey(database, "burst")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 50; i++ {
		rec := do(t, h, http.MethodGet, "/v1/models", raw, "")
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate limited; limiting was supposed to be removed", i)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d", i, rec.Code)
		}
	}
}

// The free-only switch is an operator control, so it has to survive a restart:
// an env default that silently overrode it would undo the toggle on redeploy.
func TestFreeOnlyToggleIsPersisted(t *testing.T) {
	h, database, _ := liveHandler(t)

	rec := do(t, h, http.MethodGet, "/api/settings", testAdminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get settings = %d: %s", rec.Code, rec.Body.String())
	}

	var settings struct {
		FreeOnly   bool `json:"free_only"`
		FreeModels int  `json:"free_models"`
	}
	decode(t, rec, &settings)
	if settings.FreeOnly {
		t.Fatal("free-only is on by default")
	}

	// Nothing free is known here, so turning it on would refuse every request.
	rec = do(t, h, http.MethodPut, "/api/settings", testAdminToken, `{"free_only":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enabling with no free models = %d, want 400", rec.Code)
	}
	if msg := errorMessage(t, rec); !strings.Contains(msg, "no free models") {
		t.Errorf("message = %q", msg)
	}

	// Turning it off is always allowed, and is written down.
	rec = do(t, h, http.MethodPut, "/api/settings", testAdminToken, `{"free_only":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disabling = %d: %s", rec.Code, rec.Body.String())
	}

	stored, err := db.FreeOnlySetting(context.Background(), database)
	if err != nil {
		t.Fatalf("the setting was not persisted: %v", err)
	}
	if stored {
		t.Error("stored value disagrees with what was set")
	}
}

func TestSettingsRequireTheAdminToken(t *testing.T) {
	h, _, _ := liveHandler(t)

	for _, m := range []string{http.MethodGet, http.MethodPut} {
		if rec := do(t, h, m, "/api/settings", "", `{"free_only":false}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s /api/settings unauthenticated = %d, want 401", m, rec.Code)
		}
	}
}

func TestSettingsRejectAnEmptyBody(t *testing.T) {
	h, _, _ := liveHandler(t)

	for _, body := range []string{`{}`, `{"something_else":true}`, `not json`} {
		if rec := do(t, h, http.MethodPut, "/api/settings", testAdminToken, body); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", body, rec.Code)
		}
	}
}

// Input and output tokens are reported separately, so a caller can see which
// side of a request is costing them.
func TestUsageReportsInputAndOutputSeparately(t *testing.T) {
	h, database, _ := liveHandler(t)

	if _, err := database.Exec(
		`INSERT INTO usage_logs (model, tokens_in, tokens_out, latency_ms, cost_usd, provider, status, task, cache_hit)
		 VALUES ('m', 120, 34, 500, 0.01, 'openai', 'success', 'conversation', FALSE)`); err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, http.MethodGet, "/api/usage", testAdminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d", rec.Code)
	}

	var list struct {
		Data []db.UsageEntry `json:"data"`
	}
	decode(t, rec, &list)
	if len(list.Data) != 1 {
		t.Fatalf("got %d rows", len(list.Data))
	}
	if list.Data[0].TokensIn != 120 || list.Data[0].TokensOut != 34 {
		t.Errorf("tokens = %d/%d, want 120/34", list.Data[0].TokensIn, list.Data[0].TokensOut)
	}

	rec = do(t, h, http.MethodGet, "/api/stats", testAdminToken, "")
	var stats struct {
		Summary db.Summary `json:"summary"`
	}
	decode(t, rec, &stats)
	if stats.Summary.TokensIn != 120 || stats.Summary.TokensOut != 34 {
		t.Errorf("summary tokens = %d/%d, want 120/34", stats.Summary.TokensIn, stats.Summary.TokensOut)
	}
}

// Marking a model is an operator instruction, so it has to persist and take
// effect on the very next request rather than at the next restart.
func TestModelTagLifecycle(t *testing.T) {
	h, _, _ := liveHandler(t)

	rec := do(t, h, http.MethodGet, "/v1/admin/model-tags", testAdminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}

	var listed struct {
		Data          []map[string]any `json:"data"`
		TaggableTasks []string         `json:"taggable_tasks"`
	}
	decode(t, rec, &listed)
	if len(listed.Data) != 0 {
		t.Fatalf("tags present on a clean database: %+v", listed.Data)
	}
	if len(listed.TaggableTasks) == 0 {
		t.Error("the API does not say which tasks can be marked")
	}

	// Mark one model for coding.
	rec = do(t, h, http.MethodPut, "/v1/admin/model-tags", testAdminToken,
		`{"provider":"openai","model":"gpt-4o-mini","tasks":["code_generation"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set = %d: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/v1/admin/model-tags", testAdminToken, "")
	decode(t, rec, &listed)
	if len(listed.Data) != 1 {
		t.Fatalf("after marking, tags = %+v", listed.Data)
	}

	// Replacing the set removes what is not resent.
	rec = do(t, h, http.MethodPut, "/v1/admin/model-tags", testAdminToken,
		`{"provider":"openai","model":"gpt-4o-mini","tasks":["conversation"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace = %d: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/v1/admin/model-tags", testAdminToken, "")
	decode(t, rec, &listed)
	if len(listed.Data) != 1 {
		t.Fatalf("tags = %+v", listed.Data)
	}
	tasks, _ := listed.Data[0]["tasks"].([]any)
	if len(tasks) != 1 || tasks[0] != "conversation" {
		t.Errorf("tasks = %v; replacing should not merge with what was there", tasks)
	}

	// An empty list clears the marks entirely.
	rec = do(t, h, http.MethodPut, "/v1/admin/model-tags", testAdminToken,
		`{"provider":"openai","model":"gpt-4o-mini","tasks":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/v1/admin/model-tags", testAdminToken, "")
	decode(t, rec, &listed)
	if len(listed.Data) != 0 {
		t.Errorf("clearing left %+v", listed.Data)
	}
}

func TestModelTagValidation(t *testing.T) {
	h, _, _ := liveHandler(t)

	cases := map[string]string{
		"no provider":  `{"model":"m","tasks":["code_generation"]}`,
		"no model":     `{"provider":"openai","tasks":["code_generation"]}`,
		"blank model":  `{"provider":"openai","model":"   ","tasks":[]}`,
		"unknown task": `{"provider":"openai","model":"m","tasks":["vibes"]}`,
		"malformed":    `not json`,
	}
	for name, body := range cases {
		if rec := do(t, h, http.MethodPut, "/v1/admin/model-tags", testAdminToken, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, rec.Code)
		}
	}

	// A provider this build cannot route to is refused rather than stored and
	// silently never matched.
	rec := do(t, h, http.MethodPut, "/v1/admin/model-tags", testAdminToken,
		`{"provider":"acme","model":"m","tasks":["code_generation"]}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown provider = %d, want 404", rec.Code)
	}
}

func TestModelTagsRequireTheAdminToken(t *testing.T) {
	h, _, _ := liveHandler(t)

	for _, m := range []string{http.MethodGet, http.MethodPut} {
		if rec := do(t, h, m, "/v1/admin/model-tags", "", `{}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated = %d, want 401", m, rec.Code)
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

// signIn logs a test user in and returns the session cookie.
func signIn(t *testing.T, h http.Handler, email, password string) *http.Cookie {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"email":"`+email+`","password":"`+password+`"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("login succeeded but set no session cookie")
	return nil
}

func withCookie(t *testing.T, h http.Handler, method, path string, c *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(""))
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLoginGrantsAccessToTheDashboard(t *testing.T) {
	h, database, _ := liveHandler(t)

	if _, err := db.CreateUser(context.Background(), database, "op@example.com", "correct horse battery"); err != nil {
		t.Fatal(err)
	}

	// Before signing in, the dashboard API is closed.
	if rec := withCookie(t, h, http.MethodGet, "/api/stats", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/stats = %d, want 401", rec.Code)
	}

	cookie := signIn(t, h, "op@example.com", "correct horse battery")

	// The cookie must be unreadable by scripts and not sent cross-site.
	if !cookie.HttpOnly {
		t.Error("the session cookie is readable by JavaScript")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if strings.Contains(cookie.Value, "correct horse") {
		t.Fatal("the cookie contains the password")
	}

	if rec := withCookie(t, h, http.MethodGet, "/api/stats", cookie); rec.Code != http.StatusOK {
		t.Errorf("signed-in /api/stats = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := withCookie(t, h, http.MethodGet, "/v1/admin/keys", cookie); rec.Code != http.StatusOK {
		t.Errorf("signed-in /v1/admin/keys = %d", rec.Code)
	}

	// /api/me names the signed-in operator.
	rec := withCookie(t, h, http.MethodGet, "/api/me", cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "op@example.com") {
		t.Errorf("/api/me = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	h, database, _ := liveHandler(t)
	if _, err := db.CreateUser(context.Background(), database, "op@example.com", "correct horse battery"); err != nil {
		t.Fatal(err)
	}

	cookie := signIn(t, h, "op@example.com", "correct horse battery")
	if rec := withCookie(t, h, http.MethodPost, "/api/logout", cookie); rec.Code != http.StatusOK {
		t.Fatalf("logout = %d", rec.Code)
	}

	if rec := withCookie(t, h, http.MethodGet, "/api/stats", cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("the session still worked after signing out: %d", rec.Code)
	}
}

// A wrong password must not reveal whether the address has an account.
func TestLoginDoesNotRevealWhichAccountsExist(t *testing.T) {
	h, database, _ := liveHandler(t)
	if _, err := db.CreateUser(context.Background(), database, "real@example.com", "correct horse battery"); err != nil {
		t.Fatal(err)
	}

	wrong := do(t, h, http.MethodPost, "/api/login", "", `{"email":"real@example.com","password":"nope-nope-nope"}`)
	ghost := do(t, h, http.MethodPost, "/api/login", "", `{"email":"ghost@example.com","password":"nope-nope-nope"}`)

	if wrong.Code != ghost.Code {
		t.Errorf("status codes differ: %d vs %d", wrong.Code, ghost.Code)
	}
	if errorMessage(t, wrong) != errorMessage(t, ghost) {
		t.Errorf("messages differ: %q vs %q", errorMessage(t, wrong), errorMessage(t, ghost))
	}
}

// The admin token keeps working, so scripts do not have to hold a session.
func TestAdminTokenStillWorksAlongsideLogin(t *testing.T) {
	h, _, _ := liveHandler(t)

	if rec := do(t, h, http.MethodGet, "/api/stats", testAdminToken, ""); rec.Code != http.StatusOK {
		t.Errorf("the admin token stopped working: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/me", testAdminToken, ""); rec.Code != http.StatusOK {
		t.Errorf("/api/me with a token = %d", rec.Code)
	}
}

func TestRepeatedFailuresLockTheAccountOut(t *testing.T) {
	h, database, _ := liveHandler(t)
	if _, err := db.CreateUser(context.Background(), database, "op@example.com", "correct horse battery"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < maxFailedAttempts; i++ {
		do(t, h, http.MethodPost, "/api/login", "", `{"email":"op@example.com","password":"wrong-wrong-wrong"}`)
	}

	// Even the correct password is refused while the lockout stands, which is
	// the point: an attacker who guesses it on the sixth try still cannot use it.
	rec := do(t, h, http.MethodPost, "/api/login", "", `{"email":"op@example.com","password":"correct horse battery"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures the login returned %d, want 429", maxFailedAttempts, rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("the lockout came with no Retry-After")
	}
}

func TestChangingPasswordRequiresTheCurrentOne(t *testing.T) {
	h, database, _ := liveHandler(t)
	if _, err := db.CreateUser(context.Background(), database, "op@example.com", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	cookie := signIn(t, h, "op@example.com", "correct horse battery")

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/password", strings.NewReader(body))
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// A borrowed session must not be enough to lock out the owner.
	if rec := post(`{"current_password":"guessing","new_password":"a new long passphrase"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("changed the password without the current one: %d", rec.Code)
	}
	if rec := post(`{"current_password":"correct horse battery","new_password":"short"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("accepted a weak new password: %d", rec.Code)
	}
	if rec := post(`{"current_password":"correct horse battery","new_password":"a new long passphrase"}`); rec.Code != http.StatusOK {
		t.Fatalf("could not change the password: %d %s", rec.Code, rec.Body.String())
	}

	if _, err := db.Authenticate(context.Background(), database, "op@example.com", "a new long passphrase"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}

func TestUserManagement(t *testing.T) {
	h, _, _ := liveHandler(t)

	rec := do(t, h, http.MethodPost, "/v1/admin/users", testAdminToken,
		`{"email":"second@example.com","password":"another long passphrase"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "password") || strings.Contains(rec.Body.String(), "hash") {
		t.Errorf("the response leaked credential material: %s", rec.Body.String())
	}

	var created db.User
	decode(t, rec, &created)

	rec = do(t, h, http.MethodGet, "/v1/admin/users", testAdminToken, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "second@example.com") {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}

	// Disabling is allowed here because ADMIN_TOKEN is set, so there is
	// another way in.
	rec = do(t, h, http.MethodPatch, "/v1/admin/users/"+created.ID, testAdminToken, `{"disabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d: %s", rec.Code, rec.Body.String())
	}

	if rec := do(t, h, http.MethodPost, "/api/login", "",
		`{"email":"second@example.com","password":"another long passphrase"}`); rec.Code != http.StatusForbidden {
		t.Errorf("a disabled account signed in: %d", rec.Code)
	}
}

func TestUserRoutesRequireAuthorization(t *testing.T) {
	h, _, _ := liveHandler(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/admin/users"},
		{http.MethodPost, "/v1/admin/users"},
		{http.MethodPatch, "/v1/admin/users/abc"},
	} {
		if rec := do(t, h, tc.method, tc.path, "", `{}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated = %d, want 401", tc.method, tc.path, rec.Code)
		}
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

func TestPlaygroundRequiresAuthorization(t *testing.T) {
	h, _, _ := liveHandler(t)

	if rec := do(t, h, http.MethodPost, "/api/playground", "", `{"prompt":"hi"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d, want 401", rec.Code)
	}
}

func TestPlaygroundValidatesInput(t *testing.T) {
	h, _, _ := liveHandler(t)

	for name, body := range map[string]string{
		"no prompt":    `{"model":"auto"}`,
		"blank prompt": `{"model":"auto","prompt":"   "}`,
		"malformed":    `not json`,
	} {
		if rec := do(t, h, http.MethodPost, "/api/playground", testAdminToken, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, rec.Code)
		}
	}
}

// With no providers configured the playground must report the gateway's own
// explanation, which names what it tried — that is the whole point of a
// playground when something is not working.
func TestPlaygroundSurfacesRoutingFailures(t *testing.T) {
	h, _, _ := liveHandler(t)

	rec := do(t, h, http.MethodPost, "/api/playground", testAdminToken,
		`{"model":"auto","prompt":"hello"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("= %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if msg := errorMessage(t, rec); !strings.Contains(msg, "provider") {
		t.Errorf("message does not explain the routing failure: %q", msg)
	}
}

// A playground run answers through a real upstream and reports which model
// served it, what it cost and how long it took.
func TestPlaygroundReportsWhatAnswered(t *testing.T) {
	upstream := newPlaygroundUpstream(t)

	h, database, cfg := liveHandlerWith(t, func(c *config.Config) {
		c.ProviderBaseURLs["openai"] = upstream.URL + "/v1"
		c.RoutingMode = "off"
	})
	if err := db.StoreProviderKey(database, cfg.EncryptionKey, "openai", "sk-test"); err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, http.MethodPost, "/api/playground", testAdminToken,
		`{"model":"gpt-4o-mini","prompt":"say hello","max_tokens":64}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d: %s", rec.Code, rec.Body.String())
	}

	var result struct {
		Requested string   `json:"requested"`
		Answered  string   `json:"answered"`
		Provider  string   `json:"provider"`
		Content   string   `json:"content"`
		LatencyMs int64    `json:"latency_ms"`
		TokensIn  int      `json:"tokens_in"`
		TokensOut int      `json:"tokens_out"`
		CostUSD   *float64 `json:"cost_usd"`
	}
	decode(t, rec, &result)

	if result.Content != "hello from the upstream" {
		t.Errorf("content = %q", result.Content)
	}
	if result.Answered != "gpt-4o-mini" || result.Provider != "openai" {
		t.Errorf("answered by %q via %q", result.Answered, result.Provider)
	}
	if result.TokensIn != 11 || result.TokensOut != 5 {
		t.Errorf("tokens = %d/%d, want 11/5", result.TokensIn, result.TokensOut)
	}
	// gpt-4o-mini is priced in the built-in catalogue, so a cost is reported.
	if result.CostUSD == nil {
		t.Error("no cost reported for a model with a published price")
	}
	if result.LatencyMs < 0 {
		t.Errorf("latency = %dms", result.LatencyMs)
	}
}

// Playground traffic is attributed to no client key, so it cannot distort a
// real key's usage figures.
func TestPlaygroundIsNotBilledToAnyKey(t *testing.T) {
	upstream := newPlaygroundUpstream(t)

	h, database, cfg := liveHandlerWith(t, func(c *config.Config) {
		c.ProviderBaseURLs["openai"] = upstream.URL + "/v1"
		c.RoutingMode = "off"
	})
	if err := db.StoreProviderKey(database, cfg.EncryptionKey, "openai", "sk-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateClientKey(database, "someone"); err != nil {
		t.Fatal(err)
	}

	if rec := do(t, h, http.MethodPost, "/api/playground", testAdminToken,
		`{"model":"gpt-4o-mini","prompt":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("= %d: %s", rec.Code, rec.Body.String())
	}

	var attributed int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM usage_logs WHERE api_key_id IS NOT NULL`).Scan(&attributed); err != nil {
		t.Fatal(err)
	}
	if attributed != 0 {
		t.Errorf("%d playground call(s) were billed to a client key", attributed)
	}

	// It is still recorded, so the spend is visible on the dashboard.
	var total int
	if err := database.QueryRow(`SELECT COUNT(*) FROM usage_logs`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("%d usage rows, want the playground call recorded", total)
	}
}

// newPlaygroundUpstream answers a completion with a known body.
func newPlaygroundUpstream(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"gpt-4o-mini",
			"choices":[{"index":0,"finish_reason":"stop",
			"message":{"role":"assistant","content":"hello from the upstream"}}],
			"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}
