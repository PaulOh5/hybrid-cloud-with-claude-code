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
