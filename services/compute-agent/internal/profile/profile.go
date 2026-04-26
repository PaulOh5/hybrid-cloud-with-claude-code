// Package profile parses the per-node YAML describing which slot layouts are
// permitted and which one is currently active. The agent reports the active
// layout to main-api on Register so main-api can seed gpu_slots to match.
package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// File mirrors the YAML shape. Zero or more Layouts, exactly one must match
// the Active name.
type File struct {
	Active  string   `yaml:"active"`
	Layouts []Layout `yaml:"layouts"`
}

// Layout is one named slot arrangement. All slots must be contiguous in
// slot_index starting from 0 (validated).
type Layout struct {
	Name  string     `yaml:"name"`
	Slots []SlotYAML `yaml:"slots"`
}

// SlotYAML is the YAML form of a single slot. GPU indices must exist in the
// host's reported topology.
type SlotYAML struct {
	Size         int32   `yaml:"size"`
	GpuIndices   []int32 `yaml:"gpu_indices"`
	NvlinkDomain string  `yaml:"nvlink_domain,omitempty"`
}

// ErrActiveMissing means the active layout name doesn't match any Layout.
var ErrActiveMissing = errors.New("profile: active layout not found")

// ErrSlotSizeMismatch means a slot's declared size doesn't match the number
// of GPU indices it enumerates.
var ErrSlotSizeMismatch = errors.New("profile: slot size != len(gpu_indices)")

// ErrUnknownGPU means a slot references a GPU index that isn't on the host.
var ErrUnknownGPU = errors.New("profile: slot references unknown GPU index")

// ErrDuplicateGPU means two slots in the active layout share a GPU.
var ErrDuplicateGPU = errors.New("profile: gpu claimed by two slots")

// ErrEmptyGpuIndices means a slot's gpu_indices list is empty when the
// slot's size is positive — caught explicitly because the generic size-
// mismatch message ("size=N, lists 0") loses the "missing entirely" signal.
var ErrEmptyGpuIndices = errors.New("profile: slot.gpu_indices is empty")

// ErrSharedNvlinkDomain means two slots claim GPUs that sit in the same
// NVLink domain. Sharing a domain across multi-GPU VMs splits the NVLink
// pairing between tenants and silently drops bandwidth — Phase 1 F4
// requires NVLink groups stay intact within a slot.
var ErrSharedNvlinkDomain = errors.New("profile: nvlink_domain claimed by two slots")

// Load reads a YAML file and decodes it. It does NOT validate against
// topology — call Resolve for that.
func Load(path string) (File, []byte, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied config
	if err != nil {
		return File{}, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return File{}, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return f, raw, nil
}

// Resolved is the active layout materialised into proto-friendly form plus
// the raw-bytes hash main-api uses to detect drift.
type Resolved struct {
	Name  string
	Hash  string
	Slots []SlotYAML
}

// Resolve picks the active layout out of f, validates each slot against the
// provided GPU indices, and computes the file hash. hostGPUIndices comes from
// the topology so we can reject layouts that refer to absent GPUs.
func Resolve(f File, rawYAML []byte, hostGPUIndices []int32) (Resolved, error) {
	var active *Layout
	for i := range f.Layouts {
		if f.Layouts[i].Name == f.Active {
			active = &f.Layouts[i]
			break
		}
	}
	if active == nil {
		return Resolved{}, fmt.Errorf("%w: %q", ErrActiveMissing, f.Active)
	}

	knownGPU := make(map[int32]bool, len(hostGPUIndices))
	for _, i := range hostGPUIndices {
		knownGPU[i] = true
	}

	claimed := make(map[int32]bool)
	claimedDomain := make(map[string]int) // domain → slot index that claimed it
	for i, s := range active.Slots {
		if s.Size <= 0 {
			return Resolved{}, fmt.Errorf("%w: slot %d size=%d", ErrSlotSizeMismatch, i, s.Size)
		}
		if len(s.GpuIndices) == 0 {
			return Resolved{}, fmt.Errorf("%w: slot %d declares size=%d", ErrEmptyGpuIndices, i, s.Size)
		}
		if int(s.Size) != len(s.GpuIndices) {
			return Resolved{}, fmt.Errorf("%w: slot %d declares size=%d but lists %d gpus",
				ErrSlotSizeMismatch, i, s.Size, len(s.GpuIndices))
		}
		for _, g := range s.GpuIndices {
			if !knownGPU[g] {
				return Resolved{}, fmt.Errorf("%w: gpu %d in slot %d", ErrUnknownGPU, g, i)
			}
			if claimed[g] {
				return Resolved{}, fmt.Errorf("%w: gpu %d", ErrDuplicateGPU, g)
			}
			claimed[g] = true
		}
		if s.NvlinkDomain != "" {
			if other, ok := claimedDomain[s.NvlinkDomain]; ok {
				return Resolved{}, fmt.Errorf("%w: %q in slots %d and %d",
					ErrSharedNvlinkDomain, s.NvlinkDomain, other, i)
			}
			claimedDomain[s.NvlinkDomain] = i
		}
	}

	sum := sha256.Sum256(rawYAML)
	return Resolved{
		Name:  active.Name,
		Hash:  hex.EncodeToString(sum[:]),
		Slots: active.Slots,
	}, nil
}

// Proto converts the resolved layout into the proto Profile message for
// transport to main-api.
func (r Resolved) Proto() *agentv1.Profile {
	slots := make([]*agentv1.SlotSpec, 0, len(r.Slots))
	for i, s := range r.Slots {
		slots = append(slots, &agentv1.SlotSpec{
			SlotIndex:    int32(i), //nolint:gosec // slot count bounded by operator config
			GpuCount:     s.Size,
			GpuIndices:   s.GpuIndices,
			NvlinkDomain: s.NvlinkDomain,
		})
	}
	return &agentv1.Profile{
		Name:  r.Name,
		Hash:  r.Hash,
		Slots: slots,
	}
}
