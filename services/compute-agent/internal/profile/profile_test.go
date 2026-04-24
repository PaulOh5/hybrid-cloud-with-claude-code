package profile_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"hybridcloud/services/compute-agent/internal/profile"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolve_SingleSlotPerGPU(t *testing.T) {
	t.Parallel()

	path := writeYAML(t, `active: "2x1"
layouts:
  - name: "2x1"
    slots:
      - {size: 1, gpu_indices: [0]}
      - {size: 1, gpu_indices: [1]}
`)
	f, raw, err := profile.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	res, err := profile.Resolve(f, raw, []int32{0, 1})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Name != "2x1" || len(res.Slots) != 2 {
		t.Fatalf("unexpected: %+v", res)
	}
	if res.Hash == "" {
		t.Fatal("hash must be populated")
	}
}

func TestResolve_DualGPUSlot(t *testing.T) {
	t.Parallel()

	path := writeYAML(t, `active: "1x2"
layouts:
  - name: "1x2"
    slots:
      - {size: 2, gpu_indices: [0, 1], nvlink_domain: "group-a"}
`)
	f, raw, err := profile.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	res, err := profile.Resolve(f, raw, []int32{0, 1})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(res.Slots) != 1 || res.Slots[0].Size != 2 || res.Slots[0].NvlinkDomain != "group-a" {
		t.Fatalf("unexpected: %+v", res.Slots)
	}
}

func TestResolve_HashIsStable(t *testing.T) {
	t.Parallel()

	body := `active: "x"
layouts:
  - name: "x"
    slots:
      - {size: 1, gpu_indices: [0]}
`
	p1 := writeYAML(t, body)
	p2 := writeYAML(t, body)
	f1, r1, _ := profile.Load(p1)
	f2, r2, _ := profile.Load(p2)
	res1, _ := profile.Resolve(f1, r1, []int32{0})
	res2, _ := profile.Resolve(f2, r2, []int32{0})
	if res1.Hash != res2.Hash {
		t.Fatalf("hashes differ: %s vs %s", res1.Hash, res2.Hash)
	}
}

func TestResolve_ErrorCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		body      string
		hostGPUs  []int32
		wantErrIs error
	}{
		{
			name: "active missing",
			body: `active: "nope"
layouts:
  - {name: "yes", slots: [{size: 1, gpu_indices: [0]}]}`,
			hostGPUs:  []int32{0},
			wantErrIs: profile.ErrActiveMissing,
		},
		{
			name: "size mismatch",
			body: `active: "x"
layouts:
  - name: "x"
    slots:
      - {size: 2, gpu_indices: [0]}`,
			hostGPUs:  []int32{0, 1},
			wantErrIs: profile.ErrSlotSizeMismatch,
		},
		{
			name: "unknown gpu",
			body: `active: "x"
layouts:
  - name: "x"
    slots:
      - {size: 1, gpu_indices: [5]}`,
			hostGPUs:  []int32{0, 1},
			wantErrIs: profile.ErrUnknownGPU,
		},
		{
			name: "duplicate gpu across slots",
			body: `active: "x"
layouts:
  - name: "x"
    slots:
      - {size: 1, gpu_indices: [0]}
      - {size: 1, gpu_indices: [0]}`,
			hostGPUs:  []int32{0, 1},
			wantErrIs: profile.ErrDuplicateGPU,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := writeYAML(t, tc.body)
			f, raw, err := profile.Load(p)
			if err != nil {
				t.Fatal(err)
			}
			_, err = profile.Resolve(f, raw, tc.hostGPUs)
			if !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("expected %v, got %v", tc.wantErrIs, err)
			}
		})
	}
}

func TestResolve_ProtoRoundtrip(t *testing.T) {
	t.Parallel()

	p := writeYAML(t, `active: "a"
layouts:
  - name: "a"
    slots:
      - {size: 1, gpu_indices: [0]}
      - {size: 1, gpu_indices: [1]}
`)
	f, raw, _ := profile.Load(p)
	res, _ := profile.Resolve(f, raw, []int32{0, 1})
	pb := res.Proto()
	if pb.Name != "a" || pb.Hash == "" || len(pb.Slots) != 2 {
		t.Fatalf("proto: %+v", pb)
	}
	if pb.Slots[0].SlotIndex != 0 || pb.Slots[1].SlotIndex != 1 {
		t.Fatalf("slot_index must be assigned from array position: %+v", pb.Slots)
	}
}
