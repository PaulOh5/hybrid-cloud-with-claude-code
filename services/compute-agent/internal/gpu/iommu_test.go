package gpu_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"hybridcloud/services/compute-agent/internal/gpu"
)

// sysfsFixture builds an in-memory /sys tree that looks enough like the
// real kernel layout for the IOMMU/driver helpers to work against.
type sysfsFixture struct {
	root string
	t    *testing.T
}

func newFixture(t *testing.T) *sysfsFixture {
	t.Helper()
	f := &sysfsFixture{root: t.TempDir(), t: t}
	// Ensure the root /sys/kernel/iommu_groups exists so IOMMUEnabled works
	// once at least one group is added.
	return f
}

// addDevice creates a PCI device entry in a given iommu group. class is the
// sysfs "class" file contents (e.g. "0x030000" for VGA). driver empty means
// no driver bound.
func (f *sysfsFixture) addDevice(pci, group, class, driver string) {
	f.t.Helper()

	groupDir := filepath.Join(f.root, "kernel/iommu_groups", group, "devices")
	if err := os.MkdirAll(groupDir, 0o755); err != nil {
		f.t.Fatal(err)
	}

	devDir := filepath.Join(f.root, "bus/pci/devices", pci)
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		f.t.Fatal(err)
	}

	// Cross-link: kernel/iommu_groups/<group>/devices/<pci> → bus/pci/devices/<pci>
	relTarget, err := filepath.Rel(groupDir, devDir)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(relTarget, filepath.Join(groupDir, pci)); err != nil {
		f.t.Fatal(err)
	}

	// bus/pci/devices/<pci>/iommu_group → kernel/iommu_groups/<group>
	groupRoot := filepath.Join(f.root, "kernel/iommu_groups", group)
	relGroup, err := filepath.Rel(devDir, groupRoot)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(relGroup, filepath.Join(devDir, "iommu_group")); err != nil {
		f.t.Fatal(err)
	}

	// class file.
	if err := os.WriteFile(filepath.Join(devDir, "class"), []byte(class+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}

	if driver != "" {
		driverDir := filepath.Join(f.root, "bus/pci/drivers", driver)
		if err := os.MkdirAll(driverDir, 0o755); err != nil {
			f.t.Fatal(err)
		}
		relDrv, err := filepath.Rel(devDir, driverDir)
		if err != nil {
			f.t.Fatal(err)
		}
		if err := os.Symlink(relDrv, filepath.Join(devDir, "driver")); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *sysfsFixture) sysfs() gpu.Sysfs {
	return gpu.Sysfs{Root: f.root}
}

// --- tests ---------------------------------------------------------------

func TestIOMMUEnabled(t *testing.T) {
	t.Parallel()

	empty := gpu.Sysfs{Root: t.TempDir()}
	if empty.IOMMUEnabled() {
		t.Fatal("empty tree must report disabled")
	}

	f := newFixture(t)
	f.addDevice("0000:16:00.0", "16", "0x030000", "nvidia")
	if !f.sysfs().IOMMUEnabled() {
		t.Fatal("with a group, IOMMUEnabled must be true")
	}
}

func TestIOMMUGroupAndMembers(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	// GPU + audio sharing group 16 — the h20a RTX 5090 layout.
	f.addDevice("0000:16:00.0", "16", "0x030000", "nvidia")
	f.addDevice("0000:16:00.1", "16", "0x040300", "snd_hda_intel")

	s := f.sysfs()

	group, err := s.IOMMUGroup("0000:16:00.0")
	if err != nil {
		t.Fatalf("group: %v", err)
	}
	if group != "16" {
		t.Fatalf("group: got %q, want 16", group)
	}

	members, err := s.GroupMembers("0000:16:00.0")
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if got, want := len(members), 2; got != want {
		t.Fatalf("members len: got %d, want %d", got, want)
	}
	if members[0] != "0000:16:00.0" || members[1] != "0000:16:00.1" {
		t.Fatalf("members: %+v", members)
	}

	companions, err := s.Companions("0000:16:00.0")
	if err != nil {
		t.Fatalf("companions: %v", err)
	}
	if len(companions) != 1 || companions[0] != "0000:16:00.1" {
		t.Fatalf("companions: %+v", companions)
	}
}

func TestIOMMUGroup_UnknownDevice(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.addDevice("0000:16:00.0", "16", "0x030000", "nvidia")

	_, err := f.sysfs().IOMMUGroup("0000:99:00.0")
	if !errors.Is(err, gpu.ErrDeviceNotFound) {
		t.Fatalf("expected ErrDeviceNotFound, got %v", err)
	}
}

func TestCurrentDriver(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.addDevice("0000:16:00.0", "16", "0x030000", "nvidia")
	f.addDevice("0000:16:00.1", "16", "0x040300", "") // no driver bound

	s := f.sysfs()
	if drv, _ := s.CurrentDriver("0000:16:00.0"); drv != "nvidia" {
		t.Fatalf("nvidia gpu: got %q", drv)
	}
	if drv, _ := s.CurrentDriver("0000:16:00.1"); drv != "" {
		t.Fatalf("unbound: got %q", drv)
	}
}

func TestVFIOReady(t *testing.T) {
	t.Parallel()

	t.Run("not ready — gpu still on nvidia", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.addDevice("0000:16:00.0", "16", "0x030000", "nvidia")
		f.addDevice("0000:16:00.1", "16", "0x040300", "vfio-pci")

		ok, err := f.sysfs().VFIOReady("0000:16:00.0")
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("should be not-ready when GPU is on nvidia")
		}
	})

	t.Run("ready — both on vfio-pci", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.addDevice("0000:43:00.0", "13", "0x030000", "vfio-pci")
		f.addDevice("0000:43:00.1", "13", "0x040300", "vfio-pci")

		ok, err := f.sysfs().VFIOReady("0000:43:00.0")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("should be ready when whole group is on vfio-pci")
		}
	})

	t.Run("not ready — companion still on audio driver", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.addDevice("0000:43:00.0", "13", "0x030000", "vfio-pci")
		f.addDevice("0000:43:00.1", "13", "0x040300", "snd_hda_intel")

		ok, err := f.sysfs().VFIOReady("0000:43:00.0")
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("companion blocks readiness")
		}
	})
}

func TestListNVIDIAGPUs(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	// Two GPUs + audio companions + one non-NVIDIA device.
	f.addDevice("0000:16:00.0", "16", "0x030000", "vfio-pci")
	writeVendorDevice(t, f.root, "0000:16:00.0", "0x10de", "0x2b85")

	f.addDevice("0000:16:00.1", "16", "0x040300", "vfio-pci") // audio — not a GPU
	writeVendorDevice(t, f.root, "0000:16:00.1", "0x10de", "0x22e8")

	f.addDevice("0000:43:00.0", "13", "0x030200", "vfio-pci") // 3D controller
	writeVendorDevice(t, f.root, "0000:43:00.0", "0x10de", "0x2b85")

	f.addDevice("0000:02:00.0", "1", "0x030000", "ast")
	writeVendorDevice(t, f.root, "0000:02:00.0", "0x1a03", "0x2000") // ASPEED

	pcis, err := f.sysfs().ListNVIDIAGPUs()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got, want := len(pcis), 2; got != want {
		t.Fatalf("got %d, want %d: %v", got, want, pcis)
	}
	// Only the two class=0x0300* + vendor=0x10de devices; ASPEED and the
	// audio companion must be excluded.
	if pcis[0] != "0000:16:00.0" || pcis[1] != "0000:43:00.0" {
		t.Fatalf("unexpected: %v", pcis)
	}
}

// writeVendorDevice writes the vendor/device sysfs files used by
// ListNVIDIAGPUs.
func writeVendorDevice(t *testing.T, root, pci, vendor, device string) {
	t.Helper()
	base := filepath.Join(root, "bus/pci/devices", pci)
	if err := os.WriteFile(filepath.Join(base, "vendor"), []byte(vendor+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "device"), []byte(device+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResetDevice(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.addDevice("0000:16:00.0", "16", "0x030000", "vfio-pci")
	writeVendorDevice(t, f.root, "0000:16:00.0", "0x10de", "0x2b85")

	resetPath := filepath.Join(f.root, "bus/pci/devices/0000:16:00.0/reset")
	// Real sysfs reset is write-only, but the test needs to read the value
	// back to verify; use 0o600 so both sides work inside the tmpdir.
	if err := os.WriteFile(resetPath, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.sysfs().ResetDevice("0000:16:00.0"); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// Verify sysfs fake now contains our "1".
	contents, _ := os.ReadFile(resetPath)
	if string(contents) != "1" {
		t.Fatalf("reset sysfs not written: %q", contents)
	}
}

func TestResetDevice_MissingFile(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.addDevice("0000:99:00.0", "99", "0x030000", "vfio-pci")
	// No reset file written.
	err := f.sysfs().ResetDevice("0000:99:00.0")
	if err == nil {
		t.Fatal("expected error when reset file absent")
	}
}

func TestDiagnoseBindability(t *testing.T) {
	t.Parallel()

	t.Run("iommu off", func(t *testing.T) {
		t.Parallel()
		empty := gpu.Sysfs{Root: t.TempDir()}
		ok, reason, err := empty.DiagnoseBindability("0000:16:00.0")
		if err != nil {
			t.Fatal(err)
		}
		if ok || reason == "" {
			t.Fatalf("expected iommu-off reason, got ok=%v reason=%q", ok, reason)
		}
	})

	t.Run("clean gpu + audio group", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.addDevice("0000:16:00.0", "16", "0x030000", "nvidia")
		f.addDevice("0000:16:00.1", "16", "0x040300", "snd_hda_intel")

		ok, reason, err := f.sysfs().DiagnoseBindability("0000:16:00.0")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("expected ok, got reason=%q", reason)
		}
	})

	t.Run("group has unrelated device", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.addDevice("0000:16:00.0", "16", "0x030000", "nvidia")
		f.addDevice("0000:16:00.1", "16", "0x040300", "snd_hda_intel")
		// Simulate ACS-wide root port sharing the group — common reason to
		// need ACS override.
		f.addDevice("0000:15:00.0", "16", "0x060400", "pcieport")

		ok, reason, err := f.sysfs().DiagnoseBindability("0000:16:00.0")
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("expected not-ok due to root port: reason=%q", reason)
		}
	})
}
