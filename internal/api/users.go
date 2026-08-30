package api

import (
	"net/http"

	"github.com/coderouter/coderouter/internal/db"
)

// registerUserRoutes mounts operator account management.
func (h *Handler) registerUserRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/users", h.adminListUsers)
	mux.HandleFunc("POST /v1/admin/users", h.adminCreateUser)
	mux.HandleFunc("PATCH /v1/admin/users/{id}", h.adminUpdateUser)
}

func (h *Handler) adminListUsers(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	users, err := db.ListUsers(r.Context(), h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": users})
}

func (h *Handler) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	user, err := db.CreateUser(r.Context(), h.db, body.Email, body.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	// The response carries the account, never the password or its hash.
	writeJSON(w, http.StatusCreated, user)
}

func (h *Handler) adminUpdateUser(w http.ResponseWriter, r *http.Request) {
	if !h.adminAuthorized(w, r) {
		return
	}

	// Pointers so an omitted field means "leave alone" rather than "set zero".
	var body struct {
		Disabled *bool   `json:"disabled"`
		Password *string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	id := r.PathValue("id")

	if body.Disabled != nil {
		// Refusing to disable the last active account keeps an operator from
		// locking themselves out of their own gateway with one click.
		if *body.Disabled {
			active, err := activeUserCount(r, h)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
				return
			}
			if active <= 1 && h.cfg.AdminToken == "" {
				writeError(w, http.StatusBadRequest,
					"this is the only account that can sign in; create another first, "+
						"or set ADMIN_TOKEN", "invalid_request_error")
				return
			}
		}

		if err := db.SetUserDisabled(r.Context(), h.db, id, *body.Disabled); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
	}

	if body.Password != nil {
		if err := db.SetPassword(r.Context(), h.db, id, *body.Password); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
	}

	users, err := db.ListUsers(r.Context(), h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "internal_error")
		return
	}
	for _, u := range users {
		if u.ID == id {
			writeJSON(w, http.StatusOK, u)
			return
		}
	}
	writeError(w, http.StatusNotFound, "no such user", "invalid_request_error")
}

// activeUserCount is how many accounts could currently sign in.
func activeUserCount(r *http.Request, h *Handler) (int, error) {
	users, err := db.ListUsers(r.Context(), h.db)
	if err != nil {
		return 0, err
	}

	n := 0
	for i := range users {
		if !users[i].Disabled() {
			n++
		}
	}
	return n, nil
}
