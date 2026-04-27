package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/sshkeys"
	"hybridcloud/services/main-api/internal/sshticket"
)

// fakeTunnelRegistry implements api.TunnelRegistry for tests.
type fakeTunnelRegistry struct {
	endpoints map[uuid.UUID]string
}

func (f *fakeTunnelRegistry) TunnelEndpoint(nodeID uuid.UUID) (string, bool) {
	ep, ok := f.endpoints[nodeID]
	return ep, ok
}

// fakeSSHKeyAuth implements api.SSHKeyAuth backed by a fingerprint→user_id
// map. Returns sshkeys.ErrNotFound on miss so the handler maps it to 404.
type fakeSSHKeyAuth struct {
	users map[string]uuid.UUID
}

func (f *fakeSSHKeyAuth) LookupUserByFingerprint(_ context.Context, fp string) (uuid.UUID, error) {
	uid, ok := f.users[fp]
	if !ok {
		return uuid.Nil, sshkeys.ErrNotFound
	}
	return uid, nil
}

const (
	testFingerprint  = "SHA256:0123456789abcdefABCDEF0123456789abcdefABCDEF0123"
	testFingerprint2 = "SHA256:zzzzzzzzz_other-key_zzzzzzzzz"
)

// seedRunningInstance inserts one ready-to-ticket instance owned by ownerID
// and returns the wired internal router under test.
func seedRunningInstance(t *testing.T) (http.Handler, dbstore.Instance, uuid.UUID, uuid.UUID) {
	t.Helper()

	ownerID := uuid.New()
	nodeID := uuid.New()
	instanceID := uuid.New()
	vmIP := netip.MustParseAddr("192.168.122.47")

	insts := newFakeInstanceRepo()
	insts.rows[instanceID] = &dbstore.Instance{
		ID:           instanceID,
		OwnerID:      uuid.NullUUID{UUID: ownerID, Valid: true},
		NodeID:       nodeID,
		State:        dbstore.InstanceStateRunning,
		VmInternalIp: &vmIP,
		CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}

	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{
		nodeID: {ID: nodeID, Status: dbstore.NodeStatusOnline},
	}}

	registry := &fakeTunnelRegistry{
		endpoints: map[uuid.UUID]string{nodeID: "127.0.0.1:8082"},
	}

	signer, err := sshticket.NewSigner(bytes.Repeat([]byte{1}, 32), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	auth := &fakeSSHKeyAuth{users: map[string]uuid.UUID{
		testFingerprint: ownerID,
	}}

	router := api.NewInternalRouter(api.SSHTicketDeps{
		Instances: insts,
		Nodes:     getter,
		Registry:  registry,
		Signer:    signer,
		SSHKeys:   auth,
	}, nil, "secret")

	return router, *insts.rows[instanceID], instanceID, ownerID
}

func ticketRequest(t *testing.T, prefix, fingerprint string) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"subdomain_prefix":    prefix,
		"ssh_key_fingerprint": fingerprint,
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/ssh-ticket", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	return req
}

func TestSSHTicket_Issue(t *testing.T) {
	t.Parallel()

	router, inst, instanceID, _ := seedRunningInstance(t)

	req := ticketRequest(t, instanceID.String()[:8], testFingerprint)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d; body=%s", rr.Code, rr.Body.String())
	}

	var signed sshticket.Signed
	if err := json.Unmarshal(rr.Body.Bytes(), &signed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	verifier := sshticket.NewVerifier(bytes.Repeat([]byte{1}, 32))
	ticket, err := verifier.Verify(signed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ticket.InstanceID != inst.ID {
		t.Fatalf("instance id: got %s want %s", ticket.InstanceID, inst.ID)
	}
	if ticket.VMInternalIP != "192.168.122.47" || ticket.VMPort != 22 {
		t.Fatalf("vm fields: %+v", ticket)
	}
	if ticket.TunnelEndpoint != "127.0.0.1:8082" {
		t.Fatalf("tunnel endpoint: %q", ticket.TunnelEndpoint)
	}
}

func TestSSHTicket_MissingToken(t *testing.T) {
	t.Parallel()

	router, _, instanceID, _ := seedRunningInstance(t)

	body, _ := json.Marshal(map[string]string{
		"subdomain_prefix":    instanceID.String()[:8],
		"ssh_key_fingerprint": testFingerprint,
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/ssh-ticket", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", rr.Code)
	}
}

func TestSSHTicket_UnknownPrefix(t *testing.T) {
	t.Parallel()

	router, _, _, _ := seedRunningInstance(t)

	req := ticketRequest(t, "deadbeef", testFingerprint)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: %d", rr.Code)
	}
}

func TestSSHTicket_UnknownFingerprintReturns404(t *testing.T) {
	t.Parallel()

	router, _, instanceID, _ := seedRunningInstance(t)

	// Valid prefix but fingerprint doesn't map to any user — must 404 to
	// avoid leaking instance existence.
	req := ticketRequest(t, instanceID.String()[:8], testFingerprint2)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSSHTicket_OwnershipIsolation(t *testing.T) {
	t.Parallel()

	_, _, instanceID, _ := seedRunningInstance(t)

	// Map the *other* fingerprint to a different user. They should not be
	// able to reach the seed user's instance even with the right prefix.
	otherUser := uuid.New()
	// We have to rebuild the router for the other-user scenario because
	// fakeSSHKeyAuth is configured at construction time. Reuse the helper
	// to keep the seeding consistent.

	// Build a parallel router where the same instance exists under a
	// different owner_id, simulating an attacker who somehow learned the
	// prefix but is registered under their own SSH key.
	insts := newFakeInstanceRepo()
	vmIP := netip.MustParseAddr("192.168.122.47")
	nodeID := uuid.New()
	insts.rows[instanceID] = &dbstore.Instance{
		ID:           instanceID,
		OwnerID:      uuid.NullUUID{UUID: uuid.New(), Valid: true},
		NodeID:       nodeID,
		State:        dbstore.InstanceStateRunning,
		VmInternalIp: &vmIP,
	}
	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{
		nodeID: {ID: nodeID, Status: dbstore.NodeStatusOnline},
	}}
	registry := &fakeTunnelRegistry{endpoints: map[uuid.UUID]string{nodeID: "127.0.0.1:8082"}}
	signer, _ := sshticket.NewSigner(bytes.Repeat([]byte{1}, 32), 15*time.Second)
	auth := &fakeSSHKeyAuth{users: map[string]uuid.UUID{
		testFingerprint2: otherUser, // attacker's fingerprint
	}}

	r2 := api.NewInternalRouter(api.SSHTicketDeps{
		Instances: insts, Nodes: getter, Registry: registry, Signer: signer, SSHKeys: auth,
	}, nil, "secret")

	req := ticketRequest(t, instanceID.String()[:8], testFingerprint2)
	rr := httptest.NewRecorder()
	r2.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for foreign instance; got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSSHTicket_BadPrefix(t *testing.T) {
	t.Parallel()

	router, _, _, _ := seedRunningInstance(t)

	for _, p := range []string{"short", "ABCDEFGH", "notvalid", "0123456-"} {
		req := ticketRequest(t, p, testFingerprint)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("prefix %q: expected 400 got %d", p, rr.Code)
		}
	}
}

func TestSSHTicket_InstanceNotRunning(t *testing.T) {
	t.Parallel()

	ownerID := uuid.New()
	nodeID := uuid.New()
	instanceID := uuid.New()
	insts := newFakeInstanceRepo()
	insts.rows[instanceID] = &dbstore.Instance{
		ID:      instanceID,
		OwnerID: uuid.NullUUID{UUID: ownerID, Valid: true},
		NodeID:  nodeID,
		State:   dbstore.InstanceStateStopped,
	}
	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{nodeID: {ID: nodeID, Status: dbstore.NodeStatusOnline}}}
	registry := &fakeTunnelRegistry{endpoints: map[uuid.UUID]string{nodeID: "127.0.0.1:8082"}}
	signer, _ := sshticket.NewSigner(bytes.Repeat([]byte{1}, 32), 15*time.Second)
	auth := &fakeSSHKeyAuth{users: map[string]uuid.UUID{testFingerprint: ownerID}}

	router := api.NewInternalRouter(api.SSHTicketDeps{
		Instances: insts, Nodes: getter, Registry: registry, Signer: signer, SSHKeys: auth,
	}, nil, "secret")

	req := ticketRequest(t, instanceID.String()[:8], testFingerprint)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
}

// Silence unused imports when a test file is pruned.
var (
	_ = context.Background
	_ = errors.New
)
