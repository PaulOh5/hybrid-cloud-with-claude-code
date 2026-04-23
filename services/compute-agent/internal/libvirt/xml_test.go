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
