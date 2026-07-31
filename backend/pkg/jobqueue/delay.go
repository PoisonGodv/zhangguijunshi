package jobqueue

// delay.go implements the ZSET-backed delayed-delivery lane that closes the
// CONTRACT §20.3 gap: previously Producer.Enqueue did a plain XADD and the
// consumer's XREADGROUP drained the job immediately, so a message "scheduled"
// for 18:00 fired the instant it was enqueued. Now a future RunAt is parked in
// a ZSET scored by RunAt.UnixNano() and a background DelayPump (run inside the
// worker) moves due members onto the live Stream so the existing Consumer picks
// them up with zero changes to parseJob.
//
// This file holds the SHARED mechanism (encoding + pump); the Producer's branch
// on RunAt lives in queue.go. Every job type benefits — reach today, pregen
// later — with no per-domain wiring.
//
// Atomicity / ownership model (no Lua, miniredis-safe):
//   - The pump lists due members with ZRANGEBYSCORE (via ZRangeArgs ByScore).
//   - For each member it issues ZREM; only the pump whose ZREM returns 1 owns
//     the job. Sibling pumps racing on the same member see ZREM return 0 and
//     skip it → exactly-once handoff across worker replicas.
//   - Only after a successful claim does the pump XADD the job onto the live
//     Stream. If that XADD fails, the member is re-ZADDed at now+backoff so the
//     job is retried rather than lost (at-least-once).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// delayZsetSuffix is appended to the resolved Stream name to form the delay
// ZSET key (<stream>:delay). Derived rather than configured so producer and pump
// always agree without a new config field.
const delayZsetSuffix = ":delay"

// fieldNonce is the JSON key carrying a per-job random nonce inside the delay
// ZSET member. It is NOT written to the live Stream — it only exists to force
// ZSET member uniqueness (see encodeDelayMember).
const fieldNonce = "_n"

// Defaults applied when the corresponding DelayPump tuning field is zero.
const (
	// defaultPumpInterval paces sweeps when nothing was due. ~500ms keeps a
	// parked job's delivery latency well under a second in practice without
	// burning Redis QPS when the lane is idle.
	defaultPumpInterval = 500 * time.Millisecond

	// defaultPumpBatch caps how many members one sweep claims. Bounded so a
	// huge backlog can't starve the consumer's own XREADGROUP or the ctx
	// pre-check.
	defaultPumpBatch = 64

	// requeueBackoff is the score bump applied when a claimed member's XADD
	// fails: the job is re-staged at now+backoff so the next sweep retries it
	// instead of hot-looping on a failing Stream.
	requeueBackoff = 5 * time.Second
)

// delayZsetName derives the delay ZSET key from a resolved Stream name. Shared
// by Producer.delayZset (api side) and DelayPump (worker side) so both resolve
// to the same lane from the same cfg.JobQueue.Stream.
func delayZsetName(stream string) string {
	return stream + delayZsetSuffix
}

// encodeDelayMember serializes the four canonical stream fields into the string
// used as the ZSET member, plus a random nonce.
//
// Encoding choice — JSON object (e.g.
// {"_n":"...","payload":"...","run_at":"...","tenant_id":"...","type":"..."}):
// the alternative "field=value\n" form breaks when Payload contains a newline or
// "=", which it may (Payload is opaque caller bytes, often JSON itself). JSON
// escaping round-trips arbitrary strings losslessly. tenant_id / run_at are kept
// as base-10 strings to exactly mirror the live-Stream field representation, so
// the pump can replay them verbatim.
//
// The nonce is required because ZADD collapses duplicate members (last score
// wins): two logically-distinct jobs with identical type/payload/tenant/run_at
// (e.g. two pregen_incremental jobs for the same tenant at the same minute)
// would otherwise be silently collapsed into one. The nonce makes every member
// unique regardless of field content. It never reaches the live Stream.
func encodeDelayMember(fields map[string]string) (string, error) {
	out := make(map[string]string, len(fields)+1)
	for k, v := range fields {
		out[k] = v
	}
	out[fieldNonce] = newNonce()
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeDelayMember inverts encodeDelayMember, returning the field map (nonce
// included; callers read only the four known field* keys). A corrupt member
// yields an error so the pump can drop it as a poison entry instead of
// hot-looping forever.
func decodeDelayMember(member string) (map[string]string, error) {
	var m map[string]string
	if err := json.Unmarshal([]byte(member), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// newNonce returns a 24-char hex random string. crypto/rand is used so the nonce
// is unique across processes (api replicas enqueuing concurrently); on the
// near-impossible rand.Read failure it falls back to a hex nanosecond timestamp
// rather than fail the enqueue (enqueue must never block the business flow).
func newNonce() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// truncMember shortens a ZSET member for log lines so a large Payload cannot
// spam the operator log. The full member is never needed by a human — the type
// and tenant are logged separately.
func truncMember(s string) string {
	const max = 128
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// --------------------------- DelayPump ---------------------------

// DelayPump is a background loop (run inside the worker) that moves due delayed
// jobs from the ZSET delay lane onto the live Stream so the existing Consumer
// delivers them. One pump per worker process; sibling pumps across worker
// replicas cooperate via ZREM-claim (exactly-once handoff).
//
// Nil-safe (CONTRACT §0): with a nil client Run logs and returns nil without
// looping, mirroring Consumer — dev (no Redis) simply has no delayed delivery,
// which is correct because nothing enqueues delayed jobs in dev either.
type DelayPump struct {
	client   redis.Cmdable
	stream   string
	zset     string
	maxLen   int64
	interval time.Duration
	batch    int64
	nowFunc  func() time.Time
}

// DelayPumpOption configures a DelayPump (functional-option style, matches
// Consumer's options). The documented NewDelayPump(client, cfg) call works
// unchanged — options are for tests / advanced tuning.
type DelayPumpOption func(*DelayPump)

// WithPumpInterval overrides the idle sweep pace (time between sweeps when
// nothing was due). Production uses the ~500ms default; tests set it small
// (e.g. 10ms) so ticks are observable without real sleeping on the due path.
func WithPumpInterval(d time.Duration) DelayPumpOption {
	return func(p *DelayPump) {
		if d > 0 {
			p.interval = d
		}
	}
}

// WithPumpBatch overrides the per-sweep claim cap.
func WithPumpBatch(n int64) DelayPumpOption {
	return func(p *DelayPump) {
		if n > 0 {
			p.batch = n
		}
	}
}

// WithPumpNow injects the clock used to decide "due". Tests pass a fixed /
// advancing time so they can cross RunAt with NO real sleeping; production
// leaves it at the default time.Now.
func WithPumpNow(f func() time.Time) DelayPumpOption {
	return func(p *DelayPump) {
		if f != nil {
			p.nowFunc = f
		}
	}
}

// NewDelayPump builds a DelayPump over client. cfg supplies the Stream name
// (used both for the live Stream and, derived, the ZSET lane) and the MaxLen
// trim cap applied when re-publishing; empty Stream / zero fields fall back to
// defaults consistent with Producer and Consumer. Pass a nil client in dev and
// Run returns nil without looping.
func NewDelayPump(client redis.Cmdable, cfg config.JobQueueConfig, opts ...DelayPumpOption) *DelayPump {
	stream := cfg.Stream
	if stream == "" {
		stream = defaultStream
	}
	maxLen := cfg.MaxLen
	if maxLen == 0 {
		maxLen = defaultMaxLen
	}
	p := &DelayPump{
		client:   client,
		stream:   stream,
		zset:     delayZsetName(stream),
		maxLen:   maxLen,
		interval: defaultPumpInterval,
		batch:    defaultPumpBatch,
		nowFunc:  time.Now,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Run sweeps the delay ZSET until ctx is cancelled.
//
// Loop shape:
//  1. clean-shutdown pre-check (non-blocking).
//  2. sweep: list due members (score <= now), claim each via ZREM, XADD the
//     claimed ones onto the live Stream with the same fields the immediate path
//     writes (parseJob works unchanged).
//  3. if the sweep delivered anything, loop IMMEDIATELY (keep draining while due
//     items remain — no pace); otherwise pace one interval or until ctx.
//
// now comes from the injectable nowFunc so tests are deterministic.
//
// Error handling: a sweep error (Redis flap) is logged and we back off
// errBackoff (shared with the Consumer) before the next attempt — a flapping
// Redis never hot-loops or kills the pump. ctx cancellation always returns nil.
func (p *DelayPump) Run(ctx context.Context) error {
	if p == nil || p.client == nil {
		// dev 无 Redis：不启动扫描循环，干净 no-op。
		log.Println("jobqueue: delay pump has nil redis, not starting sweep loop")
		return nil
	}
	for {
		// 1) clean-shutdown pre-check.
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// 2) one sweep over due members.
		delivered, err := p.sweep(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // graceful: ctx cancelled mid-sweep
			}
			log.Printf("jobqueue: delay pump sweep error: %v", err)
			select {
			case <-time.After(errBackoff):
			case <-ctx.Done():
				return nil
			}
			continue
		}

		// 3) pace: keep draining while there is backlog, else sleep one interval.
		if delivered > 0 {
			continue
		}
		select {
		case <-time.After(p.interval):
		case <-ctx.Done():
			return nil
		}
	}
}

// sweep lists members whose score (RunAt.UnixNano) is <= now and hands each to
// claimAndPublish. Returns how many this pump claimed-and-published this sweep
// (used by Run to decide immediate re-sweep vs. pacing). ctx cancellation
// between members short-circuits cleanly.
func (p *DelayPump) sweep(ctx context.Context) (int, error) {
	now := p.nowFunc().UnixNano()
	members, err := p.client.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key:     p.zset,
		Start:   "-inf", // 闭区间下界：最早的成员也算 due
		Stop:    strconv.FormatInt(now, 10),
		ByScore: true, // 按 score(range) 取成员，非按 index
		Count:   p.batch,
	}).Result()
	if err != nil {
		return 0, fmt.Errorf("zrangebyscore delay: %w", err)
	}
	delivered := 0
	for _, member := range members {
		select {
		case <-ctx.Done():
			return delivered, nil
		default:
		}
		if p.claimAndPublish(ctx, member) {
			delivered++
		}
	}
	return delivered, nil
}

// claimAndPublish atomically claims a ZSET member (ZREM) and, on success,
// re-publishes it onto the live Stream. Returns true iff THIS pump owned and
// disposed of the member (published OR deliberately dropped as poison); returns
// false if a sibling won the claim, the claim errored, or the job was re-staged
// for a later retry (XADD failure).
//
// Failure / claim semantics:
//   - ZREM err      → leave the member in the ZSET, return false (next sweep
//     retries; nothing is lost).
//   - ZREM == 0     → a sibling pump already claimed it, return false
//     (exactly-once across replicas).
//   - corrupt member → drop it (already removed), return true so we don't
//     re-stage garbage. Mirrors the Consumer's poison-pill guard.
//   - XADD err      → re-ZADD at now+requeueBackoff so the job is retried (do
//     NOT lose it); return false. If the re-ZADD also fails the job is gone —
//     logged loudly; inherent to a Redis-down mid-sweep.
func (p *DelayPump) claimAndPublish(ctx context.Context, member string) bool {
	// 原子抢占：只有 ZREM 返回 1 的那个 pump 拿到这条 job。
	removed, err := p.client.ZRem(ctx, p.zset, member).Result()
	if err != nil {
		log.Printf("jobqueue: delay pump zrem failed (member=%s): %v", truncMember(member), err)
		return false
	}
	if removed == 0 {
		return false // 被别的 pump 抢走了
	}

	fields, err := decodeDelayMember(member)
	if err != nil {
		// 解析失败：垃圾成员，丢弃，不再放回（否则永远卡住）。
		log.Printf("jobqueue: delay pump dropping corrupt delay member: %v (member=%s)",
			err, truncMember(member))
		return true
	}
	if fields[fieldType] == "" {
		// 缺 type：消费者无法路由，等同 poison，丢弃。
		log.Printf("jobqueue: delay pump dropping member with empty type (member=%s)", truncMember(member))
		return true
	}

	// 复用立即路径的四个字段名投递到 live Stream，parseJob 无需改动。
	args := &redis.XAddArgs{
		Stream: p.stream,
		Approx: true,
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
		// XADD 失败：把 job 以 now+backoff 重新放回延迟队列，避免丢失。
		requeueScore := float64(p.nowFunc().Add(requeueBackoff).UnixNano())
		if zerr := p.client.ZAdd(ctx, p.zset, redis.Z{Score: requeueScore, Member: member}).Err(); zerr != nil {
			log.Printf("jobqueue: delay pump XADD failed AND re-stage failed (member=%s): xadd=%v zadd=%v — JOB LOST",
				truncMember(member), err, zerr)
		} else {
			log.Printf("jobqueue: delay pump xadd failed (member=%s): %v — re-staged at +%s",
				truncMember(member), err, requeueBackoff)
		}
		return false
	}
	log.Printf("jobqueue: delay pump delivered type=%s tenant=%s stream_id=%s",
		fields[fieldType], fields[fieldTenant], id)
	return true
}
