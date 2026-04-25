package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// NodeLister is the slice of node.Repo the user-facing handler needs.
type NodeLister interface {
	List(ctx context.Context) ([]dbstore.Node, error)
}

// UserNodeHandlers exposes a read-only view of compute nodes for the user
// dashboard. Phase 1 surface: every authenticated user can see every node so
// the create form has a node picker. Per-tenant filtering arrives once
// Phase 2/Private Zone lands.
type UserNodeHandlers struct {
	Nodes NodeLister
}

// UserNodeView is the JSON shape returned to the dashboard. Hides the raw
// topology blob — only the derived GPU count is useful here.
type UserNodeView struct {
	ID       uuid.UUID `json:"id"`
	NodeName string    `json:"node_name"`
	Status   string    `json:"status"`
	GPUCount int       `json:"gpu_count"`
}

// List serves GET /api/v1/nodes — only online nodes, sorted by name.
func (h *UserNodeHandlers) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Nodes.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	views := make([]UserNodeView, 0, len(rows))
	for _, n := range rows {
		if n.Status != dbstore.NodeStatusOnline {
			continue
		}
		views = append(views, UserNodeView{
			ID:       n.ID,
			NodeName: n.NodeName,
			Status:   string(n.Status),
			GPUCount: gpuCountFromTopology(n.TopologyJson),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": views})
}
