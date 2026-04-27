package muxserver

import "github.com/prometheus/client_golang/prometheus"

// PromMetrics is the Prometheus implementation of the Metrics interface.
// Construct via NewPromMetrics and pass the result to Serve via Deps.
type PromMetrics struct {
	authFailures     *prometheus.CounterVec
	sessionsAccepted prometheus.Counter
}

// NewPromMetrics registers the Phase 2.1 auth/session counters on reg
// (typically the same prometheus.Registerer ssh-proxy exposes on
// /metrics). The counters surface in Grafana as mux_auth_failures_total
// and mux_sessions_accepted_total — see plan §4.3.
func NewPromMetrics(reg prometheus.Registerer) *PromMetrics {
	m := &PromMetrics{
		authFailures: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "mux_auth_failures_total",
				Help: "ssh-proxy muxserver authentication / handshake failures by reason.",
			},
			[]string{"reason"},
		),
		sessionsAccepted: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "mux_sessions_accepted_total",
				Help: "ssh-proxy muxserver successfully authenticated sessions.",
			},
		),
	}
	if reg != nil {
		reg.MustRegister(m.authFailures, m.sessionsAccepted)
	}
	return m
}

// AuthFailure implements muxserver.Metrics.
func (m *PromMetrics) AuthFailure(reason string) {
	m.authFailures.WithLabelValues(reason).Inc()
}

// SessionAccepted implements muxserver.Metrics.
func (m *PromMetrics) SessionAccepted() { m.sessionsAccepted.Inc() }
