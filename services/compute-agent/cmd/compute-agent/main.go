// compute-agent connects to main-api, registers the GPU node, and pumps
// heartbeats. In Phase 2 this is all it does; Phase 3 wires CreateInstance /
// DestroyInstance control messages to the libvirt manager.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"hybridcloud/services/compute-agent/internal/stream"
	"hybridcloud/services/compute-agent/internal/topology"
)

func main() {
	var (
		endpoint     = flag.String("endpoint", env("AGENT_API_ENDPOINT", "localhost:8081"), "main-api gRPC endpoint")
		nodeName     = flag.String("node-name", env("AGENT_NODE_NAME", ""), "stable node name")
		token        = flag.String("agent-token", env("AGENT_API_TOKEN", ""), "shared secret for main-api")
		agentVersion = flag.String("agent-version", "0.1.0", "agent build version reported on Register")
		fakeTopology = flag.Bool("fake-topology", env("AGENT_FAKE_TOPOLOGY", "") == "1", "skip nvidia-smi and report no gpus (useful on dev boxes)")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if *nodeName == "" {
		hn, _ := os.Hostname()
		*nodeName = hn
	}
	if *token == "" {
		log.Error("AGENT_API_TOKEN is required")
		os.Exit(2)
	}

	hostname, _ := os.Hostname()

	var collector topology.Collector
	if *fakeTopology {
		collector = topology.Empty()
	} else {
		collector = topology.LinuxCollector{}
	}

	client, err := stream.New(stream.Config{
		Endpoint:     *endpoint,
		NodeName:     *nodeName,
		Hostname:     hostname,
		AgentVersion: *agentVersion,
		AgentToken:   *token,
		Topology:     collector,
		Log:          log,
	})
	if err != nil {
		log.Error("stream.New", "err", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Info("compute-agent starting", "endpoint", *endpoint, "node_name", *nodeName)

	if err := client.Run(ctx); err != nil && err != context.Canceled {
		log.Error("agent exited", "err", err)
		os.Exit(1)
	}
	log.Info("compute-agent shut down")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
