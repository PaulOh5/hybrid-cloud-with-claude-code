package muxconfig_test

import (
	"testing"
	"time"

	"hybridcloud/shared/muxconfig"
)

// Phase 2.1 P10 — both muxserver (ssh-proxy) and muxclient (compute-agent)
// import this single config to ensure their yamux sessions agree on
// keepalive cadence and stream timeouts.

func TestConfig_P10Values(t *testing.T) {
	t.Parallel()

	c := muxconfig.Config()

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"KeepAliveInterval", c.KeepAliveInterval, 15 * time.Second},
		{"StreamOpenTimeout", c.StreamOpenTimeout, 30 * time.Second},
		{"EnableKeepAlive", c.EnableKeepAlive, true},
		{"MaxStreamWindowSize", c.MaxStreamWindowSize, uint32(256 * 1024)},
		{"ConnectionWriteTimeout", c.ConnectionWriteTimeout, 10 * time.Second},
		{"StreamCloseTimeout", c.StreamCloseTimeout, 5 * time.Minute},
		{"AcceptBacklog", c.AcceptBacklog, 256},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s: got %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestConfig_ReturnsFreshInstance(t *testing.T) {
	t.Parallel()

	a := muxconfig.Config()
	a.AcceptBacklog = 9999
	b := muxconfig.Config()
	if b.AcceptBacklog == 9999 {
		t.Fatal("Config() must return a fresh value, not a shared pointer")
	}
}
