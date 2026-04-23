package api

import (
	"net/http"
)

// NewAdminRouter wires the admin-scoped handlers behind the bearer-token
// middleware. It returns a plain *http.ServeMux so the main process can
// compose it with future routers (/api/v1/*, /internal/*, ...).
func NewAdminRouter(h *AdminHandlers, adminToken string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/nodes", h.ListNodes)

	return RequireAdminToken(adminToken)(mux)
}
