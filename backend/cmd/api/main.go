// Command api is the single HTTP entry point for the modular monolith
// (tech plan §1.2.1 / §6.1.3). It loads layered config, builds the shared
// composition root via internal/bootstrap, and serves the three BFF groups with
// graceful shutdown on SIGINT/SIGTERM.
//
// All domain wiring (repos, services, AI pipeline, payment ports, COS, auth)
// now lives in internal/bootstrap so the worker / cron binaries reuse the exact
// same object graph (CONTRACT §19). This binary only owns: config + logger,
// BuildServices, the optional in-process pregen scheduler (dev), the HTTP
// server, and graceful shutdown.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"

	httpserver "github.com/zhangguijunshi/backend/interfaces/http"
	"github.com/zhangguijunshi/backend/internal/bootstrap"
	"github.com/zhangguijunshi/backend/pkg/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.WithEnv(envOr("ZG_ENV", "dev")))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log, err := bootstrap.BuildLogger(cfg)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	log.Info("starting zhanggui backend",
		zap.String("env", cfg.App.Env),
		zap.String("version", cfg.App.Version),
	)

	// Shared composition root: infra (db/redis/jwt/session) + the full domain
	// graph. Degrades gracefully in dev (no Postgres/Redis → in-memory).
	svc, err := bootstrap.BuildServices(context.Background(), cfg, log, nil)
	if err != nil {
		return fmt.Errorf("build services: %w", err)
	}
	defer svc.Close()

	// dev runs the pregen scheduler in-process (single-binary convenience); prod
	// sets scheduler.run_in_api=false and lets the cron binary own it so the two
	// never double-tick (CONTRACT §19).
	if cfg.Scheduler.RunInAPI {
		svc.StartScheduler()
	}

	srv := &http.Server{
		Addr: fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler: httpserver.New(httpserver.Deps{
			Cfg:          cfg,
			DB:           svc.DB,
			JWT:          svc.JWT,
			Store:        svc.Sess,
			AuthSvc:      svc.AuthSvc,
			Catalog:      svc.Catalog,
			Billing:      svc.Billing,
			Inventory:    svc.Inventory,
			Membership:   svc.Membership,
			Marketing:    svc.Marketing,
			Reach:        svc.Reach,
			Groupbuy:     svc.Groupbuy,
			CRM:          svc.CRM,
			Strategy:     svc.Strategy,
			Pipeline:     svc.Pipeline,
			Executor:     svc.Executor,
			Attribution:  svc.Attribution,
			Payment:      svc.Payment,
			Subscription: svc.Subscription,
			StoreSvc:     svc.StoreSvc,
			COS:          svc.COS,
			DevMock:      cfg.Multitenancy.DevMockHeader,
		}),
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
	}

	go func() {
		log.Info("http listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("http server crashed", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}

func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}
