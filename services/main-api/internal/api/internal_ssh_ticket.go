package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/db/dbstore"
	grpcsrv "hybridcloud/services/main-api/internal/grpc"
	"hybridcloud/services/main-api/internal/sshticket"
)

// SSHTicketDeps describes what POST /internal/ssh-ticket needs.
type SSHTicketDeps struct {
	Instances InstanceRepo
	Nodes     NodeGetter
	Registry  TunnelRegistry
	Signer    *sshticket.Signer
	// SSHKeys resolves the client's SHA-256 SSH key fingerprint (presented to
	// ssh-proxy at handshake) to the owning user. Required for owner
	// scoping; nil leaves the gate disabled, which production boot must
	// reject.
	SSHKeys SSHKeyAuth
}

// SSHKeyAuth is the subset of sshkeys.Repo used to authenticate ssh-proxy
// requests. Returns sshkeys.ErrNotFound when the fingerprint is unknown.
type SSHKeyAuth interface {
	LookupUserByFingerprint(ctx context.Context, fingerprint string) (uuid.UUID, error)
}

// TunnelRegistry exposes the tunnel endpoint the agent advertised at Register.
type TunnelRegistry interface {
	TunnelEndpoint(nodeID uuid.UUID) (string, bool)
}

type sshTicketRequest struct {
	SubdomainPrefix   string `json:"subdomain_prefix"`
	SSHKeyFingerprint string `json:"ssh_key_fingerprint"`
}

// prefix must be the leading hex segment of an instance UUID. We accept 8-32
// hex chars; below 8 the collision space gets uncomfortable and we'd rather
// fail loudly than route a stray client to the wrong VM.
const (
	minSubdomainPrefixLen = 8
	maxSubdomainPrefixLen = 32
)

func isHexLower(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// SSHTicketHandler builds the http.HandlerFunc. Wrapped separately so the
// router only needs the internal token middleware + this func.
func SSHTicketHandler(deps SSHTicketDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<10))
		if err != nil {
			writeError(w, http.StatusBadRequest, "read_body", err.Error())
			return
		}
		var req sshTicketRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		req.SubdomainPrefix = strings.ToLower(strings.TrimSpace(req.SubdomainPrefix))
		if req.SubdomainPrefix == "" {
			writeError(w, http.StatusBadRequest, "missing_prefix", "subdomain_prefix required")
			return
		}
		if n := len(req.SubdomainPrefix); n < minSubdomainPrefixLen || n > maxSubdomainPrefixLen || !isHexLower(req.SubdomainPrefix) {
			writeError(w, http.StatusBadRequest, "bad_prefix",
				"subdomain_prefix must be 8-32 lowercase hex chars")
			return
		}
		req.SSHKeyFingerprint = strings.TrimSpace(req.SSHKeyFingerprint)
		if req.SSHKeyFingerprint == "" {
			writeError(w, http.StatusBadRequest, "missing_fingerprint",
				"ssh_key_fingerprint required")
			return
		}
		if deps.SSHKeys == nil {
			writeError(w, http.StatusServiceUnavailable, "ssh_auth_disabled",
				"ssh-key authentication is not configured on this server")
			return
		}
		ownerID, err := deps.SSHKeys.LookupUserByFingerprint(r.Context(), req.SSHKeyFingerprint)
		if err != nil {
			// Same 404 the unknown-prefix branch returns, so a probe can't
			// distinguish "no such fingerprint" from "no such instance".
			writeError(w, http.StatusNotFound, "instance_lookup", "instance not found")
			return
		}

		inst, err := resolveInstanceByPrefix(r.Context(), deps.Instances, ownerID, req.SubdomainPrefix)
		if err != nil {
			code := http.StatusNotFound
			if errors.Is(err, errAmbiguousPrefix) {
				code = http.StatusConflict
			}
			writeError(w, code, "instance_lookup", err.Error())
			return
		}
		if inst.State != dbstore.InstanceStateRunning {
			writeError(w, http.StatusConflict, "not_running", string(inst.State))
			return
		}
		if inst.VmInternalIp == nil {
			writeError(w, http.StatusConflict, "no_vm_ip", "instance has no vm_internal_ip yet")
			return
		}

		node, err := deps.Nodes.Get(r.Context(), inst.NodeID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "node_lookup", err.Error())
			return
		}
		if node.Status != dbstore.NodeStatusOnline {
			writeError(w, http.StatusConflict, "node_offline", "target node is not online")
			return
		}

		tunnelEndpoint, ok := deps.Registry.TunnelEndpoint(node.ID)
		if !ok {
			writeError(w, http.StatusConflict, "no_tunnel_endpoint",
				"agent has not advertised a tunnel endpoint")
			return
		}

		signed, err := deps.Signer.Issue(sshticket.Ticket{
			InstanceID:     inst.ID,
			NodeID:         node.ID,
			VMInternalIP:   inst.VmInternalIp.String(),
			VMPort:         22,
			TunnelEndpoint: tunnelEndpoint,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "sign", err.Error())
			return
		}

		writeJSON(w, http.StatusOK, signed)
	}
}

var errAmbiguousPrefix = errors.New("multiple instances match the prefix")

func resolveInstanceByPrefix(
	ctx context.Context, repo InstanceRepo, ownerID uuid.UUID, prefix string,
) (dbstore.Instance, error) {
	// Owner scoping is mandatory — the SQL query filters by owner_id and
	// matches `id::text LIKE prefix || '%'`, with LIMIT 2 so we can detect
	// ambiguity without paying for the full set.
	matches, err := repo.FindByOwnerAndIDPrefix(ctx, ownerID, prefix)
	if err != nil {
		return dbstore.Instance{}, fmt.Errorf("lookup instance by prefix: %w", err)
	}
	switch len(matches) {
	case 0:
		return dbstore.Instance{}, errors.New("instance not found")
	case 1:
		return matches[0], nil
	default:
		return dbstore.Instance{}, errAmbiguousPrefix
	}
}

// RequireInternalToken is a bearer-token middleware for internal-only
// endpoints (today: ssh-proxy → main-api ticket calls). Uses a secret
// distinct from the admin token so a compromised admin token can't mint
// tunnels.
func RequireInternalToken(expected string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if expected == "" {
				next.ServeHTTP(w, r)
				return
			}
			h := r.Header.Get("Authorization")
			token := strings.TrimPrefix(h, "Bearer ")
			if token == h || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthenticated",
					"invalid or missing internal bearer token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Ensure SSHTicketDeps satisfies the compile-time reference to grpcsrv
// registry (the production wiring uses *grpcsrv.AgentRegistry).
var _ TunnelRegistry = (*grpcsrv.AgentRegistry)(nil)
