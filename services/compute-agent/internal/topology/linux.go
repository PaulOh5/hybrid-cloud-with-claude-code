package topology

import (
	"context"
	"encoding/xml"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"hybridcloud/services/compute-agent/internal/gpu"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// LinuxCollector inspects the host via nvidia-smi and sysfs. On hosts without
// nvidia-smi (e.g. dev laptops) Collect returns an empty topology with no
// error — that lets Phase 2 agent bring-up work everywhere.
type LinuxCollector struct {
	// NvidiaSmiPath overrides the command path; zero value uses PATH lookup.
	NvidiaSmiPath string
	// SysfsRoot overrides /sys for tests. Empty uses the real kernel root.
	SysfsRoot string
	// Profile is the resolved slot layout embedded on every report. May be
	// nil when no profile is configured (the node registers but no slots
	// will be seeded — useful for pre-production bring-up).
	Profile *agentv1.Profile
}

// Collect satisfies Collector.
func (c LinuxCollector) Collect(ctx context.Context) (*agentv1.Topology, error) {
	sysfs := gpu.Sysfs{Root: c.SysfsRoot}

	// Try nvidia-smi first — it has the best metadata (model, memory). When
	// GPUs are bound to vfio-pci nvidia-smi fails with "No devices" (exit 9),
	// so we fall back to sysfs enumeration which works regardless of driver
	// binding.
	gpus := c.collectViaNvidiaSmi(ctx)
	if gpus == nil {
		gpus = c.collectViaSysfs(sysfs)
	}

	top := &agentv1.Topology{
		Gpus:         gpus,
		IommuEnabled: sysfs.IOMMUEnabled(),
		Profile:      c.Profile,
	}

	for _, g := range top.Gpus {
		if g.PciAddress == "" {
			continue
		}
		if grp, err := sysfs.IOMMUGroup(g.PciAddress); err == nil {
			g.IommuGroup = grp
		}
		if drv, err := sysfs.CurrentDriver(g.PciAddress); err == nil {
			g.Driver = drv
		}
		if companions, err := sysfs.Companions(g.PciAddress); err == nil {
			g.CompanionPciAddresses = companions
		}
		ok, reason, err := sysfs.DiagnoseBindability(g.PciAddress)
		if err == nil {
			g.VfioReady, _ = sysfs.VFIOReady(g.PciAddress)
			if !ok {
				g.BindBlocker = reason
			} else if !g.VfioReady {
				g.BindBlocker = "host drivers hold the group — run scripts/node-bootstrap.sh"
			}
		}
	}
	return top, nil
}

// collectViaNvidiaSmi returns parsed GPU metadata when nvidia-smi is
// available and succeeds. Returns nil when the binary is missing or fails
// (e.g. GPUs are on vfio-pci) so the caller can fall back.
func (c LinuxCollector) collectViaNvidiaSmi(ctx context.Context) []*agentv1.Gpu {
	smi := c.NvidiaSmiPath
	if smi == "" {
		smi = "nvidia-smi"
	}
	if _, err := exec.LookPath(smi); err != nil {
		return nil
	}
	// nvidia-smi has been observed to hang for tens of seconds in edge
	// cases (driver in bad state, GPU pinned by another process). Bound
	// it independently of the caller ctx so a degraded GPU doesn't park
	// the entire topology collector at startup.
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, smi, "-q", "-x")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	parsed, err := parseNvidiaSmiXML(out)
	if err != nil {
		return nil
	}
	return parsed
}

// collectViaSysfs enumerates NVIDIA GPUs from sysfs. It works whether the
// cards are on the host driver or vfio-pci because it only reads PCI vendor
// and class fields. Model name is synthesised from the PCI vendor:device
// hex pair since nvidia-smi (the usual source) is out of the picture.
func (c LinuxCollector) collectViaSysfs(sysfs gpu.Sysfs) []*agentv1.Gpu {
	pcis, err := sysfs.ListNVIDIAGPUs()
	if err != nil {
		return nil
	}
	out := make([]*agentv1.Gpu, 0, len(pcis))
	for i, pci := range pcis {
		model := "NVIDIA"
		if vendor, verr := sysfs.DeviceVendor(pci); verr == nil {
			if devID, derr := sysfs.DeviceID(pci); derr == nil {
				model = "NVIDIA " + trimHexPrefix(vendor) + ":" + trimHexPrefix(devID)
			}
		}
		out = append(out, &agentv1.Gpu{
			Index:      int32(i), //nolint:gosec // gpu count bounded by host hardware
			PciAddress: pci,
			Model:      model,
		})
	}
	return out
}

func trimHexPrefix(s string) string {
	if strings.HasPrefix(s, "0x") {
		return s[2:]
	}
	return s
}

// --- parsing helpers -------------------------------------------------------

type smiGPU struct {
	ID          string `xml:"id,attr"`
	ProductName string `xml:"product_name"`
	MinorNumber string `xml:"minor_number"`
	MemoryInfo  struct {
		Total string `xml:"total"`
	} `xml:"fb_memory_usage"`
}

type smiLog struct {
	GPUs []smiGPU `xml:"gpu"`
}

func parseNvidiaSmiXML(data []byte) ([]*agentv1.Gpu, error) {
	var l smiLog
	if err := xml.Unmarshal(data, &l); err != nil {
		return nil, err
	}
	out := make([]*agentv1.Gpu, 0, len(l.GPUs))
	for i, g := range l.GPUs {
		out = append(out, &agentv1.Gpu{
			Index:       int32(i), //nolint:gosec // gpu count is bounded by nvidia-smi output (<= 16)
			PciAddress:  normalisePCI(g.ID),
			Model:       strings.TrimSpace(g.ProductName),
			MemoryBytes: parseMemMiB(g.MemoryInfo.Total),
		})
	}
	return out, nil
}

// normalisePCI: nvidia-smi reports "00000000:81:00.0" (16 chars, 8-digit
// domain) but sysfs uses "0000:81:00.0" (12 chars, 4-digit domain). Trim the
// leading four zeros when present so addresses match sysfs lookups.
func normalisePCI(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 16 && s[8] == ':' && s[11] == ':' && strings.HasPrefix(s, "0000") {
		return s[4:]
	}
	return s
}

// parseMemMiB parses "48601 MiB" → bytes. Returns 0 on parse error — memory
// is cosmetic metadata, not control-plane critical.
func parseMemMiB(s string) uint64 {
	parts := strings.Fields(s)
	if len(parts) != 2 {
		return 0
	}
	var mib uint64
	if _, err := fmt.Sscanf(parts[0], "%d", &mib); err != nil {
		return 0
	}
	switch parts[1] {
	case "MiB":
		return mib * 1024 * 1024
	case "GiB":
		return mib * 1024 * 1024 * 1024
	default:
		return 0
	}
}
