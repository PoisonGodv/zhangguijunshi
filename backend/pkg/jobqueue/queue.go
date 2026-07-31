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
// at-least-once across replicas (CONTRACT §19): delivery does NOT depend on a
// same-named consumer restarting to re-read its own pending list. A consumer
// that crashes after XREADGROUP but before XAck strands its entries in ITS
// pending list, which no other consumer's "0" read ever touches. The Consumer's
// Run loop therefore also runs a periodic XAUTOCLAIM sweep that re-homes any
// entry idle longer than MinIdle onto a surviving consumer and re-processes it.
// Entries that keep failing past MaxDeliveries are parked on the <stream>:dlq
// dead-letter stream instead of being retried forever.
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

	// defaultMinIdle is the XAUTOCLAIM MIN-IDLE-TIME: a pending entry must have
	// been idle (delivered but neither ACKed nor re-delivered) for at least this
	// long before another consumer may claim it. It MUST exceed the longest
	// Handle duration so a slow-but-healthy handler is not robbed mid-flight;
	// 30s is a conservative default for the reach/pregen handlers.
	defaultMinIdle = 30 * time.Second

	// defaultClaimInterval paces the idle-pending reclaim sweep inside Run. The
	// loop body already ticks at ~cfg.Block cadence, so this only decides how
	// many of those ticks actually issue an XAUTOCLAIM. 5s keeps orphan pickup
	// latency low without burning Redis QPS when the PEL is empty.
	defaultClaimInterval = 5 * time.Second

	// defaultMaxDeliveries is the poison-pill ceiling: once an entry's delivery
	// count (XPENDING detail) exceeds this it is parked on the <stream>:dlq
	// stream and ACKed away instead of being retried forever. In the recommended
	// 5..10 band; 8 gives a transiently-failing job plenty of retries.
	defaultMaxDeliveries = 8

	// pendingScanCap bounds the XPENDING detail query used to read per-entry
	// delivery counts for poison detection. Generous so a large PEL cannot
	// truncate the lookup and hide a poison entry's true count.
	pendingScanCap = 1000
)

// dlqSuffix is appended to the resolved Stream name to form the dead-letter
// stream (<stream>:dlq). Poison entries (delivery count over the ceiling) are
// XADDed there — preserving the raw fields for later inspection — before being
// ACKed + XDel'ed off the live stream so they stop blocking the PEL.
const dlqSuffix = ":dlq"

// dlqStreamName derives the dead-letter stream key from a resolved Stream name.
func dlqStreamName(stream string) string {
	return stream + dlqSuffix
}

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
// entry leaves the Stream; non-nil → the entry is left pending and redelivered
// — by the same consumer's next "0" read, OR, if that consumer dies, by any
// sibling via the XAUTOCLAIM reclaim sweep (so at-least-once is guaranteed
// across replicas, not just within one consumer). Handle must be idempotent
// because redelivery is at-least-once.
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
//
// at-least-once guarantee (CONTRACT §19): it does NOT rely on a same-named
// consumer restarting to re-read its own pending list. A consumer that crashes
// (or whose Handle keeps failing) leaves entries stranded in ITS pending list,
// which no other consumer's "0" read ever touches. Run therefore also runs a
// periodic XAUTOCLAIM sweep (see reclaimIdlePending) that re-homes any entry
// idle longer than MinIdle onto THIS consumer and re-processes it — so a dead
// replica's orphans are recovered by any surviving sibling. Entries that keep
// failing past MaxDeliveries are parked on the <stream>:dlq dead-letter stream
// instead of being retried forever.
type Consumer struct {
	client  redis.Cmdable
	cfg     config.JobQueueConfig
	handler Handler
	count   int64 // XREADGROUP COUNT (overrides via WithCount)

	// reclaim tuning (defaults applied in NewConsumer; override via options).
	minIdle       time.Duration // XAUTOCLAIM MIN-IDLE-TIME (must exceed longest Handle)
	claimInterval time.Duration // how often Run issues an XAUTOCLAIM sweep
	maxDeliveries int64         // poison-pill ceiling (XPENDING delivery count)
	lastClaim     time.Time     // wall-clock of the last reclaim sweep (paces sweeps)
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

// WithMinIdle overrides the XAUTOCLAIM MIN-IDLE-TIME: how long a pending entry
// must sit idle before another consumer may claim it. MUST exceed the longest
// Handle duration or a slow-but-healthy handler could be robbed mid-flight.
// Production uses the 30s default; tests set it tiny (e.g. 1ms) so reclaim is
// observable without real sleeping.
func WithMinIdle(d time.Duration) ConsumerOption {
	return func(c *Consumer) {
		if d > 0 {
			c.minIdle = d
		}
	}
}

// WithClaimInterval overrides how often Run issues an idle-pending reclaim
// sweep (XAUTOCLAIM). Production uses the ~5s default; tests set it tiny so a
// dead consumer's orphan is re-homed promptly.
func WithClaimInterval(d time.Duration) ConsumerOption {
	return func(c *Consumer) {
		if d > 0 {
			c.claimInterval = d
		}
	}
}

// WithMaxDeliveries overrides the poison-pill ceiling: once an entry's delivery
// count exceeds n it is parked on the <stream>:dlq dead-letter stream and ACKed
// away instead of being retried forever.
func WithMaxDeliveries(n int64) ConsumerOption {
	return func(c *Consumer) {
		if n > 0 {
			c.maxDeliveries = n
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
		client:        client,
		cfg:           cfg,
		handler:       handler,
		count:         defaultReadCount,
		minIdle:       defaultMinIdle,
		claimInterval: defaultClaimInterval,
		maxDeliveries: defaultMaxDeliveries,
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
//  4. Periodically (every claimInterval) run an XAUTOCLAIM sweep
//     (reclaimIdlePending) that re-homes entries stranded in OTHER consumers'
//     pending lists — e.g. a replica that crashed mid-Handle — onto THIS
//     consumer and re-processes them. This is what actually delivers the
//     at-least-once contract across replicas: it does NOT depend on a
//     same-named consumer coming back to re-read its own PEL. Entries whose
//     delivery count exceeds maxDeliveries are parked on <stream>:dlq instead
//     of being retried forever (poison-pill guard).
//
// Per-entry ACK/redelivery policy (see process):
//   - Handle ok            → XAck + XDel (entry leaves the group PEL and the Stream).
//   - Handle error         → log + do NOT ack; entry stays pending and is
//     redelivered on the next iteration's "0" read (same consumer) or, if that
//     consumer dies, reclaimed via XAUTOCLAIM by a sibling.
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
	// first run. Start at "0" (NOT "$") so entries XADDed BEFORE the group was
	// created — e.g. an api producer that enqueued while no worker was up, or
	// before this replica joined — are still reachable by the ">" read. "$"
	// would silently drop them (CONTRACT §19 gap closure). BUSYGROUP (group
	// already exists from a prior start / another worker) is expected and
	// ignored — idempotent bootstrap.
	if err := c.client.XGroupCreateMkStream(ctx, stream, group, "0").Err(); err != nil {
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

		// 4) periodically reclaim orphans stranded in other consumers' PELs
		//    (crashed / failing replicas). Paced by claimInterval so the hot
		//    loop does not issue an XAUTOCLAIM on every tick.
		if time.Since(c.lastClaim) >= c.claimInterval {
			c.lastClaim = time.Now()
			c.reclaimIdlePending(ctx, stream, group, consumer)
		}
	}
}

// reclaimIdlePending issues one XAUTOCLAIM to re-home entries that have been
// idle (delivered but not ACKed) for at least minIdle — i.e. stranded in some
// other consumer's pending list, typically because that consumer crashed or its
// Handle keeps failing. Every claimed entry is pushed through the SAME process()
// pipeline as a normal read (so ACK / redelivery / poison handling is uniform).
//
// This is the cross-replica half of the at-least-once contract: without it, a
// consumer that dies after XREADGROUP but before XAck leaves its entries
// pending FOREVER — no other consumer's "0" read ever sees another consumer's
// PEL. XAUTOCLAIM is the standard Redis mechanism to recover them.
//
// minIdle MUST exceed the longest Handle duration (default 30s) so a slow but
// healthy handler is not robbed mid-flight by a sibling sweep.
//
// Poison guard: before processing, entries whose delivery count (from XPENDING
// detail) already exceeds maxDeliveries are parked on the <stream>:dlq
// dead-letter stream and ACKed away — a message that keeps failing is almost
// certainly a poison pill and must not be retried forever.
//
// Errors are logged and swallowed: reclaim is best-effort and must never crash
// the read loop. A NOGROUP here means the group vanished; the next readOnce
// rebuilds it (see its NOGROUP branch), so we just return.
func (c *Consumer) reclaimIdlePending(ctx context.Context, stream, group, consumer string) {
	if ctx.Err() != nil {
		return
	}
	msgs, _, err := c.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   stream,
		Group:    group,
		Consumer: consumer,
		MinIdle:  c.minIdle,
		Start:    "0", // scan the whole PEL; XAUTOCLAIM returns a cursor but one pass per interval is enough
		Count:    c.count,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) || ctx.Err() != nil {
			return
		}
		if isNoGroup(err) {
			// group gone; readOnce's NOGROUP branch will recreate it next tick.
			return
		}
		log.Printf("jobqueue: xautoclaim error (%s/%s): %v", stream, group, err)
		return
	}
	if len(msgs) == 0 {
		return
	}
	log.Printf("jobqueue: reclaimed %d idle pending entr%s (min_idle=%s) on %s/%s",
		len(msgs), plural(len(msgs)), c.minIdle, stream, consumer)

	// Read delivery counts once for poison detection. Best-effort: if XPENDING
	// fails we fall through with an empty map and simply retry next sweep.
	counts := c.pendingCounts(ctx, stream, group)

	for _, msg := range msgs {
		if ctx.Err() != nil {
			return
		}
		if n := counts[msg.ID]; n > c.maxDeliveries {
			// Poison pill: retried too many times. Park it on the DLQ (raw
			// fields preserved for inspection) and ACK it away so it stops
			// blocking the PEL and being reclaimed forever.
			c.deadLetter(ctx, stream, group, msg, n)
			continue
		}
		c.process(ctx, stream, group, msg)
	}
}

// pendingCounts returns the delivery count for every currently-pending entry in
// the group (XPENDING detail form). Used by reclaimIdlePending to detect poison
// entries. Returns an empty map on any error (reclaim then just retries later).
func (c *Consumer) pendingCounts(ctx context.Context, stream, group string) map[string]int64 {
	out := make(map[string]int64)
	ext, err := c.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: stream,
		Group:  group,
		Start:  "-",
		End:    "+",
		Count:  pendingScanCap,
	}).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) && !isNoGroup(err) && ctx.Err() == nil {
			log.Printf("jobqueue: xpending error (%s/%s): %v", stream, group, err)
		}
		return out
	}
	for _, e := range ext {
		out[e.ID] = e.RetryCount
	}
	return out
}

// deadLetter parks a poison entry on the <stream>:dlq stream (preserving its raw
// fields for later inspection) and then ACKs + XDel's it off the live stream so
// it leaves the group PEL and stops being retried. All steps are best-effort and
// logged; a DLQ write failure still ACKs the entry away (a poison message must
// not wedge the consumer forever — losing it to the DLQ is the lesser evil).
func (c *Consumer) deadLetter(ctx context.Context, stream, group string, msg redis.XMessage, deliveries int64) {
	dlq := dlqStreamName(stream)
	values := make(map[string]interface{}, len(msg.Values)+1)
	for k, v := range msg.Values {
		values[k] = v
	}
	values["orig_id"] = msg.ID // traceability: which live-stream entry this was
	if _, err := c.client.XAdd(ctx, &redis.XAddArgs{Stream: dlq, Values: values}).Result(); err != nil {
		log.Printf("jobqueue: dlq xadd failed id=%s (dlq=%s): %v — acking away anyway", msg.ID, dlq, err)
	} else {
		log.Printf("jobqueue: poison entry id=%s parked on %s after %d deliveries (> max %d)",
			msg.ID, dlq, deliveries, c.maxDeliveries)
	}
	c.ackAndDel(ctx, stream, group, msg.ID)
}

// readOnce issues one XREADGROUP at the given id and dispatches every returned
// entry through the handler. blockArg is the BLOCK duration; pass 0 to rely on
// Redis ignoring BLOCK for non-">" IDs (pending reads) — but we always forward
// the configured block for the ">" read so the idle loop paces itself.
//
// Error handling: redis.Nil (no data) is normal and silent; a cancelled context
// is a clean stop (returns silently so the top-level loop's pre-check returns
// nil); a NOGROUP error means the consumer group vanished at runtime (e.g. an
// operator XGROUP DESTROY, or the stream was trimmed to empty and recreated) —
// we rebuild it at "0" so the loop keeps draining instead of logging + backing
// off forever; any other error is logged and we back off errBackoff before
// returning, so a flapping Redis never hot-loops or kills the worker.
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
		if isNoGroup(err) {
			// The group is gone. Recreate it at "0" (MKSTREAM also re-creates
			// the stream if it was deleted) so the loop self-heals instead of
			// spinning on log+backoff forever. BUSYGROUP means a sibling beat
			// us to it — fine either way.
			log.Printf("jobqueue: NOGROUP on %s/%s (id=%q): recreating group at \"0\"", stream, group, id)
			if cerr := c.client.XGroupCreateMkStream(ctx, stream, group, "0").Err(); cerr != nil && !isBusyGroup(cerr) {
				log.Printf("jobqueue: recreate group failed (%s/%s): %v", stream, group, cerr)
			}
			select {
			case <-time.After(errBackoff):
			case <-ctx.Done():
			}
			return
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

// isNoGroup reports whether err is the "no such consumer group" error emitted by
// both real Redis and miniredis ("NOGROUP No such consumer group '...' for key
// name '...'"). readOnce uses it to rebuild a group that vanished at runtime;
// reclaimIdlePending / pendingCounts use it to silently skip a sweep that raced
// a group deletion.
func isNoGroup(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "nogroup")
}

// plural returns "y" or "ies" so reclaim log lines read naturally ("1 entry" /
// "3 entries") without pulling in a pluralization library.
func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
