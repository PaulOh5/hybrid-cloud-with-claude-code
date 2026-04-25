// Package vm implements the compute-agent's CreateInstance/DestroyInstance
// control-message handlers: prepare cloud-init, define + start the libvirt
// domain, and report progress back through the stream.
package vm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"hybridcloud/services/compute-agent/internal/cloudinit"
	"hybridcloud/services/compute-agent/internal/gpu"
	"hybridcloud/services/compute-agent/internal/libvirt"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// Config describes the host paths the agent uses to stage per-instance files.
type Config struct {
	// ImageDir is where per-VM qcow2 disks live.
	ImageDir string
	// SeedDir is where cloud-init NoCloud ISOs live.
	SeedDir string
	// BaseImage is the backing-file source for each created disk (e.g.
	// /var/lib/hybrid/images/ubuntu-24.04.qcow2). An empty string disables
	// backing; Phase 3 always wants one.
	BaseImage string
	// NetworkName is the libvirt network to attach the VM to. Defaults to
	// "default" (libvirt's NAT network) when empty.
	NetworkName string
	// DiskSizeGB is the virtual disk size passed to qemu-img resize after the
	// backing-file clone is created. cloud-init's growpart then expands the
	// root filesystem to fill it on first boot. Zero keeps the base image
	// size (typically 3-4 GB — too small for most real workloads).
	DiskSizeGB int
}

// Handler processes CreateInstance / DestroyInstance control messages and
// writes InstanceStatus updates via the provided send callback.
type Handler struct {
	mgr      libvirt.Manager
	sysfs    gpu.Sysfs
	cfg      Config
	log      *slog.Logger
	imgFn    func(ctx context.Context, src, dst string) error
	resizeFn func(ctx context.Context, dst string, sizeGB int) error

	mu       sync.Mutex
	inFlight map[string]struct{}
}

// New returns a Handler wired to the given libvirt manager and host paths.
func New(mgr libvirt.Manager, cfg Config, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		mgr:      mgr,
		sysfs:    gpu.Sysfs{},
		cfg:      cfg,
		log:      log,
		imgFn:    qemuImgCreate,
		resizeFn: qemuImgResize,
		inFlight: make(map[string]struct{}),
	}
}

// OnControl dispatches one ControlMessage. It returns immediately; actual VM
// work runs on goroutines so the stream reader stays hot.
func (h *Handler) OnControl(
	ctx context.Context,
	msg *agentv1.ControlMessage,
	send func(*agentv1.AgentMessage),
) {
	switch p := msg.Payload.(type) {
	case *agentv1.ControlMessage_CreateInstance:
		go h.handleCreate(ctx, p.CreateInstance, send)
	case *agentv1.ControlMessage_DestroyInstance:
		go h.handleDestroy(ctx, p.DestroyInstance, send)
	case *agentv1.ControlMessage_Ping, *agentv1.ControlMessage_RegisterAck:
		// nothing to do
	default:
		h.log.Warn("unhandled control message", "type", fmt.Sprintf("%T", p))
	}
}

func (h *Handler) handleCreate(
	ctx context.Context,
	req *agentv1.CreateInstance,
	send func(*agentv1.AgentMessage),
) {
	id := req.InstanceId
	if !h.acquire(id) {
		h.log.Warn("duplicate create dropped", "instance_id", id)
		return
	}
	defer h.release(id)

	sendStatus := func(state agentv1.InstanceState, opts statusOpts) {
		send(&agentv1.AgentMessage{
			Payload: &agentv1.AgentMessage_InstanceStatus{
				InstanceStatus: &agentv1.InstanceStatus{
					InstanceId:   id,
					State:        state,
					VmInternalIp: opts.IP,
					ErrorMessage: opts.Err,
					ObservedAt:   timestamppb.Now(),
				},
			},
		})
	}

	sendStatus(agentv1.InstanceState_INSTANCE_STATE_PROVISIONING, statusOpts{})

	diskPath, seedPath, err := h.prepareFiles(ctx, req)
	if err != nil {
		h.log.Error("prepare files", "instance_id", id, "err", err)
		sendStatus(agentv1.InstanceState_INSTANCE_STATE_FAILED, statusOpts{Err: err.Error()})
		return
	}

	spec := libvirt.DomainSpec{
		// instance_id as the libvirt domain name keeps the key stable across
		// create/destroy without maintaining a separate name→id map. The
		// user-facing display name lives only on the instances row.
		Name:             id,
		MemoryMiB:        req.MemoryMb,
		VCPUs:            req.Vcpus,
		DiskPath:         diskPath,
		CloudInitISOPath: seedPath,
		NetworkName:      h.networkName(),
		PassthroughPCI:   req.PassthroughPciAddresses,
	}
	info, err := h.mgr.CreateDomain(ctx, spec)
	if err != nil {
		h.log.Error("libvirt create", "instance_id", id, "err", err)
		sendStatus(agentv1.InstanceState_INSTANCE_STATE_FAILED, statusOpts{Err: err.Error()})
		return
	}

	h.log.Info("domain started", "instance_id", id, "display_name", req.Name, "uuid", info.UUID)
	sendStatus(agentv1.InstanceState_INSTANCE_STATE_RUNNING, statusOpts{})

	// Poll libvirt for the VM's DHCP lease and resend RUNNING with the IP
	// so main-api can populate instances.vm_internal_ip — the ssh-ticket
	// endpoint refuses to issue otherwise. Up to ~90s; cloud-init usually
	// finishes within 30-60s.
	go h.detectAndPublishIP(ctx, id, sendStatus)
}

// detectAndPublishIP polls DomainIPv4 every 3s for up to 90s. Once a non-
// empty IPv4 lease is observed, sends another InstanceStatus(running) with
// the IP populated. main-api's running→running transition is idempotent and
// updates only the IP column via coalesce.
func (h *Handler) detectAndPublishIP(ctx context.Context, id string, sendStatus func(agentv1.InstanceState, statusOpts)) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		ip, err := h.mgr.DomainIPv4(ctx, id)
		if err != nil {
			h.log.Warn("vm ip lookup", "instance_id", id, "err", err)
			return
		}
		if ip == "" {
			continue
		}
		h.log.Info("vm ip resolved", "instance_id", id, "ip", ip)
		sendStatus(agentv1.InstanceState_INSTANCE_STATE_RUNNING, statusOpts{IP: ip})
		return
	}
	h.log.Warn("vm ip not resolved within deadline", "instance_id", id)
}

func (h *Handler) handleDestroy(
	ctx context.Context,
	req *agentv1.DestroyInstance,
	send func(*agentv1.AgentMessage),
) {
	id := req.InstanceId
	if !h.acquire(id) {
		h.log.Warn("duplicate destroy dropped", "instance_id", id)
		return
	}
	defer h.release(id)

	send(&agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_InstanceStatus{
			InstanceStatus: &agentv1.InstanceStatus{
				InstanceId: id,
				State:      agentv1.InstanceState_INSTANCE_STATE_STOPPING,
				ObservedAt: timestamppb.Now(),
			},
		},
	})

	// Capture the hostdev PCI list *before* destroy so we can reset each
	// device afterwards; post-destroy the domain is gone.
	pcis, err := h.mgr.DomainPassthroughPCI(ctx, id)
	if err != nil {
		h.log.Warn("read passthrough pci list", "instance_id", id, "err", err)
	}

	// libvirt domain name == instance_id (set in handleCreate) so lookup on
	// destroy uses the same key without needing a local map.
	if err := h.mgr.DestroyDomain(ctx, id); err != nil {
		h.log.Error("destroy failed", "instance_id", id, "err", err)
		// Still report stopped after a best-effort cleanup — the row is gone
		// from our perspective.
		send(&agentv1.AgentMessage{
			Payload: &agentv1.AgentMessage_InstanceStatus{
				InstanceStatus: &agentv1.InstanceStatus{
					InstanceId:   id,
					State:        agentv1.InstanceState_INSTANCE_STATE_FAILED,
					ErrorMessage: err.Error(),
					ObservedAt:   timestamppb.Now(),
				},
			},
		})
		return
	}

	h.cleanupFiles(id)

	// Reset every passthrough device so the next tenant starts from a clean
	// GPU state (A6 gate). Audio / USB companions in the same IOMMU group
	// usually don't expose a reset file — skip them silently and only log
	// when the device supports reset but actually fails (permission, device
	// still busy, etc.).
	for _, pci := range pcis {
		if !h.sysfs.HasResetFile(pci) {
			continue
		}
		if err := h.sysfs.ResetDevice(pci); err != nil {
			h.log.Warn("gpu reset", "instance_id", id, "pci", pci, "err", err)
		}
	}

	send(&agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_InstanceStatus{
			InstanceStatus: &agentv1.InstanceStatus{
				InstanceId: id,
				State:      agentv1.InstanceState_INSTANCE_STATE_STOPPED,
				ObservedAt: timestamppb.Now(),
			},
		},
	})
}

// --- helpers ---------------------------------------------------------------

type statusOpts struct {
	IP  string
	Err string
}

// prepareFiles writes the cloud-init ISO and creates a qcow2 disk with the
// configured base image as its backing file.
func (h *Handler) prepareFiles(
	ctx context.Context,
	req *agentv1.CreateInstance,
) (diskPath string, seedPath string, err error) {
	if err := os.MkdirAll(h.cfg.ImageDir, 0o750); err != nil {
		return "", "", fmt.Errorf("mkdir image dir: %w", err)
	}
	if err := os.MkdirAll(h.cfg.SeedDir, 0o750); err != nil {
		return "", "", fmt.Errorf("mkdir seed dir: %w", err)
	}

	diskPath = filepath.Join(h.cfg.ImageDir, req.InstanceId+".qcow2")
	seedPath = filepath.Join(h.cfg.SeedDir, req.InstanceId+".iso")

	if h.cfg.BaseImage != "" {
		if err := h.imgFn(ctx, h.cfg.BaseImage, diskPath); err != nil {
			return "", "", fmt.Errorf("create disk: %w", err)
		}
		if h.cfg.DiskSizeGB > 0 {
			if err := h.resizeFn(ctx, diskPath, h.cfg.DiskSizeGB); err != nil {
				return "", "", fmt.Errorf("resize disk: %w", err)
			}
		}
	}

	f, err := os.Create(seedPath) //nolint:gosec // path joined with controlled dir above
	if err != nil {
		return "", "", fmt.Errorf("create seed: %w", err)
	}
	defer func() { _ = f.Close() }()
	err = cloudinit.BuildSeed(f, cloudinit.Request{
		InstanceID: req.InstanceId,
		Hostname:   req.Name,
		SSHPubkeys: req.SshPubkeys,
	})
	if err != nil {
		return "", "", fmt.Errorf("build seed: %w", err)
	}
	return diskPath, seedPath, nil
}

func (h *Handler) cleanupFiles(instanceID string) {
	for _, p := range []string{
		filepath.Join(h.cfg.ImageDir, instanceID+".qcow2"),
		filepath.Join(h.cfg.SeedDir, instanceID+".iso"),
	} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			h.log.Warn("cleanup file", "path", p, "err", err)
		}
	}
}

func (h *Handler) networkName() string {
	if h.cfg.NetworkName != "" {
		return h.cfg.NetworkName
	}
	return "default"
}

func (h *Handler) acquire(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.inFlight[id]; ok {
		return false
	}
	h.inFlight[id] = struct{}{}
	return true
}

func (h *Handler) release(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.inFlight, id)
}

// qemuImgCreate is the production disk creator. Tests inject a no-op instead
// so they do not require qemu-img on the host.
func qemuImgCreate(ctx context.Context, backing, dst string) error {
	// qemu-img create -f qcow2 -F qcow2 -b <backing> <dst>
	//nolint:gosec // args are controlled (backing configured by operator, dst under ImageDir)
	cmd := exec.CommandContext(ctx, "qemu-img", "create",
		"-f", "qcow2",
		"-F", "qcow2",
		"-b", backing,
		dst,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img: %w: %s", err, string(out))
	}
	return nil
}

// qemuImgResize grows the virtual disk. cloud-init's growpart handles the
// filesystem side on first boot.
func qemuImgResize(ctx context.Context, dst string, sizeGB int) error {
	//nolint:gosec // dst is agent-controlled, sizeGB is an int
	cmd := exec.CommandContext(ctx, "qemu-img", "resize", dst, fmt.Sprintf("%dG", sizeGB))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img resize: %w: %s", err, string(out))
	}
	return nil
}

// WithImageCreator overrides the disk-creation function. Tests pass a noop.
func (h *Handler) WithImageCreator(fn func(ctx context.Context, src, dst string) error) *Handler {
	h.imgFn = fn
	return h
}

// WithDiskResizer overrides qemu-img resize. Tests pass a noop.
func (h *Handler) WithDiskResizer(fn func(ctx context.Context, dst string, sizeGB int) error) *Handler {
	h.resizeFn = fn
	return h
}

// WithSysfs overrides the sysfs root used for ResetDevice. Tests point it at
// a tmpdir so they don't need real sysfs.
func (h *Handler) WithSysfs(s gpu.Sysfs) *Handler {
	h.sysfs = s
	return h
}
