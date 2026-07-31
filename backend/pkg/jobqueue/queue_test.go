package jobqueue

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// newMiniredis builds a miniredis + a connected go-redis client. The miniredis
// is tied to the test lifetime via RunT; the caller defers client.Close().
func newMiniredis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

// startConsumer runs cons.Run in a goroutine under a fresh cancellable ctx and
// returns the cancel func. Fire-and-forget: the consumer stops when the test
// cancels or the miniredis shuts down.
func startConsumer(cons *Consumer) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = cons.Run(ctx) }()
	return cancel
}

// ensureGroup creates the consumer group with start "$" BEFORE any messages are
// enqueued. The group's start position decides which messages a subsequent
// XREADGROUP ">" ever sees: "$" means "only messages added after group
// creation", so enqueuing only after the group exists is what makes the
// messages reachable. Doing it explicitly (rather than relying on the consumer
// goroutine winning the race against Enqueue) keeps the tests deterministic.
// A BUSYGROUP result is fine — another caller already created the group.
func ensureGroup(t *testing.T, c *redis.Client, stream, group string) {
	t.Helper()
	err := c.XGroupCreateMkStream(context.Background(), stream, group, "$").Err()
	if err != nil && !isBusyGroup(err) {
		require.NoError(t, err)
	}
}

// waitXLen asserts the stream length reaches want within 3s.
func waitXLen(t *testing.T, c *redis.Client, stream string, want int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		n, err := c.XLen(context.Background(), stream).Result()
		require.NoError(t, err)
		return n == want
	}, 3*time.Second, 5*time.Millisecond, "XLen(%s) never reached %d", stream, want)
}

// TestProducer_NilSafe: a Producer built over a nil client (dev: no Redis) must
// Enqueue as a clean no-op — it logs and returns nil instead of panicking or
// blocking the request flow.
func TestProducer_NilSafe(t *testing.T) {
	p := NewProducer(nil, config.JobQueueConfig{Stream: "s", MaxLen: 10})
	require.NoError(t, p.Enqueue(context.Background(), Job{Type: JobReachDueSend, TenantID: 1}))

	// A typed-nil *Producer must also be safe.
	var np *Producer
	require.NoError(t, np.Enqueue(context.Background(), Job{Type: JobReachDueSend}))
}

// TestConsumer_NilSafe: a Consumer built over a nil client must Run as a clean
// no-op — it logs and returns nil instead of starting a read loop that would
// nil-panic.
func TestConsumer_NilSafe(t *testing.T) {
	c := NewConsumer(nil, config.JobQueueConfig{Stream: "s", Group: "g", Consumer: "c"}, nil)
	require.NoError(t, c.Run(context.Background()))

	var nc *Consumer
	require.NoError(t, nc.Run(context.Background()))
}

// TestJobQueue_EnqueueConsumeAck: the happy path. Enqueue one job, the consumer
// delivers it to the handler with all fields intact, and the entry leaves the
// stream (XAck + XDel) so XLen returns to zero.
func TestJobQueue_EnqueueConsumeAck(t *testing.T) {
	c, _ := newMiniredis(t)
	ctx := context.Background()

	cfg := config.JobQueueConfig{
		Stream:   "happy-jobs",
		Group:    "happy-grp",
		Consumer: "c1",
		Block:    50 * time.Millisecond, // short block so the loop spins fast in tests
		MaxLen:   1000,
	}
	prod := NewProducer(c, cfg)

	got := make(chan Job, 1)
	h := HandlerFunc(func(_ context.Context, j Job) error {
		got <- j
		return nil
	})
	cons := NewConsumer(c, cfg, h, WithCount(2))

	want := Job{
		Type:     JobReachDueSend,
		Payload:  []byte(`{"msg":"hello","n":42}`),
		TenantID: 7,
		RunAt:    time.Unix(0, 1700000000_000000000),
	}
	// group must exist before the message is added ("$" start captures only
	// messages added after group creation).
	ensureGroup(t, c, cfg.Stream, cfg.Group)
	require.NoError(t, prod.Enqueue(ctx, want))
	cancel := startConsumer(cons)
	defer cancel()

	select {
	case j := <-got:
		require.Equal(t, want.Type, j.Type)
		require.True(t, bytes.Equal(want.Payload, j.Payload), "payload round-trip mismatch")
		require.Equal(t, want.TenantID, j.TenantID)
		require.True(t, want.RunAt.Equal(j.RunAt), "run_at round-trip mismatch: got %v want %v", j.RunAt, want.RunAt)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: handler never received the job")
	}

	// After a successful handle the entry is ACKed + XDel'ed: stream drains.
	waitXLen(t, c, "happy-jobs", 0)
}

// TestJobQueue_MultiConsumerDistribution: two consumers in the same group with
// distinct consumer names split the workload. Every message is delivered to
// exactly one consumer (no duplicates) and both consumers participate.
func TestJobQueue_MultiConsumerDistribution(t *testing.T) {
	c, _ := newMiniredis(t)
	ctx := context.Background()

	const (
		stream = "mc-jobs"
		group  = "mc-grp"
		N      = 6
	)
	prod := NewProducer(c, config.JobQueueConfig{Stream: stream, MaxLen: 1000})
	// group must exist before messages are added so the "$" start captures them.
	ensureGroup(t, c, stream, group)
	for i := 0; i < N; i++ {
		require.NoError(t, prod.Enqueue(ctx, Job{
			Type:     JobPregenIncremental,
			TenantID: int64(i + 1),
			Payload:  []byte(strconv.Itoa(i)), // unique payload identifies each msg
		}))
	}

	var processed int64
	var c1mu, c2mu sync.Mutex
	var c1, c2 []string

	mkHandler := func(mu *sync.Mutex, out *[]string) Handler {
		return HandlerFunc(func(_ context.Context, j Job) error {
			atomic.AddInt64(&processed, 1)
			mu.Lock()
			*out = append(*out, string(j.Payload))
			mu.Unlock()
			// a short sleep yields the goroutine so the sibling consumer
			// participates instead of being starved by a faster loop.
			time.Sleep(8 * time.Millisecond)
			return nil
		})
	}

	base := config.JobQueueConfig{Stream: stream, Group: group, Block: 50 * time.Millisecond, MaxLen: 1000}
	cons1 := NewConsumer(c, setConsumer(base, "w1"), mkHandler(&c1mu, &c1), WithCount(1))
	cons2 := NewConsumer(c, setConsumer(base, "w2"), mkHandler(&c2mu, &c2), WithCount(1))
	cancel1 := startConsumer(cons1)
	cancel2 := startConsumer(cons2)
	defer cancel1()
	defer cancel2()

	// wait until all N messages are processed.
	require.Eventually(t, func() bool {
		return atomic.LoadInt64(&processed) == N
	}, 3*time.Second, 5*time.Millisecond, "not all %d messages were processed", N)

	// stop both consumers before reading the results to guarantee they can no
	// longer write while we read.
	cancel1()
	cancel2()

	c1mu.Lock()
	p1 := append([]string{}, c1...)
	c1mu.Unlock()
	c2mu.Lock()
	p2 := append([]string{}, c2...)
	c2mu.Unlock()

	// each handled a portion.
	require.NotEmpty(t, p1, "consumer w1 processed nothing")
	require.NotEmpty(t, p2, "consumer w2 processed nothing")
	// total accounts for every enqueued message exactly once.
	require.Equal(t, N, len(p1)+len(p2), "total processed != N")

	// no message was delivered twice (consumer-group guarantee).
	seen := make(map[string]int, N)
	for _, p := range append(append([]string{}, p1...), p2...) {
		seen[p]++
	}
	require.Len(t, seen, N, "duplicate or missing payloads across consumers")
	for p, n := range seen {
		require.Equalf(t, 1, n, "payload %q delivered %d times", p, n)
	}
}

// TestJobQueue_Redelivery: when Handle returns an error the consumer must NOT
// ACK; the entry stays pending and is redelivered on the next loop iteration's
// "0" read, where the handler now succeeds and the entry is finally ACKed.
func TestJobQueue_Redelivery(t *testing.T) {
	c, _ := newMiniredis(t)
	ctx := context.Background()

	cfg := config.JobQueueConfig{
		Stream:   "rd-jobs",
		Group:    "rd-grp",
		Consumer: "c1",
		Block:    50 * time.Millisecond,
		MaxLen:   1000,
	}
	prod := NewProducer(c, cfg)

	var calls int32
	succeeded := make(chan struct{}, 1)
	h := HandlerFunc(func(_ context.Context, j Job) error {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			// first delivery: simulate a transient failure → must not be ACKed.
			return errors.New("simulated transient failure")
		default:
			succeeded <- struct{}{}
			return nil
		}
	})
	cons := NewConsumer(c, cfg, h, WithCount(2))

	ensureGroup(t, c, cfg.Stream, cfg.Group)
	require.NoError(t, prod.Enqueue(ctx, Job{Type: JobReachDueSend, TenantID: 99, Payload: []byte("xyz")}))
	cancel := startConsumer(cons)
	defer cancel()

	select {
	case <-succeeded:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: redelivered job never succeeded")
	}

	// first attempt failed, second attempt succeeded → at least 2 Handle calls.
	require.GreaterOrEqualf(t, atomic.LoadInt32(&calls), int32(2), "expected >=2 handle calls (1 fail + 1 success)")
	// entry was finally ACKed + XDel'ed.
	waitXLen(t, c, "rd-jobs", 0)
}

// TestParseJob: table-driven decode of Stream entries.
func TestParseJob(t *testing.T) {
	tests := []struct {
		name    string
		msg     redis.XMessage
		wantJob Job
		wantErr bool
	}{
		{
			name: "full entry decodes",
			msg: redis.XMessage{ID: "1-0", Values: map[string]interface{}{
				fieldType:    JobReachDueSend,
				fieldPayload: `{"x":1}`,
				fieldTenant:  "42",
				fieldRunAt:   strconv.FormatInt(time.Unix(0, 123).UnixNano(), 10),
			}},
			wantJob: Job{
				Type:     JobReachDueSend,
				Payload:  []byte(`{"x":1}`),
				TenantID: 42,
				RunAt:    time.Unix(0, 123),
			},
		},
		{
			name: "missing type is an error",
			msg: redis.XMessage{ID: "2-0", Values: map[string]interface{}{
				fieldPayload: "p", fieldTenant: "1",
			}},
			wantErr: true,
		},
		{
			name:    "empty values is an error",
			msg:     redis.XMessage{ID: "3-0", Values: map[string]interface{}{}},
			wantErr: true,
		},
		{
			name: "non-integer tenant tolerated (zero)",
			msg: redis.XMessage{ID: "4-0", Values: map[string]interface{}{
				fieldType: JobPregenIncremental, fieldTenant: "not-a-number",
			}},
			wantJob: Job{Type: JobPregenIncremental},
		},
		{
			name: "extra unknown fields ignored",
			msg: redis.XMessage{ID: "5-0", Values: map[string]interface{}{
				fieldType: JobPregenIncremental, "noise": "ignored",
			}},
			wantJob: Job{Type: JobPregenIncremental},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseJob(tt.msg)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantJob.Type, got.Type)
			require.True(t, bytes.Equal(tt.wantJob.Payload, got.Payload), "payload mismatch")
			require.Equal(t, tt.wantJob.TenantID, got.TenantID)
			require.True(t, tt.wantJob.RunAt.Equal(got.RunAt), "run_at mismatch: got %v want %v", got.RunAt, tt.wantJob.RunAt)
		})
	}
}

// TestIsBusyGroup: the BUSYGROUP guard must match both real-Redis and miniredis
// wording so XGroupCreateMkStream is idempotent across worker restarts.
func TestIsBusyGroup(t *testing.T) {
	require.True(t, isBusyGroup(errors.New("BUSYGROUP Consumer Group name already exists")))
	require.True(t, isBusyGroup(errors.New("busygroup something")))
	require.False(t, isBusyGroup(errors.New("NOGROUP no such key")))
	require.False(t, isBusyGroup(nil))
}

// setConsumer returns a shallow copy of base with Consumer overridden. Keeps the
// distribution test DRY when building two sibling configs.
func setConsumer(base config.JobQueueConfig, consumer string) config.JobQueueConfig {
	out := base
	out.Consumer = consumer
	return out
}

// TestConsumer_GroupStartZeroReadsPreGroupMessages guards fix (2): the consumer
// group is created at "0" (not "$"), so a message XADDed BEFORE the group exists
// — e.g. an api producer enqueued while no worker was up — is still delivered.
// With the old "$" start this message would be silently dropped forever.
func TestConsumer_GroupStartZeroReadsPreGroupMessages(t *testing.T) {
	c, _ := newMiniredis(t)
	ctx := context.Background()

	cfg := config.JobQueueConfig{
		Stream:   "pre-jobs",
		Group:    "pre-grp",
		Consumer: "c1",
		Block:    20 * time.Millisecond,
		MaxLen:   1000,
	}
	// Enqueue BEFORE any group exists (the stream is created by this XADD).
	prod := NewProducer(c, cfg)
	require.NoError(t, prod.Enqueue(ctx, Job{
		Type:     JobReachDueSend,
		TenantID: 3,
		Payload:  []byte("enqueued-before-group"),
	}))
	// Precondition: no group yet, message sitting on the stream.
	n, err := c.XLen(ctx, cfg.Stream).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	got := make(chan Job, 1)
	cons := NewConsumer(c, cfg, HandlerFunc(func(_ context.Context, j Job) error {
		got <- j
		return nil
	}), WithCount(1))
	cancel := startConsumer(cons) // Run creates the group at "0"
	defer cancel()

	select {
	case j := <-got:
		require.Equal(t, "enqueued-before-group", string(j.Payload))
		require.Equal(t, int64(3), j.TenantID)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: pre-group message was dropped (group start not \"0\"?)")
	}
	waitXLen(t, c, cfg.Stream, 0)
}

// TestConsumer_ReclaimsOrphanPending guards fix (1): a message stranded in a
// DEAD consumer's pending list (read via XREADGROUP but never ACKed — e.g. the
// consumer crashed mid-Handle) is re-homed onto a surviving sibling via
// XAUTOCLAIM and delivered. No same-named restart is required.
func TestConsumer_ReclaimsOrphanPending(t *testing.T) {
	c, _ := newMiniredis(t)
	ctx := context.Background()

	const (
		stream = "orphan-jobs"
		group  = "orphan-grp"
	)
	cfg := config.JobQueueConfig{Stream: stream, Group: group, Consumer: "survivor", Block: 20 * time.Millisecond, MaxLen: 1000}

	// Group at "0" (as Run would), then enqueue one job.
	require.NoError(t, c.XGroupCreateMkStream(ctx, stream, group, "0").Err())
	prod := NewProducer(c, cfg)
	require.NoError(t, prod.Enqueue(ctx, Job{Type: JobReachDueSend, TenantID: 5, Payload: []byte("orphan")}))

	// Simulate a consumer "dead" that read the message (XREADGROUP ">") but
	// crashed before ACKing: the entry is now pending under "dead" and no live
	// consumer's "0" read will ever touch it.
	res, err := c.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: group, Consumer: "dead", Streams: []string{stream, ">"}, Count: 1,
	}).Result()
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Len(t, res[0].Messages, 1, "precondition: dead consumer read the message")

	// A surviving consumer with a tiny MinIdle + ClaimInterval must reclaim the
	// orphan via XAUTOCLAIM and deliver it through the normal handler path.
	got := make(chan Job, 1)
	cons := NewConsumer(c, cfg, HandlerFunc(func(_ context.Context, j Job) error {
		got <- j
		return nil
	}), WithCount(1), WithMinIdle(1*time.Millisecond), WithClaimInterval(5*time.Millisecond))
	cancel := startConsumer(cons)
	defer cancel()

	select {
	case j := <-got:
		require.Equal(t, "orphan", string(j.Payload))
		require.Equal(t, int64(5), j.TenantID)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: orphan pending entry was never reclaimed (no XAUTOCLAIM?)")
	}
	waitXLen(t, c, stream, 0)
}

// TestConsumer_PoisonGoesToDLQ guards the poison-pill ceiling of fix (1): an
// entry whose delivery count exceeds MaxDeliveries is parked on the
// <stream>:dlq dead-letter stream and ACKed away instead of being reclaimed /
// redelivered forever. We drive the count up with a handler that always fails;
// the blocking ">" read creates idle windows that let the XAUTOCLAIM sweep
// reclaim the entry, observe its delivery count over the ceiling, and DLQ it.
func TestConsumer_PoisonGoesToDLQ(t *testing.T) {
	c, _ := newMiniredis(t)
	ctx := context.Background()

	cfg := config.JobQueueConfig{
		Stream:   "poison-jobs",
		Group:    "poison-grp",
		Consumer: "c1",
		Block:    20 * time.Millisecond,
		MaxLen:   1000,
	}
	prod := NewProducer(c, cfg)
	ensureGroup(t, c, cfg.Stream, cfg.Group)
	require.NoError(t, prod.Enqueue(ctx, Job{Type: JobReachDueSend, TenantID: 6, Payload: []byte("poison")}))

	var calls int64
	cons := NewConsumer(c, cfg, HandlerFunc(func(_ context.Context, j Job) error {
		atomic.AddInt64(&calls, 1)
		return errors.New("always fails — poison")
	}),
		WithCount(1),
		WithMinIdle(1*time.Millisecond),       // reclaim during the ">" block idle window
		WithClaimInterval(1*time.Millisecond), // sweep every loop
		WithMaxDeliveries(2),                  // low ceiling so the test is fast
	)
	cancel := startConsumer(cons)
	defer cancel()

	dlq := dlqStreamName(cfg.Stream)
	// The poison entry must land on the DLQ stream...
	require.Eventually(t, func() bool {
		n, err := c.XLen(ctx, dlq).Result()
		require.NoError(t, err)
		return n == 1
	}, 3*time.Second, 5*time.Millisecond, "poison entry never reached the DLQ")

	// ...and leave the live stream (ACKed + XDel'ed) so it stops blocking the PEL.
	waitXLen(t, c, cfg.Stream, 0)

	// The DLQ entry preserves the raw fields + the original stream id.
	msgs, err := c.XRange(ctx, dlq, "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, JobReachDueSend, msgs[0].Values[fieldType])
	require.Equal(t, "poison", msgs[0].Values[fieldPayload])
	require.NotEmpty(t, msgs[0].Values["orig_id"], "dlq entry must carry the original stream id")
}

// TestConsumer_NoGroupRecovery guards fix (4): if the consumer group is destroyed
// at runtime (operator XGROUP DESTROY, or the stream trimmed away), the running
// consumer must NOT just log + back off forever — it rebuilds the group at "0"
// and keeps draining. We destroy the group mid-run, enqueue a fresh message, and
// assert it is still delivered.
func TestConsumer_NoGroupRecovery(t *testing.T) {
	c, _ := newMiniredis(t)
	ctx := context.Background()

	cfg := config.JobQueueConfig{
		Stream:   "nogroup-jobs",
		Group:    "nogroup-grp",
		Consumer: "c1",
		Block:    20 * time.Millisecond,
		MaxLen:   1000,
	}
	prod := NewProducer(c, cfg)

	delivered := make(chan string, 4)
	cons := NewConsumer(c, cfg, HandlerFunc(func(_ context.Context, j Job) error {
		delivered <- string(j.Payload)
		return nil
	}), WithCount(1))
	cancel := startConsumer(cons) // creates the group at "0"
	defer cancel()

	// First message proves the consumer is up and draining.
	require.NoError(t, prod.Enqueue(ctx, Job{Type: JobReachDueSend, TenantID: 1, Payload: []byte("first")}))
	select {
	case p := <-delivered:
		require.Equal(t, "first", p)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: first message not delivered")
	}

	// Destroy the group out from under the running consumer.
	require.NoError(t, c.XGroupDestroy(ctx, cfg.Stream, cfg.Group).Err())

	// Enqueue a second message. The consumer's next read hits NOGROUP, rebuilds
	// the group at "0", and delivers it — it must not wedge on log+backoff.
	require.NoError(t, prod.Enqueue(ctx, Job{Type: JobReachDueSend, TenantID: 1, Payload: []byte("second")}))
	select {
	case p := <-delivered:
		require.Equal(t, "second", p)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: consumer did not recover from NOGROUP (second message lost)")
	}
}

// TestIsNoGroup: the NOGROUP guard must match both real-Redis and miniredis
// wording ("NOGROUP No such consumer group '...' for key name '...'") so the
// runtime-recovery branch triggers, while BUSYGROUP / nil do not.
func TestIsNoGroup(t *testing.T) {
	require.True(t, isNoGroup(errors.New("NOGROUP No such consumer group 'g' for key name 's'")))
	require.True(t, isNoGroup(errors.New("nogroup something")))
	require.False(t, isNoGroup(errors.New("BUSYGROUP Consumer Group name already exists")))
	require.False(t, isNoGroup(nil))
}
