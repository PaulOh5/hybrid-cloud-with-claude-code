package libvirt

import (
	"encoding/xml"
	"fmt"
)

// BuildDomainXML renders a libvirt <domain> definition for the given spec.
// The layout targets q35 + kvm on Ubuntu hosts; Phase 4 adds <hostdev> for
// GPU passthrough.
func BuildDomainXML(spec DomainSpec) ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	d := domainXML{
		Type: "kvm",
		Name: spec.Name,
		OS: osXML{
			Type: osTypeXML{
				Arch:    "x86_64",
				Machine: machineType(spec.MachineType),
				Value:   "hvm",
			},
			Boot: []bootXML{{Dev: "hd"}},
		},
		Memory:     memXML{Unit: "MiB", Value: spec.MemoryMiB},
		CurMemory:  memXML{Unit: "MiB", Value: spec.MemoryMiB},
		VCPU:       vcpuXML{Placement: "static", Value: spec.VCPUs},
		Features:   featuresXML{ACPI: &emptyXML{}, APIC: &emptyXML{}},
		CPU:        cpuXML{Mode: "host-passthrough", Check: "none"},
		OnPoweroff: "destroy",
		OnReboot:   "restart",
		OnCrash:    "destroy",
		Devices: devicesXML{
			Emulator: emulator(spec.Emulator),
			Disks: []diskXML{
				{
					Type:   "file",
					Device: "disk",
					Driver: driverXML{Name: "qemu", Type: "qcow2", Cache: "writeback"},
					Source: sourceXML{File: spec.DiskPath},
					Target: targetXML{Dev: "vda", Bus: "virtio"},
				},
			},
			Consoles: []consoleXML{{
				Type:   "pty",
				Target: consoleTargetXML{Type: "serial", Port: 0},
			}},
			Serials: []serialXML{{
				Type:   "pty",
				Target: serialTargetXML{Port: 0},
			}},
			Graphics: []graphicsXML{{
				Type:   "vnc",
				Port:   -1,
				Listen: "127.0.0.1",
			}},
		},
	}

	if spec.CloudInitISOPath != "" {
		d.Devices.Disks = append(d.Devices.Disks, diskXML{
			Type:     "file",
			Device:   "cdrom",
			Driver:   driverXML{Name: "qemu", Type: "raw"},
			Source:   sourceXML{File: spec.CloudInitISOPath},
			Target:   targetXML{Dev: "sda", Bus: "sata"},
			Readonly: &emptyXML{},
		})
	}

	if spec.NetworkName != "" {
		d.Devices.Interfaces = append(d.Devices.Interfaces, interfaceXML{
			Type:   "network",
			Source: interfaceSourceXML{Network: spec.NetworkName},
			Model:  interfaceModelXML{Type: "virtio"},
		})
	}

	buf, err := xml.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal domain xml: %w", err)
	}
	return buf, nil
}

func machineType(v string) string {
	if v != "" {
		return v
	}
	return "q35"
}

func emulator(v string) string {
	if v != "" {
		return v
	}
	return "/usr/bin/qemu-system-x86_64"
}

// --- xml structs -----------------------------------------------------------
// The types mirror the subset of the libvirt domain schema we emit. Fields
// live as a single contiguous block so adding new elements stays grep-able.

type domainXML struct {
	XMLName    xml.Name    `xml:"domain"`
	Type       string      `xml:"type,attr"`
	Name       string      `xml:"name"`
	Memory     memXML      `xml:"memory"`
	CurMemory  memXML      `xml:"currentMemory"`
	VCPU       vcpuXML     `xml:"vcpu"`
	OS         osXML       `xml:"os"`
	Features   featuresXML `xml:"features"`
	CPU        cpuXML      `xml:"cpu"`
	OnPoweroff string      `xml:"on_poweroff"`
	OnReboot   string      `xml:"on_reboot"`
	OnCrash    string      `xml:"on_crash"`
	Devices    devicesXML  `xml:"devices"`
}

type memXML struct {
	Unit  string `xml:"unit,attr"`
	Value uint32 `xml:",chardata"`
}

type vcpuXML struct {
	Placement string `xml:"placement,attr"`
	Value     uint32 `xml:",chardata"`
}

type osXML struct {
	Type osTypeXML `xml:"type"`
	Boot []bootXML `xml:"boot"`
}

type osTypeXML struct {
	Arch    string `xml:"arch,attr"`
	Machine string `xml:"machine,attr"`
	Value   string `xml:",chardata"`
}

type bootXML struct {
	Dev string `xml:"dev,attr"`
}

type featuresXML struct {
	ACPI *emptyXML `xml:"acpi,omitempty"`
	APIC *emptyXML `xml:"apic,omitempty"`
}

type cpuXML struct {
	Mode  string `xml:"mode,attr"`
	Check string `xml:"check,attr,omitempty"`
}

type devicesXML struct {
	Emulator   string         `xml:"emulator"`
	Disks      []diskXML      `xml:"disk"`
	Interfaces []interfaceXML `xml:"interface"`
	Consoles   []consoleXML   `xml:"console"`
	Serials    []serialXML    `xml:"serial"`
	Graphics   []graphicsXML  `xml:"graphics"`
}

type diskXML struct {
	Type     string    `xml:"type,attr"`
	Device   string    `xml:"device,attr"`
	Driver   driverXML `xml:"driver"`
	Source   sourceXML `xml:"source"`
	Target   targetXML `xml:"target"`
	Readonly *emptyXML `xml:"readonly,omitempty"`
}

type driverXML struct {
	Name  string `xml:"name,attr"`
	Type  string `xml:"type,attr"`
	Cache string `xml:"cache,attr,omitempty"`
}

type sourceXML struct {
	File string `xml:"file,attr"`
}

type targetXML struct {
	Dev string `xml:"dev,attr"`
	Bus string `xml:"bus,attr"`
}

type interfaceXML struct {
	Type   string             `xml:"type,attr"`
	Source interfaceSourceXML `xml:"source"`
	Model  interfaceModelXML  `xml:"model"`
}

type interfaceSourceXML struct {
	Network string `xml:"network,attr"`
}

type interfaceModelXML struct {
	Type string `xml:"type,attr"`
}

type consoleXML struct {
	Type   string           `xml:"type,attr"`
	Target consoleTargetXML `xml:"target"`
}

type consoleTargetXML struct {
	Type string `xml:"type,attr"`
	Port int    `xml:"port,attr"`
}

type serialXML struct {
	Type   string          `xml:"type,attr"`
	Target serialTargetXML `xml:"target"`
}

type serialTargetXML struct {
	Port int `xml:"port,attr"`
}

type graphicsXML struct {
	Type   string `xml:"type,attr"`
	Port   int    `xml:"port,attr"`
	Listen string `xml:"listen,attr"`
}

type emptyXML struct{}
