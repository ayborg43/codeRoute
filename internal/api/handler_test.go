package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/gateway"
	"github.com/coderouter/coderouter/internal/iot"
	"github.com/coderouter/coderouter/internal/routing"
)

const testAdminToken = "test-admin-token"

// testHandler builds a handler with no database. Every route exercised here
// answers before it would touch one; anything that needs storage is covered by
// the integration tests instead.
func testHandler(t *testing.T, adminToken string) http.Handler {
	t.Helper()

	cfg := config.Load()
	cfg.AdminToken = adminToken

	gw, err := gateway.New(nil, gateway.Options{Config: cfg})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	bridge := iot.NewBridge(iot.Config{}, gw, nil)

	return NewHandler(gw, nil, cfg, bridge)
}

func do(t *testing.T, h http.Handler, method, path, token string, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response was not a JSON error envelope: %q", rec.Body.String())
	}
	return body.Error.Message
}

// --- admin surface ----------------------------------------------------------

func TestAdminRoutesAreDisabledWithoutAToken(t *testing.T) {
	h := testHandler(t, "")

	// Even holding "the" token cannot reach a disabled management API.
	for _, path := range []string{"/v1/admin/keys", "/v1/admin/providers"} {
		rec := do(t, h, http.MethodGet, path, "anything", "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404 when ADMIN_TOKEN is unset", path, rec.Code)
		}
		if !strings.Contains(errorMessage(t, rec), "ADMIN_TOKEN") {
			t.Errorf("%s did not explain why it is disabled: %q", path, rec.Body.String())
		}
	}
}

func TestAdminRoutesRejectAWrongToken(t *testing.T) {
	h := testHandler(t, testAdminToken)

	cases := []struct{ method, path string }{
		{http.MethodGet, "/v1/admin/keys"},
		{http.MethodPost, "/v1/admin/keys"},
		{http.MethodDelete, "/v1/admin/keys/abc"},
	}

	for _, tc := range cases {
		for _, token := range []string{"", "wrong", testAdminToken + "x", strings.ToUpper(testAdminToken)} {
			rec := do(t, h, tc.method, tc.path, token, "{}")
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q = %d, want 401",
					tc.method, tc.path, token, rec.Code)
			}
		}
	}
}

// A token that is a prefix of the real one must not be accepted; the header is
// sliced before comparison, so this guards against a length mistake there.
func TestAdminTokenIsNotPrefixMatched(t *testing.T) {
	h := testHandler(t, testAdminToken)

	for _, token := range []string{"test", "test-admin", testAdminToken[:len(testAdminToken)-1]} {
		if rec := do(t, h, http.MethodGet, "/v1/admin/keys", token, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("prefix %q was accepted (%d)", token, rec.Code)
		}
	}
}

func TestAdminTokenWithoutBearerPrefixIsRejected(t *testing.T) {
	h := testHandler(t, testAdminToken)

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/keys", nil)
	req.Header.Set("Authorization", testAdminToken) // no "Bearer "
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("raw token without the Bearer prefix = %d, want 401", rec.Code)
	}
}

// --- dashboard surface ------------------------------------------------------

func TestDashboardAPIIsDisabledWithoutAToken(t *testing.T) {
	h := testHandler(t, "")

	for _, path := range []string{"/api/stats", "/api/usage", "/api/keys", "/api/models"} {
		rec := do(t, h, http.MethodGet, path, "anything", "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s = %d, want 503 when ADMIN_TOKEN is unset", path, rec.Code)
		}
		if !strings.Contains(errorMessage(t, rec), "ADMIN_TOKEN") {
			t.Errorf("%s did not explain why it is disabled: %q", path, rec.Body.String())
		}
	}
}

// The previous dashboard served traffic data, and a key-minting endpoint, to
// anyone who could reach the port. It must never be reachable unauthenticated.
func TestDashboardAPIRejectsUnauthenticatedCallers(t *testing.T) {
	h := testHandler(t, testAdminToken)

	for _, path := range []string{"/api/stats", "/api/usage", "/api/keys", "/api/models"} {
		if rec := do(t, h, http.MethodGet, path, "", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with no token = %d, want 401", path, rec.Code)
		}
		if rec := do(t, h, http.MethodGet, path, "wrong", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a wrong token = %d, want 401", path, rec.Code)
		}
	}
}

func TestDashboardAPIRejectsWrongMethods(t *testing.T) {
	h := testHandler(t, testAdminToken)

	// The data API is read-only; minting happens through /v1/admin/keys.
	rec := do(t, h, http.MethodPost, "/api/keys", testAdminToken, "{}")
	if rec.Code == http.StatusOK {
		t.Fatal("POST /api/keys succeeded; the data API should be GET-only")
	}
	if rec := do(t, h, http.MethodPost, "/api/stats", testAdminToken, "{}"); rec.Code == http.StatusOK {
		t.Error("POST /api/stats succeeded; the data API should be GET-only")
	}
}

func TestRootServesTheDashboardPage(t *testing.T) {
	rec := do(t, testHandler(t, testAdminToken), http.MethodGet, "/", "", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("dashboard page is served without nosniff")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "<title>CodeRouter Dashboard</title>") {
		t.Error("the embedded dashboard was not served")
	}
	if strings.Contains(body, "Coming soon") {
		t.Error("the placeholder page is still being served")
	}
	if strings.Contains(body, "placeholder-key-generation") {
		t.Error("the placeholder key generator is still in the page")
	}

	// A minted key is useless without the endpoint to send it to, so the page
	// must show the base URL as well.
	if !strings.Contains(body, `id="base-url"`) {
		t.Error("the page does not show the base URL")
	}
	if !strings.Contains(body, "window.location.origin + '/v1'") {
		t.Error("the base URL is not derived from the address the page was reached on")
	}
}

func TestUnknownPathsAre404(t *testing.T) {
	h := testHandler(t, testAdminToken)

	for _, path := range []string{"/nope", "/v1/nope", "/v2/chat/completions"} {
		rec := do(t, h, http.MethodGet, path, "", "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		if !strings.Contains(errorMessage(t, rec), path) {
			t.Errorf("404 for %s did not name the path: %q", path, rec.Body.String())
		}
	}
}

// --- caller-facing surface --------------------------------------------------

func TestCompletionsRejectsNonPost(t *testing.T) {
	h := testHandler(t, testAdminToken)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if rec := do(t, h, method, "/v1/chat/completions", "", ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /v1/chat/completions = %d, want 405", method, rec.Code)
		}
	}
}

func TestIoTInferenceRejectsNonPost(t *testing.T) {
	h := testHandler(t, testAdminToken)

	if rec := do(t, h, http.MethodGet, "/v1/iot/inference", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/iot/inference = %d, want 405", rec.Code)
	}
}

func TestAuthenticatedRoutesRejectAMissingKey(t *testing.T) {
	h := testHandler(t, testAdminToken)

	cases := []struct{ method, path string }{
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodGet, "/v1/models"},
		{http.MethodGet, "/v1/iot/telemetry?device_id=x"},
		{http.MethodPost, "/v1/iot/inference"},
	}

	for _, tc := range cases {
		rec := do(t, h, tc.method, tc.path, "", `{"model":"gpt-4o-mini","messages":[]}`)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no key = %d, want 401", tc.method, tc.path, rec.Code)
		}
		if msg := errorMessage(t, rec); !strings.Contains(msg, "API key") {
			t.Errorf("%s %s said %q, which does not mention the API key", tc.method, tc.path, msg)
		}
	}
}

// --- helpers ----------------------------------------------------------------

func TestWindowParsing(t *testing.T) {
	cases := map[string]time.Duration{
		"":        24 * time.Hour,
		"1h":      time.Hour,
		"168h":    168 * time.Hour,
		"garbage": 24 * time.Hour,
		"-5h":     24 * time.Hour,
		"0s":      24 * time.Hour,
		// Clamped, so one query cannot scan the whole table.
		"8760h": maxWindow,
	}

	for raw, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/stats?window="+raw, nil)
		if got := window(req); got != want {
			t.Errorf("window(%q) = %s, want %s", raw, got, want)
		}
	}
}

func TestWriteErrorEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusTeapot, "no coffee", "invalid_request_error")

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}

	var body struct {
		Error struct{ Message, Type string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if body.Error.Message != "no coffee" || body.Error.Type != "invalid_request_error" {
		t.Errorf("envelope = %+v", body.Error)
	}
}

// A nil *sql.DB is what the unit tests above pass; this documents that the
// routes they exercise genuinely never reach storage.
var _ *sql.DB = nil

// Truncating a provider-sorted catalogue hid whole providers below the cut:
// with four configured, two were invisible in the dashboard.
func TestCatalogueCapKeepsEveryProviderVisible(t *testing.T) {
	var pool []routing.ModelProfile
	for _, p := range []string{"aaa", "bbb", "ccc", "zzz"} {
		for i := 0; i < 300; i++ {
			pool = append(pool, routing.ModelProfile{Provider: p, Model: fmt.Sprintf("%s-%03d", p, i)})
		}
	}

	got := shareAcrossProviders(pool, 400)
	if len(got) != 400 {
		t.Fatalf("returned %d rows, want the full 400", len(got))
	}

	seen := map[string]int{}
	for _, p := range got {
		seen[p.Provider]++
	}
	for _, p := range []string{"aaa", "bbb", "ccc", "zzz"} {
		if seen[p] == 0 {
			t.Errorf("provider %q vanished from the capped view", p)
		}
	}
	// Roughly even, so no provider is nearly squeezed out either.
	for p, n := range seen {
		if n < 90 {
			t.Errorf("provider %q got only %d of 400 rows", p, n)
		}
	}
}

// A cap must not reorder or duplicate within a provider.
func TestCatalogueCapPreservesOrderWithinAProvider(t *testing.T) {
	var pool []routing.ModelProfile
	for i := 0; i < 10; i++ {
		pool = append(pool, routing.ModelProfile{Provider: "a", Model: fmt.Sprintf("a-%d", i)})
		pool = append(pool, routing.ModelProfile{Provider: "b", Model: fmt.Sprintf("b-%d", i)})
	}

	got := shareAcrossProviders(pool, 6)
	var a, b []string
	for _, p := range got {
		if p.Provider == "a" {
			a = append(a, p.Model)
		} else {
			b = append(b, p.Model)
		}
	}
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("uneven share: a=%v b=%v", a, b)
	}
	if a[0] != "a-0" || a[2] != "a-2" || b[0] != "b-0" {
		t.Errorf("order within a provider was disturbed: a=%v b=%v", a, b)
	}
}

// Fewer models than the cap must pass through untouched.
func TestCatalogueCapIsANoOpBelowTheLimit(t *testing.T) {
	pool := []routing.ModelProfile{{Provider: "a", Model: "one"}, {Provider: "b", Model: "two"}}
	if got := shareAcrossProviders(pool, 400); len(got) != 2 {
		t.Errorf("got %d rows from a 2-row pool", len(got))
	}
}
