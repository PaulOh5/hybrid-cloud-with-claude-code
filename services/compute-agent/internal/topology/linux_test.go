package topology

import (
	"context"
	"testing"
)

const sampleSMIXML = `
<nvidia_smi_log>
  <gpu id="00000000:81:00.0">
    <product_name>NVIDIA RTX A6000</product_name>
    <minor_number>0</minor_number>
    <fb_memory_usage>
      <total>48601 MiB</total>
    </fb_memory_usage>
  </gpu>
  <gpu id="00000000:82:00.0">
    <product_name>NVIDIA RTX A6000</product_name>
    <minor_number>1</minor_number>
    <fb_memory_usage>
      <total>48601 MiB</total>
    </fb_memory_usage>
  </gpu>
</nvidia_smi_log>`

func TestParseNvidiaSmiXML(t *testing.T) {
	t.Parallel()

	gpus, err := parseNvidiaSmiXML([]byte(sampleSMIXML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := len(gpus), 2; got != want {
		t.Fatalf("gpu count: got %d, want %d", got, want)
	}
	if gpus[0].PciAddress != "0000:81:00.0" {
		t.Fatalf("pci0: %q", gpus[0].PciAddress)
	}
	if gpus[1].PciAddress != "0000:82:00.0" {
		t.Fatalf("pci1: %q", gpus[1].PciAddress)
	}
	if gpus[0].Model != "NVIDIA RTX A6000" {
		t.Fatalf("model: %q", gpus[0].Model)
	}
	expected := uint64(48601) * 1024 * 1024
	if gpus[0].MemoryBytes != expected {
		t.Fatalf("memory: got %d, want %d", gpus[0].MemoryBytes, expected)
	}
}

func TestStatic_Empty(t *testing.T) {
	t.Parallel()
	top, err := Empty().Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(top.Gpus) != 0 {
		t.Fatalf("expected empty topology, got %d gpus", len(top.Gpus))
	}
}
