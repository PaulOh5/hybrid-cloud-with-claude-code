package topology

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// LinuxCollector inspects the host via nvidia-smi and sysfs. On hosts without
// nvidia-smi (e.g. dev laptops) Collect returns an empty topology with no
// error — that lets Phase 2 agent bring-up work everywhere.
type LinuxCollector struct {
	// NvidiaSmiPath overrides the command path; zero value uses PATH lookup.
	NvidiaSmiPath string
	// IOMMURoot overrides /sys/kernel/iommu_groups for tests.
	IOMMURoot string
}

// Collect satisfies Collector.
func (c LinuxCollector) Collect(ctx context.Context) (*agentv1.Topology, error) {
	smi := c.NvidiaSmiPath
	if smi == "" {
		smi = "nvidia-smi"
	}
	if _, err := exec.LookPath(smi); err != nil {
		return &agentv1.Topology{IommuEnabled: c.iommuEnabled()}, nil
	}

	cmd := exec.CommandContext(ctx, smi, "-q", "-x")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi -q -x: %w", err)
	}

	gpus, err := parseNvidiaSmiXML(out)
	if err != nil {
		return nil, fmt.Errorf("parse nvidia-smi xml: %w", err)
	}

	top := &agentv1.Topology{
		Gpus:         gpus,
		IommuEnabled: c.iommuEnabled(),
	}

	// Best-effort: populate iommu_group and current driver from sysfs.
	for _, g := range top.Gpus {
		if g.PciAddress == "" {
			continue
		}
		g.IommuGroup = iommuGroup(g.PciAddress)
		g.Driver = pciDriver(g.PciAddress)
	}
	return top, nil
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

func (c LinuxCollector) iommuEnabled() bool {
	root := c.IOMMURoot
	if root == "" {
		root = "/sys/kernel/iommu_groups"
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

func iommuGroup(pci string) string {
	link, err := os.Readlink(filepath.Join("/sys/bus/pci/devices", pci, "iommu_group"))
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

func pciDriver(pci string) string {
	link, err := os.Readlink(filepath.Join("/sys/bus/pci/devices", pci, "driver"))
	if err != nil {
		return "unknown"
	}
	return filepath.Base(link)
}
