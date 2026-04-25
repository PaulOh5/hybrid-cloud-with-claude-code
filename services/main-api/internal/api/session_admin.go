package api

import "net/http"

// RequireAdmin gates a handler to authenticated users with is_admin=true.
// Non-admins (including unauthenticated requests) get 404 — the spec calls
// for "일반 사용자 접근 시 404" so URL discovery doesn't leak the existence
// of admin routes.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := UserFromContext(r.Context())
		if !ok || !user.IsAdmin {
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}
