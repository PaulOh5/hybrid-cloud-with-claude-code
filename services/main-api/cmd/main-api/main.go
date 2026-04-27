// main-api runs the central control plane: gRPC server for compute-agent
// streams, REST for operators and (Phase 7+) users, and a background sweeper
// that flips stale nodes to offline.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
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

	"hybridcloud/services/main-api/internal/agentauth"
	"hybridcloud/services/main-api/internal/api"
	"hybridcloud/services/main-api/internal/auth"
	"hybridcloud/services/main-api/internal/billing"
	"hybridcloud/services/main-api/internal/config"
	"hybridcloud/services/main-api/internal/credit"
	"hybridcloud/services/main-api/internal/db/dbstore"
	"hybridcloud/services/main-api/internal/db/migrations"
	grpcsrv "hybridcloud/services/main-api/internal/grpc"
	"hybridcloud/services/main-api/internal/instance"
	"hybridcloud/services/main-api/internal/metrics"
	"hybridcloud/services/main-api/internal/node"
	"hybridcloud/services/main-api/internal/slot"
	"hybridcloud/services/main-api/internal/sshkeys"
	"hybridcloud/services/main-api/internal/sshticket"
	agentv1 "hybridcloud/shared/proto/agent/v1"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

func main() {
	// --migrate-only lets the deploy pipeline apply schema changes from the
	// new binary BEFORE swapping the running service. Exits 0 on success,
	// non-zero on migration failure so CD can abort cleanly.
	migrateOnly := flag.Bool("migrate-only", false, "run goose migrations and exit")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.FromEnv()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if runMigrations(ctx, log, cfg.DatabaseURL, *migrateOnly) {
		return
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
	slots := slot.NewRepo(pool, queries).WithLogger(log)

	recoverOrphanReservations(ctx, log, slots)

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
	sshKeysRepo := sshkeys.NewRepo(queries)
	creditRepo := credit.NewRepo(pool, queries)
	adminInstances := &api.InstanceHandlers{
		Instances:  instances,
		Nodes:      nodes,
		Slots:      slots,
		Dispatcher: registry,
		ExtraSSHKeysForOwner: func(ctx context.Context, ownerID uuid.UUID) []string {
			keys, err := sshKeysRepo.PubkeysForUser(ctx, ownerID)
			if err != nil {
				log.Warn("ssh_keys lookup", "user_id", ownerID, "err", err)
				return nil
			}
			return keys
		},
		BalanceForOwner: func(ctx context.Context, ownerID uuid.UUID) (int64, error) {
			return creditRepo.Balance(ctx, ownerID)
		},
	}
	adminCredits := &api.AdminCreditHandlers{Credits: creditRepo}
	adminRouter := api.NewAdminRouter(
		&api.AdminHandlers{Nodes: nodes},
		adminInstances,
		adminCredits,
		cfg.AdminToken,
	)
	var internalRouter http.Handler
	if cfg.InternalToken != "" {
		signer, err := sshticket.NewSigner(cfg.TunnelSecret, cfg.TicketTTL)
		if err != nil {
			log.Error("ticket signer", "err", err)
			os.Exit(2)
		}
		// Phase 2 ADR-009 — ssh-proxy validates agent (node_id, token) at
		// mux handshake time by posting to /internal/agent-auth. Backed by
		// node_tokens (Phase 2 Task 0.3) with a 60s in-memory cache so
		// revocation surfaces within the cache TTL window (S2).
		agentAuth := agentauth.NewHandler(agentauth.Config{
			Repo:     agentauth.NewPgRepo(queries),
			CacheTTL: 60 * time.Second,
		})

		internalRouter = api.NewInternalRouter(api.SSHTicketDeps{
			Instances: instances,
			Nodes:     nodes,
			Registry:  registry,
			Signer:    signer,
			SSHKeys:   sshKeysRepo,
		}, agentAuth, cfg.InternalToken)
		log.Info("ssh-ticket endpoint enabled", "ttl", cfg.TicketTTL)
		log.Info("agent-auth endpoint enabled", "cache_ttl", "60s")
	}

	// User-facing /api/v1/* router (Phase 7).
	authRepo := auth.NewRepo(queries)
	loginLimiter := auth.NewRateLimiter(auth.LoginRateLimit, auth.LoginRateWindow)
	authHandlers := &api.AuthHandlers{
		Users:   authRepo,
		Limiter: loginLimiter,
		Config: api.AuthConfig{
			SessionTTL:       cfg.SessionTTL,
			CookieSecure:     cfg.CookieSecure,
			CookieDomain:     cfg.CookieDomain,
			TrustedProxyHops: cfg.TrustedProxyHops,
		},
	}
	userRouter := api.NewUserRouter(api.UserHandlers{
		Auth:      authHandlers,
		Instances: api.NewUserInstanceHandlers(adminInstances),
		Nodes:     &api.UserNodeHandlers{Nodes: nodes},
		SSHKeys:   &api.UserSSHKeyHandlers{Keys: sshKeysRepo},
		Credits:   &api.UserCreditHandlers{Credits: creditRepo},
		// Phase 10.1 — admin routes also live under /api/v1/* so the
		// dashboard can reach them with the session cookie.
		Admin:          &api.SessionAdminHandlers{Queries: queries},
		AdminInstances: adminInstances,
		AdminCredits:   adminCredits,
		AdminNodes:     &api.AdminHandlers{Nodes: nodes},
	}, authRepo)

	// Metrics (Phase 10.2) — register a private registry, attach /metrics to
	// the same HTTP listener, and start a domain-state refresher.
	metricsReg := prometheus.NewRegistry()
	collectors := metrics.NewCollectors(metricsReg)

	mainHandler := api.NewRouter(adminRouter, internalRouter, userRouter)
	muxWithMetrics := http.NewServeMux()
	muxWithMetrics.Handle("/metrics", metrics.Handler(metricsReg))
	muxWithMetrics.Handle("/", collectors.HTTPMiddleware("api")(mainHandler))

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           muxWithMetrics,
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

	startBillingWorker(ctx, log, &wg, cfg, queries, creditRepo, registry)
	startMetricsRefresher(ctx, log, &wg, queries, collectors)

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	grpcServer.GracefulStop()
	wg.Wait()
	log.Info("exit")
}

// recoverOrphanReservations releases slot rows still in 'reserved' state at
// boot — they belong to a previous main-api process that died between
// Reserve and BindToInstance. Logs the count when nonzero; fatal-exits on
// query failure since we can't safely accept new instance creates with
// stale reservations holding capacity.
func recoverOrphanReservations(ctx context.Context, log *slog.Logger, slots *slot.Repo) {
	released, err := slots.RecoverOrphanReservations(ctx)
	if err != nil {
		log.Error("recover orphan reservations", "err", err)
		os.Exit(1)
	}
	if released > 0 {
		log.Warn("released orphan slot reservations on boot", "count", released)
	}
}

// runMigrations applies schema changes and returns true when the caller
// should exit before starting the service (i.e. --migrate-only mode).
// On migration failure it terminates the process with exit code 1.
func runMigrations(ctx context.Context, log *slog.Logger, dbURL string, migrateOnly bool) bool {
	if err := migrate(ctx, dbURL); err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	if migrateOnly {
		log.Info("migrate-only complete")
		return true
	}
	return false
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

// startBillingWorker spawns the Phase 9 billing worker iff a rates path is
// configured. Empty path disables billing entirely (handy for local dev).
func startBillingWorker(
	ctx context.Context,
	log *slog.Logger,
	wg *sync.WaitGroup,
	cfg config.Config,
	queries *dbstore.Queries,
	creditRepo *credit.Repo,
	registry *grpcsrv.AgentRegistry,
) {
	if cfg.BillingRatesPath == "" {
		return
	}
	rates, err := billing.LoadRates(cfg.BillingRatesPath)
	if err != nil {
		log.Error("load rates", "path", cfg.BillingRatesPath, "err", err)
		os.Exit(2)
	}
	w := &billing.Worker{
		Instances:  queries,
		Credits:    creditRepo,
		Rates:      rates,
		Dispatcher: registry,
		Tick:       cfg.BillingTick,
		Log:        log,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info("billing worker started", "tick", cfg.BillingTick, "rates", cfg.BillingRatesPath)
		if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("billing worker", "err", err)
		}
	}()
}

// startMetricsRefresher spawns the Phase 10.2 domain-state gauge sampler.
func startMetricsRefresher(
	ctx context.Context,
	log *slog.Logger,
	wg *sync.WaitGroup,
	queries *dbstore.Queries,
	collectors *metrics.Collectors,
) {
	r := &metrics.DomainRefresher{
		Queries:  queries,
		Coll:     collectors,
		Interval: 15 * time.Second,
		Log:      log,
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("metrics refresher", "err", err)
		}
	}()
}
