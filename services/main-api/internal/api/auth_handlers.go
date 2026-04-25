package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/auth"
	"hybridcloud/services/main-api/internal/db/dbstore"
)

// SessionCookieName is the name of the cookie carrying the opaque session
// token. Kept in api so handlers + middleware agree without a circular dep.
const SessionCookieName = "hc_session"

// UserStore is the slice of auth.Repo the handlers use. Narrow interface so
// the tests don't need a Postgres container.
type UserStore interface {
	CreateUser(ctx context.Context, email, password string) (dbstore.User, error)
	Authenticate(ctx context.Context, email, password string) (dbstore.User, error)
	GetUser(ctx context.Context, id uuid.UUID) (dbstore.User, error)
	CreateSession(ctx context.Context, userID uuid.UUID, ttl time.Duration) (string, dbstore.Session, error)
	LookupSession(ctx context.Context, rawToken string) (dbstore.Session, dbstore.User, error)
	RevokeSession(ctx context.Context, rawToken string) error
}

// LoginLimiter is the slice of *auth.RateLimiter the handlers depend on.
type LoginLimiter interface {
	Allow(key string) bool
}

// AuthConfig describes the cookie + ttl knobs.
type AuthConfig struct {
	SessionTTL   time.Duration
	CookieSecure bool   // false in dev, true in production
	CookieDomain string // optional; "" means no Domain attribute
	CookiePath   string // typically "/"
}

// AuthHandlers wires the /api/v1/auth/* handlers.
type AuthHandlers struct {
	Users   UserStore
	Limiter LoginLimiter
	Config  AuthConfig
}

// --- request bodies --------------------------------------------------------

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// userView is the JSON shape returned for auth/me + register/login bodies.
type userView struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	IsAdmin   bool      `json:"is_admin"`
	CreatedAt time.Time `json:"created_at"`
}

func toUserView(u dbstore.User) userView {
	return userView{
		ID:        u.ID,
		Email:     u.Email,
		IsAdmin:   u.IsAdmin,
		CreatedAt: u.CreatedAt.Time,
	}
}

// --- handlers --------------------------------------------------------------

// Register handles POST /api/v1/auth/register.
func (h *AuthHandlers) Register(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var req registerRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}

	user, err := h.Users.CreateUser(r.Context(), req.Email, req.Password)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrEmailTaken):
			writeError(w, http.StatusConflict, "email_taken", "email already registered")
		case errors.Is(err, auth.ErrInvalidEmail):
			writeError(w, http.StatusBadRequest, "invalid_email", err.Error())
		case errors.Is(err, auth.ErrWeakPassword):
			writeError(w, http.StatusBadRequest, "weak_password", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "register_failed", err.Error())
		}
		return
	}

	if err := h.issueSessionCookie(w, r, user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "session_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": toUserView(user)})
}

// Login handles POST /api/v1/auth/login. Rate-limited per source IP at 5/min.
func (h *AuthHandlers) Login(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	if h.Limiter != nil {
		if !h.Limiter.Allow(clientIP(r)) {
			writeError(w, http.StatusTooManyRequests, "rate_limited",
				"too many login attempts, try again later")
			return
		}
	}

	var req loginRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}

	user, err := h.Users.Authenticate(r.Context(), req.Email, req.Password)
	if err != nil {
		// Always 401 + opaque message so attackers can't enumerate users.
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}
	if err := h.issueSessionCookie(w, r, user.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "session_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserView(user)})
}

// Logout handles POST /api/v1/auth/logout. Idempotent: missing cookie still 204.
func (h *AuthHandlers) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil {
		_ = h.Users.RevokeSession(r.Context(), c.Value)
	}
	http.SetCookie(w, h.expireCookie())
	w.WriteHeader(http.StatusNoContent)
}

// Me handles GET /api/v1/auth/me. Returns 401 when no valid session.
func (h *AuthHandlers) Me(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "no session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserView(user)})
}

// --- helpers ---------------------------------------------------------------

func (h *AuthHandlers) issueSessionCookie(w http.ResponseWriter, r *http.Request, userID uuid.UUID) error {
	raw, sess, err := h.Users.CreateSession(r.Context(), userID, h.Config.SessionTTL)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    raw,
		Path:     orDefault(h.Config.CookiePath, "/"),
		Domain:   h.Config.CookieDomain,
		Expires:  sess.ExpiresAt.Time,
		MaxAge:   int(h.Config.SessionTTL.Seconds()),
		Secure:   h.Config.CookieSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (h *AuthHandlers) expireCookie() *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     orDefault(h.Config.CookiePath, "/"),
		Domain:   h.Config.CookieDomain,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		Secure:   h.Config.CookieSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func decodeJSON(r io.Reader, v any) error {
	body, err := io.ReadAll(io.LimitReader(r, 1<<14))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// clientIP extracts the source IP for rate-limiting. It prefers
// X-Forwarded-For's first entry when set (the deployment puts main-api behind
// a reverse proxy), falling back to RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if comma := strings.Index(xff, ","); comma > 0 {
			return strings.TrimSpace(xff[:comma])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
