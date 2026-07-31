// Command worker is the async job consumer binary (CONTRACT §19). It reuses the
// shared composition root (internal/bootstrap) — the SAME object graph the api
// and cron binaries run — and blocks on the Redis Stream job queue, dispatching
// each jobqueue.Job by its Type:
//
//	pregen_incremental → pregen Scheduler.RefreshTenant (one tenant, overwrites
//	                     both AI caches)
//	reach_due_send     → reach Service.Send (deferred message whose window opened)
//
// It runs TWO Redis loops side by side (CONTRACT §20.3):
//   - jobqueue.Consumer drains the live Stream (XREADGROUP) and dispatches.
//   - jobqueue.DelayPump sweeps the <stream>:delay ZSET and XADDs due members
//     onto the live Stream once their RunAt arrives. This is what makes a
//     scheduled (future RunAt) message actually wait — without it, the consumer
//     would fire it the instant the producer enqueued it. The pump is a shared
//     mechanism; every job type with a future RunAt benefits automatically.
//
// Nil-safe degradation (CONTRACT §0): with no Redis (local dev) there is nothing
// to consume, so the worker logs and exits 0 cleanly instead of spinning. With
// no scheduler / reach wired the corresponding job is logged + dropped (nil) so
// a missing dependency never wedges the stream.
//
// The binary owns ONLY: config + logger, BuildServices, the redis.Cmdable
// decision, the consumer + pump lifecycles, and graceful shutdown on
// SIGINT/SIGTERM. All domain wiring lives in internal/bootstrap.
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

// shutdownGrace bounds how long run waits for the consumer's and pump's Run
// loops to drain after cancel before giving up and closing infra. The consumer's
// idle loop paces itself on the XREADGROUP BLOCK (cfg.JobQueue.Block); the pump
// paces on its sweep interval. A few seconds is generous for both.
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
	// DelayPump moves future-RunAt jobs out of the <stream>:delay ZSET and onto
	// the live Stream once they are due, so the consumer above actually delivers
	// them at RunAt (CONTRACT §20.3). Same cfg.JobQueue so producer (api) and
	// pump (worker) resolve the same stream + delay lane.
	pump := jobqueue.NewDelayPump(cmdable, cfg.JobQueue)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Both loops return nil on clean ctx cancel; the pump is nil-safe just like
	// the consumer (a nil cmdable already exited above, so both really run here).
	type runResult struct {
		name string
		err  error
	}
	results := make(chan runResult, 2)
	go func() { results <- runResult{"consumer", consumer.Run(ctx)} }()
	go func() { results <- runResult{"pump", pump.Run(ctx)} }()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// drainRemaining waits (bounded by shutdownGrace PER remaining loop) for the
	// goroutines still outstanding after cancel. already is how many have already
	// reported (0 on signal path, 1 on the "loop stopped on its own" path).
	drainRemaining := func(already int) error {
		for i := 0; i < 2-already; i++ {
			select {
			case r := <-results:
				if r.err != nil {
					return fmt.Errorf("%s run: %w", r.name, r.err)
				}
			case <-time.After(shutdownGrace):
				log.Warn("loop did not stop within grace period",
					zap.Duration("grace", shutdownGrace))
			}
		}
		return nil
	}

	select {
	case sig := <-stop:
		log.Info("shutdown signal received", zap.String("signal", sig.String()))
		cancel()
		// Wait (bounded) for both loops to drain before closing infra.
		if err := drainRemaining(0); err != nil {
			return err
		}
	case r := <-results:
		// One loop returned on its own (nil client handled earlier; here it is
		// either a clean nil or a fatal group-create / sweep error). Cancel so
		// the sibling drains too, then wait for it.
		cancel()
		if r.err != nil {
			return fmt.Errorf("%s run: %w", r.name, r.err)
		}
		log.Info("loop stopped on its own", zap.String("loop", r.name))
		if err := drainRemaining(1); err != nil {
			return err
		}
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
//	      invalid tenant / missing dependency). The queue ACKs + XDel's the entry.
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
	// Poison-pill guard: a message that decodes fine but carries an invalid
	// tenant (TenantID <= 0) can NEVER be delivered — reach.Send enforces the
	// tenant from ctx and rejects a non-positive one every time. Returning an
	// error here would redeliver it forever (and, via the queue's XAUTOCLAIM
	// sweep, re-home it across replicas indefinitely). Treat it like a corrupt
	// payload: Warn + return nil so the queue ACKs it away.
	if job.TenantID <= 0 {
		d.svc.Log.Warn("worker: dropping reach_due_send with invalid tenant (poison pill)",
			zap.Int64("tenant_id", job.TenantID))
		return nil
	}

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
	// Finalize the pending ReachTask row that was persisted at enqueue time
	// (payload.TaskID > 0) so delivery UPDATES that row instead of inserting a
	// duplicate; legacy jobs enqueued before that field existed carry TaskID==0
	// and fall back to creating a fresh row. A compliance-blocked send returns
	// (task, nil) — NOT an error — so it is ACKed. Only a real system failure
	// (tenant/render/repo) returns err and is redelivered.
	_, err := d.svc.Reach.SendFinalize(tctx, in, payload.TaskID)
	return err
}

// envOr returns the value of env key, or dflt when unset/empty.
func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}
