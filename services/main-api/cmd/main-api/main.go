// main-api runs the central control plane: gRPC server for compute-agent
// streams, REST for operators and (Phase 7+) users, and a background sweeper
// that flips stale nodes to offline.
package main

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"google.golang.org/grpc"

	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/config"
	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/db/migrations"
	grpcsrv "hybridcloud/services/main-api/internal/grpc"
	"hybridcloud/services/main-api/internal/instance"
	"hybridcloud/services/main-api/internal/node"
	"hybridcloud/services/main-api/internal/slot"
	"hybridcloud/services/main-api/internal/sshticket"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.FromEnv()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := migrate(ctx, cfg.DatabaseURL); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("pgxpool", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	queries := dbstore.New(pool)
	nodes := node.NewDBRepo(queries)
	instances := instance.NewRepo(pool, queries)
	slots := slot.NewRepo(pool, queries)

	zoneID, err := nodes.DefaultZoneID(ctx)
	if err != nil {
		log.Error("default zone", "err", err)
		os.Exit(1)
	}

	registry := grpcsrv.NewAgentRegistry()
	agentSvc := &grpcsrv.AgentStreamService{
		Nodes:         nodes,
		Instances:     instances,
		Slots:         slots,
		Profiles:      slots,
		Registry:      registry,
		ExpectedToken: cfg.AgentToken,
		DefaultZoneID: zoneID,
		Log:           log,
	}

	var wg sync.WaitGroup
	wg.Add(3)

	// gRPC server.
	grpcLis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		log.Error("grpc listen", "addr", cfg.GRPCAddr, "err", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcServer, agentSvc)
	go func() {
		defer wg.Done()
		log.Info("grpc listening", "addr", cfg.GRPCAddr)
		if err := grpcServer.Serve(grpcLis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			log.Error("grpc serve", "err", err)
		}
	}()

	// HTTP server.
	adminRouter := api.NewAdminRouter(
		&api.AdminHandlers{Nodes: nodes},
		&api.InstanceHandlers{
			Instances:  instances,
			Nodes:      nodes,
			Slots:      slots,
			Dispatcher: registry,
		},
		cfg.AdminToken,
	)
	var internalRouter http.Handler
	if cfg.InternalToken != "" {
		signer, err := sshticket.NewSigner(cfg.TunnelSecret, cfg.TicketTTL)
		if err != nil {
			log.Error("ticket signer", "err", err)
			os.Exit(2)
		}
		internalRouter = api.NewInternalRouter(api.SSHTicketDeps{
			Instances: instances,
			Nodes:     nodes,
			Registry:  registry,
			Signer:    signer,
		}, cfg.InternalToken)
		log.Info("ssh-ticket endpoint enabled", "ttl", cfg.TicketTTL)
	}
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewRouter(adminRouter, internalRouter),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		defer wg.Done()
		log.Info("http listening", "addr", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http serve", "err", err)
		}
	}()

	// Stale sweeper.
	go func() {
		defer wg.Done()
		err := agentSvc.StaleSweeper(ctx, cfg.SweepInterval, cfg.HeartbeatTTL)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("sweeper", "err", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	grpcServer.GracefulStop()
	wg.Wait()
	log.Info("exit")
}

func migrate(ctx context.Context, url string) error {
	dbh, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer func() { _ = dbh.Close() }()

	sub, err := fs.Sub(migrations.FS(), ".")
	if err != nil {
		return err
	}
	goose.SetBaseFS(sub)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.UpContext(ctx, dbh, ".")
}
