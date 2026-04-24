// Package gpu inspects and manipulates GPU passthrough state on the host.
//
// Phase 4 scope is diagnostic: we read sysfs to tell main-api which GPUs are
// currently bound to vfio-pci, what's in each IOMMU group (the companion
// devices like HDMI audio that must be passed through together), and whether
// the host is safely bindable. Actual runtime bind/unbind stays out of the
// agent — a one-time bootstrap script (scripts/node-bootstrap.sh) flips
// drivers at boot so the agent can treat the state as read-only.
package gpu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrIOMMUDisabled is returned when the host has no /sys/kernel/iommu_groups
// tree — the BIOS VT-d / AMD-Vi flag is off or the kernel was booted without
// intel_iommu=on / amd_iommu=on.
var ErrIOMMUDisabled = errors.New("gpu: iommu disabled")

// ErrDeviceNotFound wraps a sysfs read failure with the device address so
// higher layers can surface it.
var ErrDeviceNotFound = errors.New("gpu: pci device not found in sysfs")

// Sysfs is a small abstraction over /sys so tests can substitute a tmpdir.
// The default zero value uses the real /sys root.
type Sysfs struct {
	Root string
}

func (s Sysfs) root() string {
	if s.Root == "" {
		return "/sys"
	}
	return s.Root
}

// IOMMUEnabled reports whether the host kernel has any IOMMU groups, which
// is the simplest proxy for "VT-d/AMD-Vi is live".
func (s Sysfs) IOMMUEnabled() bool {
	entries, err := os.ReadDir(filepath.Join(s.root(), "kernel/iommu_groups"))
	if err != nil {
		return false
	}
	return len(entries) > 0
}

// IOMMUGroup returns the numeric group id for the given PCI address. For a
// device like "0000:16:00.0" the sysfs symlink
// /sys/bus/pci/devices/0000:16:00.0/iommu_group points at
// /sys/kernel/iommu_groups/16, so we return "16".
func (s Sysfs) IOMMUGroup(pciAddr string) (string, error) {
	link, err := os.Readlink(filepath.Join(s.root(), "bus/pci/devices", pciAddr, "iommu_group"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrDeviceNotFound, pciAddr)
		}
		return "", err
	}
	return filepath.Base(link), nil
}

// GroupMembers lists every PCI address in the same IOMMU group as pciAddr,
// sorted for stable output. For a typical GPU with audio companion the
// result has two entries.
func (s Sysfs) GroupMembers(pciAddr string) ([]string, error) {
	group, err := s.IOMMUGroup(pciAddr)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root(), "kernel/iommu_groups", group, "devices")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read iommu group %s: %w", group, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// Companions returns every device in the same IOMMU group as pciAddr *except*
// pciAddr itself — typically the GPU's audio/USB sidecars that must be
// passed through together.
func (s Sysfs) Companions(pciAddr string) ([]string, error) {
	members, err := s.GroupMembers(pciAddr)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(members))
	for _, m := range members {
		if m != pciAddr {
			out = append(out, m)
		}
	}
	return out, nil
}

// CurrentDriver returns the kernel driver bound to pciAddr (e.g. "nvidia",
// "vfio-pci", "snd_hda_intel"). Returns "" when no driver is bound.
func (s Sysfs) CurrentDriver(pciAddr string) (string, error) {
	link, err := os.Readlink(filepath.Join(s.root(), "bus/pci/devices", pciAddr, "driver"))
	if err != nil {
		if os.IsNotExist(err) {
			// Device exists but no driver attached.
			if _, statErr := os.Stat(filepath.Join(s.root(), "bus/pci/devices", pciAddr)); statErr == nil {
				return "", nil
			}
			return "", fmt.Errorf("%w: %s", ErrDeviceNotFound, pciAddr)
		}
		return "", err
	}
	return filepath.Base(link), nil
}

// VFIOReady is true when pciAddr and every companion in its IOMMU group are
// currently bound to vfio-pci — the necessary precondition to hand the group
// to a VM.
func (s Sysfs) VFIOReady(pciAddr string) (bool, error) {
	members, err := s.GroupMembers(pciAddr)
	if err != nil {
		return false, err
	}
	for _, m := range members {
		drv, err := s.CurrentDriver(m)
		if err != nil {
			return false, err
		}
		if drv != "vfio-pci" {
			return false, nil
		}
	}
	return true, nil
}

// DiagnoseBindability returns (ok, reason) — ok is true when the group looks
// passthrough-safe, otherwise reason explains the blocker in human terms so
// admin dashboards can guide fixes.
func (s Sysfs) DiagnoseBindability(pciAddr string) (bool, string, error) {
	if !s.IOMMUEnabled() {
		return false, "iommu not enabled on host (enable VT-d / intel_iommu=on)", nil
	}
	members, err := s.GroupMembers(pciAddr)
	if err != nil {
		return false, "", err
	}
	// We accept GPU + audio companions + USB controllers. Anything else
	// usually means ACS override is needed and the operator should review.
	var nonGPUCount, totalCount int
	for _, m := range members {
		totalCount++
		class, err := s.deviceClass(m)
		if err != nil {
			return false, "", err
		}
		switch {
		case strings.HasPrefix(class, "0x030000"), strings.HasPrefix(class, "0x030200"):
			// VGA / 3D controller — the GPU itself.
		case strings.HasPrefix(class, "0x040"):
			// Multimedia / audio (e.g. HDMI audio).
		case strings.HasPrefix(class, "0x0c0330"):
			// USB 3.0 controller (some GPUs ship one).
		default:
			nonGPUCount++
		}
	}
	if nonGPUCount > 0 {
		return false, fmt.Sprintf("iommu group has %d non-GPU/non-audio devices — needs review", nonGPUCount), nil
	}
	if totalCount == 0 {
		return false, "iommu group empty", nil
	}
	return true, "", nil
}

func (s Sysfs) deviceClass(pciAddr string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.root(), "bus/pci/devices", pciAddr, "class"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// DeviceVendor returns the PCI vendor id (e.g. "0x10de" for NVIDIA) for
// pciAddr.
func (s Sysfs) DeviceVendor(pciAddr string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.root(), "bus/pci/devices", pciAddr, "vendor"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// DeviceID returns the PCI device id (e.g. "0x2b85" for RTX 5090) for
// pciAddr.
func (s Sysfs) DeviceID(pciAddr string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.root(), "bus/pci/devices", pciAddr, "device"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// ListNVIDIAGPUs walks sysfs and returns PCI addresses for every NVIDIA
// device with a VGA (0x0300) or 3D controller (0x0302) class — i.e. GPUs.
// Used by the topology collector when nvidia-smi is unavailable because the
// GPUs are already bound to vfio-pci.
func (s Sysfs) ListNVIDIAGPUs() ([]string, error) {
	dir := filepath.Join(s.root(), "bus/pci/devices")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		pci := e.Name()
		vendor, err := s.DeviceVendor(pci)
		if err != nil {
			continue
		}
		if vendor != "0x10de" {
			continue
		}
		class, err := s.deviceClass(pci)
		if err != nil {
			continue
		}
		if strings.HasPrefix(class, "0x030000") || strings.HasPrefix(class, "0x030200") {
			out = append(out, pci)
		}
	}
	sort.Strings(out)
	return out, nil
}
