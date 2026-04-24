package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/db/dbstore"
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

// seedRunningInstance inserts one ready-to-ticket instance into the fake
// repo + node getter and returns the handler under test.
func seedRunningInstance(t *testing.T) (http.Handler, dbstore.Instance, uuid.UUID) {
	t.Helper()

	nodeID := uuid.New()
	instanceID := uuid.New()
	vmIP := netip.MustParseAddr("192.168.122.47")

	insts := newFakeInstanceRepo()
	insts.rows[instanceID] = &dbstore.Instance{
		ID:           instanceID,
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

	router := api.NewInternalRouter(api.SSHTicketDeps{
		Instances: insts,
		Nodes:     getter,
		Registry:  registry,
		Signer:    signer,
	}, "secret")

	return router, *insts.rows[instanceID], instanceID
}

func TestSSHTicket_Issue(t *testing.T) {
	t.Parallel()

	router, inst, instanceID := seedRunningInstance(t)

	body, _ := json.Marshal(map[string]string{
		"subdomain_prefix": instanceID.String()[:8],
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/ssh-ticket", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
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

	router, _, instanceID := seedRunningInstance(t)

	body, _ := json.Marshal(map[string]string{
		"subdomain_prefix": instanceID.String()[:8],
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

	router, _, _ := seedRunningInstance(t)

	body, _ := json.Marshal(map[string]string{"subdomain_prefix": "deadbeef"})
	req := httptest.NewRequest(http.MethodPost, "/internal/ssh-ticket", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: %d", rr.Code)
	}
}

func TestSSHTicket_InstanceNotRunning(t *testing.T) {
	t.Parallel()

	// Mutate the fake to stopped state.
	nodeID := uuid.New()
	instanceID := uuid.New()
	insts := newFakeInstanceRepo()
	insts.rows[instanceID] = &dbstore.Instance{
		ID:     instanceID,
		NodeID: nodeID,
		State:  dbstore.InstanceStateStopped,
	}
	getter := nodeOnly{nodes: map[uuid.UUID]dbstore.Node{nodeID: {ID: nodeID, Status: dbstore.NodeStatusOnline}}}
	registry := &fakeTunnelRegistry{endpoints: map[uuid.UUID]string{nodeID: "127.0.0.1:8082"}}
	signer, _ := sshticket.NewSigner(bytes.Repeat([]byte{1}, 32), 15*time.Second)

	router := api.NewInternalRouter(api.SSHTicketDeps{
		Instances: insts, Nodes: getter, Registry: registry, Signer: signer,
	}, "secret")

	body, _ := json.Marshal(map[string]string{"subdomain_prefix": instanceID.String()[:8]})
	req := httptest.NewRequest(http.MethodPost, "/internal/ssh-ticket", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
}

// Silence unused imports when a test file is pruned.
var _ = context.Background
