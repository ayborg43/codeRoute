package api

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coderouter/coderouter/internal/db"
)

// sessionCookie is the browser's proof of sign-in.
const sessionCookie = "coderouter_session"

// Login attempts are throttled per email and per client address.
//
// Password checking is deliberately slow, which protects against offline
// guessing but means an online attacker can still tie up the server. A lockout
// after a handful of wrong answers costs a forgetful operator half a minute
// and costs a guesser everything.
const (
	maxFailedAttempts = 5
	lockoutWindow     = 15 * time.Minute
	lockoutDuration   = 5 * time.Minute
)

// attemptRecord tracks recent failures for one identity.
type attemptRecord struct {
	failures int
	first    time.Time
	until    time.Time
}

// loginThrottle counts failed sign-ins. It is in-process, which is the right
// scope for a single-instance gateway; behind several replicas it would need
// to move into the database.
type loginThrottle struct {
	mu      sync.Mutex
	records map[string]*attemptRecord
	swept   time.Time
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{records: map[string]*attemptRecord{}}
}

// locked reports whether an identity is currently barred, and for how long.
func (t *loginThrottle) locked(key string) (bool, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.sweepLocked()

	r, ok := t.records[key]
	if !ok || time.Now().After(r.until) {
		return false, 0
	}
	return true, time.Until(r.until)
}

// fail records a wrong answer, locking the identity once too many accumulate.
func (t *loginThrottle) fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	r, ok := t.records[key]
	if !ok || now.Sub(r.first) > lockoutWindow {
		t.records[key] = &attemptRecord{failures: 1, first: now}
		return
	}

	r.failures++
	if r.failures >= maxFailedAttempts {
		r.until = now.Add(lockoutDuration)
		r.failures = 0
		r.first = now
	}
}

// succeed clears the record, so a correct password ends any partial lockout.
func (t *loginThrottle) succeed(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.records, key)
}

// sweepLocked drops records nobody is counting any more.
func (t *loginThrottle) sweepLocked() {
	if time.Since(t.swept) < lockoutWindow {
		return
	}
	t.swept = time.Now()

	for key, r := range t.records {
		if time.Now().After(r.until) && time.Since(r.first) > lockoutWindow {
			delete(t.records, key)
		}
	}
}

// clientAddr identifies the caller for throttling. X-Forwarded-For is honoured
// only when a proxy is declared, because a header anyone can set would let an
// attacker dodge the lockout by varying it.
func (h *Handler) clientAddr(r *http.Request) string {
	if h.cfg.TrustProxyHeaders {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if first, _, found := strings.Cut(fwd, ","); found {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(fwd)
		}
	}
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}

func (h *Handler) registerAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login", h.handleLogin)
	mux.HandleFunc("POST /api/logout", h.handleLogout)
	mux.HandleFunc("GET /api/me", h.handleMe)
	mux.HandleFunc("POST /api/password", h.handleChangePassword)
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	email := db.NormalizeEmail(body.Email)
	if email == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required", "invalid_request_error")
		return
	}

	// Both the account and the source are throttled: the first stops one
	// account being ground down, the second stops one attacker working
	// through a list of addresses.
	keys := []string{"email:" + email, "addr:" + h.clientAddr(r)}
	for _, key := range keys {
		if locked, wait := h.throttle.locked(key); locked {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeError(w, http.StatusTooManyRequests,
				"too many failed sign-in attempts; try again in "+wait.Round(time.Second).String(),
				"rate_limit_error")
			return
		}
	}

	user, err := db.Authenticate(r.Context(), h.db, email, body.Password)
	if err != nil {
		for _, key := range keys {
			h.throttle.fail(key)
		}

		switch {
		case errors.Is(err, db.ErrUserDisabled):
			writeError(w, http.StatusForbidden, err.Error(), "invalid_request_error")
		case errors.Is(err, db.ErrInvalidCredentials):
			// One message for a wrong password and an unknown address alike,
			// so neither reveals whether an account exists.
			writeError(w, http.StatusUnauthorized, err.Error(), "invalid_request_error")
		default:
			log.Printf("login failed for %s: %v", email, err)
			writeError(w, http.StatusInternalServerError, "could not sign in", "internal_error")
		}
		return
	}

	for _, key := range keys {
		h.throttle.succeed(key)
	}

	token, expires, err := db.CreateSession(r.Context(), h.db, user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start a session", "internal_error")
		return
	}

	http.SetCookie(w, h.sessionCookieFor(r, token, expires))
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "expires_at": expires})
}

// sessionCookieFor builds the cookie. HttpOnly keeps it away from scripts,
// SameSite=Lax keeps it off cross-site requests, and Secure is set whenever
// the request arrived over TLS — omitting it on plain HTTP is what lets the
// dashboard still work on localhost.
func (h *Handler) sessionCookieFor(r *http.Request, token string, expires time.Time) *http.Cookie {
	secure := r.TLS != nil
	if h.cfg.TrustProxyHeaders && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		secure = true
	}

	return &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := db.DeleteSession(r.Context(), h.db, c.Value); err != nil {
			log.Printf("could not end session: %v", err)
		}
	}

	// Expire the cookie whether or not a session was found, so a stale one
	// does not linger in the browser.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "signed out"})
}

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	session, ok := h.session(r)
	if !ok {
		// A valid admin token is still an authenticated caller, just not a
		// person with an account.
		if h.adminTokenPresented(r) {
			writeJSON(w, http.StatusOK, map[string]any{"admin_token": true})
			return
		}
		writeError(w, http.StatusUnauthorized, "not signed in", "invalid_request_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"email":      session.Email,
		"expires_at": session.ExpiresAt,
	})
}

func (h *Handler) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	session, ok := h.session(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "sign in to change your password", "invalid_request_error")
		return
	}

	var body struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	// The current password is required even though the caller is signed in:
	// otherwise anyone with a borrowed session could lock out its owner.
	if _, err := db.Authenticate(r.Context(), h.db, session.Email, body.Current); err != nil {
		writeError(w, http.StatusUnauthorized, "current password is incorrect", "invalid_request_error")
		return
	}

	if err := db.SetPassword(r.Context(), h.db, session.UserID, body.New); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "password changed"})
}

// session resolves the caller's cookie, if any.
func (h *Handler) session(r *http.Request) (*db.Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, false
	}

	s, err := db.ValidateSession(r.Context(), h.db, c.Value)
	if err != nil {
		return nil, false
	}
	return s, true
}
