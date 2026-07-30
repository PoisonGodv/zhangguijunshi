// Command worker is the async job consumer binary (CONTRACT §19). It reuses the
// shared composition root (internal/bootstrap) — the SAME object graph the api
// and cron binaries run — and blocks on the Redis Stream job queue, dispatching
// each jobqueue.Job by its Type:
//
//	pregen_incremental → pregen Scheduler.RefreshTenant (one tenant, overwrites
//	                     both AI caches)
//	reach_due_send     → reach Service.Send (deferred message whose window opened)
//
// Nil-safe degradation (CONTRACT §0): with no Redis (local dev) there is nothing
// to consume, so the worker logs and exits 0 cleanly instead of spinning. With
// no scheduler / reach wired the corresponding job is logged + dropped (nil) so
// a missing dependency never wedges the stream.
//
// The binary owns ONLY: config + logger, BuildServices, the redis.Cmdable
// decision, the consumer lifecycle, and graceful shutdown on SIGINT/SIGTERM. All
// domain wiring lives in internal/bootstrap.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/internal/bootstrap"
	"github.com/zhangguijunshi/backend/internal/reach"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/jobqueue"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// shutdownGrace bounds how long run waits for the consumer's Run loop to drain
// after cancel before giving up and closing infra. The idle loop paces itself
// on the XREADGROUP BLOCK (cfg.JobQueue.Block), so a few seconds is generous.
const shutdownGrace = 5 * time.Second

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

	log.Info("starting zhanggui worker",
		zap.String("env", cfg.App.Env),
		zap.String("version", cfg.App.Version),
	)

	// Shared composition root: infra (db/redis/jwt/session) + the full domain
	// graph. Degrades gracefully in dev (no Postgres/Redis → in-memory / nil).
	svc, err := bootstrap.BuildServices(context.Background(), cfg, log, nil)
	if err != nil {
		return fmt.Errorf("build services: %w", err)
	}
	defer svc.Close()

	// Decide the queue client. redisx.Client embeds *redis.Client, but its own
	// Ping(ctx) error helper shadows the embedded client's Ping, so *redisx.Client
	// itself does NOT satisfy redis.Cmdable — use the embedded *redis.Client
	// (svc.RDB.Client), exactly as bootstrap does when wiring the producer. nil
	// RDB (dev, no Redis) means there is nothing to consume → clean exit 0.
	var cmdable redis.Cmdable
	if svc.RDB != nil {
		cmdable = svc.RDB.Client
	}
	if cmdable == nil {
		log.Info("no redis; worker has nothing to consume — clean exit")
		return nil
	}

	consumer := jobqueue.NewConsumer(cmdable, cfg.JobQueue, &dispatcher{svc: svc})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- consumer.Run(ctx)
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-stop:
		log.Info("shutdown signal received", zap.String("signal", sig.String()))
		cancel()
		// Wait (bounded) for the read loop to drain before closing infra.
		select {
		case err := <-runErr:
			if err != nil {
				return fmt.Errorf("consumer run: %w", err)
			}
		case <-time.After(shutdownGrace):
			log.Warn("consumer did not stop within grace period",
				zap.Duration("grace", shutdownGrace))
		}
	case err := <-runErr:
		// Run returned on its own (nil client, or a fatal group-create error).
		cancel()
		if err != nil {
			return fmt.Errorf("consumer run: %w", err)
		}
		log.Info("consumer stopped on its own")
	}

	log.Info("shutdown complete")
	return nil
}

// dispatcher routes a jobqueue.Job to the right domain service by Type. It
// implements jobqueue.Handler.
//
// Return contract (drives the queue's ACK / redelivery — see jobqueue.Consumer):
//
//	nil → job is done, OR deliberately dropped (unknown type / corrupt payload /
//	      missing dependency). The queue ACKs + XDel's the entry.
//	err → transient failure. The queue leaves the entry pending and redelivers it
//	      on the next loop (at-least-once). Handlers must therefore be idempotent.
type dispatcher struct {
	svc *bootstrap.Services
}

// Handle implements jobqueue.Handler.
func (d *dispatcher) Handle(ctx context.Context, job jobqueue.Job) error {
	switch job.Type {
	case jobqueue.JobPregenIncremental:
		return d.handlePregen(ctx, job)
	case jobqueue.JobReachDueSend:
		return d.handleReachDueSend(ctx, job)
	default:
		// Unknown type = poison pill (or a job type from a newer producer this
		// worker does not know). Drop it (nil) so it cannot redeliver forever;
		// log loudly for operators.
		d.svc.Log.Warn("worker: dropping job of unknown type",
			zap.String("type", job.Type),
			zap.Int64("tenant_id", job.TenantID))
		return nil
	}
}

// handlePregen refreshes ONE tenant's "today" AI caches on demand. RefreshTenant
// builds the tenant-scoped ctx (Source=system) internally, so the caller passes
// the bare queue ctx. A pipeline failure returns a real error → redelivery.
func (d *dispatcher) handlePregen(ctx context.Context, job jobqueue.Job) error {
	if d.svc.Scheduler == nil {
		d.svc.Log.Warn("worker: pregen_incremental dropped; scheduler not wired",
			zap.Int64("tenant_id", job.TenantID))
		return nil
	}
	return d.svc.Scheduler.RefreshTenant(ctx, job.TenantID)
}

// handleReachDueSend delivers a deferred reach message whose send window opened.
// The job body is a reach.ScheduledSendPayload (JSON); the tenant is carried on
// the Job (the producer stripped it from ctx when enqueuing) and is rebuilt into
// a tenant-scoped ctx here because reach.Send enforces tenant from ctx.
func (d *dispatcher) handleReachDueSend(ctx context.Context, job jobqueue.Job) error {
	var payload reach.ScheduledSendPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		// Corrupt payload: it can never decode, so drop (nil) rather than
		// redeliver forever. Poison-pill guard, logged for operators.
		d.svc.Log.Warn("worker: dropping corrupt reach_due_send payload",
			zap.Int64("tenant_id", job.TenantID),
			zap.Error(err))
		return nil
	}
	if d.svc.Reach == nil {
		d.svc.Log.Warn("worker: reach_due_send dropped; reach not wired",
			zap.Int64("tenant_id", job.TenantID))
		return nil
	}

	in := payload.In
	// CRITICAL: clear ScheduledAt. This worker IS the deferred-delivery path. In
	// prod the producer is wired (Redis present), so if we re-handed Send a
	// still-future ScheduledAt it would re-ENQUEUE instead of deliver — an
	// infinite enqueue loop. Zeroing it forces immediate delivery (Send's
	// deferred branch requires a non-zero, future ScheduledAt).
	in.ScheduledAt = time.Time{}

	tctx := multitenancy.WithContext(ctx, multitenancy.Context{
		TenantID: job.TenantID,
		Source:   multitenancy.SourceSystem, // background job, not a user request
	})
	// A compliance-blocked send returns (task, nil) — NOT an error — so it is
	// ACKed. Only a real system failure (tenant/render/repo) returns err and is
	// redelivered.
	_, err := d.svc.Reach.Send(tctx, in)
	return err
}

// envOr returns the value of env key, or dflt when unset/empty.
func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}
