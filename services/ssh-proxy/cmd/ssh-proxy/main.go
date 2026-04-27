// ssh-proxy serves the public SSH entry point for hybrid-cloud VMs. Users
// configure ~/.ssh/config with `ProxyJump proxy.<zone>` and connect to
// `{uuid8}.<zone>:22`; the proxy accepts direct-tcpip channel requests, maps
// the subdomain to a compute-agent, and (Phase 6.2/6.3) tunnels bytes to the
// VM's sshd.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/ssh"

	"hybridcloud/services/ssh-proxy/internal/hostkey"
	"hybridcloud/services/ssh-proxy/internal/muxregistry"
	"hybridcloud/services/ssh-proxy/internal/muxserver"
	"hybridcloud/services/ssh-proxy/internal/server"
	"hybridcloud/services/ssh-proxy/internal/ticketclient"
	"hybridcloud/services/ssh-proxy/internal/tunnelhandler"
)

func main() {
	var (
		listen      = flag.String("listen", env("SSH_PROXY_LISTEN", ":22"), "TCP address to listen on")
		zone        = flag.String("zone", env("SSH_PROXY_ZONE", "hybrid-cloud.com"), "DNS suffix accepted as tunnel target")
		hostKeyPath = flag.String("host-key", env("SSH_PROXY_HOST_KEY", "/var/lib/hybrid/ssh-proxy-hostkey"), "PEM file holding the proxy's ed25519 host key")
		apiBaseURL  = flag.String("api", env("SSH_PROXY_API_ENDPOINT", "http://127.0.0.1:8080"), "main-api base URL for ticket lookups")
		internalTok = flag.String("internal-token", env("SSH_PROXY_INTERNAL_TOKEN", ""), "bearer token matching main-api's MAIN_API_INTERNAL_TOKEN")

		// Phase 2.1 mux endpoint — empty string disables the mux listener
		// entirely so dev bring-up doesn't need cert files.
		muxListen     = flag.String("mux-listen", env("SSH_PROXY_MUX_LISTEN", ""), "TCP address for the Phase 2 mux endpoint (e.g. :2233). Empty disables.")
		muxCertPath   = flag.String("mux-cert", env("SSH_PROXY_MUX_CERT", ""), "PEM file with the mux endpoint's TLS certificate chain")
		muxKeyPath    = flag.String("mux-key", env("SSH_PROXY_MUX_KEY", ""), "PEM file with the mux endpoint's TLS private key")
		metricsListen = flag.String("metrics-listen", env("SSH_PROXY_METRICS_LISTEN", ""), "TCP address for /metrics (e.g. :9092). Empty disables.")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	signer, err := hostkey.LoadOrGenerate(*hostKeyPath)
	if err != nil {
		log.Error("host key", "err", err)
		os.Exit(2)
	}
	log.Info("host key loaded",
		"path", *hostKeyPath,
		"fingerprint", ssh.FingerprintSHA256(signer.PublicKey()),
	)

	var handler server.Handler = server.DenyHandler{Reason: "internal token not configured"}
	if *internalTok != "" {
		client := ticketclient.New(*apiBaseURL, *internalTok)
		handler = &tunnelhandler.Handler{
			Tickets:     client,
			AfterTicket: tunnelhandler.Relay,
			Log:         log,
		}
		log.Info("ticket client + relay configured", "api", *apiBaseURL)
	}

	srv, err := server.New(server.Config{
		Zone:     *zone,
		HostKeys: []ssh.Signer{signer},
		Handler:  handler,
		Log:      log,
	})
	if err != nil {
		log.Error("server.New", "err", err)
		os.Exit(2)
	}

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Error("listen", "addr", *listen, "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Phase 2.1 — mux endpoint + Prometheus exposure are opt-in. When
	// SSH_PROXY_MUX_LISTEN is set we also stand up the metrics listener
	// (no metrics to scrape without it).
	promReg := prometheus.NewRegistry()
	var wg sync.WaitGroup
	registry := startMuxIfConfigured(ctx, &wg, log, promReg, muxConfig{
		listen:      *muxListen,
		certPath:    *muxCertPath,
		keyPath:     *muxKeyPath,
		apiBaseURL:  *apiBaseURL,
		internalTok: *internalTok,
	})
	startMetricsIfConfigured(ctx, &wg, log, promReg, *metricsListen)

	log.Info("ssh-proxy listening", "addr", *listen, "zone", *zone)
	if err := srv.Serve(ctx, lis); err != nil && err != context.Canceled {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
	if registry != nil {
		registry.Close()
	}
	cancel() // signal mux + metrics listeners to shut down
	wg.Wait()
	log.Info("ssh-proxy shut down")
}

type muxConfig struct {
	listen      string
	certPath    string
	keyPath     string
	apiBaseURL  string
	internalTok string
}

func startMuxIfConfigured(
	ctx context.Context,
	wg *sync.WaitGroup,
	log *slog.Logger,
	promReg prometheus.Registerer,
	cfg muxConfig,
) *muxregistry.Registry {
	if cfg.listen == "" {
		log.Info("mux endpoint disabled (SSH_PROXY_MUX_LISTEN unset)")
		return nil
	}
	if cfg.certPath == "" || cfg.keyPath == "" {
		log.Error("mux endpoint requires SSH_PROXY_MUX_CERT and SSH_PROXY_MUX_KEY")
		os.Exit(2)
	}
	if cfg.internalTok == "" {
		log.Error("mux endpoint requires SSH_PROXY_INTERNAL_TOKEN to authenticate against main-api")
		os.Exit(2)
	}
	cert, err := tls.LoadX509KeyPair(cfg.certPath, cfg.keyPath)
	if err != nil {
		log.Error("mux cert load", "cert", cfg.certPath, "err", err)
		os.Exit(2)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
	muxLis, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		log.Error("mux listen", "addr", cfg.listen, "err", err)
		os.Exit(1)
	}

	registry := muxregistry.New(muxregistry.Config{
		Reporter: muxregistry.LogReporter{Log: log},
		Log:      log,
	})
	verifier := muxserver.NewHTTPVerifier(muxserver.HTTPVerifierConfig{
		BaseURL:       cfg.apiBaseURL,
		InternalToken: cfg.internalTok,
		// Cache disabled — main-api caches; doubling cuts revocation
		// budget. See plan §S2 + muxserver/auth.go doc.
		CacheTTL: 0,
	})
	metrics := muxserver.NewPromMetrics(promReg)
	deps := muxserver.Deps{
		TLSConfig: tlsCfg,
		Verifier:  verifier,
		Registry:  registry,
		Metrics:   metrics,
		Log:       log,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info("mux endpoint listening", "addr", cfg.listen)
		if err := muxserver.Serve(ctx, muxLis, deps); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("muxserver.Serve", "err", err)
		}
	}()

	return registry
}

func startMetricsIfConfigured(
	ctx context.Context,
	wg *sync.WaitGroup,
	log *slog.Logger,
	promReg *prometheus.Registry,
	listen string,
) {
	if listen == "" {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(promReg, promhttp.HandlerOpts{Registry: promReg}))
	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info("metrics endpoint listening", "addr", listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics ListenAndServe", "err", err)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
