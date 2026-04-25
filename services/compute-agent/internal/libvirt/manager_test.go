package libvirt

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// fakeConn is an in-memory Connector used by unit tests so they can exercise
// LibvirtManager without libvirtd on the host.
type fakeConn struct {
	mu          sync.Mutex
	domains     map[string]*golibvirt.Domain
	states      map[string]int32
	createFail  bool
	destroyFail bool
	notRunning  bool
}

func newFakeConn() *fakeConn {
	return &fakeConn{
		domains: map[string]*golibvirt.Domain{},
		states:  map[string]int32{},
	}
}

func (f *fakeConn) DomainDefineXML(_ string) (golibvirt.Domain, error) {
	// digitalocean/go-libvirt would parse the XML and find <name>; the real
	// code path in BuildDomainXML is covered by xml_test. Here the fake
	// simply relies on the last-defined name via DomainDefineXML's caller to
	// push through DomainLookupByName.
	return golibvirt.Domain{}, errors.New("fakeConn.DomainDefineXML should be driven via define helper")
}

// define is a fake-only helper so tests can register a domain without caring
// about XML parsing.
func (f *fakeConn) define(name string) golibvirt.Domain {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := golibvirt.Domain{Name: name}
	f.domains[name] = &d
	f.states[name] = 5 // shutoff
	return d
}

func (f *fakeConn) DomainCreate(d golibvirt.Domain) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createFail {
		return errors.New("fake create failure")
	}
	if _, ok := f.domains[d.Name]; !ok {
		return errors.New("not defined")
	}
	f.states[d.Name] = 1 // running
	return nil
}

func (f *fakeConn) DomainDestroy(d golibvirt.Domain) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyFail {
		return errors.New("fake destroy failure")
	}
	if f.notRunning {
		return errors.New("domain is not running")
	}
	f.states[d.Name] = 5
	return nil
}

func (f *fakeConn) DomainUndefineFlags(d golibvirt.Domain, _ golibvirt.DomainUndefineFlagsValues) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.domains, d.Name)
	delete(f.states, d.Name)
	return nil
}

func (f *fakeConn) DomainLookupByName(name string) (golibvirt.Domain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.domains[name]
	if !ok {
		return golibvirt.Domain{}, fmt.Errorf("not found: %s", name)
	}
	return *d, nil
}

func (f *fakeConn) DomainGetState(d golibvirt.Domain, _ uint32) (int32, int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.states[d.Name]
	if !ok {
		return 0, 0, errors.New("unknown")
	}
	return s, 0, nil
}

func (f *fakeConn) DomainInterfaceAddresses(_ golibvirt.Domain, _ uint32, _ uint32) ([]golibvirt.DomainInterface, error) {
	return nil, nil
}

func (f *fakeConn) DomainGetXMLDesc(d golibvirt.Domain, _ golibvirt.DomainXMLFlags) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.domains[d.Name]; !ok {
		return "", fmt.Errorf("not defined")
	}
	// A minimal XML with one hostdev so tests exercise the parse path.
	return `<domain><devices><hostdev mode='subsystem' type='pci'><source><address domain='0x0000' bus='0x16' slot='0x00' function='0x0'/></source></hostdev></devices></domain>`, nil
}

func (f *fakeConn) Disconnect() error { return nil }

// --- tests -----------------------------------------------------------------

// liveConn wraps fakeConn and adds a DomainDefineXML that doesn't panic — the
// manager calls DomainDefineXML before DomainLookupByName, and LookupByName
// is what matters for the fake state transitions in these tests.
type liveConn struct {
	*fakeConn
}

func newLiveConn() *liveConn { return &liveConn{fakeConn: newFakeConn()} }

func (l *liveConn) DomainDefineXML(x string) (golibvirt.Domain, error) {
	// Parse the name out of the XML by substring; production uses real
	// libvirtd so this shortcut only hurts tests.
	name := extractName(x)
	if name == "" {
		return golibvirt.Domain{}, errors.New("no <name> in xml")
	}
	return l.define(name), nil
}

// extractName does a minimal <name>...</name> search so the test helper does
// not pull in encoding/xml.
func extractName(x string) string {
	start := 0
	for i := 0; i+6 <= len(x); i++ {
		if x[i:i+6] == "<name>" {
			start = i + 6
			break
		}
	}
	if start == 0 {
		return ""
	}
	end := start
	for end < len(x) && x[end] != '<' {
		end++
	}
	return x[start:end]
}

func TestManager_CreateRunDestroy(t *testing.T) {
	t.Parallel()

	c := newLiveConn()
	m := NewFromConnector(c, nil)
	defer func() { _ = m.Close() }()

	info, err := m.CreateDomain(context.Background(), DomainSpec{
		Name:      "inst-1",
		MemoryMiB: 1024,
		VCPUs:     1,
		DiskPath:  "/tmp/disk.qcow2",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.Name != "inst-1" || info.InitialState != StateRunning {
		t.Fatalf("info: %+v", info)
	}

	st, err := m.DomainState(context.Background(), "inst-1")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st != StateRunning {
		t.Fatalf("state: %s", st)
	}

	if err := m.DestroyDomain(context.Background(), "inst-1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	// Second destroy should report ErrDomainNotFound.
	err = m.DestroyDomain(context.Background(), "inst-1")
	if !errors.Is(err, ErrDomainNotFound) {
		t.Fatalf("expected ErrDomainNotFound, got %v", err)
	}
}

func TestManager_DuplicateCreateFails(t *testing.T) {
	t.Parallel()

	c := newLiveConn()
	m := NewFromConnector(c, nil)
	defer func() { _ = m.Close() }()

	spec := DomainSpec{
		Name:      "dupe",
		MemoryMiB: 512,
		VCPUs:     1,
		DiskPath:  "/tmp/d.qcow2",
	}
	if _, err := m.CreateDomain(context.Background(), spec); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := m.CreateDomain(context.Background(), spec)
	if !errors.Is(err, ErrDomainExists) {
		t.Fatalf("expected ErrDomainExists, got %v", err)
	}
}

func TestManager_CreateRollsBackOnStartFailure(t *testing.T) {
	t.Parallel()

	c := newLiveConn()
	c.createFail = true
	m := NewFromConnector(c, nil)
	defer func() { _ = m.Close() }()

	_, err := m.CreateDomain(context.Background(), DomainSpec{
		Name:      "roll",
		MemoryMiB: 512,
		VCPUs:     1,
		DiskPath:  "/tmp/d.qcow2",
	})
	if err == nil {
		t.Fatal("expected create to fail")
	}

	// The failed define+create pair should be rolled back, so re-attempting
	// with createFail=false must succeed as a fresh define.
	c.createFail = false
	if _, err := m.CreateDomain(context.Background(), DomainSpec{
		Name:      "roll",
		MemoryMiB: 512,
		VCPUs:     1,
		DiskPath:  "/tmp/d.qcow2",
	}); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}

func TestManager_DestroyAlreadyOffSucceeds(t *testing.T) {
	t.Parallel()

	c := newLiveConn()
	c.notRunning = true
	m := NewFromConnector(c, nil)
	defer func() { _ = m.Close() }()

	// Pre-populate a shut-off domain directly.
	c.define("off-1")
	if err := m.DestroyDomain(context.Background(), "off-1"); err != nil {
		t.Fatalf("destroy off domain: %v", err)
	}
}
