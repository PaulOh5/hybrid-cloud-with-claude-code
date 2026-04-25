package libvirt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

// Connector is the narrow slice of digitalocean/go-libvirt the manager uses.
// Abstracting it lets integration tests swap in a fake without pulling the
// real libvirt dependency into every unit test.
type Connector interface {
	DomainDefineXML(x string) (golibvirt.Domain, error)
	DomainCreate(d golibvirt.Domain) error
	DomainDestroy(d golibvirt.Domain) error
	DomainUndefineFlags(d golibvirt.Domain, flags golibvirt.DomainUndefineFlagsValues) error
	DomainLookupByName(name string) (golibvirt.Domain, error)
	DomainGetState(d golibvirt.Domain, flags uint32) (int32, int32, error)
	DomainGetXMLDesc(d golibvirt.Domain, flags golibvirt.DomainXMLFlags) (string, error)
	DomainInterfaceAddresses(d golibvirt.Domain, source uint32, flags uint32) ([]golibvirt.DomainInterface, error)
	Disconnect() error
}

// LibvirtManager talks to libvirtd through the standard UNIX socket
// /var/run/libvirt/libvirt-sock. It satisfies Manager.
type LibvirtManager struct {
	conn Connector
	log  *slog.Logger
}

// NewLibvirtManager dials libvirtd via the default qemu:///system endpoint
// (UNIX socket). The caller owns Close.
func NewLibvirtManager(log *slog.Logger) (*LibvirtManager, error) {
	l := golibvirt.NewWithDialer(dialers.NewLocal(dialers.WithLocalTimeout(5 * time.Second)))
	if err := l.Connect(); err != nil {
		return nil, fmt.Errorf("libvirt handshake: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &LibvirtManager{conn: libvirtConnAdapter{l: l}, log: log}, nil
}

// NewFromConnector builds a manager around any Connector. Tests pass a fake
// here; production uses the libvirtd socket via NewLibvirtManager.
func NewFromConnector(c Connector, log *slog.Logger) *LibvirtManager {
	if log == nil {
		log = slog.Default()
	}
	return &LibvirtManager{conn: c, log: log}
}

// CreateDomain defines the domain XML, starts it, and returns info. If a
// domain with the same name already exists we return ErrDomainExists so the
// caller can decide whether to treat it as success (idempotent re-create).
func (m *LibvirtManager) CreateDomain(_ context.Context, spec DomainSpec) (DomainInfo, error) {
	xmlBytes, err := BuildDomainXML(spec)
	if err != nil {
		return DomainInfo{}, err
	}

	if existing, lookupErr := m.conn.DomainLookupByName(spec.Name); lookupErr == nil && existing.Name != "" {
		return DomainInfo{}, fmt.Errorf("%w: %s", ErrDomainExists, spec.Name)
	}

	dom, err := m.conn.DomainDefineXML(string(xmlBytes))
	if err != nil {
		return DomainInfo{}, fmt.Errorf("define domain: %w", err)
	}
	if err := m.conn.DomainCreate(dom); err != nil {
		// Roll back the definition so a retry does not hit ErrDomainExists.
		_ = m.conn.DomainUndefineFlags(dom, 0)
		return DomainInfo{}, fmt.Errorf("start domain: %w", err)
	}

	st, err := m.DomainState(nil, spec.Name) //nolint:staticcheck // ctx optional
	if err != nil {
		st = StateUnknown
	}

	m.log.Info("domain created", "name", spec.Name, "state", st.String())
	return DomainInfo{
		Name:         spec.Name,
		UUID:         uuidString(dom.UUID),
		InitialState: st,
	}, nil
}

// DestroyDomain powers off the VM and removes its libvirt definition.
// Missing domains return ErrDomainNotFound so the caller can treat double-
// destroy as success.
func (m *LibvirtManager) DestroyDomain(_ context.Context, name string) error {
	dom, err := m.conn.DomainLookupByName(name)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrDomainNotFound, name)
	}
	// DomainDestroy is idempotent against an already-off domain in libvirt,
	// but we ignore the specific error to keep the caller's code simple.
	if err := m.conn.DomainDestroy(dom); err != nil && !isLibvirtDomainNotRunning(err) {
		return fmt.Errorf("destroy: %w", err)
	}
	if err := m.conn.DomainUndefineFlags(dom, 0); err != nil {
		return fmt.Errorf("undefine: %w", err)
	}
	m.log.Info("domain destroyed", "name", name)
	return nil
}

// DomainState translates the libvirt state enum into our narrow set.
func (m *LibvirtManager) DomainState(_ context.Context, name string) (DomainState, error) {
	dom, err := m.conn.DomainLookupByName(name)
	if err != nil {
		return StateUnknown, fmt.Errorf("%w: %s", ErrDomainNotFound, name)
	}
	raw, _, err := m.conn.DomainGetState(dom, 0)
	if err != nil {
		return StateUnknown, fmt.Errorf("get state: %w", err)
	}
	return mapLibvirtState(raw), nil
}

// DomainPassthroughPCI parses the domain's live XML and returns the PCI
// addresses of every <hostdev> attached. Called before DestroyDomain so the
// agent can reset each device after libvirt releases it.
func (m *LibvirtManager) DomainPassthroughPCI(_ context.Context, name string) ([]string, error) {
	dom, err := m.conn.DomainLookupByName(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrDomainNotFound, name)
	}
	raw, err := m.conn.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return nil, fmt.Errorf("get xml: %w", err)
	}
	return ParsePassthroughPCIFromXML(raw)
}

// DomainIPv4 reads the dnsmasq-lease IP for the domain's first virtio NIC.
// Returns "" with nil error when no lease is yet known so the caller can
// retry; only persistent failures bubble up as errors.
func (m *LibvirtManager) DomainIPv4(_ context.Context, name string) (string, error) {
	dom, err := m.conn.DomainLookupByName(name)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrDomainNotFound, name)
	}
	// 0 = VIR_DOMAIN_INTERFACE_ADDRESSES_SRC_LEASE
	ifaces, err := m.conn.DomainInterfaceAddresses(dom, 0, 0)
	if err != nil {
		return "", fmt.Errorf("interface addresses: %w", err)
	}
	for _, ifc := range ifaces {
		for _, a := range ifc.Addrs {
			if a.Type == 0 && a.Addr != "" {
				return a.Addr, nil
			}
		}
	}
	return "", nil
}

// StreamEvents returns a channel; the lifecycle-event wiring is finished in
// Task 3.3 once the state machine is merged. For Phase 2 the channel simply
// closes when ctx is done.
func (m *LibvirtManager) StreamEvents(ctx context.Context) (<-chan DomainEvent, error) {
	ch := make(chan DomainEvent)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// Close releases the connection to libvirtd.
func (m *LibvirtManager) Close() error { return m.conn.Disconnect() }

// --- libvirt-go adapter ----------------------------------------------------

type libvirtConnAdapter struct {
	l *golibvirt.Libvirt
}

func (a libvirtConnAdapter) DomainDefineXML(x string) (golibvirt.Domain, error) {
	return a.l.DomainDefineXML(x)
}

func (a libvirtConnAdapter) DomainCreate(d golibvirt.Domain) error {
	return a.l.DomainCreate(d)
}

func (a libvirtConnAdapter) DomainDestroy(d golibvirt.Domain) error {
	return a.l.DomainDestroy(d)
}

func (a libvirtConnAdapter) DomainUndefineFlags(d golibvirt.Domain, flags golibvirt.DomainUndefineFlagsValues) error {
	return a.l.DomainUndefineFlags(d, flags)
}

func (a libvirtConnAdapter) DomainLookupByName(name string) (golibvirt.Domain, error) {
	return a.l.DomainLookupByName(name)
}

func (a libvirtConnAdapter) DomainGetState(d golibvirt.Domain, flags uint32) (int32, int32, error) {
	return a.l.DomainGetState(d, flags)
}

func (a libvirtConnAdapter) DomainGetXMLDesc(d golibvirt.Domain, flags golibvirt.DomainXMLFlags) (string, error) {
	return a.l.DomainGetXMLDesc(d, flags)
}

func (a libvirtConnAdapter) DomainInterfaceAddresses(d golibvirt.Domain, source, flags uint32) ([]golibvirt.DomainInterface, error) {
	return a.l.DomainInterfaceAddresses(d, source, flags)
}

func (a libvirtConnAdapter) Disconnect() error { return a.l.Disconnect() }

// --- helpers ---------------------------------------------------------------

// mapLibvirtState converts libvirt DomainState integers (NoState=0, Running=1,
// Blocked=2, Paused=3, Shutdown=4, Shutoff=5, Crashed=6, PMSuspended=7) to
// our narrower enum.
func mapLibvirtState(raw int32) DomainState {
	switch raw {
	case 1, 2: // running, blocked (still "running" from a caller's pov)
		return StateRunning
	case 4, 5: // shutdown, shutoff
		return StateStopped
	case 6: // crashed
		return StateFailed
	default:
		return StateUnknown
	}
}

func isLibvirtDomainNotRunning(err error) bool {
	// libvirtd returns this specific text when DomainDestroy is called on a
	// VM that is already off. We cannot rely on a typed error because
	// digitalocean/go-libvirt returns a formatted string.
	return err != nil && errContains(err, "domain is not running")
}

func errContains(err error, sub string) bool {
	if err == nil {
		return false
	}
	return len(err.Error()) >= len(sub) && hasSub(err.Error(), sub)
}

func hasSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func uuidString(uuid [golibvirt.UUIDBuflen]byte) string {
	const hex = "0123456789abcdef"
	var buf [36]byte
	j := 0
	for i, b := range uuid {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf[j] = '-'
			j++
		}
		buf[j] = hex[b>>4]
		buf[j+1] = hex[b&0xF]
		j += 2
	}
	return string(buf[:])
}

// Ensure interface compliance at compile time.
var _ Manager = (*LibvirtManager)(nil)
var _ error = ErrDomainNotFound
var _ error = ErrDomainExists
var _ = errors.Is
