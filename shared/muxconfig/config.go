// Package muxconfig holds the yamux Config shared by ssh-proxy's muxserver
// and compute-agent's muxclient. Phase 2.1 plan P10 fixes these values for
// both ends so they agree on keepalive cadence, stream timeouts, and
// flow-control window. Centralising here also keeps the documentation
// referenced by ADR-008 in one place.
//
// Phase 2.4 N5/N2 load testing may revisit these — adjust the constants
// then, not the call sites.
package muxconfig

import (
	"io"
	"time"

	"github.com/hashicorp/yamux"
)

// Config returns a fresh yamux.Config carrying the Phase 2.1 P10 values.
// Callers are free to mutate the returned struct (e.g. inject a logger).
func Config() *yamux.Config {
	return &yamux.Config{
		// 한국 가정망 NAT idle drop이 30~60s인 경우가 있어 yamux 디폴트
		// 30s보다 짧게. Phase 2.5 A1 데이터 수령 후 조정 가능 (Q6).
		KeepAliveInterval: 15 * time.Second,
		// 디폴트 75s는 사용자가 SSH 명령 한 번에 너무 오래 기다리는 느낌.
		StreamOpenTimeout:      30 * time.Second,
		EnableKeepAlive:        true,
		MaxStreamWindowSize:    256 * 1024,
		ConnectionWriteTimeout: 10 * time.Second,
		StreamCloseTimeout:     5 * time.Minute,
		AcceptBacklog:          256,

		// yamux requires Logger or LogOutput to be set; default to
		// /dev/null so callers don't have to — production wiring routes
		// real signals through Prometheus + slog separately. Callers
		// that want yamux's internal trace can swap LogOutput after
		// calling Config().
		LogOutput: io.Discard,
	}
}
