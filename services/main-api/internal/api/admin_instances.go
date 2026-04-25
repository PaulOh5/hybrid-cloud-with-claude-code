package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"

	"hybridcloud/services/main-api/internal/db/dbstore"
	grpcsrv "hybridcloud/services/main-api/internal/grpc"
	"hybridcloud/services/main-api/internal/instance"
	"hybridcloud/services/main-api/internal/slot"
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

// SlotRepo is the subset of slot.Repo the handler uses.
type SlotRepo interface {
	Reserve(ctx context.Context, nodeID uuid.UUID, gpuSize, count int32) (slot.Reservation, error)
	BindToInstance(ctx context.Context, res slot.Reservation, instanceID uuid.UUID) error
	ReleaseReserved(ctx context.Context, res slot.Reservation) error
	ReleaseForInstance(ctx context.Context, instanceID uuid.UUID) (int64, error)
}

// InstanceHandlers wires the admin instance endpoints.
type InstanceHandlers struct {
	Instances  InstanceRepo
	Nodes      NodeGetter
	Slots      SlotRepo
	Dispatcher AgentDispatcher
	// ExtraSSHKeysForOwner, when set, returns the user's persistently-stored
	// SSH pubkeys at create time. Phase 8.3 wires this so user-created VMs
	// get the dashboard-managed keys without the caller having to repeat
	// them in the JSON body.
	ExtraSSHKeysForOwner func(ctx context.Context, ownerID uuid.UUID) []string
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
	GPUCount   int32    `json:"gpu_count"`
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
	GPUCount     int32     `json:"gpu_count"`
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
		GPUCount:     in.GpuCount,
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

// Create serves POST /admin/instances. When gpu_count > 0 the handler
// reserves the appropriate slot(s) on the target node, resolves each slot's
// GPU indices against the node's stored topology so the agent knows which
// PCI devices to pass through, persists the instance row, binds the slots,
// and dispatches CreateInstance. Any failure between reservation and
// dispatch releases the reservation so slots do not stay reserved.
func (h *InstanceHandlers) Create(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()

	req, node, httpCode, errCode, errMsg := h.validateCreate(r)
	if httpCode != 0 {
		writeError(w, httpCode, errCode, errMsg)
		return
	}
	h.runCreate(w, r, req, node, uuid.NullUUID{Valid: false})
}

// CreateForOwner is the user-facing entry: admin Create's logic, but the row
// is stamped with the supplied OwnerID. Phase 7 calls this after auth.
// Phase 8.3 also merges the user's stored SSH pubkeys into req.SSHPubkeys so
// dashboard-managed keys flow into cloud-init without the JSON body
// repeating them.
func (h *InstanceHandlers) CreateForOwner(w http.ResponseWriter, r *http.Request, ownerID uuid.UUID) {
	defer func() { _ = r.Body.Close() }()

	req, node, httpCode, errCode, errMsg := h.validateCreate(r)
	if httpCode != 0 {
		writeError(w, httpCode, errCode, errMsg)
		return
	}
	if h.ExtraSSHKeysForOwner != nil {
		extras := h.ExtraSSHKeysForOwner(r.Context(), ownerID)
		req.SSHPubkeys = mergeSSHPubkeys(req.SSHPubkeys, extras)
	}
	h.runCreate(w, r, req, node, uuid.NullUUID{UUID: ownerID, Valid: true})
}

// mergeSSHPubkeys returns the union of two pubkey slices preserving order
// and dropping exact-string duplicates. Cheap O(n+m) is fine for the small
// number of keys per user.
func mergeSSHPubkeys(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, k := range a {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	for _, k := range b {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// runCreate implements the reservation → create → bind → dispatch sequence
// shared by Create and CreateForOwner.
func (h *InstanceHandlers) runCreate(
	w http.ResponseWriter, r *http.Request,
	req createInstanceRequest, node dbstore.Node, ownerID uuid.NullUUID,
) {
	reservation, passthroughPCI, httpCode, errCode, errMsg := h.reserveIfNeeded(r.Context(), req, node)
	if httpCode != 0 {
		writeError(w, httpCode, errCode, errMsg)
		return
	}

	inst, err := h.Instances.Create(r.Context(), instance.CreateInput{
		OwnerID:     ownerID,
		NodeID:      node.ID,
		Name:        req.Name,
		MemoryMiB:   req.MemoryMb,
		VCPUs:       req.VCPUs,
		GPUCount:    req.GPUCount,
		SlotIndices: reservation.SlotIndices(),
		SSHPubkeys:  req.SSHPubkeys,
		ImageRef:    req.ImageRef,
	})
	if err != nil {
		if req.GPUCount > 0 {
			_ = h.Slots.ReleaseReserved(r.Context(), reservation)
		}
		writeError(w, http.StatusInternalServerError, "create_failed", err.Error())
		return
	}

	if req.GPUCount > 0 {
		if err := h.Slots.BindToInstance(r.Context(), reservation, inst.ID); err != nil {
			_ = h.Slots.ReleaseReserved(r.Context(), reservation)
			h.markFailed(r.Context(), inst.ID, "bind_failed", err.Error())
			writeError(w, http.StatusInternalServerError, "bind_failed", err.Error())
			return
		}
	}

	if err := h.dispatchCreate(r.Context(), node.ID, inst, reservation, passthroughPCI); err != nil {
		if req.GPUCount > 0 {
			_, _ = h.Slots.ReleaseForInstance(r.Context(), inst.ID)
		}
		h.markFailed(r.Context(), inst.ID, "dispatch_failed", err.Error())
		writeError(w, http.StatusBadGateway, "dispatch_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"instance": toInstanceView(inst)})
}

// validateCreate parses + validates the request body and loads the target
// node. Returns httpCode=0 when everything is OK; otherwise the caller
// should writeError with the returned fields.
func (h *InstanceHandlers) validateCreate(r *http.Request) (req createInstanceRequest, node dbstore.Node, httpCode int, errCode, errMsg string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<15))
	if err != nil {
		return req, node, http.StatusBadRequest, "read_body", err.Error()
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return req, node, http.StatusBadRequest, "bad_json", err.Error()
	}
	nodeID, err := uuid.Parse(req.NodeID)
	if err != nil {
		return req, node, http.StatusBadRequest, "bad_node_id", err.Error()
	}
	if req.Name == "" || req.MemoryMb <= 0 || req.VCPUs <= 0 {
		return req, node, http.StatusBadRequest, "bad_fields", "name, memory_mb, vcpus required"
	}
	if req.GPUCount < 0 {
		return req, node, http.StatusBadRequest, "bad_gpu_count", "gpu_count must be >= 0"
	}
	node, err = h.Nodes.Get(r.Context(), nodeID)
	if err != nil {
		return req, node, http.StatusNotFound, "node_not_found", err.Error()
	}
	if node.Status != dbstore.NodeStatusOnline {
		return req, node, http.StatusConflict, "node_offline", "target node is not online"
	}
	if !h.Dispatcher.Connected(node.ID) {
		return req, node, http.StatusConflict, "agent_not_connected", "agent stream is not open"
	}
	return req, node, 0, "", ""
}

// reserveIfNeeded performs the GPU slot reservation + PCI resolution when
// req.GPUCount > 0. For gpu_count=0 it returns zero values so the caller can
// proceed with a GPU-less instance.
func (h *InstanceHandlers) reserveIfNeeded(
	ctx context.Context, req createInstanceRequest, node dbstore.Node,
) (res slot.Reservation, pci []string, httpCode int, errCode, errMsg string) {
	if req.GPUCount == 0 {
		return res, nil, 0, "", ""
	}
	if h.Slots == nil {
		return res, nil, http.StatusInternalServerError, "slots_unavailable", "slot repo not wired"
	}
	res, err := h.Slots.Reserve(ctx, node.ID, req.GPUCount, 1)
	if err != nil {
		if errors.Is(err, slot.ErrNoFreeSlots) {
			return res, nil, http.StatusConflict, "no_free_slots", err.Error()
		}
		return res, nil, http.StatusInternalServerError, "reserve_failed", err.Error()
	}
	pci, err = resolvePassthroughPCI(node.TopologyJson, res.Slots)
	if err != nil {
		_ = h.Slots.ReleaseReserved(ctx, res)
		return slot.Reservation{}, nil, http.StatusInternalServerError, "resolve_pci", err.Error()
	}
	return res, pci, 0, "", ""
}

func (h *InstanceHandlers) dispatchCreate(
	ctx context.Context, nodeID uuid.UUID, inst dbstore.Instance, res slot.Reservation, passthroughPCI []string,
) error {
	msg := &agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_CreateInstance{
			CreateInstance: &agentv1.CreateInstance{
				InstanceId:              inst.ID.String(),
				Name:                    inst.Name,
				MemoryMb:                uint32(inst.MemoryMb), //nolint:gosec // DB constraint > 0
				Vcpus:                   uint32(inst.Vcpus),    //nolint:gosec // DB constraint > 0
				SshPubkeys:              inst.SshPubkeys,
				SlotIndices:             res.SlotIndices(),
				ImageRef:                inst.ImageRef,
				PassthroughPciAddresses: passthroughPCI,
			},
		},
	}
	return h.Dispatcher.Send(nodeID, msg)
}

func (h *InstanceHandlers) markFailed(ctx context.Context, id uuid.UUID, reason, errMsg string) {
	_, _ = h.Instances.Transition(ctx, id, instance.StateFailed, instance.TransitionOptions{
		Reason:       reason,
		ErrorMessage: errMsg,
	})
}

// resolvePassthroughPCI maps each reserved slot's GPU indices to PCI
// addresses by looking them up in the node's stored topology JSON (produced
// by protojson.Marshal of agentv1.Topology). Each GPU's companion PCI
// addresses are included so libvirt can bind the whole IOMMU group.
func resolvePassthroughPCI(topologyJSON []byte, slots []dbstore.GpuSlot) ([]string, error) {
	if len(topologyJSON) == 0 || string(topologyJSON) == "{}" {
		return nil, fmt.Errorf("node has no stored topology")
	}
	var top agentv1.Topology
	if err := protojson.Unmarshal(topologyJSON, &top); err != nil {
		return nil, fmt.Errorf("parse topology: %w", err)
	}
	byIndex := make(map[int32]*agentv1.Gpu, len(top.Gpus))
	for _, g := range top.Gpus {
		byIndex[g.Index] = g
	}

	var pcis []string
	for _, s := range slots {
		for _, idx := range s.GpuIndices {
			gpu, ok := byIndex[idx]
			if !ok {
				return nil, fmt.Errorf("gpu index %d not in topology", idx)
			}
			pcis = append(pcis, gpu.PciAddress)
			pcis = append(pcis, gpu.CompanionPciAddresses...)
		}
	}
	return pcis, nil
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

	// Already terminal → release slots and drop the row.
	if inst.State == instance.StateStopped || inst.State == instance.StateFailed {
		if h.Slots != nil {
			_, _ = h.Slots.ReleaseForInstance(r.Context(), id)
		}
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
