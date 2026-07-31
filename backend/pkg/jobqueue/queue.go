// Package jobqueue implements a lightweight, nil-safe job queue on top of a
// Redis Stream (CONTRACT §19). It is the shared backbone between the api
// process's in-process producers (reach scheduled-send / pregen incremental)
// and the worker binary's consumers.
//
// Delayed delivery (CONTRACT §20.3 gap closure): a Job whose RunAt is in the
// future is NOT XADDed to the live Stream immediately — that would make a
// "scheduled" message fire the instant the worker picks it up. Instead the
// Producer ZADDs it into a ZSET delay lane (<stream>:delay) scored by
// RunAt.UnixNano(), and the worker's DelayPump loop ZREM-claims each due member
// and XADDs it onto the live Stream only once RunAt arrives. The consumer's
// parseJob is unchanged because the pump replays the SAME four stream fields the
// immediate path writes. Zero/past RunAt still takes the immediate XADD path, so
// existing callers and tests are unaffected.
//
// Both Producer and Consumer depend only on redis.Cmdable so that:
//   - production wires a real *redis.Client (from pkg/redisx);
//   - tests wire an in-process miniredis client.
//
// Nil-safe degradation (CONTRACT §0 "tolerate a missing dependency"): when the
// caller hands a nil Cmdable — i.e. local dev has no Redis — the Producer logs
// a warning and returns nil (enqueue becomes a no-op so business flows are not
// blocked), and the Consumer logs a warning and returns nil without starting
// the read loop (there is simply nothing to consume). This mirrors the
// redisx.Client / memory-fallback philosophy used across pkg/jwt, gateway and
// pregen.
package jobqueue

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// Stream field names. Every XADD entry is a flat map of these four string
// fields so that the consumer can decode a Job without any binary framing.
const (
	fieldType    = "type"      // Job.Type
	fieldPayload = "payload"   // Job.Payload (stored as a raw string)
	fieldTenant  = "tenant_id" // Job.TenantID (strconv.FormatInt, base 10)
	fieldRunAt   = "run_at"    // Job.RunAt (strconv.FormatInt of UnixNano)
)

// Defaults applied when the corresponding JobQueueConfig field is empty / zero.
// They keep the queue usable from tests and from a config that omits the
// job_queue block; production overrides them via configs/<env>.yaml.
const (
	defaultStream   = "zg:jobs"
	defaultGroup    = "zg-workers"
	defaultConsumer = "worker-0"
	defaultBlock    = 5 * time.Second
	defaultMaxLen   = 10000

	// readCount is the XREADGROUP COUNT: how many entries one read drains.
	defaultReadCount = 10

	// errBackoff caps the spin rate when XREADGROUP returns a real error
	// (e.g. Redis flapped). The idle path already paces itself via Block on the
	// ">" read, so this only guards the error path.
	errBackoff = 200 * time.Millisecond
)

// Job is one unit of asynchronous work carried over the Stream. Payload is an
// opaque caller-defined blob (typically JSON); the queue never inspects it.
type Job struct {
	Type     string    // routing key, must match a Job* constant
	Payload  []byte    // opaque, round-tripped as a string field
	TenantID int64     // multi-tenant scoping for the consumer handler
	RunAt    time.Time // earliest delivery time; future & non-zero routes into the ZSET delay lane (CONTRACT §20.3)
}

// Job type constants. Producers and consumers agree on these strings; adding a
// new job type means adding a constant here and a branch in the worker's
// dispatch Handler.
const (
	// JobPregenIncremental is the daytime incremental re-compute of the AI
	// engine's "today's 3 things" (CONTRACT §13.3.5 / pregen incremental).
	JobPregenIncremental = "pregen_incremental"
	// JobReachDueSend delivers a scheduled reach message whose send window has
	// opened (CONTRACT §11 reach closed-loop).
	JobReachDueSend = "reach_due_send"
)

// Handler processes a single Job inside the Consumer read loop. The returned
// error decides ACK vs. redelivery (see Consumer.Run): nil → ACK + XDel so the
// entry leaves the Stream; non-nil → the entry is left in the consumer's
// pending list and redelivered on the next loop iteration. Handle must be
// idempotent because redelivery is at-least-once.
type Handler interface {
	Handle(ctx context.Context, job Job) error
}

// HandlerFunc lets a plain function satisfy Handler.
type HandlerFunc func(ctx context.Context, job Job) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, job Job) error { return f(ctx, job) }

// --------------------------- Producer ---------------------------

// Producer enqueues Jobs onto the Stream. It is safe for concurrent use: every
// call is a single independent XADD. The zero-value-ish Producer (client==nil)
// is a clean no-op, so the api process can hold one unconditionally and simply
// skip publishing when Redis is absent (dev).
type Producer struct {
	client redis.Cmdable
	stream string
	maxLen int64 // XADD MAXLEN ~ N trim cap; <=0 means no trimming
}

// NewProducer builds a Producer over client. cfg supplies the Stream name and
// the approximate trim cap (MaxLen); empty Stream / zero MaxLen fall back to
// defaults. Pass a nil client in dev (no Redis) and Enqueue becomes a no-op.
func NewProducer(client redis.Cmdable, cfg config.JobQueueConfig) *Producer {
	p := &Producer{client: client, stream: cfg.Stream, maxLen: cfg.MaxLen}
	if p.stream == "" {
		p.stream = defaultStream
	}
	if p.maxLen == 0 {
		p.maxLen = defaultMaxLen
	}
	return p
}

// Enqueue appends job to the Stream (XADD with approximate MAXLEN trimming).
// With a nil client it logs a warning and returns nil so dev (no Redis) never
// breaks a request flow on the queue being absent.
//
// Routing (CONTRACT §20.3):
//   - RunAt zero or in the past → immediate XADD (unchanged; fieldRunAt still
//     stamped for observability).
//   - RunAt in the future → ZADD into the <stream>:delay ZSET scored by
//     RunAt.UnixNano(); the worker's DelayPump later moves it onto the live
//     Stream once RunAt arrives. The ZSET member is the serialized field map
//     (see encodeDelayMember) so the pump can replay the exact same four fields
//     the immediate path writes — parseJob stays unchanged.
func (p *Producer) Enqueue(ctx context.Context, job Job) error {
	if p == nil || p.client == nil {
		// dev 无 Redis：记一条警告后干净退化为 no-op，不阻断业务。
		log.Printf("jobqueue: producer has nil redis, skip enqueue (type=%s tenant=%d)",
			job.Type, job.TenantID)
		return nil
	}

	// Build the canonical four stream fields once; both branches write the same
	// field set so the consumer's parseJob decodes either path identically.
	fields := map[string]string{
		fieldType:    job.Type,
		fieldPayload: string(job.Payload),
		fieldTenant:  strconv.FormatInt(job.TenantID, 10),
		fieldRunAt:   strconv.FormatInt(job.RunAt.UnixNano(), 10),
	}

	// 延迟投递：RunAt 在未来 → 进 ZSET 延迟队列，由 worker 端 DelayPump 到点搬运。
	if now := time.Now(); !job.RunAt.IsZero() && job.RunAt.After(now) {
		member, err := encodeDelayMember(fields)
		if err != nil {
			return fmt.Errorf("jobqueue: encode delay member: %w", err)
		}
		zset := p.delayZset()
		if err := p.client.ZAdd(ctx, zset, redis.Z{
			Score:  float64(job.RunAt.UnixNano()),
			Member: member,
		}).Err(); err != nil {
			return fmt.Errorf("jobqueue: zadd delay: %w", err)
		}
		log.Printf("jobqueue: delayed enqueue type=%s tenant=%d run_at=%s (zset=%s)",
			job.Type, job.TenantID, job.RunAt.UTC().Format(time.RFC3339Nano), zset)
		return nil
	}

	// 立即投递：XADD + MAXLEN ~ N 近似裁剪（原行为不变）。
	args := &redis.XAddArgs{
		Stream: p.stream,
		Approx: true, // MAXLEN ~ N : 近似裁剪，摊薄 XADD 延迟
		Values: map[string]interface{}{
			fieldType:    fields[fieldType],
			fieldPayload: fields[fieldPayload],
			fieldTenant:  fields[fieldTenant],
			fieldRunAt:   fields[fieldRunAt],
		},
	}
	if p.maxLen > 0 {
		args.MaxLen = p.maxLen
	}
	id, err := p.client.XAdd(ctx, args).Result()
	if err != nil {
		return fmt.Errorf("jobqueue: xadd: %w", err)
	}
	log.Printf("jobqueue: enqueued type=%s tenant=%d id=%s", job.Type, job.TenantID, id)
	return nil
}

// delayZset returns the ZSET key used as the delay lane. It is DERIVED from the
// Stream name so the producer (api process) and the DelayPump (worker process)
// agree on the same lane without a separate config field: <stream>:delay. With
// the default stream "zg:jobs" this is "zg:jobs:delay".
func (p *Producer) delayZset() string {
	return delayZsetName(p.stream)
}

// --------------------------- Consumer ---------------------------

// Consumer drains the Stream inside a consumer group. Run blocks until ctx is
// cancelled. Multiple Consumer instances sharing the same group but distinct
// Consumer names split the workload with no double-delivery (Redis consumer
// group semantics).
type Consumer struct {
	client  redis.Cmdable
	cfg     config.JobQueueConfig
	handler Handler
	count   int64 // XREADGROUP COUNT (overrides via WithCount)
}

// ConsumerOption configures a Consumer (functional-option style, matches the
// pregen Scheduler pattern). The documented NewConsumer(client, cfg, handler)
// call works unchanged — options are purely for tests / advanced tuning.
type ConsumerOption func(*Consumer)

// WithCount overrides the XREADGROUP COUNT (entries drained per read). The
// default defaultReadCount is fine for production; tests set it small (e.g. 1)
// to deterministically exercise multi-consumer distribution.
func WithCount(n int64) ConsumerOption {
	return func(c *Consumer) {
		if n > 0 {
			c.count = n
		}
	}
}

// NewConsumer builds a Consumer over client. cfg supplies Stream / Group /
// Consumer / Block (empty / zero fall back to defaults). handler is invoked per
// delivered entry; a nil handler is replaced by a no-op so a mis-wired
// consumer never nil-panics. Pass a nil client in dev and Run returns nil
// without starting the loop.
func NewConsumer(client redis.Cmdable, cfg config.JobQueueConfig, handler Handler, opts ...ConsumerOption) *Consumer {
	if handler == nil {
		handler = HandlerFunc(func(context.Context, Job) error { return nil })
	}
	c := &Consumer{
		client:  client,
		cfg:     cfg,
		handler: handler,
		count:   defaultReadCount,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Run creates the consumer group (MKSTREAM, idempotent) and then loops:
//  1. select on ctx.Done() (clean shutdown) — non-blocking pre-check; the real
//     pacing happens inside the blocking XREADGROUP ">" read.
//  2. XREADGROUP ... ID "0" — drain THIS consumer's pending entries first
//     (redeliver anything previously delivered but not yet ACK'd, e.g. after a
//     Handle failure or a crash). BLOCK is ignored by Redis for non-">" IDs so
//     this returns immediately when the pending list is empty.
//  3. XREADGROUP ... ID ">" — block up to cfg.Block for never-delivered entries.
//
// Per-entry ACK/redelivery policy (see process):
//   - Handle ok            → XAck + XDel (entry leaves the group PEL and the Stream).
//   - Handle error         → log + do NOT ack; entry stays pending and is
//     redelivered on the next iteration's "0" read.
//   - unparseable entry    → log + XAck + XDel (a corrupt entry can never be
//     handled, so we drop it rather than hot-loop forever;
//     this is a deliberate poison-pill guard, documented
//     here so it is not a surprise).
//
// Run returns nil when ctx is cancelled (graceful stop). Read errors are logged
// and retried after a short backoff (the worker is not crashed); only consumer
// group creation failure (non-BUSYGROUP) is returned as a fatal error.
func (c *Consumer) Run(ctx context.Context) error {
	if c == nil || c.client == nil {
		// dev 无 Redis：不启动消费循环，干净 no-op。
		log.Println("jobqueue: consumer has nil redis, not starting read loop")
		return nil
	}
	stream, group, consumer, block := c.resolved()

	// Create the consumer group if absent. MKSTREAM also creates the stream on
	// first run. BUSYGROUP (group already exists from a prior start / another
	// worker) is expected and ignored — idempotent bootstrap.
	if err := c.client.XGroupCreateMkStream(ctx, stream, group, "$").Err(); err != nil {
		if !isBusyGroup(err) {
			return fmt.Errorf("jobqueue: xgroup create mkstream (%s/%s): %w", stream, group, err)
		}
		log.Printf("jobqueue: consumer group %q already exists on %q, reusing", group, stream)
	}

	for {
		// 1) clean-shutdown pre-check (non-blocking).
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// 2) redeliver this consumer's pending first (ID "0", non-blocking).
		c.readOnce(ctx, stream, group, consumer, "0", 0)

		// 3) block for new entries (ID ">").
		c.readOnce(ctx, stream, group, consumer, ">", block)
	}
}

// readOnce issues one XREADGROUP at the given id and dispatches every returned
// entry through the handler. blockArg is the BLOCK duration; pass 0 to rely on
// Redis ignoring BLOCK for non-">" IDs (pending reads) — but we always forward
// the configured block for the ">" read so the idle loop paces itself.
//
// Error handling: redis.Nil (no data) is normal and silent; a cancelled context
// is a clean stop (returns silently so the top-level loop's pre-check returns
// nil); any other error is logged and we back off errBackoff before returning,
// so a flapping Redis never hot-loops or kills the worker.
func (c *Consumer) readOnce(ctx context.Context, stream, group, consumer, id string, blockArg time.Duration) {
	args := &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{stream, id},
		Count:    c.count,
		Block:    blockArg,
		NoAck:    false,
	}
	res, err := c.client.XReadGroup(ctx, args).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return // nothing to deliver at this id right now
		}
		if ctx.Err() != nil {
			return // graceful: ctx cancelled mid-read
		}
		log.Printf("jobqueue: xreadgroup error (id=%q): %v", id, err)
		select {
		case <-time.After(errBackoff):
		case <-ctx.Done():
		}
		return
	}
	for _, s := range res {
		for _, msg := range s.Messages {
			c.process(ctx, stream, group, msg)
		}
	}
}

// process decodes one entry, invokes the handler and applies the ACK/redelivery
// policy documented on Run.
func (c *Consumer) process(ctx context.Context, stream, group string, msg redis.XMessage) {
	job, perr := parseJob(msg)
	if perr != nil {
		// corrupt entry: can never be handled, so drop it to avoid an infinite
		// hot-loop and keep the stream draining. Logged at warning level.
		log.Printf("jobqueue: dropping unparseable entry id=%s: %v", msg.ID, perr)
		c.ackAndDel(ctx, stream, group, msg.ID)
		return
	}
	if err := c.handler.Handle(ctx, job); err != nil {
		// 失败：只记日志，不 ACK，留在 pending 等下轮 "0" 读重投递。
		log.Printf("jobqueue: handle failed id=%s type=%s tenant=%d: %v (left pending for redelivery)",
			msg.ID, job.Type, job.TenantID, err)
		return
	}
	c.ackAndDel(ctx, stream, group, msg.ID)
}

// ackAndDel ACKs the entry out of the group PEL and removes it from the Stream.
// XAck + XDel together keep XLen at zero once everything is processed, which the
// tests assert. Both are best-effort: a failure here only leaves a harmless
// stray entry / pending ref that the next loop re-attempts.
func (c *Consumer) ackAndDel(ctx context.Context, stream, group, id string) {
	if err := c.client.XAck(ctx, stream, group, id).Err(); err != nil {
		log.Printf("jobqueue: xack failed id=%s: %v", id, err)
	}
	if err := c.client.XDel(ctx, stream, id).Err(); err != nil {
		log.Printf("jobqueue: xdel failed id=%s: %v", id, err)
	}
}

// resolved returns the effective Stream / Group / Consumer / Block after
// applying defaults for empty / zero config fields.
func (c *Consumer) resolved() (stream, group, consumer string, block time.Duration) {
	stream = c.cfg.Stream
	if stream == "" {
		stream = defaultStream
	}
	group = c.cfg.Group
	if group == "" {
		group = defaultGroup
	}
	consumer = c.cfg.Consumer
	if consumer == "" {
		consumer = defaultConsumer
	}
	block = c.cfg.Block
	if block <= 0 {
		block = defaultBlock
	}
	return stream, group, consumer, block
}

// parseJob rebuilds a Job from a Stream entry. Missing / malformed fields are
// tolerated (zero value); the only hard error is an empty type, since the
// consumer cannot route an entry without a type.
func parseJob(msg redis.XMessage) (Job, error) {
	var job Job
	if v, ok := msg.Values[fieldType].(string); ok {
		job.Type = v
	}
	if v, ok := msg.Values[fieldPayload].(string); ok {
		job.Payload = []byte(v)
	}
	if v, ok := msg.Values[fieldTenant].(string); ok {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			job.TenantID = id
		}
	}
	if v, ok := msg.Values[fieldRunAt].(string); ok {
		if ns, err := strconv.ParseInt(v, 10, 64); err == nil {
			job.RunAt = time.Unix(0, ns)
		}
	}
	if job.Type == "" {
		return job, fmt.Errorf("entry %s missing %q field", msg.ID, fieldType)
	}
	return job, nil
}

// isBusyGroup reports whether err is the "consumer group already exists" error
// emitted by both real Redis and miniredis ("BUSYGROUP ..."). Used to make
// XGroupCreateMkStream idempotent across restarts / sibling workers.
func isBusyGroup(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "busygroup")
}
