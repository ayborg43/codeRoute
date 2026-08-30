package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/coderouter/coderouter/internal/provider"
)

// Provider keys are the most sensitive values in the deployment, so they are
// unreachable until something can authorize a caller.
func TestProviderRoutesAreClosedWhenNothingCanAuthorize(t *testing.T) {
	h := testHandler(t, "")

	cases := []struct{ method, path string }{
		{http.MethodGet, "/v1/admin/providers"},
		{http.MethodPut, "/v1/admin/providers/openai"},
		{http.MethodDelete, "/v1/admin/providers/openai"},
	}
	for _, tc := range cases {
		rec := do(t, h, tc.method, tc.path, "anything", `{"api_key":"sk-live"}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tc.method, tc.path, rec.Code)
		}
	}
}

func TestProviderRoutesRejectAWrongToken(t *testing.T) {
	h := testHandler(t, testAdminToken)

	cases := []struct{ method, path string }{
		{http.MethodGet, "/v1/admin/providers"},
		{http.MethodPut, "/v1/admin/providers/openai"},
		{http.MethodDelete, "/v1/admin/providers/openai"},
	}
	for _, tc := range cases {
		for _, token := range []string{"", "wrong", testAdminToken + "x"} {
			rec := do(t, h, tc.method, tc.path, token, `{"api_key":"sk-live"}`)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %q = %d, want 401", tc.method, tc.path, token, rec.Code)
			}
		}
	}
}

// An unknown provider is refused before anything is stored, and the error names
// what this build actually supports.
func TestUnknownProviderIsRefused(t *testing.T) {
	h := testHandler(t, testAdminToken)

	rec := do(t, h, http.MethodPut, "/v1/admin/providers/acme-llm", testAdminToken, `{"api_key":"sk-x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("= %d, want 404", rec.Code)
	}

	msg := errorMessage(t, rec)
	for _, want := range []string{"acme-llm", "openai", "groq", "openrouter"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestSetProviderKeyRequiresAKey(t *testing.T) {
	h := testHandler(t, testAdminToken)

	for _, body := range []string{`{}`, `{"api_key":""}`, `{"api_key":"   "}`} {
		rec := do(t, h, http.MethodPut, "/v1/admin/providers/openai", testAdminToken, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", body, rec.Code)
		}
	}

	rec := do(t, h, http.MethodPut, "/v1/admin/providers/openai", testAdminToken, `not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", rec.Code)
	}
}

// The stored key must never come back out — only a fingerprint of it.
func TestProviderStatusNeverCarriesTheKey(t *testing.T) {
	var status providerStatus
	raw, err := json.Marshal(providerStatus{
		Provider: "openai", Configured: true, Fingerprint: "abcd1234",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}

	body := string(raw)
	for _, forbidden := range []string{"api_key", "encrypted", "sk-"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("provider status serialises %q: %s", forbidden, body)
		}
	}
	if !strings.Contains(body, "abcd1234") {
		t.Errorf("fingerprint missing from %s", body)
	}
}

// Four presets serve their model list without authentication. Reporting a key
// as verified there would be a lie: the save only proved the endpoint answered.
func TestPublicModelProvidersAreNotClaimedVerified(t *testing.T) {
	public := map[string]bool{
		"openrouter": true, "sambanova": true, "nvidia": true,
		"huggingface": true, "xkiro": true,
	}

	for _, spec := range provider.Presets() {
		if got := spec.PublicModels; got != public[spec.Name] {
			t.Errorf("%s PublicModels = %v, want %v", spec.Name, got, public[spec.Name])
		}
	}
}

// Every preset must carry enough for the dashboard to offer it usefully.
func TestPresetsAreComplete(t *testing.T) {
	for _, spec := range provider.Presets() {
		if err := spec.Validate(); err != nil {
			t.Errorf("preset %q is invalid: %v", spec.Name, err)
		}
		if spec.Label == "" {
			t.Errorf("preset %q has no label", spec.Name)
		}
		if spec.ConsoleURL == "" {
			t.Errorf("preset %q has no console URL; a user cannot find where to get a key", spec.Name)
		}
		if !strings.HasPrefix(spec.BaseURL, "https://") {
			t.Errorf("preset %q is not https: %s", spec.Name, spec.BaseURL)
		}
	}
}
