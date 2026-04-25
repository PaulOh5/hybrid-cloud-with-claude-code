// Package metrics holds the Prometheus collectors for main-api. Custom
// metrics tied to domain state (gpu_slot_used, instance_total{state}) are
// exposed alongside HTTP request duration so operators can answer "how busy
// are we" and "is anything slow".
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Collectors are the metrics main-api exports. Bundle them so callers can
// register everything against one Registry.
type Collectors struct {
	HTTPDuration  *prometheus.HistogramVec
	GPUSlotUsed   *prometheus.GaugeVec
	InstanceTotal *prometheus.GaugeVec
}

// NewCollectors registers the standard metric set on the given registry.
func NewCollectors(reg prometheus.Registerer) *Collectors {
	c := &Collectors{
		HTTPDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "api_request_duration_seconds",
				Help:    "HTTP request duration by route + status.",
				Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
			},
			[]string{"method", "route", "status"},
		),
		GPUSlotUsed: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "gpu_slot_used",
				Help: "1 when a GPU slot is in_use, 0 otherwise. Labels by node + slot index.",
			},
			[]string{"node_name", "slot_index"},
		),
		InstanceTotal: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "instance_total",
				Help: "Number of instances by state.",
			},
			[]string{"state"},
		),
	}
	reg.MustRegister(c.HTTPDuration, c.GPUSlotUsed, c.InstanceTotal)
	return c
}

// Handler returns the /metrics http.Handler bound to the given registry.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// HTTPMiddleware wraps the http.Handler chain to observe request duration.
// Status codes are captured via a tiny ResponseWriter shim.
func (c *Collectors) HTTPMiddleware(routePattern string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
			start := time.Now()
			next.ServeHTTP(rec, r)
			c.HTTPDuration.WithLabelValues(
				r.Method,
				routePattern,
				strconv.Itoa(rec.code),
			).Observe(time.Since(start).Seconds())
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code        int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.code = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}
