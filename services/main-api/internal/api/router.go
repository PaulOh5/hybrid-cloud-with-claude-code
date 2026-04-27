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
//
// Phase 2 ADR-009: ssh-proxy validates agent (node_id, token) pairs by
// posting to /internal/agent-auth. The agentAuth handler is optional so
// Phase 1 deployments without the Phase 2 schema still boot.
func NewInternalRouter(deps SSHTicketDeps, agentAuth http.Handler, internalToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/ssh-ticket", SSHTicketHandler(deps))
	if agentAuth != nil {
		mux.Handle("POST /internal/agent-auth", agentAuth)
	}
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
	// Admin endpoints living under /api/v1/admin/* — session-authenticated
	// + is_admin gated. Phase 10.1 wires this so the dashboard can drive
	// node/instance/user/credit screens without sharing the bearer token
	// with the browser.
	Admin *SessionAdminHandlers
	// AdminInstances reuses the bearer-token admin instance handlers under
	// /api/v1/admin/instances so an admin user can list/destroy across all
	// owners.
	AdminInstances *InstanceHandlers
	// AdminCredits reuses POST /admin/users/{id}/credits semantics under
	// the session-auth path.
	AdminCredits *AdminCreditHandlers
	// AdminNodes lists nodes from the same source as /admin/nodes.
	AdminNodes *AdminHandlers
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

	// Admin section: same session check + is_admin guard. Mounted on its
	// own mux so RequireAdmin only wraps these routes (other authenticated
	// routes stay reachable for non-admin users).
	adminMux := http.NewServeMux()
	if h.AdminNodes != nil {
		adminMux.HandleFunc("GET /api/v1/admin/nodes", h.AdminNodes.ListNodes)
	}
	if h.AdminInstances != nil {
		adminMux.HandleFunc("GET /api/v1/admin/instances", h.AdminInstances.List)
		adminMux.HandleFunc("DELETE /api/v1/admin/instances/{id}", h.AdminInstances.Delete)
	}
	if h.Admin != nil {
		adminMux.HandleFunc("GET /api/v1/admin/users", h.Admin.ListUsers)
		adminMux.HandleFunc("GET /api/v1/admin/slots", h.Admin.ListSlots)
	}
	if h.AdminCredits != nil {
		adminMux.HandleFunc("POST /api/v1/admin/users/{id}/credits", h.AdminCredits.Recharge)
		adminMux.HandleFunc("GET /api/v1/admin/users/{id}/credits", h.AdminCredits.Balance)
	}
	// Admin routes return 404 to both unauthenticated probes and authenticated
	// non-admins so URL enumeration cannot tell them apart. RequireAdmin
	// already returns 404 for both cases — wrapping it in RequireUser would
	// leak admin-route existence to unauthenticated probes via 401.
	adminHandler := RequireAdmin(adminMux)

	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Routes registered on `open` win first (login/register/logout);
		// admin routes go to the admin-gated mux; everything else falls
		// through to the authenticated mux. ServeMux's Handler() returns a
		// non-empty pattern only when a route matched.
		if hh, pattern := open.Handler(r); pattern != "" {
			hh.ServeHTTP(w, r)
			return
		}
		if hh, pattern := adminMux.Handler(r); pattern != "" {
			_ = hh
			adminHandler.ServeHTTP(w, r)
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
