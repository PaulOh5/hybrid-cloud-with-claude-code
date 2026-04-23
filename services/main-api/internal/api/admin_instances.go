package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/db/dbstore"
	grpcsrv "hybridcloud/services/main-api/internal/grpc"
	"hybridcloud/services/main-api/internal/instance"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// InstanceRepo is the subset of instance.Repo the admin handler uses. Narrow
// interface keeps tests free of testcontainers.
type InstanceRepo interface {
	Create(ctx context.Context, in instance.CreateInput) (dbstore.Instance, error)
	Transition(
		ctx context.Context,
		id uuid.UUID,
		to instance.State,
		opts instance.TransitionOptions,
	) (dbstore.Instance, error)
	Get(ctx context.Context, id uuid.UUID) (dbstore.Instance, error)
	Delete(ctx context.Context, id uuid.UUID) error
	ListForOwner(ctx context.Context, ownerID uuid.NullUUID) ([]dbstore.Instance, error)
}

// AgentDispatcher is the subset of grpcsrv.AgentRegistry the handler uses.
type AgentDispatcher interface {
	Send(nodeID uuid.UUID, msg *agentv1.ControlMessage) error
	Connected(nodeID uuid.UUID) bool
}

// InstanceHandlers wires the admin instance endpoints.
type InstanceHandlers struct {
	Instances  InstanceRepo
	Nodes      NodeGetter
	Dispatcher AgentDispatcher
}

// NodeGetter is the slice of node.Repo this handler depends on.
type NodeGetter interface {
	Get(ctx context.Context, id uuid.UUID) (dbstore.Node, error)
}

// --- request / response types ----------------------------------------------

type createInstanceRequest struct {
	NodeID     string   `json:"node_id"`
	Name       string   `json:"name"`
	MemoryMb   int32    `json:"memory_mb"`
	VCPUs      int32    `json:"vcpus"`
	SSHPubkeys []string `json:"ssh_pubkeys"`
	ImageRef   string   `json:"image_ref"`
}

// InstanceView is the JSON shape returned for a single instance.
type InstanceView struct {
	ID           uuid.UUID `json:"id"`
	NodeID       uuid.UUID `json:"node_id"`
	Name         string    `json:"name"`
	State        string    `json:"state"`
	MemoryMb     int32     `json:"memory_mb"`
	VCPUs        int32     `json:"vcpus"`
	SSHPubkeys   []string  `json:"ssh_pubkeys"`
	VMInternalIP string    `json:"vm_internal_ip,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func toInstanceView(in dbstore.Instance) InstanceView {
	v := InstanceView{
		ID:           in.ID,
		NodeID:       in.NodeID,
		Name:         in.Name,
		State:        string(in.State),
		MemoryMb:     in.MemoryMb,
		VCPUs:        in.Vcpus,
		SSHPubkeys:   in.SshPubkeys,
		ErrorMessage: in.ErrorMessage,
		CreatedAt:    in.CreatedAt.Time,
		UpdatedAt:    in.UpdatedAt.Time,
	}
	if in.VmInternalIp != nil {
		v.VMInternalIP = in.VmInternalIp.String()
	}
	return v
}

// --- handlers --------------------------------------------------------------

// Create serves POST /admin/instances. The instance is persisted in state
// "pending" before the CreateInstance control message is dispatched; if the
// dispatch fails we mark the row Failed so operators can see the cause.
func (h *InstanceHandlers) Create(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<15))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_body", err.Error())
		return
	}
	var req createInstanceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}

	nodeID, err := uuid.Parse(req.NodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_node_id", err.Error())
		return
	}
	if req.Name == "" || req.MemoryMb <= 0 || req.VCPUs <= 0 {
		writeError(w, http.StatusBadRequest, "bad_fields", "name, memory_mb, vcpus required")
		return
	}

	node, err := h.Nodes.Get(r.Context(), nodeID)
	if err != nil {
		writeError(w, http.StatusNotFound, "node_not_found", err.Error())
		return
	}
	if node.Status != dbstore.NodeStatusOnline {
		writeError(w, http.StatusConflict, "node_offline", "target node is not online")
		return
	}
	if !h.Dispatcher.Connected(node.ID) {
		writeError(w, http.StatusConflict, "agent_not_connected", "agent stream is not open")
		return
	}

	inst, err := h.Instances.Create(r.Context(), instance.CreateInput{
		NodeID:      node.ID,
		Name:        req.Name,
		MemoryMiB:   req.MemoryMb,
		VCPUs:       req.VCPUs,
		GPUCount:    0,
		SlotIndices: []int32{},
		SSHPubkeys:  req.SSHPubkeys,
		ImageRef:    req.ImageRef,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create_failed", err.Error())
		return
	}

	msg := &agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_CreateInstance{
			CreateInstance: &agentv1.CreateInstance{
				InstanceId:  inst.ID.String(),
				Name:        inst.Name,
				MemoryMb:    uint32(inst.MemoryMb), //nolint:gosec // bounded by DB check (> 0)
				Vcpus:       uint32(inst.Vcpus),    //nolint:gosec // bounded by DB check (> 0)
				SshPubkeys:  inst.SshPubkeys,
				SlotIndices: []int32{},
				ImageRef:    inst.ImageRef,
			},
		},
	}
	if err := h.Dispatcher.Send(node.ID, msg); err != nil {
		// Failed to dispatch → mark the instance Failed so the operator sees
		// the root cause rather than a forever-pending row.
		_, _ = h.Instances.Transition(r.Context(), inst.ID, instance.StateFailed, instance.TransitionOptions{
			Reason:       "dispatch_failed",
			ErrorMessage: err.Error(),
		})
		writeError(w, http.StatusBadGateway, "dispatch_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"instance": toInstanceView(inst)})
}

// Delete serves DELETE /admin/instances/{id}. The handler records an intent
// transition to "stopping" and pushes DestroyInstance to the agent. Actual
// row removal happens when the agent reports "stopped" via InstanceStatus.
func (h *InstanceHandlers) Delete(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", err.Error())
		return
	}

	inst, err := h.Instances.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}

	// Already terminal → just drop the row.
	if inst.State == instance.StateStopped || inst.State == instance.StateFailed {
		if err := h.Instances.Delete(r.Context(), id); err != nil {
			writeError(w, http.StatusInternalServerError, "delete_failed", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if _, err := h.Instances.Transition(r.Context(), id, instance.StateStopping, instance.TransitionOptions{
		Reason: "admin_delete",
	}); err != nil && !errors.Is(err, instance.ErrInvalidTransition) {
		writeError(w, http.StatusInternalServerError, "transition_failed", err.Error())
		return
	}

	msg := &agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_DestroyInstance{
			DestroyInstance: &agentv1.DestroyInstance{InstanceId: inst.ID.String()},
		},
	}
	if err := h.Dispatcher.Send(inst.NodeID, msg); err != nil && !errors.Is(err, grpcsrv.ErrAgentNotConnected) {
		writeError(w, http.StatusBadGateway, "dispatch_failed", err.Error())
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// List serves GET /admin/instances.
func (h *InstanceHandlers) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Instances.ListForOwner(r.Context(), uuid.NullUUID{Valid: false})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	views := make([]InstanceView, 0, len(rows))
	for _, row := range rows {
		views = append(views, toInstanceView(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": views})
}

// --- compile-time assertions ----------------------------------------------

var (
	_ = netip.MustParseAddr // keeps netip imported for future VMInternalIP parsing in PUT handlers
)
