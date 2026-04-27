package muxserver_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"hybridcloud/services/ssh-proxy/internal/muxserver"
)

func TestPromMetrics_AuthFailureCountsByReason(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m := muxserver.NewPromMetrics(reg)

	m.AuthFailure("unauthenticated")
	m.AuthFailure("unauthenticated")
	m.AuthFailure("tls_downgrade")

	body := `
# HELP mux_auth_failures_total ssh-proxy muxserver authentication / handshake failures by reason.
# TYPE mux_auth_failures_total counter
mux_auth_failures_total{reason="tls_downgrade"} 1
mux_auth_failures_total{reason="unauthenticated"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(body), "mux_auth_failures_total"); err != nil {
		t.Fatalf("gather/compare: %v", err)
	}
}

func TestPromMetrics_SessionsAcceptedIncrements(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m := muxserver.NewPromMetrics(reg)

	m.SessionAccepted()
	m.SessionAccepted()
	m.SessionAccepted()

	body := `
# HELP mux_sessions_accepted_total ssh-proxy muxserver successfully authenticated sessions.
# TYPE mux_sessions_accepted_total counter
mux_sessions_accepted_total 3
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(body), "mux_sessions_accepted_total"); err != nil {
		t.Fatalf("gather/compare: %v", err)
	}
}
