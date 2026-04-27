// compute-agent connects to main-api, registers the GPU node, and executes
// CreateInstance/DestroyInstance control messages via libvirt + cloud-init.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hybridcloud/services/compute-agent/internal/libvirt"
	"hybridcloud/services/compute-agent/internal/profile"
	"hybridcloud/services/compute-agent/internal/stream"
	"hybridcloud/services/compute-agent/internal/topology"
	"hybridcloud/services/compute-agent/internal/vm"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// shutdownGrace is how long we wait for in-flight VM ops (qemu-img,
// libvirt destroy, GPU reset) to finish after SIGTERM before forcing exit.
const shutdownGrace = 30 * time.Second

func main() {
	var (
		endpoint     = flag.String("endpoint", env("AGENT_API_ENDPOINT", "localhost:8081"), "main-api gRPC endpoint")
		nodeName     = flag.String("node-name", env("AGENT_NODE_NAME", ""), "stable node name")
		token        = flag.String("agent-token", env("AGENT_API_TOKEN", ""), "shared secret for main-api")
		agentVersion = flag.String("agent-version", "0.1.0", "agent build version")
		fakeTopology = flag.Bool("fake-topology", env("AGENT_FAKE_TOPOLOGY", "") == "1", "report empty topology")
		disableVMs   = flag.Bool("disable-vms", env("AGENT_DISABLE_VMS", "") == "1", "skip libvirt wiring (control-plane-only mode)")
		imageDir     = flag.String("image-dir", env("AGENT_IMAGE_DIR", "/var/lib/hybrid/images"), "per-VM qcow2 directory")
		seedDir      = flag.String("seed-dir", env("AGENT_SEED_DIR", "/var/lib/hybrid/seeds"), "cloud-init ISO directory")
		baseImage    = flag.String("base-image", env("AGENT_BASE_IMAGE", ""), "backing qcow2 for new VM disks")
		netName      = flag.String("network", env("AGENT_LIBVIRT_NETWORK", "default"), "libvirt network to attach VMs to")
		diskGB       = flag.Int("disk-gb", envInt("AGENT_DISK_GB", 50), "per-VM virtual disk size in GiB (cloud-init growpart fills it on boot)")
		profilePath  = flag.String("profile", env("AGENT_PROFILE", ""), "path to slot-layout YAML (see docs/specs/phase-1-mvp.md §GPU partitioning)")
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

	var resolvedProfile *agentv1.Profile
	if *profilePath != "" {
		file, raw, err := profile.Load(*profilePath)
		if err != nil {
			log.Error("profile load", "err", err, "path", *profilePath)
			os.Exit(2)
		}
		// Resolve with the host's actual GPU indices. When the topology
		// collector can't reach nvidia-smi/sysfs (tests, degraded hosts),
		// collector.Collect would still work, but profile validation needs
		// some ground truth — probe sysfs now.
		hostGPUIndices := detectGPUIndices(*fakeTopology)
		res, err := profile.Resolve(file, raw, hostGPUIndices)
		if err != nil {
			log.Error("profile resolve", "err", err, "path", *profilePath)
			os.Exit(2)
		}
		log.Info("profile loaded", "name", res.Name, "hash", res.Hash[:12], "slots", len(res.Slots))
		resolvedProfile = res.Proto()
	}

	var collector topology.Collector
	if *fakeTopology {
		collector = topology.Empty()
	} else {
		collector = topology.LinuxCollector{Profile: resolvedProfile}
	}

	var onControl func(ctx context.Context, m *agentv1.ControlMessage, send func(*agentv1.AgentMessage))
	var vmHandler *vm.Handler
	if *disableVMs {
		log.Warn("VM handling disabled — control messages will be ignored")
	} else {
		mgr, err := libvirt.NewLibvirtManager(log)
		if err != nil {
			log.Error("libvirt connect failed", "err", err)
			os.Exit(1)
		}
		defer func() { _ = mgr.Close() }()

		vmHandler = vm.New(mgr, vm.Config{
			ImageDir:    *imageDir,
			SeedDir:     *seedDir,
			BaseImage:   *baseImage,
			NetworkName: *netName,
			DiskSizeGB:  *diskGB,
		}, log)
		onControl = vmHandler.OnControl
	}

	client, err := stream.New(stream.Config{
		Endpoint:     *endpoint,
		NodeName:     *nodeName,
		Hostname:     hostname,
		AgentVersion: *agentVersion,
		AgentToken:   *token,
		Topology:     collector,
		OnControl:    onControl,
		Log:          log,
	})
	if err != nil {
		log.Error("stream.New", "err", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Info("compute-agent starting", "endpoint", *endpoint, "node_name", *nodeName, "vms_disabled", *disableVMs)

	runErr := client.Run(ctx)

	// SIGTERM grace: the stream is gone but the VM handler may still be
	// running qemu-img / libvirt destroy / GPU reset for an in-flight
	// CreateInstance or DestroyInstance. Wait so we don't half-create a
	// VM or leave a slot's GPU un-reset (Phase 1 A6 gate).
	if vmHandler != nil {
		if drained := vmHandler.Wait(shutdownGrace); !drained {
			log.Warn("vm handler did not drain within grace; forcing shutdown",
				"grace", shutdownGrace)
		}
	}

	if runErr != nil && runErr != context.Canceled {
		log.Error("agent exited", "err", runErr)
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

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fallback
	}
	return n
}

// detectGPUIndices returns the GPU indices present on the host so profile
// validation can reject layouts that reference GPUs that do not exist. Uses
// the same sysfs path the production collector uses; fake mode returns
// an empty slice so profile references fail deterministically (which is
// fine because no real VM will be scheduled either).
func detectGPUIndices(fakeTopology bool) []int32 {
	if fakeTopology {
		return nil
	}
	// Reuse the topology collector to avoid duplicating the sysfs walk.
	top, err := topology.LinuxCollector{}.Collect(context.Background())
	if err != nil || top == nil {
		return nil
	}
	out := make([]int32, 0, len(top.Gpus))
	for _, g := range top.Gpus {
		out = append(out, g.Index)
	}
	return out
}
