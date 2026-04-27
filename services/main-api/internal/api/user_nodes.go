package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// NodeLister is the slice of node.Repo the user-facing handler needs.
// Phase 2.3 — ListAccessibleToUser filters out owner_team nodes whose
// team does not contain the calling user, so beta nodes never enumerate
// to non-members (S3 enumerate prevention).
type NodeLister interface {
	ListAccessibleToUser(ctx context.Context, userID uuid.UUID) ([]dbstore.Node, error)
}

// UserNodeHandlers exposes an ACL-filtered view of compute nodes to the
// user dashboard. Public nodes are visible to every authenticated user;
// owner_team nodes are visible only to team members.
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

// List serves GET /api/v1/nodes — only online nodes, ACL-filtered to
// the calling user, sorted by name.
func (h *UserNodeHandlers) List(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		// Should never reach here: RequireUser already gated this route.
		writeError(w, http.StatusUnauthorized, "unauthenticated", "session required")
		return
	}
	rows, err := h.Nodes.ListAccessibleToUser(r.Context(), user.ID)
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
