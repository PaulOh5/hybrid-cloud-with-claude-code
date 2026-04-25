package api

import (
	"net/http"
)

// NewAdminRouter wires the admin-scoped handlers behind the bearer-token
// middleware.
func NewAdminRouter(nodes *AdminHandlers, instances *InstanceHandlers, credits *AdminCreditHandlers, adminToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/nodes", nodes.ListNodes)

	if instances != nil {
		mux.HandleFunc("GET /admin/instances", instances.List)
		mux.HandleFunc("POST /admin/instances", instances.Create)
		mux.HandleFunc("DELETE /admin/instances/{id}", instances.Delete)
	}
	if credits != nil {
		mux.HandleFunc("POST /admin/users/{id}/credits", credits.Recharge)
		mux.HandleFunc("GET /admin/users/{id}/credits", credits.Balance)
	}

	return RequireAdminToken(adminToken)(mux)
}

// NewInternalRouter wires the /internal/* endpoints (ssh-proxy ↔ main-api
// today) behind the internal bearer token.
func NewInternalRouter(deps SSHTicketDeps, internalToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/ssh-ticket", SSHTicketHandler(deps))
	return RequireInternalToken(internalToken)(mux)
}

// UserHandlers bundles the per-feature handler types attached to /api/v1/*.
// Each field may be nil — only the wired-up groups are exposed.
type UserHandlers struct {
	Auth      *AuthHandlers
	Instances *UserInstanceHandlers
	Nodes     *UserNodeHandlers
	SSHKeys   *UserSSHKeyHandlers
	Credits   *UserCreditHandlers
}

// NewUserRouter wires the /api/v1/* user-facing endpoints. The outer mux
// dispatches by pattern; LoadSession runs on every request so cookie data is
// available, and RequireUser gates the authenticated subset.
func NewUserRouter(h UserHandlers, resolver SessionResolver) http.Handler {
	open := http.NewServeMux()
	if h.Auth != nil {
		open.HandleFunc("POST /api/v1/auth/register", h.Auth.Register)
		open.HandleFunc("POST /api/v1/auth/login", h.Auth.Login)
		open.HandleFunc("POST /api/v1/auth/logout", h.Auth.Logout)
	}

	authed := http.NewServeMux()
	if h.Auth != nil {
		authed.HandleFunc("GET /api/v1/auth/me", h.Auth.Me)
	}
	if h.Instances != nil {
		authed.HandleFunc("GET /api/v1/instances", h.Instances.List)
		authed.HandleFunc("POST /api/v1/instances", h.Instances.Create)
		authed.HandleFunc("GET /api/v1/instances/{id}", h.Instances.Get)
		authed.HandleFunc("DELETE /api/v1/instances/{id}", h.Instances.Delete)
	}
	if h.Nodes != nil {
		authed.HandleFunc("GET /api/v1/nodes", h.Nodes.List)
	}
	if h.SSHKeys != nil {
		authed.HandleFunc("GET /api/v1/ssh-keys", h.SSHKeys.List)
		authed.HandleFunc("POST /api/v1/ssh-keys", h.SSHKeys.Add)
		authed.HandleFunc("DELETE /api/v1/ssh-keys/{id}", h.SSHKeys.Delete)
	}
	if h.Credits != nil {
		authed.HandleFunc("GET /api/v1/credits", h.Credits.Balance)
	}
	authedHandler := RequireUser(authed)

	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Routes registered on `open` win first (login/register/logout);
		// the rest fall through to the authenticated mux. ServeMux exposes
		// Handler() which returns a non-empty pattern only when a route
		// matched.
		if hh, pattern := open.Handler(r); pattern != "" {
			hh.ServeHTTP(w, r)
			return
		}
		authedHandler.ServeHTTP(w, r)
	})

	return LoadSession(resolver)(root)
}

// NewRouter combines admin + internal + user routes on a single mux so the
// cmd layer wires one http.Server. Each prefix goes through its own bearer-
// token middleware; the internal router stays off the public admin scope.
func NewRouter(admin http.Handler, internal http.Handler, user http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/admin/", admin)
	if internal != nil {
		mux.Handle("/internal/", internal)
	}
	if user != nil {
		mux.Handle("/api/", user)
	}
	return mux
}
