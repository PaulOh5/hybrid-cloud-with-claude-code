// ssh-proxy serves the public SSH entry point for hybrid-cloud VMs. Users
// configure ~/.ssh/config with `ProxyJump proxy.<zone>` and connect to
// `{uuid8}.<zone>:22`; the proxy accepts direct-tcpip channel requests, maps
// the subdomain to a compute-agent, and (Phase 6.2/6.3) tunnels bytes to the
// VM's sshd.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/ssh"

	"hybridcloud/services/ssh-proxy/internal/hostkey"
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
		handler = &tunnelhandler.Handler{Tickets: client, Log: log}
		log.Info("ticket client configured", "api", *apiBaseURL)
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

	log.Info("ssh-proxy listening", "addr", *listen, "zone", *zone)
	if err := srv.Serve(ctx, lis); err != nil && err != context.Canceled {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
	log.Info("ssh-proxy shut down")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
