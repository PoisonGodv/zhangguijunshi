package jobqueue

import (
	"bytes"
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// startPump runs pump.Run in a goroutine under a fresh cancellable ctx and
// returns the cancel func (mirrors startConsumer for the pump side).
func startPump(p *DelayPump) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = p.Run(ctx) }()
	return cancel
}

// zsetMembers returns all members of zset (ZRANGE 0 -1) for assertions.
func zsetMembers(t *testing.T, c *redis.Client, zset string) []string {
	t.Helper()
	ms, err := c.ZRange(context.Background(), zset, 0, -1).Result()
	require.NoError(t, err)
	return ms
}

// zsetLen returns the cardinality of zset.
func zsetLen(t *testing.T, c *redis.Client, zset string) int64 {
	t.Helper()
	n, err := c.ZCard(context.Background(), zset).Result()
	require.NoError(t, err)
	return n
}

// TestProducer_ImmediatePathUnchanged: zero / past RunAt MUST take the existing
// immediate XADD path — the job lands in the live Stream right away (XLen 1) and
// the delay ZSET stays empty. Guards against the delay branch accidentally
// capturing the non-delayed cases.
func TestProducer_ImmediatePathUnchanged(t *testing.T) {
	c, _ := newMiniredis(t)
	ctx := context.Background()

	t.Run("zero RunAt", func(t *testing.T) {
		stream := "imm-zero"
		prod := NewProducer(c, config.JobQueueConfig{Stream: stream, MaxLen: 1000})
		require.NoError(t, prod.Enqueue(ctx, Job{Type: JobPregenIncremental, TenantID: 3}))
		n, err := c.XLen(ctx, stream).Result()
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "zero RunAt must XADD immediately")
		require.Equal(t, int64(0), zsetLen(t, c, delayZsetName(stream)), "nothing in the delay zset")
	})

	t.Run("past RunAt", func(t *testing.T) {
		stream := "imm-past"
		prod := NewProducer(c, config.JobQueueConfig{Stream: stream, MaxLen: 1000})
		require.NoError(t, prod.Enqueue(ctx, Job{
			Type:     JobReachDueSend,
			TenantID: 4,
			RunAt:    time.Now().Add(-1 * time.Minute), // 过去时间 → 立即投递
		}))
		n, err := c.XLen(ctx, stream).Result()
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "past RunAt must XADD immediately")
		require.Equal(t, int64(0), zsetLen(t, c, delayZsetName(stream)), "nothing in the delay zset")
	})
}

// TestProducer_DelayPathParksInZSet: a future RunAt MUST route into the delay
// ZSET (member present, scored by RunAt.UnixNano) and NOT yet appear on the live
// Stream. This is the core gap-closure behavior (CONTRACT §20.3).
func TestProducer_DelayPathParksInZSet(t *testing.T) {
	c, _ := newMiniredis(t)
	ctx := context.Background()

	stream := "dl-jobs"
	prod := NewProducer(c, config.JobQueueConfig{Stream: stream, MaxLen: 1000})
	runAt := time.Now().Add(time.Hour).UTC() // 远未来：触发 Producer 的延迟分支（Producer 用真实 time.Now 判定）
	require.NoError(t, prod.Enqueue(ctx, Job{
		Type:     JobReachDueSend,
		Payload:  []byte(`{"task_id":42}`),
		TenantID: 9,
		RunAt:    runAt,
	}))

	// NOT on the live stream yet.
	n, err := c.XLen(ctx, stream).Result()
	require.NoError(t, err)
	require.Equal(t, int64(0), n, "future RunAt must not XADD to the live stream")

	// Parked in the delay ZSET with the correct score (RunAt.UnixNano).
	zset := delayZsetName(stream)
	ms := zsetMembers(t, c, zset)
	require.Len(t, ms, 1, "exactly one delayed member")
	score, err := c.ZScore(ctx, zset, ms[0]).Result()
	require.NoError(t, err)
	require.Equal(t, float64(runAt.UnixNano()), score, "zset score must equal RunAt.UnixNano")

	// The member decodes back to the four canonical fields (nonce is extra).
	fields, err := decodeDelayMember(ms[0])
	require.NoError(t, err)
	require.Equal(t, JobReachDueSend, fields[fieldType])
	require.Equal(t, `{"task_id":42}`, fields[fieldPayload])
	require.Equal(t, "9", fields[fieldTenant])
	require.Equal(t, strconv.FormatInt(runAt.UnixNano(), 10), fields[fieldRunAt])
	require.NotEmpty(t, fields[fieldNonce], "nonce must be present for uniqueness")
}

// TestDelayPump_DeliversOnTime: with the future job parked and WithPumpNow
// returning a time >= RunAt, the pump must move it onto the live Stream so the
// consumer delivers it — with Type/TenantID/Payload/RunAt round-tripped intact —
// and the member leaves the ZSET. Deterministic: NO real sleeping on the due
// path (the clock is injected).
func TestDelayPump_DeliversOnTime(t *testing.T) {
	c, _ := newMiniredis(t)
	ctx := context.Background()

	cfg := config.JobQueueConfig{
		Stream:   "due-jobs",
		Group:    "due-grp",
		Consumer: "c1",
		Block:    50 * time.Millisecond,
		MaxLen:   1000,
	}
	zset := delayZsetName(cfg.Stream)
	runAt := time.Now().Add(time.Hour).UTC() // 远未来：触发 Producer 的延迟分支（Producer 用真实 time.Now 判定）

	prod := NewProducer(c, cfg)
	want := Job{
		Type:     JobReachDueSend,
		Payload:  []byte(`{"msg":"hi","n":7}`),
		TenantID: 77,
		RunAt:    runAt,
	}
	// group must exist before the pump XADDs so the "$" start captures the message.
	ensureGroup(t, c, cfg.Stream, cfg.Group)
	require.NoError(t, prod.Enqueue(ctx, want))
	require.Len(t, zsetMembers(t, c, zset), 1, "precondition: parked in zset")

	got := make(chan Job, 1)
	cons := NewConsumer(c, cfg, HandlerFunc(func(_ context.Context, j Job) error {
		got <- j
		return nil
	}), WithCount(2))
	consCancel := startConsumer(cons)
	defer consCancel()

	// Inject a clock already past RunAt → the member is "due" on the first sweep.
	pump := NewDelayPump(c, cfg,
		WithPumpNow(func() time.Time { return runAt.Add(1 * time.Second) }),
		WithPumpInterval(10*time.Millisecond),
	)
	pumpCancel := startPump(pump)
	defer pumpCancel()

	select {
	case j := <-got:
		require.Equal(t, want.Type, j.Type)
		require.True(t, bytes.Equal(want.Payload, j.Payload), "payload round-trip mismatch")
		require.Equal(t, want.TenantID, j.TenantID)
		require.True(t, want.RunAt.Equal(j.RunAt), "run_at round-trip mismatch: got %v want %v", j.RunAt, want.RunAt)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: pump never delivered the due job to the consumer")
	}

	// Member has left the ZSET; entry ACKed + XDel'ed so the live stream drains.
	require.Eventually(t, func() bool { return zsetLen(t, c, zset) == 0 },
		2*time.Second, 5*time.Millisecond, "zset not drained after delivery")
	waitXLen(t, c, cfg.Stream, 0)
}

// TestDelayPump_DoesNotDeliverEarly: with WithPumpNow returning a time BEFORE
// RunAt, the job must stay parked in the ZSET and the live Stream must stay
// empty across several pump ticks. Proves RunAt is a REAL delivery time, not a
// hint.
func TestDelayPump_DoesNotDeliverEarly(t *testing.T) {
	c, _ := newMiniredis(t)
	ctx := context.Background()

	cfg := config.JobQueueConfig{Stream: "early-jobs", MaxLen: 1000}
	zset := delayZsetName(cfg.Stream)
	runAt := time.Now().Add(time.Hour).UTC() // 远未来：触发 Producer 的延迟分支（Producer 用真实 time.Now 判定）

	prod := NewProducer(c, cfg)
	require.NoError(t, prod.Enqueue(ctx, Job{
		Type:     JobReachDueSend,
		TenantID: 5,
		RunAt:    runAt,
	}))
	require.Len(t, zsetMembers(t, c, zset), 1, "precondition: parked in zset")

	// Clock frozen BEFORE RunAt → member is never due.
	pump := NewDelayPump(c, cfg,
		WithPumpNow(func() time.Time { return runAt.Add(-1 * time.Second) }),
		WithPumpInterval(5*time.Millisecond), // fast ticks
	)
	cancel := startPump(pump)
	defer cancel()

	// Let the pump sweep several times (5ms tick → ~40 sweeps in 200ms).
	time.Sleep(200 * time.Millisecond)

	require.Len(t, zsetMembers(t, c, zset), 1, "job must remain parked (not delivered early)")
	n, err := c.XLen(ctx, cfg.Stream).Result()
	require.NoError(t, err)
	require.Equal(t, int64(0), n, "live stream must stay empty before RunAt")
}

// TestDelayPump_NilSafe: a DelayPump over a nil client (dev: no Redis) must Run
// as a clean no-op — logs and returns nil without looping. Symmetrically a nil
// Producer must make a delayed Enqueue a no-op.
func TestDelayPump_NilSafe(t *testing.T) {
	// nil client → Run returns nil without looping (would otherwise block on a
	// nil Cmdable and panic / hang).
	p := NewDelayPump(nil, config.JobQueueConfig{Stream: "s"})
	require.NoError(t, p.Run(context.Background()))

	var np *DelayPump
	require.NoError(t, np.Run(context.Background()))

	// nil producer → delayed Enqueue is a no-op (does not panic, does not block).
	prod := NewProducer(nil, config.JobQueueConfig{Stream: "s", MaxLen: 10})
	require.NoError(t, prod.Enqueue(context.Background(), Job{
		Type:     JobReachDueSend,
		TenantID: 1,
		RunAt:    time.Now().Add(1 * time.Hour), // future → would hit delay branch if not nil
	}))
}

// TestDelayPump_ConcurrentExactlyOnce: two DelayPumps over one Redis (sibling
// worker replicas) racing on ONE due job must deliver it EXACTLY ONCE. The
// ZREM-claim is the ownership boundary: only the pump whose ZREM returns 1
// publishes; the loser's ZREM returns 0 and it skips. Asserted via a handler
// counter that must end at 1 even after both pumps have swept many times.
func TestDelayPump_ConcurrentExactlyOnce(t *testing.T) {
	c, _ := newMiniredis(t)
	ctx := context.Background()

	cfg := config.JobQueueConfig{
		Stream:   "conc-jobs",
		Group:    "conc-grp",
		Consumer: "c1",
		Block:    20 * time.Millisecond,
		MaxLen:   1000,
	}
	zset := delayZsetName(cfg.Stream)
	runAt := time.Now().Add(time.Hour).UTC() // 远未来：触发 Producer 的延迟分支（Producer 用真实 time.Now 判定）

	prod := NewProducer(c, cfg)
	ensureGroup(t, c, cfg.Stream, cfg.Group)
	require.NoError(t, prod.Enqueue(ctx, Job{
		Type:     JobReachDueSend,
		Payload:  []byte("race"),
		TenantID: 11,
		RunAt:    runAt,
	}))

	var delivered int64
	cons := NewConsumer(c, cfg, HandlerFunc(func(_ context.Context, j Job) error {
		atomic.AddInt64(&delivered, 1)
		return nil
	}), WithCount(1))
	consCancel := startConsumer(cons)
	defer consCancel()

	// Both pumps see "now >= RunAt" → both list the member, but ZREM-claim grants
	// ownership to exactly one publisher per member.
	nowFn := func() time.Time { return runAt.Add(1 * time.Second) }
	p1 := NewDelayPump(c, cfg, WithPumpNow(nowFn), WithPumpInterval(5*time.Millisecond), WithPumpBatch(8))
	p2 := NewDelayPump(c, cfg, WithPumpNow(nowFn), WithPumpInterval(5*time.Millisecond), WithPumpBatch(8))
	c1 := startPump(p1)
	c2 := startPump(p2)
	defer c1()
	defer c2()

	// Wait for first delivery.
	require.Eventually(t, func() bool { return atomic.LoadInt64(&delivered) >= 1 },
		2*time.Second, 2*time.Millisecond, "job was never delivered")

	// Let BOTH pumps keep sweeping so a missing claim guard would surface as a
	// second delivery. With ZREM-claim the counter stays at 1.
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, int64(1), atomic.LoadInt64(&delivered),
		"due job delivered more than once across concurrent pumps (ZREM-claim broken)")
	require.Equal(t, int64(0), zsetLen(t, c, zset), "zset must be drained after exactly-once delivery")
}

// TestNewNonce_Unique: nonces must be unique across many calls (guards the ZSET
// member-uniqueness invariant that prevents duplicate-job collapse).
func TestNewNonce_Unique(t *testing.T) {
	const n = 1024
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		s := newNonce()
		require.NotEmpty(t, s)
		_, dup := seen[s]
		require.False(t, dup, "nonce collision after %d draws: %s", i, s)
		seen[s] = struct{}{}
	}
}

// TestEncodeDecodeDelayMember_RoundTrip: the four canonical fields survive an
// encode/decode cycle, including a payload that contains newlines and "="
// (which would break a naive "k=v\n" encoding).
func TestEncodeDecodeDelayMember_RoundTrip(t *testing.T) {
	in := map[string]string{
		fieldType:    JobReachDueSend,
		fieldPayload: "line1\nline2\t=x=y\r\nz", // hostile content for k=v encodings
		fieldTenant:  "123",
		fieldRunAt:   strconv.FormatInt(time.Unix(0, 999).UnixNano(), 10),
	}
	member, err := encodeDelayMember(in)
	require.NoError(t, err)

	out, err := decodeDelayMember(member)
	require.NoError(t, err)
	require.Equal(t, in[fieldType], out[fieldType])
	require.Equal(t, in[fieldPayload], out[fieldPayload])
	require.Equal(t, in[fieldTenant], out[fieldTenant])
	require.Equal(t, in[fieldRunAt], out[fieldRunAt])
	require.NotEmpty(t, out[fieldNonce])
	_, ok := out[fieldNonce]
	require.True(t, ok, "nonce key present")
}
