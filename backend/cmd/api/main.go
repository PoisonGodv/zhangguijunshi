// Command api is the single HTTP entry point for the modular monolith
// (tech plan §1.2.1 / §6.1.3). It loads layered config, initialises structured
// logging, optionally opens the DB pool, and serves the three BFF groups with
// graceful shutdown on SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	httpserver "github.com/zhangguijunshi/backend/interfaces/http"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/db"
	"github.com/zhangguijunshi/backend/pkg/logger"
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

	log, err := logger.New(cfg.Log)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()
	logger.SetDefault(log)

	log.Info("starting zhanggui backend",
		zap.String("env", cfg.App.Env),
		zap.String("version", cfg.App.Version),
	)

	// The DB pool is optional at boot: in local dev without Docker/Postgres we
	// log a warning and continue with a nil pool; readyz reports it as skipped.
	var database *db.DB
	if cfg.Postgres.Host != "" {
		database, err = db.New(context.Background(), cfg.Postgres)
		if err != nil {
			log.Warn("db pool init failed; continuing without database", zap.Error(err))
			database = nil
		} else {
			pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if perr := database.Ping(pctx); perr != nil {
				log.Warn("db ping failed; continuing without database",
					zap.String("dsn", database.DSN()), zap.Error(perr))
				database.Close()
				database = nil
			} else {
				log.Info("db connected", zap.String("dsn", database.DSN()))
			}
			cancel()
		}
	}

	srv := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler:      httpserver.New(httpserver.Deps{Cfg: cfg, DB: database}),
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
	if database != nil {
		database.Close()
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
