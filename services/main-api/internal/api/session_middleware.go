package api

import (
	"context"
	"net/http"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// userCtxKey is the unexported context-key type for the resolved User.
type userCtxKey struct{}

// SessionResolver is the slice of UserStore the middleware needs. Kept
// separate so the test layer can plug in a tiny in-memory implementation.
type SessionResolver interface {
	LookupSession(ctx context.Context, rawToken string) (dbstore.Session, dbstore.User, error)
}

// LoadSession middleware reads the session cookie and, when valid, attaches
// the User to the request context. Always continues — RequireUser enforces
// the 401 separately.
func LoadSession(resolver SessionResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(SessionCookieName)
			if err != nil || cookie.Value == "" {
				next.ServeHTTP(w, r)
				return
			}
			_, user, err := resolver.LookupSession(r.Context(), cookie.Value)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), user)))
		})
	}
}

// RequireUser short-circuits with 401 when LoadSession did not attach a user.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserFromContext(r.Context()); !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "valid session required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WithUser stores the user in ctx.
func WithUser(ctx context.Context, u dbstore.User) context.Context {
	return context.WithValue(ctx, userCtxKey{}, u)
}

// UserFromContext returns the resolved user (or zero + false).
func UserFromContext(ctx context.Context) (dbstore.User, bool) {
	u, ok := ctx.Value(userCtxKey{}).(dbstore.User)
	return u, ok
}
