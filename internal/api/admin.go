package api

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/coderouter/coderouter/internal/db"
)

// registerAdminRoutes mounts client-key and provider-key management. The
// endpoints exist only when ADMIN_TOKEN is set: an unauthenticated key-minting
// surface would be worse than having no management API at all.
func (h *Handler) registerAdminRoutes(mux *http.ServeMux) {
	if h.cfg.AdminToken == "" {
		log.Print("ADMIN_TOKEN not set; management endpoints are disabled")
		// Answer explicitly rather than letting these paths fall through to
		// the catch-all, which would make a disabled API look reachable.
		mux.HandleFunc("/v1/admin/", func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound,
				"management is disabled; set ADMIN_TOKEN to enable it", "invalid_request_error")
		})
		return
	}

	mux.HandleFunc("GET /v1/admin/keys", h.adminListKeys)
	mux.HandleFunc("POST /v1/admin/keys", h.adminCreateKey)
	mux.HandleFunc("DELETE /v1/admin/keys/{id}", h.adminRevokeKey)

	h.registerProviderRoutes(mux)
	h.registerTagRoutes(mux)
}

// adminAuthorized compares the bearer token in constant time.
func (h *Handler) adminAuthorized(w http.ResponseWriter, r *http.Request) bool {
	provided := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(provided) > len(prefix) {
		provided = provided[len(prefix):]
	}

	if subtle.ConstantTimeCompare([]byte(provided), []byte(h.cfg.AdminToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid admin token", "invalid_request_error")
		return false
	}
	return true
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_request_error")
		return false
	}
	return true
}

func (h *Handler) adminListKeys(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	keys, err := db.ListClientKeys(h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": keys})
}

func (h *Handler) adminCreateKey(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	raw, err := db.CreateClientKey(h.db, body.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	// The raw key is shown exactly once; only its hash is stored.
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":     raw,
		"name":    body.Name,
		"warning": "store this now; it cannot be retrieved again",
	})
}

func (h *Handler) adminRevokeKey(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	if err := db.RevokeClientKey(h.db, r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error(), "invalid_request_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked"})
}
