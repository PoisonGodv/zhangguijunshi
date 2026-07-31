//go:build integration

package jobqueue_test

// This is a REAL-Redis integration test (CONTRACT §20.7). It is excluded from
// the normal `go test` run (build tag: integration) because it needs a live
// Redis reachable at $ZG_TEST_REDIS (default localhost:16379). Run it via a
// port-forward / published Redis:
//
//	docker run --rm -d --network zg-net --name zg-redis-fwd \
//	  -p 16379:6379 alpine/socat TCP-LISTEN:6379,fork,reuseaddr TCP:redis:6379
//	ZG_TEST_REDIS=localhost:16379 \
//	  go test -tags=integration -race -count=1 -run TestRealRedis ./pkg/jobqueue/...
//
// It proves the delay lane works against real Redis (not just miniredis):
// a future-RunAt job sits in the ZSET (NOT the Stream) until due, then the
// DelayPump hands it to the Consumer exactly once, at/after RunAt.

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/jobqueue"
)

// realStream is a dedicated Stream so this test never clobbers a real worker's
// zg:jobs lane. The delay ZSET is derived as realStream + ":delay".
const realStream = "zg:jobs:integration"

func testRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("ZG_TEST_REDIS")
	if addr == "" {
		addr = "localhost:16379"
	}
	c := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Fatalf("ZG_TEST_REDIS (%s) unreachable: %v (start the socat forwarder, see file header)", addr, err)
	}
	return c
}

func TestRealRedis_DelayLaneDeliversOnTimeNotEarly(t *testing.T) {
	c := testRedisClient(t)
	defer c.Close()

	ctx := context.Background()
	zset := realStream + ":delay"
	// Clean slate for our dedicated keys (idempotent across re-runs).
	c.Del(ctx, realStream, zset)

	cfg := config.JobQueueConfig{Stream: realStream, Group: "zg-int-group", Consumer: "int-0", Block: 200 * time.Millisecond}

	// 1) Enqueue a job due in 1.2s. Producer must route it to the ZSET.
	prod := jobqueue.NewProducer(c, cfg)
	due := time.Now().Add(1200 * time.Millisecond)
	if err := prod.Enqueue(ctx, jobqueue.Job{
		Type:     jobqueue.JobReachDueSend,
		Payload:  []byte("real-redis-delay-probe"),
		TenantID: 4242,
		RunAt:    due,
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// 2) NOT in the Stream yet; IS in the ZSET.
	if n, _ := c.XLen(ctx, realStream).Result(); n != 0 {
		t.Fatalf("job leaked into live Stream before due: XLen=%d", n)
	}
	if n, _ := c.ZCard(ctx, zset).Result(); n != 1 {
		t.Fatalf("delayed job not in ZSET: ZCard=%d", n)
	}

	// 3) Start consumer (records delivery) + pump (real clock, 80ms sweeps).
	var (
		mu        sync.Mutex
		got       int64
		gotAt     time.Time
		gotJob    jobqueue.Job
		delivered = make(chan struct{}, 1)
	)
	handler := jobqueue.HandlerFunc(func(_ context.Context, j jobqueue.Job) error {
		mu.Lock()
		got++
		gotAt = time.Now()
		gotJob = j
		mu.Unlock()
		select {
		case delivered <- struct{}{}:
		default:
		}
		return nil
	})
	consumer := jobqueue.NewConsumer(c, cfg, handler)
	pump := jobqueue.NewDelayPump(c, cfg, jobqueue.WithPumpInterval(80*time.Millisecond))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = consumer.Run(runCtx) }()
	go func() { _ = pump.Run(runCtx) }()

	// 4) It must NOT be delivered before due. At ~halfway (600ms) it should
	// still be pending in the ZSET.
	time.Sleep(600 * time.Millisecond)
	mu.Lock()
	earlyGot := got
	mu.Unlock()
	if earlyGot != 0 {
		mu.Lock()
		ea := gotAt
		mu.Unlock()
		t.Fatalf("job delivered BEFORE due (early fire): at=%v due=%v", ea, due)
	}

	// 5) It must be delivered at/after due, within a generous bound.
	select {
	case <-delivered:
	case <-time.After(4 * time.Second):
		t.Fatalf("job not delivered within 4s of enqueue (due=%v)", due)
	}
	mu.Lock()
	finalGot, finalAt, finalJob := got, gotAt, gotJob
	mu.Unlock()
	if finalGot != 1 {
		t.Fatalf("expected exactly 1 delivery, got %d (duplicate delivery?)", finalGot)
	}
	if finalAt.Before(due.Add(-50 * time.Millisecond)) {
		t.Fatalf("delivered early: at=%v due=%v", finalAt, due)
	}
	if finalJob.Type != jobqueue.JobReachDueSend || finalJob.TenantID != 4242 || string(finalJob.Payload) != "real-redis-delay-probe" {
		t.Fatalf("payload round-trip mismatch: %#v", finalJob)
	}

	// 6) After delivery the ZSET is drained and the Stream is consumed (XDel'd).
	cancel()
	time.Sleep(300 * time.Millisecond)
	if n, _ := c.ZCard(ctx, zset).Result(); n != 0 {
		t.Fatalf("ZSET not drained after delivery: ZCard=%d", n)
	}

	c.Del(ctx, realStream, zset)
}

// TestRealRedis_ProducerImmediatePathBypassesZSET guards the non-delay branch:
// a zero/past RunAt job goes straight to the Stream (the pump never sees it).
func TestRealRedis_ProducerImmediatePathBypassesZSET(t *testing.T) {
	c := testRedisClient(t)
	defer c.Close()
	ctx := context.Background()
	zset := realStream + ":delay"
	c.Del(ctx, realStream, zset)
	defer c.Del(ctx, realStream, zset)

	cfg := config.JobQueueConfig{Stream: realStream, Group: "zg-int-group2", Consumer: "int-1"}
	prod := jobqueue.NewProducer(c, cfg)
	if err := prod.Enqueue(ctx, jobqueue.Job{Type: jobqueue.JobPregenIncremental, TenantID: 7, RunAt: time.Time{}}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n, _ := c.XLen(ctx, realStream).Result(); n != 1 {
		t.Fatalf("immediate job should be in Stream: XLen=%d", n)
	}
	if n, _ := c.ZCard(ctx, zset).Result(); n != 0 {
		t.Fatalf("immediate job must not touch ZSET: ZCard=%d", n)
	}
}
