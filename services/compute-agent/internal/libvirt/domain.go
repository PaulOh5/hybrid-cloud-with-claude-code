// Package libvirt wraps libvirtd so the rest of compute-agent deals in
// Domain / Spec / Event types rather than XML and libvirt-go handles.
package libvirt

import (
	"context"
	"errors"
)

// DomainState mirrors the subset of libvirt states we care about.
type DomainState int

const (
	StateUnknown DomainState = iota
	StateRunning
	StateStopped
	StateFailed
)

func (s DomainState) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StateStopped:
		return "stopped"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// DomainSpec describes the VM we want to create.
type DomainSpec struct {
	// Name: libvirt domain name, caller-provided, globally unique on the host.
	Name string
	// MemoryMiB / VCPUs: straight-through to the domain <memory> and <vcpu>
	// elements.
	MemoryMiB uint32
	VCPUs     uint32

	// DiskPath: path to the backing qcow2 image the VM boots from.
	DiskPath string
	// CloudInitISOPath: NoCloud seed ISO. Empty means no cloud-init.
	CloudInitISOPath string
	// NetworkName: libvirt network to attach (e.g. "default"). Empty means no
	// NIC, which the caller should only use for tests.
	NetworkName string

	// MachineType / Emulator: host-specific overrides. Zero values fall back
	// to sane kvm defaults ("pc-q35-*" / /usr/bin/qemu-system-x86_64).
	MachineType string
	Emulator    string

	// PassthroughPCI lists PCI addresses (sysfs format, e.g. "0000:16:00.0")
	// that should be handed to the guest via vfio. Every address must already
	// be bound to vfio-pci on the host (see scripts/node-bootstrap.sh).
	// Phase 4 caller passes the GPU + its IOMMU-group companions here.
	PassthroughPCI []string
}

// DomainInfo is returned after a successful create. The caller correlates
// DomainInfo.Name with their own instance_id; libvirt UUIDs are opaque.
type DomainInfo struct {
	Name string
	UUID string
	// InitialState is the state libvirt reported right after DomainCreate.
	InitialState DomainState
}

// DomainEvent is emitted by StreamEvents whenever a domain's lifecycle state
// changes (running→shutdown, shutdown→destroyed, and crash→failed are the
// cases Phase 3 reacts to).
type DomainEvent struct {
	Name  string
	UUID  string
	State DomainState
}

// Manager is the narrow interface compute-agent handlers code against.
type Manager interface {
	CreateDomain(ctx context.Context, spec DomainSpec) (DomainInfo, error)
	DestroyDomain(ctx context.Context, name string) error
	DomainState(ctx context.Context, name string) (DomainState, error)
	// StreamEvents returns a channel that closes when ctx is done.
	StreamEvents(ctx context.Context) (<-chan DomainEvent, error)
	Close() error
}

// Errors surfaced to callers. Mirrored from libvirt to avoid leaking the
// underlying library's enum.
var (
	ErrDomainNotFound = errors.New("libvirt: domain not found")
	ErrDomainExists   = errors.New("libvirt: domain already defined")
)

// Validate returns nil if spec is minimally consistent.
func (s DomainSpec) Validate() error {
	switch {
	case s.Name == "":
		return errors.New("libvirt: DomainSpec.Name required")
	case s.MemoryMiB == 0:
		return errors.New("libvirt: DomainSpec.MemoryMiB must be > 0")
	case s.VCPUs == 0:
		return errors.New("libvirt: DomainSpec.VCPUs must be > 0")
	case s.DiskPath == "":
		return errors.New("libvirt: DomainSpec.DiskPath required")
	}
	return nil
}
