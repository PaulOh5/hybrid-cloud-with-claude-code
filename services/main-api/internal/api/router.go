package api

import (
	"net/http"
)

// NewAdminRouter wires the admin-scoped handlers behind the bearer-token
// middleware.
func NewAdminRouter(nodes *AdminHandlers, instances *InstanceHandlers, adminToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/nodes", nodes.ListNodes)

	if instances != nil {
		mux.HandleFunc("GET /admin/instances", instances.List)
		mux.HandleFunc("POST /admin/instances", instances.Create)
		mux.HandleFunc("DELETE /admin/instances/{id}", instances.Delete)
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

// NewRouter combines admin + internal routes on a single mux so the cmd
// layer wires one http.Server. Each prefix goes through its own bearer-
// token middleware; the internal router stays off the public admin scope.
func NewRouter(admin http.Handler, internal http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/admin/", admin)
	if internal != nil {
		mux.Handle("/internal/", internal)
	}
	return mux
}
