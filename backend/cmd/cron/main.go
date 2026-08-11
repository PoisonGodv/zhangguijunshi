// Command cron is the dedicated periodic-job binary (CONTRACT §19). In prod the
// api process sets scheduler.run_in_api=false and THIS binary owns all timing:
//
//   - the 4 pregen crons (batch 0 3 * * * / incremental */10 / attribution 0 4 /
//     bandit 0 2) run inside svc.Scheduler's OWN internal cron loop — we only
//     Start/Stop it here;
//   - the 3 non-pregen jobs (groupbuy 成团超时 / subscription 到期 / dataquality
//     销售反推+标签) run on a SEPARATE *cron.Cron owned by this binary, in the
//     Asia/Shanghai location.
//
// Every dependency degrades gracefully (CONTRACT §0): with no Postgres/Redis the
// per-tenant loops enumerate zero tenants and the jobs are clean no-ops, so the
// binary still boots and ticks on a bare dev box.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/internal/bootstrap"
	"github.com/zhangguijunshi/backend/pkg/config"
)

// healthAddr is the cron binary's liveness endpoint listener (CONTRACT §25.4).
// cron is a pure scheduler loop with no HTTP surface; a minimal /healthz lets
// compose healthcheck detect a dead/wedged cron. Distinct port from api (:8080)
// and worker (:8091).
const healthAddr = ":8092"

// serveHealth exposes a minimal liveness endpoint for the cron binary (a pure
// scheduler loop with no other HTTP surface). Returns the server so run can
// shut it down on signal.
func serveHealth(log *zap.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
	srv := &http.Server{Addr: healthAddr, Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Warn("cron healthz server stopped", zap.Error(err))
		}
	}()
	return srv
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cron:", err)
		os.Exit(1)
	}
}

// run wires the composition root, starts the pregen scheduler + the non-pregen
// cron, then blocks until SIGINT/SIGTERM and shuts down gracefully.
func run() error {
	cfg, err := config.Load(config.WithEnv(envOr("ZG_ENV", "dev")))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log, err := bootstrap.BuildLogger(cfg)
	if err != nil {
		return fmt.Errorf("build logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	ctx := context.Background()
	svc, err := bootstrap.BuildServices(ctx, cfg, log, nil)
	if err != nil {
		return fmt.Errorf("build services: %w", err)
	}
	// svc.Close is nil-safe and idempotent enough for a single deferred call; it
	// releases the DB pool / Redis client / session store on shutdown.
	defer svc.Close()

	// ---- pregen (4 jobs): the Scheduler owns its OWN internal cron loop. We
	// start it in a goroutine (Start blocks on cron.Start) and Stop it on
	// shutdown. Guard nil so a stripped-down graph still boots. ----
	if svc.Scheduler != nil {
		go func() {
			if err := svc.Scheduler.Start(); err != nil {
				log.Warn("pregen scheduler start failed", zap.Error(err))
			}
		}()
		log.Info("pregen scheduler started (owns batch/incremental/attribution/bandit)")
	} else {
		log.Warn("pregen scheduler is nil; skipping pregen crons")
	}

	// ---- non-pregen jobs on a SEPARATE cron, location Asia/Shanghai (falls
	// back to time.Local if the tz database is unavailable). ----
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		log.Warn("load Asia/Shanghai failed; falling back to time.Local", zap.Error(err))
		loc = time.Local
	}

	c := cron.New(cron.WithLocation(loc))
	if err := registerJobs(c, svc, log); err != nil {
		return fmt.Errorf("register jobs: %w", err)
	}
	c.Start()
	log.Info("cron binary started",
		zap.Int("non_pregen_entries", len(c.Entries())),
		zap.String("location", loc.String()))

	// Liveness endpoint (§25.4): a dead/wedged cron is otherwise invisible.
	healthSrv := serveHealth(log)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = healthSrv.Shutdown(ctx)
	}()

	// ---- block until signalled, then graceful stop. ----
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	sig := <-stop
	log.Info("shutdown signal received", zap.String("signal", sig.String()))

	// Stop the non-pregen cron and wait for any in-flight job to finish.
	stopCtx := c.Stop()
	<-stopCtx.Done()

	// Stop the pregen scheduler (idempotent, never panics).
	if svc.Scheduler != nil {
		svc.Scheduler.Stop()
	}

	log.Info("shutdown complete")
	return nil
}

// envOr returns the value of env var key, or dflt when it is empty/unset.
func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}
