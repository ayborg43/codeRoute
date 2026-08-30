package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/coderouter/coderouter/internal/db"
	"github.com/coderouter/coderouter/internal/provider"
)

// registerProviderRoutes mounts BYOK management. These endpoints accept the
// most sensitive values in the deployment, so they sit behind the admin token
// alongside client-key management and are registered only when it is set.
func (h *Handler) registerProviderRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/providers", h.adminListProviders)
	mux.HandleFunc("PUT /v1/admin/providers/{name}", h.adminSetProviderKey)
	mux.HandleFunc("DELETE /v1/admin/providers/{name}", h.adminDeleteProviderKey)
}

// providerStatus is one row of the providers view: which upstreams this build
// supports, and which of them currently have a usable key.
type providerStatus struct {
	Provider    string `json:"provider"`
	Label       string `json:"label"`
	ConsoleURL  string `json:"console_url,omitempty"`
	FreeTier    bool   `json:"free_tier"`
	Configured  bool   `json:"configured"`
	Fingerprint string `json:"fingerprint,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`

	// Models and FreeModels report what discovery found for this provider.
	Models     int `json:"models"`
	FreeModels int `json:"free_models"`

	// Verified says whether the stored key was actually proven to work. It is
	// false for providers whose model list is public, where a save confirms
	// only that the endpoint answered.
	Verified bool   `json:"verified"`
	Note     string `json:"note,omitempty"`
}

// unverifiableNote explains why a key was accepted without being proven.
const unverifiableNote = "this provider serves its model list without " +
	"authentication, so the key could not be checked; a bad key will surface " +
	"on the first completion"

func (h *Handler) adminListProviders(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	stored, err := db.ListProviderKeys(h.db, h.cfg.EncryptionKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	byName := make(map[string]db.ProviderKeyInfo, len(stored))
	for _, info := range stored {
		byName[info.Provider] = info
	}

	counts := h.gw.Catalog().DiscoveredCount()
	freeByProvider := map[string]int{}
	for _, m := range h.gw.Catalog().FreeModels() {
		freeByProvider[m.Provider]++
	}

	// Every configured provider is listed, keyed or not, so the dashboard can
	// offer the empty ones rather than the operator having to know which
	// names this build accepts.
	data := []providerStatus{}
	for _, spec := range h.gw.Specs() {
		status := providerStatus{
			Provider:   spec.Name,
			Label:      spec.DisplayName(),
			ConsoleURL: spec.ConsoleURL,
			FreeTier:   spec.FreeTier,
			Models:     counts[spec.Name],
			FreeModels: freeByProvider[spec.Name],
		}
		if info, ok := byName[spec.Name]; ok {
			status.Configured = true
			status.Fingerprint = info.Fingerprint
			status.UpdatedAt = info.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
			status.Verified = !spec.PublicModels
			if spec.PublicModels {
				status.Note = unverifiableNote
			}
		}
		data = append(data, status)
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) adminSetProviderKey(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	name := r.PathValue("name")
	if !h.gw.KnowsProvider(name) {
		writeError(w, http.StatusNotFound,
			"unknown provider "+name+"; this build supports "+strings.Join(h.gw.ProviderNames(), ", "),
			"invalid_request_error")
		return
	}

	var body struct {
		APIKey string `json:"api_key"`

		// Skip is an escape hatch for an upstream that cannot be reached from
		// the gateway at save time, or a proxy that does not serve /models.
		Skip bool `json:"skip_verification"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	body.APIKey = strings.TrimSpace(body.APIKey)
	if body.APIKey == "" {
		writeError(w, http.StatusBadRequest, "api_key is required", "invalid_request_error")
		return
	}

	// Verification is a model listing, so a successful check also tells us
	// what this provider serves. Holding on to that avoids asking twice.
	var discovered []provider.DiscoveredModel
	if !body.Skip {
		models, err := h.gw.VerifyProviderKey(r.Context(), name, body.APIKey)
		if err != nil {
			// Refusing to store an unusable key is the whole point: otherwise
			// the dashboard reports the provider as configured and every
			// completion through it fails.
			//
			// A rejection and an unreachable endpoint need different
			// responses from the operator, so they are worded differently.
			msg := "could not reach " + name + " to verify this key: " + err.Error()
			if errors.Is(err, provider.ErrRejected) {
				msg = name + " rejected this key: " + err.Error()
			}
			writeError(w, http.StatusBadRequest, msg, "invalid_request_error")
			return
		}
		discovered = models
	}

	if err := db.StoreProviderKey(h.db, h.cfg.EncryptionKey, name, body.APIKey); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	// The models came back with the verification, so the provider is routable
	// immediately without waiting for the next scheduled refresh. A key saved
	// with verification skipped has no list yet, so ask for one.
	if len(discovered) > 0 {
		h.gw.AdoptModels(r.Context(), name, discovered)
	} else {
		h.gw.RefreshProvider(r.Context(), name)
	}

	// A new key may carry different entitlements, so models sidelined under
	// the old one get another chance.
	h.gw.ClearBench()

	// The fingerprint goes back so the caller can confirm which key landed
	// without the key itself being echoed anywhere.
	spec, _ := h.gw.Spec(name)
	counts := h.gw.Catalog().DiscoveredCount()
	free := 0
	for _, m := range h.gw.Catalog().FreeModels() {
		if m.Provider == name {
			free++
		}
	}

	status := providerStatus{
		Provider:    name,
		Label:       spec.DisplayName(),
		ConsoleURL:  spec.ConsoleURL,
		FreeTier:    spec.FreeTier,
		Configured:  true,
		Fingerprint: db.FingerprintKey(body.APIKey),
		Models:      counts[name],
		FreeModels:  free,
		Verified:    !body.Skip && !spec.PublicModels,
	}
	switch {
	case body.Skip:
		status.Note = "verification was skipped at your request"
	case spec.PublicModels:
		status.Note = unverifiableNote
	}

	writeJSON(w, http.StatusOK, status)
}

func (h *Handler) adminDeleteProviderKey(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	name := r.PathValue("name")
	existed, err := db.DeleteProviderKey(h.db, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "no key stored for "+name, "invalid_request_error")
		return
	}

	// Its models are no longer reachable, so they leave the catalogue too.
	h.gw.RefreshProvider(r.Context(), name)

	writeJSON(w, http.StatusOK, providerStatus{Provider: name, Configured: false})
}
