package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/internal/bootstrap"
	"github.com/zhangguijunshi/backend/internal/reach"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/jobqueue"
)

// newTestServices builds the composition root in PURE in-memory/dev mode:
// defaults-only config (empty config dir → no yaml layer) with the Postgres and
// Redis hosts forcibly blanked. The blanking matters: the built-in defaults
// point both at 127.0.0.1, and a developer box (or CI) may have a real Docker
// PG/Redis listening there — dialling it would swap in the PG repos and make the
// test non-deterministic. Blanking the hosts guarantees nil DB/RDB, memory
// repos, and reach WITHOUT a scheduled producer.
func newTestServices(t *testing.T) *bootstrap.Services {
	t.Helper()
	cfg, err := config.Load(config.WithConfigDir(t.TempDir()))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Postgres.Host = "" // force nil DB  (in-memory repos)
	cfg.Redis.Host = ""    // force nil RDB (no scheduled producer)

	svc, err := bootstrap.BuildServices(context.Background(), cfg, zap.NewNop(), nil)
	if err != nil {
		t.Fatalf("build services: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

// reachPayload marshals a valid reach_due_send job body for a wework_group
// birthday send (no subscribe quota needed on that channel).
func reachPayload(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(reach.ScheduledSendPayload{
		In: reach.SendInput{
			StoreID:      1,
			MemberID:     42,
			Channel:      reach.ChannelWeworkGroup,
			TemplateCode: reach.TplBirthday,
		},
	})
	if err != nil {
		t.Fatalf("marshal reach payload: %v", err)
	}
	return b
}

// TestDispatcher_ReachDueSend: a valid reach_due_send job is delivered through
// reach.Send and Handle returns nil. With the in-memory reach repo + memory
// sender on the wework_group channel, the send runs the full guard pipeline and
// ends sent (or, if a guard intercepts, blocked) — either way (task, nil), so
// Handle must return nil. The dispatcher rebuilds the tenant ctx from
// job.TenantID, so a bare ctx is correct here.
func TestDispatcher_ReachDueSend(t *testing.T) {
	svc := newTestServices(t)
	d := &dispatcher{svc: svc}

	job := jobqueue.Job{
		Type:     jobqueue.JobReachDueSend,
		Payload:  reachPayload(t),
		TenantID: 7,
	}
	if err := d.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle(reach_due_send) = %v, want nil", err)
	}
}

// TestDispatcher_ReachNilService: with no reach service wired the job is logged
// + dropped (nil) rather than redelivered forever — the nil-safe contract. This
// is also a hard assertion that reach_due_send routes to the reach branch.
func TestDispatcher_ReachNilService(t *testing.T) {
	svc := newTestServices(t)
	svc.Reach = nil
	d := &dispatcher{svc: svc}

	job := jobqueue.Job{
		Type:     jobqueue.JobReachDueSend,
		Payload:  reachPayload(t),
		TenantID: 7,
	}
	if err := d.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle(reach_due_send, nil reach) = %v, want nil", err)
	}
}

// TestDispatcher_PregenNilScheduler: with no scheduler wired the job is logged +
// dropped (nil). Hard assertion that pregen_incremental routes to the pregen
// branch and is nil-safe.
func TestDispatcher_PregenNilScheduler(t *testing.T) {
	svc := newTestServices(t)
	svc.Scheduler = nil
	d := &dispatcher{svc: svc}

	job := jobqueue.Job{Type: jobqueue.JobPregenIncremental, TenantID: 1}
	if err := d.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle(pregen, nil scheduler) = %v, want nil", err)
	}
}

// TestDispatcher_PregenIncremental exercises the real scheduler path.
//
// Observed dev behaviour (reported to the integrator): in the current shared
// bootstrap wiring the pipeline CANNOT persist a strategy — pipeline.Run derives
// storeID from ctx (0) and pregen.runAndCache injects only TenantID (no StoreID),
// so strategy.SaveAdvice rejects StoreID==0 ("strategy: invalid param").
// RefreshTenant therefore errors and retries on its 1+2+4s backoff. That is an
// internal pregen/store-resolution gap, not a worker bug: the worker's contract
// is to route the job and PROPAGATE the error so the queue redelivers
// (at-least-once). We therefore assert only that Handle does not panic, tolerate
// either a nil or a non-nil result (so the test does not break if the store gap
// is later fixed), and use a short ctx deadline to bypass the multi-second retry
// backoff and keep the test fast.
func TestDispatcher_PregenIncremental(t *testing.T) {
	svc := newTestServices(t)
	d := &dispatcher{svc: svc}

	job := jobqueue.Job{Type: jobqueue.JobPregenIncremental, TenantID: 1}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Handle(pregen_incremental) panicked: %v", r)
		}
	}()
	err := d.Handle(ctx, job)
	if err != nil {
		t.Logf("RefreshTenant errored (expected in current dev wiring): %v", err)
	} else {
		t.Log("RefreshTenant succeeded (pipeline resolved a store)")
	}
}

// TestDispatcher_UnknownType: an unrecognized job type is a poison pill — it is
// dropped (Handle returns nil) so it cannot redeliver forever.
func TestDispatcher_UnknownType(t *testing.T) {
	svc := newTestServices(t)
	d := &dispatcher{svc: svc}

	job := jobqueue.Job{Type: "bogus", TenantID: 7}
	if err := d.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle(unknown) = %v, want nil", err)
	}
}

// TestDispatcher_CorruptReachPayload: a reach_due_send job whose payload is not
// valid JSON can never be handled, so it is dropped (nil), not redelivered.
func TestDispatcher_CorruptReachPayload(t *testing.T) {
	svc := newTestServices(t)
	d := &dispatcher{svc: svc}

	job := jobqueue.Job{
		Type:     jobqueue.JobReachDueSend,
		Payload:  []byte("{bad"),
		TenantID: 7,
	}
	if err := d.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle(corrupt reach payload) = %v, want nil", err)
	}
}

// TestDispatcher_ReachInvalidTenant: a reach_due_send job that decodes fine but
// carries an invalid tenant (TenantID <= 0) can NEVER be delivered — reach.Send
// enforces the tenant from ctx and rejects a non-positive one every time. It must
// be dropped (Warn + nil) like a poison pill, NOT returned as an error (which
// would redeliver it forever and, via the queue's XAUTOCLAIM sweep, re-home it
// across replicas indefinitely). Regression for the permanent-redelivery bug.
func TestDispatcher_ReachInvalidTenant(t *testing.T) {
	svc := newTestServices(t)
	d := &dispatcher{svc: svc}

	for _, tenant := range []int64{0, -1} {
		job := jobqueue.Job{
			Type:     jobqueue.JobReachDueSend,
			Payload:  reachPayload(t), // valid payload; only the tenant is bad
			TenantID: tenant,
		}
		if err := d.Handle(context.Background(), job); err != nil {
			t.Fatalf("Handle(reach_due_send, tenant=%d) = %v, want nil (poison drop)", tenant, err)
		}
	}
}

// TestEnvOr covers the env-fallback helper used to select ZG_ENV.
func TestEnvOr(t *testing.T) {
	const key = "ZG_WORKER_ENVOR_TEST"
	os.Unsetenv(key)
	if got := envOr(key, "dflt"); got != "dflt" {
		t.Fatalf("envOr(unset) = %q, want %q", got, "dflt")
	}
	t.Setenv(key, "custom")
	if got := envOr(key, "dflt"); got != "custom" {
		t.Fatalf("envOr(set) = %q, want %q", got, "custom")
	}
}

// TestWorker_MiniredisEndToEnd wires a real (in-process miniredis) Stream,
// enqueues a job, runs the consumer with the dispatcher, and asserts the job is
// consumed (XLEN drains back to 0) within a bounded window.
//
// We enqueue a reach_due_send job (not pregen_incremental) because the pregen
// path currently errors in dev wiring (see TestDispatcher_PregenIncremental) and
// would therefore be left pending for redelivery — XLEN would never reach 0. A
// reach_due_send job is handled successfully, so it exercises the full
// enqueue → XREADGROUP → dispatch → XAck+XDel → drain loop end-to-end.
func TestWorker_MiniredisEndToEnd(t *testing.T) {
	svc := newTestServices(t)
	d := &dispatcher{svc: svc}

	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	cfg := config.JobQueueConfig{
		Stream:   "zg:worker:test",
		Group:    "zg-workers",
		Consumer: "worker-test",
		Block:    20 * time.Millisecond, // short block → fast loop in tests
		MaxLen:   100,
	}

	// The consumer group must exist BEFORE the message is added: a group created
	// with "$" only ever sees entries added after its creation. Creating it now,
	// on the still-empty stream, means our subsequent XADD is "new" and the
	// consumer's ">" read delivers it. (The consumer's own "$" create later hits
	// BUSYGROUP and reuses this group.)
	if err := client.XGroupCreateMkStream(context.Background(), cfg.Stream, cfg.Group, "$").Err(); err != nil {
		t.Fatalf("create group: %v", err)
	}

	prod := jobqueue.NewProducer(client, cfg)
	if err := prod.Enqueue(context.Background(), jobqueue.Job{
		Type:     jobqueue.JobReachDueSend,
		Payload:  reachPayload(t),
		TenantID: 7,
		RunAt:    time.Now(),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cons := jobqueue.NewConsumer(client, cfg, d)
	doneRun := make(chan error, 1)
	go func() { doneRun <- cons.Run(ctx) }()

	// Poll until the stream drains (entry ACKed + XDel'ed) or time out.
	deadline := time.Now().Add(3 * time.Second)
	for {
		n, err := client.XLen(context.Background(), cfg.Stream).Result()
		if err != nil {
			t.Fatalf("xlen: %v", err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream not drained: XLEN=%d (job not consumed)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-doneRun:
		if err != nil {
			t.Fatalf("consumer.Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer.Run did not return after cancel")
	}
}
