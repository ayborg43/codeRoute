package api

import (
	"net/http"
	"strings"

	"github.com/coderouter/coderouter/internal/db"
)

// registerBlacklistRoutes mounts the routing-exclusion API. Provider and
// model travel in the JSON body rather than the URL, because a model name
// can itself contain a slash (an OpenRouter model is "vendor/model").
func (h *Handler) registerBlacklistRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/model-blacklist", h.adminListBlacklist)
	mux.HandleFunc("POST /v1/admin/model-blacklist", h.adminBlacklistModel)
	mux.HandleFunc("DELETE /v1/admin/model-blacklist", h.adminUnblacklistModel)
}

func (h *Handler) adminListBlacklist(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	entries, err := db.ListBlacklistedModels(r.Context(), h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   entries,
		"note":   "routing will never choose a blacklisted model, even when a request names it directly",
	})
}

type blacklistRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func (h *Handler) adminBlacklistModel(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	body, ok := decodeBlacklistRequest(w, r)
	if !ok {
		return
	}
	if !h.gw.KnowsProvider(body.Provider) {
		writeError(w, http.StatusNotFound, "unknown provider "+body.Provider, "invalid_request_error")
		return
	}

	if err := db.BlacklistModel(r.Context(), h.db, body.Provider, body.Model); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	// Reload so the exclusion applies to the very next request rather than at
	// the next restart.
	if err := h.gw.LoadBlacklist(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"provider":    body.Provider,
		"model":       body.Model,
		"blacklisted": true,
	})
}

func (h *Handler) adminUnblacklistModel(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	body, ok := decodeBlacklistRequest(w, r)
	if !ok {
		return
	}

	if err := db.UnblacklistModel(r.Context(), h.db, body.Provider, body.Model); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	if err := h.gw.LoadBlacklist(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"provider":    body.Provider,
		"model":       body.Model,
		"blacklisted": false,
	})
}

func decodeBlacklistRequest(w http.ResponseWriter, r *http.Request) (blacklistRequest, bool) {
	var body blacklistRequest
	if !decodeBody(w, r, &body) {
		return body, false
	}

	body.Provider = strings.TrimSpace(body.Provider)
	body.Model = strings.TrimSpace(body.Model)
	if body.Provider == "" || body.Model == "" {
		writeError(w, http.StatusBadRequest, "provider and model are required", "invalid_request_error")
		return body, false
	}

	return body, true
}
