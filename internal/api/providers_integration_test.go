package api

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coderouter/coderouter/internal/config"
	"github.com/coderouter/coderouter/internal/db"
)

// fakeUpstream stands in for a provider's /models endpoint, which is what key
// verification calls. It records how many times it was asked.
func fakeUpstream(t *testing.T, accept string) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)

		if r.Header.Get("Authorization") != "Bearer "+accept {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o-mini"}]}`))
	}))
	t.Cleanup(srv.Close)

	return srv, &calls
}

func providerHandler(t *testing.T, upstream string) (http.Handler, *sql.DB, *config.Config) {
	t.Helper()

	return liveHandlerWith(t, func(c *config.Config) {
		c.ProviderBaseURLs["openai"] = upstream + "/v1"
	})
}

func TestProviderKeyLifecycle(t *testing.T) {
	upstream, calls := fakeUpstream(t, "sk-good")
	h, database, cfg := providerHandler(t, upstream.URL)

	// Nothing configured: every supported provider is still listed, so the
	// dashboard can offer the empty ones.
	rec := do(t, h, http.MethodGet, "/v1/admin/providers", testAdminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}

	var list struct {
		Data []providerStatus `json:"data"`
	}
	decode(t, rec, &list)
	if len(list.Data) < 10 {
		t.Fatalf("got %d providers, expected the full preset list: %+v", len(list.Data), list.Data)
	}
	byName := map[string]providerStatus{}
	for _, p := range list.Data {
		byName[p.Provider] = p
		if p.Configured {
			t.Errorf("%s reported as configured on a clean database", p.Provider)
		}
		if p.Label == "" {
			t.Errorf("%s has no display label", p.Provider)
		}
	}
	// The vendors this build ships presets for must all be offered.
	for _, want := range []string{"openai", "anthropic", "google", "openrouter", "groq",
		"sambanova", "mistral", "nvidia", "huggingface", "gmicloud", "xai",
		"xkiro", "teamorouter", "bai"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("preset %q is not offered", want)
		}
	}

	// Save a key the upstream accepts.
	rec = do(t, h, http.MethodPut, "/v1/admin/providers/openai", testAdminToken, `{"api_key":"sk-good"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
	}
	// Verification is itself a model listing, so saving a key must cost
	// exactly one round trip — not one to verify and another to discover.
	if calls.Load() != 1 {
		t.Errorf("upstream was called %d times; verification should double as discovery", calls.Load())
	}

	var saved providerStatus
	decode(t, rec, &saved)
	if !saved.Configured || saved.Fingerprint == "" {
		t.Fatalf("save returned %+v", saved)
	}
	if strings.Contains(rec.Body.String(), "sk-good") {
		t.Fatal("the response echoed the key back")
	}

	// It now reads as configured, still without the key itself.
	rec = do(t, h, http.MethodGet, "/v1/admin/providers", testAdminToken, "")
	decode(t, rec, &list)
	if strings.Contains(rec.Body.String(), "sk-good") {
		t.Fatal("the provider list leaked the key")
	}

	var openai providerStatus
	for _, p := range list.Data {
		if p.Provider == "openai" {
			openai = p
		}
	}
	if !openai.Configured || openai.Fingerprint != saved.Fingerprint {
		t.Fatalf("openai = %+v", openai)
	}

	// The key that came back out of storage is the key that went in.
	roundTripped, err := db.ProviderKey(database, cfg.EncryptionKey, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if roundTripped != "sk-good" {
		t.Errorf("stored key round-tripped as %q", roundTripped)
	}

	// Routing now sees the provider, so /v1/models advertises its models.
	raw, err := db.CreateClientKey(database, "k")
	if err != nil {
		t.Fatal(err)
	}

	rec = do(t, h, http.MethodGet, "/v1/models", raw, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gpt-4o-mini") {
		t.Errorf("models did not appear after the key was saved: %s", rec.Body.String())
	}

	// Remove it, and the provider goes quiet again.
	rec = do(t, h, http.MethodDelete, "/v1/admin/providers/openai", testAdminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/v1/models", raw, "")
	if strings.Contains(rec.Body.String(), "gpt-4o-mini") {
		t.Error("models still advertised after the key was removed")
	}

	// Deleting again has nothing to do, and says so.
	if rec := do(t, h, http.MethodDelete, "/v1/admin/providers/openai", testAdminToken, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}

	// And the key is gone from storage, not merely hidden.
	if _, err := db.ProviderKey(database, cfg.EncryptionKey, "openai"); !errors.Is(err, db.ErrNoProviderKey) {
		t.Errorf("ProviderKey after delete = %v, want ErrNoProviderKey", err)
	}
}

// A key the provider rejects must not be stored: otherwise the dashboard shows
// the provider as configured and every completion through it fails.
func TestBadProviderKeyIsRefusedAndNotStored(t *testing.T) {
	upstream, _ := fakeUpstream(t, "sk-good")
	h, database, cfg := providerHandler(t, upstream.URL)

	rec := do(t, h, http.MethodPut, "/v1/admin/providers/openai", testAdminToken, `{"api_key":"sk-typo"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("= %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if msg := errorMessage(t, rec); !strings.Contains(msg, "rejected") {
		t.Errorf("message = %q; it should say the provider rejected the key", msg)
	}

	stored, err := db.ListProviderKeys(database, cfg.EncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Errorf("a rejected key was stored anyway: %+v", stored)
	}
}

// Verification is skippable for an upstream the gateway cannot reach at save
// time, or a proxy that does not serve /models.
func TestVerificationCanBeSkipped(t *testing.T) {
	h, database, cfg := providerHandler(t, "http://127.0.0.1:1") // nothing listening

	rec := do(t, h, http.MethodPut, "/v1/admin/providers/openai", testAdminToken,
		`{"api_key":"sk-unverifiable","skip_verification":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("= %d: %s", rec.Code, rec.Body.String())
	}

	stored, err := db.ListProviderKeys(database, cfg.EncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Provider != "openai" {
		t.Fatalf("stored = %+v", stored)
	}

	// Without the flag, an unreachable upstream is a refusal, not a silent save.
	rec = do(t, h, http.MethodPut, "/v1/admin/providers/anthropic", testAdminToken, `{"api_key":"sk-x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unreachable upstream without skip = %d, want 400", rec.Code)
	}
}

func TestSavingReplacesTheExistingKey(t *testing.T) {
	upstream, _ := fakeUpstream(t, "sk-good")
	h, database, cfg := providerHandler(t, upstream.URL)

	if rec := do(t, h, http.MethodPut, "/v1/admin/providers/openai", testAdminToken,
		`{"api_key":"sk-first","skip_verification":true}`); rec.Code != http.StatusOK {
		t.Fatalf("first save = %d: %s", rec.Code, rec.Body.String())
	}
	first, err := db.ListProviderKeys(database, cfg.EncryptionKey)
	if err != nil {
		t.Fatal(err)
	}

	if rec := do(t, h, http.MethodPut, "/v1/admin/providers/openai", testAdminToken,
		`{"api_key":"sk-good"}`); rec.Code != http.StatusOK {
		t.Fatalf("rotation = %d: %s", rec.Code, rec.Body.String())
	}
	second, err := db.ListProviderKeys(database, cfg.EncryptionKey)
	if err != nil {
		t.Fatal(err)
	}

	if len(second) != 1 {
		t.Fatalf("rotation created %d rows, want 1", len(second))
	}
	if first[0].Fingerprint == second[0].Fingerprint {
		t.Error("the fingerprint did not change after rotation")
	}
}
