package libvirt

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestBuildDomainXML_MinimalValidInput(t *testing.T) {
	t.Parallel()

	out, err := BuildDomainXML(DomainSpec{
		Name:      "inst-abc",
		MemoryMiB: 4096,
		VCPUs:     2,
		DiskPath:  "/var/lib/libvirt/images/inst-abc.qcow2",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(out)

	for _, want := range []string{
		`<domain type="kvm">`,
		`<name>inst-abc</name>`,
		`<memory unit="MiB">4096</memory>`,
		`<currentMemory unit="MiB">4096</currentMemory>`,
		`<vcpu placement="static">2</vcpu>`,
		`machine="q35"`,
		`<emulator>/usr/bin/qemu-system-x86_64</emulator>`,
		`<source file="/var/lib/libvirt/images/inst-abc.qcow2"></source>`,
		`<cpu mode="host-passthrough" check="none"></cpu>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n---\n%s", want, s)
		}
	}

	// Make sure the result is well-formed XML.
	var parsed struct {
		XMLName xml.Name `xml:"domain"`
	}
	if err := xml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
}

func TestBuildDomainXML_WithCloudInitAndNetwork(t *testing.T) {
	t.Parallel()

	out, err := BuildDomainXML(DomainSpec{
		Name:             "inst-xyz",
		MemoryMiB:        2048,
		VCPUs:            1,
		DiskPath:         "/imgs/inst-xyz.qcow2",
		CloudInitISOPath: "/imgs/inst-xyz-seed.iso",
		NetworkName:      "default",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(out)

	for _, want := range []string{
		`<disk type="file" device="cdrom">`,
		`<source file="/imgs/inst-xyz-seed.iso"></source>`,
		`<readonly></readonly>`,
		`<interface type="network">`,
		`<source network="default"></source>`,
		`<model type="virtio"></model>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n---\n%s", want, s)
		}
	}
}

func TestBuildDomainXML_Hostdev_GPUPassthrough(t *testing.T) {
	t.Parallel()

	out, err := BuildDomainXML(DomainSpec{
		Name:      "inst-gpu",
		MemoryMiB: 16384,
		VCPUs:     4,
		DiskPath:  "/imgs/inst-gpu.qcow2",
		// RTX 5090 + HDMI audio companion from h20a layout.
		PassthroughPCI: []string{"0000:16:00.0", "0000:16:00.1"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(out)

	// There should be exactly two <hostdev> blocks with managed="yes" and
	// the vfio driver.
	if strings.Count(s, "<hostdev") != 2 {
		t.Fatalf("expected 2 hostdev entries, got %d:\n%s", strings.Count(s, "<hostdev"), s)
	}
	for _, want := range []string{
		`<hostdev mode="subsystem" type="pci" managed="yes">`,
		`<driver name="vfio"></driver>`,
		// GPU itself.
		`<address domain="0x0000" bus="0x16" slot="0x00" function="0x0"></address>`,
		// HDMI audio companion (function 0x1).
		`<address domain="0x0000" bus="0x16" slot="0x00" function="0x1"></address>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q\n---\n%s", want, s)
		}
	}
}

func TestBuildDomainXML_Hostdev_RejectsBadPCI(t *testing.T) {
	t.Parallel()

	cases := []string{
		"16:00.0",      // missing domain
		"0000-16-00.0", // wrong separator
		"0000:zz:00.0", // non-hex
		"0000:16:00",   // missing function
	}
	for _, pci := range cases {
		pci := pci
		t.Run(pci, func(t *testing.T) {
			t.Parallel()
			_, err := BuildDomainXML(DomainSpec{
				Name:           "x",
				MemoryMiB:      1024,
				VCPUs:          1,
				DiskPath:       "/x",
				PassthroughPCI: []string{pci},
			})
			if err == nil {
				t.Fatalf("expected error for %q", pci)
			}
		})
	}
}

func TestParsePassthroughPCIFromXML_Roundtrip(t *testing.T) {
	t.Parallel()

	out, err := BuildDomainXML(DomainSpec{
		Name:           "rt",
		MemoryMiB:      4096,
		VCPUs:          2,
		DiskPath:       "/x.qcow2",
		PassthroughPCI: []string{"0000:16:00.0", "0000:16:00.1", "0000:43:00.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParsePassthroughPCIFromXML(string(out))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"0000:16:00.0", "0000:16:00.1", "0000:43:00.0"}
	if len(got) != len(want) {
		t.Fatalf("count: got %d, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("got[%d]=%q want %q", i, got[i], w)
		}
	}
}

func TestBuildDomainXML_ValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		spec DomainSpec
	}{
		{"no name", DomainSpec{MemoryMiB: 1024, VCPUs: 1, DiskPath: "/x"}},
		{"no memory", DomainSpec{Name: "a", VCPUs: 1, DiskPath: "/x"}},
		{"no vcpus", DomainSpec{Name: "a", MemoryMiB: 1024, DiskPath: "/x"}},
		{"no disk", DomainSpec{Name: "a", MemoryMiB: 1024, VCPUs: 1}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := BuildDomainXML(tc.spec); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}
