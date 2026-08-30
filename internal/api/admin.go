package api

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"

	"github.com/coderouter/coderouter/internal/db"
)

// registerAdminRoutes mounts client-key and provider-key management. The
// endpoints exist only when ADMIN_TOKEN is set: an unauthenticated key-minting
// surface would be worse than having no management API at all.
func (h *Handler) registerAdminRoutes(mux *http.ServeMux) {
	// Registered unconditionally now that there are two ways in. Whether
	// management is actually reachable is decided per request by
	// adminAuthorized, which knows about both.
	mux.HandleFunc("GET /v1/admin/keys", h.adminListKeys)
	mux.HandleFunc("POST /v1/admin/keys", h.adminCreateKey)
	mux.HandleFunc("DELETE /v1/admin/keys/{id}", h.adminRevokeKey)

	h.registerProviderRoutes(mux)
	h.registerTagRoutes(mux)
	h.registerUserRoutes(mux)
}

// adminAuthorized admits either a signed-in operator or a valid admin token.
//
// People sign in with an email and password; scripts present the token. Both
// reach the same endpoints, because forcing automation through a login flow
// would push people towards keeping an email and password in shell history.
func (h *Handler) adminAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := h.session(r); ok {
		return true
	}
	if h.adminTokenPresented(r) {
		return true
	}

	// Neither credential worked. If neither is even configured, say so: an
	// operator staring at "unauthorized" on a fresh install needs to know
	// there is nothing to be authorized against yet.
	if h.cfg.AdminToken == "" && !h.anyUsersExist(r) {
		writeError(w, http.StatusServiceUnavailable,
			"management is not set up; create the first account by setting ADMIN_EMAIL "+
				"and ADMIN_PASSWORD, or set ADMIN_TOKEN", "invalid_request_error")
		return false
	}

	writeError(w, http.StatusUnauthorized,
		"sign in, or present a valid admin token", "invalid_request_error")
	return false
}

// anyUsersExist reports whether any account has been created. Only consulted
// on the failure path, so the query costs nothing in normal use.
func (h *Handler) anyUsersExist(r *http.Request) bool {
	if h.db == nil {
		return false
	}
	n, err := db.CountUsers(r.Context(), h.db)
	return err == nil && n > 0
}

// adminTokenPresented reports whether the request carries the admin token.
//
// The comparison is constant time, and an unset token never matches: without
// that guard an empty ADMIN_TOKEN would be satisfied by an empty header, which
// is how a disabled feature becomes an open door.
func (h *Handler) adminTokenPresented(r *http.Request) bool {
	if h.cfg.AdminToken == "" {
		return false
	}

	provided := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(provided) > len(prefix) {
		provided = provided[len(prefix):]
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(h.cfg.AdminToken)) == 1
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
