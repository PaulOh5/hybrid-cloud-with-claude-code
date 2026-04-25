package vm

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"hybridcloud/services/compute-agent/internal/libvirt"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// duplicateDropSink signals on each "duplicate create dropped" warning so
// tests can synchronize on the rejected goroutine without racy sleeps.
type duplicateDropSink struct {
	fired chan struct{}
}

func (s *duplicateDropSink) Enabled(context.Context, slog.Level) bool { return true }
func (s *duplicateDropSink) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn && r.Message == "duplicate create dropped" {
		select {
		case s.fired <- struct{}{}:
		default:
		}
	}
	return nil
}
func (s *duplicateDropSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *duplicateDropSink) WithGroup(string) slog.Handler      { return s }

// --- fake libvirt manager --------------------------------------------------

type fakeMgr struct {
	mu         sync.Mutex
	created    []libvirt.DomainSpec
	destroyed  []string
	createErr  error
	destroyErr error
}

func (f *fakeMgr) CreateDomain(_ context.Context, s libvirt.DomainSpec) (libvirt.DomainInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return libvirt.DomainInfo{}, f.createErr
	}
	f.created = append(f.created, s)
	return libvirt.DomainInfo{Name: s.Name, UUID: "fake-uuid", InitialState: libvirt.StateRunning}, nil
}
func (f *fakeMgr) DestroyDomain(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = append(f.destroyed, name)
	return nil
}
func (f *fakeMgr) DomainState(_ context.Context, _ string) (libvirt.DomainState, error) {
	return libvirt.StateRunning, nil
}
func (f *fakeMgr) DomainPassthroughPCI(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (f *fakeMgr) DomainIPv4(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (f *fakeMgr) StreamEvents(_ context.Context) (<-chan libvirt.DomainEvent, error) {
	ch := make(chan libvirt.DomainEvent)
	close(ch)
	return ch, nil
}
func (f *fakeMgr) Close() error { return nil }

// --- helpers ---------------------------------------------------------------

func collectStatuses(t *testing.T, ch <-chan *agentv1.AgentMessage, want int, timeout time.Duration) []*agentv1.InstanceStatus {
	t.Helper()
	out := make([]*agentv1.InstanceStatus, 0, want)
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case msg := <-ch:
			if s := msg.GetInstanceStatus(); s != nil {
				out = append(out, s)
			}
		case <-deadline:
			return out
		}
	}
	return out
}

// --- tests -----------------------------------------------------------------

func TestHandler_CreateThenDestroy(t *testing.T) {
	t.Parallel()

	mgr := &fakeMgr{}
	cfg := Config{
		ImageDir:  filepath.Join(t.TempDir(), "images"),
		SeedDir:   filepath.Join(t.TempDir(), "seeds"),
		BaseImage: "/does/not/matter",
	}
	h := New(mgr, cfg, nil)
	// Replace qemu-img with a touch-file no-op so the test works without qemu-img.
	h.WithImageCreator(func(_ context.Context, _ string, dst string) error {
		return os.WriteFile(dst, []byte{}, 0o600)
	})

	statuses := make(chan *agentv1.AgentMessage, 16)
	send := func(m *agentv1.AgentMessage) { statuses <- m }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h.OnControl(ctx, &agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_CreateInstance{
			CreateInstance: &agentv1.CreateInstance{
				// Use different InstanceId and Name so tests catch the
				// libvirt-domain-name bug (instance_id, not display name,
				// must be the key).
				InstanceId: "inst-1",
				Name:       "user-friendly-display-name",
				MemoryMb:   1024,
				Vcpus:      1,
				SshPubkeys: []string{"ssh-ed25519 AAAA"},
			},
		},
	}, send)

	got := collectStatuses(t, statuses, 2, 3*time.Second)
	if len(got) != 2 {
		t.Fatalf("expected 2 statuses (provisioning + running), got %d", len(got))
	}
	if got[0].State != agentv1.InstanceState_INSTANCE_STATE_PROVISIONING {
		t.Fatalf("first state: %v", got[0].State)
	}
	if got[1].State != agentv1.InstanceState_INSTANCE_STATE_RUNNING {
		t.Fatalf("second state: %v", got[1].State)
	}

	mgr.mu.Lock()
	if len(mgr.created) != 1 || mgr.created[0].Name != "inst-1" {
		t.Fatalf("create not called correctly: %+v", mgr.created)
	}
	if mgr.created[0].CloudInitISOPath == "" {
		t.Fatal("expected cloud-init ISO path set")
	}
	mgr.mu.Unlock()

	// Now destroy.
	h.OnControl(ctx, &agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_DestroyInstance{
			DestroyInstance: &agentv1.DestroyInstance{InstanceId: "inst-1"},
		},
	}, send)

	got = collectStatuses(t, statuses, 2, 3*time.Second)
	if len(got) != 2 {
		t.Fatalf("expected 2 destroy statuses, got %d", len(got))
	}
	if got[0].State != agentv1.InstanceState_INSTANCE_STATE_STOPPING {
		t.Fatalf("destroy first: %v", got[0].State)
	}
	if got[1].State != agentv1.InstanceState_INSTANCE_STATE_STOPPED {
		t.Fatalf("destroy second: %v", got[1].State)
	}

	// The per-instance files should be cleaned up.
	if _, err := os.Stat(filepath.Join(cfg.ImageDir, "inst-1.qcow2")); !os.IsNotExist(err) {
		t.Fatalf("disk should be removed, got err=%v", err)
	}
}

func TestHandler_CreateReportsFailureOnLibvirtError(t *testing.T) {
	t.Parallel()

	mgr := &fakeMgr{createErr: errors.New("boom")}
	cfg := Config{ImageDir: t.TempDir(), SeedDir: t.TempDir(), BaseImage: "/x"}
	h := New(mgr, cfg, nil)
	h.WithImageCreator(func(_ context.Context, _, dst string) error {
		return os.WriteFile(dst, []byte{}, 0o600)
	})

	statuses := make(chan *agentv1.AgentMessage, 16)
	send := func(m *agentv1.AgentMessage) { statuses <- m }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	h.OnControl(ctx, &agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_CreateInstance{
			CreateInstance: &agentv1.CreateInstance{InstanceId: "x", Name: "x", MemoryMb: 512, Vcpus: 1},
		},
	}, send)

	got := collectStatuses(t, statuses, 2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("statuses: %d; first=%+v", len(got), got[0])
	}
	if got[1].State != agentv1.InstanceState_INSTANCE_STATE_FAILED {
		t.Fatalf("expected FAILED, got %v", got[1].State)
	}
	if got[1].ErrorMessage == "" {
		t.Fatal("expected error message")
	}
}

func TestHandler_DuplicateCreateIgnored(t *testing.T) {
	t.Parallel()

	mgr := &fakeMgr{}
	cfg := Config{ImageDir: t.TempDir(), SeedDir: t.TempDir(), BaseImage: "/x"}

	// Custom slog handler signals when the rejected goroutine logs the drop —
	// the test relies on this to know G2 has run acquire and failed before
	// releasing G1.
	sink := &duplicateDropSink{fired: make(chan struct{}, 2)}

	// Block the image creator so the first create is still in flight when the
	// second arrives. `entered` confirms G1 has acquired the in-flight slot
	// before G2 fires.
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	h := New(mgr, cfg, slog.New(sink))
	h.WithImageCreator(func(ctx context.Context, _, dst string) error {
		entered <- struct{}{}
		<-release
		return os.WriteFile(dst, []byte{}, 0o600)
	})

	statuses := make(chan *agentv1.AgentMessage, 16)
	send := func(m *agentv1.AgentMessage) { statuses <- m }

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	create := &agentv1.ControlMessage{
		Payload: &agentv1.ControlMessage_CreateInstance{
			CreateInstance: &agentv1.CreateInstance{InstanceId: "dup", Name: "dup", MemoryMb: 512, Vcpus: 1},
		},
	}

	// First create — wait until G1 is blocked in imgFn (past acquire).
	h.OnControl(ctx, create, send)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first create did not enter imgFn")
	}

	// Duplicate — must hit acquire and be rejected because G1 still holds
	// the in-flight slot. Wait for the drop log to confirm.
	h.OnControl(ctx, create, send)
	select {
	case <-sink.fired:
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate goroutine did not log drop")
	}

	close(release)
	got := collectStatuses(t, statuses, 4, 2*time.Second)
	// 2 statuses expected (provisioning + running from G1). Anything > 2
	// means the duplicate leaked through.
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 statuses, got %d", len(got))
	}
}
