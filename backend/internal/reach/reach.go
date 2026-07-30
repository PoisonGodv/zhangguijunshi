// Package reach implements the reach (触达) domain — the central hub that turns a
// marketing-strategy decision into an actual message delivered to a member on one
// of four channels (BE-B-07+08+10, CONTRACT §11.3.2 / tech plan §4.3). A ReachTask
// is the audit-trail row recording every attempt: which tenant/store/strategy it
// belongs to, which channel+template it used, and whether it ended up sent,
// blocked (with a reason), or failed.
//
// The Send pipeline runs its guards in a FIXED order (render → content filter →
// frequency circuit → unsubscribe → subscribe quota → sender). A guard that
// rejects the message writes status=blocked + blocked_reason and returns
// (task, nil) — NOT an error — so the caller can distinguish "intercepted by a
// compliance guard" (expected, branch on code) from "system failure" (real error).
//
// Anti-corruption: reach imports neither freqcircuit, contentfilter, nor any
// domain package. The three compliance checks are injected as the narrow FreqGate
// / ContentFilter / Sender interfaces and adapted at the composition root
// (cmd/api/main.go). Tenant is ALWAYS resolved from ctx via
// multitenancy.EnforceTenant; cross-tenant access is reported as ErrNotFound.
package reach

import (
	"context"
	"errors"
	"time"
)

// ReachTask is the audit-trail row for one reach attempt. Status is one of
// pending|sent|blocked|failed; BlockedReason is empty unless Status==blocked,
// in which case it carries the guard that intercepted (freq_circuit |
// content_filter | unsubscribed | subscribe_quota | risk). StrategyID is the AI
// attribution link (0 = non-AI / manual).
type ReachTask struct {
	ID, TenantID, StoreID, StrategyID int64
	Channel                           string // wework_group|wework_1v1|subscribe_msg|sms
	TemplateCode                      string
	TargetCount                       int
	Status                            string // pending|sent|blocked|failed
	BlockedReason                     string // 空=非拦截；freq_circuit|content_filter|unsubscribed|subscribe_quota|risk
	ScheduledAt, SentAt               time.Time
	CreatedAt                         time.Time
}

// SubscribeAuth is the one-time subscription-message authorization counter for a
// (tenant, member, template) triple (tech plan §4.3.5). Each user authorization
// increments GrantedQuota (idempotent accumulate); each sent subscribe message
// increments UsedQuota atomically under used<granted.
type SubscribeAuth struct {
	ID, TenantID, MemberID  int64
	TemplateID              string
	GrantedQuota, UsedQuota int
	GrantedAt               time.Time
}

// UnsubscribeRecord marks that a member has opted out of a channel. One row per
// (tenant, member, channel) — UNIQUE semantics; re-subscribe clears the row.
type UnsubscribeRecord struct {
	ID, TenantID, MemberID int64
	Channel, Reason        string
	UnsubscribedAt         time.Time
}

// Channel constants — the four reach channels.
const (
	ChannelWeworkGroup  = "wework_group"
	ChannelWeworkOneV   = "wework_1v1"
	ChannelSubscribeMsg = "subscribe_msg"
	ChannelSMS          = "sms"
)

// The 8 reach template codes (tech plan §4.3, M2 set).
const (
	TplNewWelcome   = "new_welcome"
	TplBirthday     = "birthday"
	TplRecall30     = "recall_30"
	TplRecall60     = "recall_60"
	TplRecall90     = "recall_90"
	TplRepurchase   = "repurchase"
	TplBalanceLow   = "balance_low"
	TplPointsExpire = "points_expire"
)

// ReachTask status values.
const (
	StatusPending = "pending"
	StatusSent    = "sent"
	StatusBlocked = "blocked"
	StatusFailed  = "failed"
)

// BlockedReason values carried by a blocked ReachTask.
const (
	BlockFreqCircuit    = "freq_circuit"
	BlockContentFilter  = "content_filter"
	BlockUnsubscribed   = "unsubscribed"
	BlockSubscribeQuota = "subscribe_quota"
	BlockRisk           = "risk"
)

// Sentinel errors. Cross-tenant access is reported as ErrNotFound (absence and
// foreign ownership are indistinguishable by design — ARCH-01).
var (
	ErrNotFound           = errors.New("reach: task not found")
	ErrTemplateUnknown    = errors.New("reach: unknown template code")
	ErrSubscribeExhausted = errors.New("reach: subscribe quota exhausted")
)

// Sender is the channel adapter (WeCom / subscribe / SMS). The real
// implementations land later; the in-memory stub records the payload and returns
// nil. nil Sender in the Service means "skip the send step" (dev renders only).
type Sender interface {
	Send(ctx context.Context, p *SendPayload) error
}

// SendPayload is the DTO handed to a Sender: everything needed to deliver one
// rendered message to one member.
type SendPayload struct {
	TenantID, MemberID int64
	Channel            string
	TemplateCode       string
	Title, Body        string
}

// FreqGate is the anti-corruption port for the frequency circuit breaker (reach
// must not import freqcircuit). allowed=false + a non-empty reason means the
// channel's frequency cap was hit. The adapter converts string→freqcircuit.Channel.
type FreqGate interface {
	Allow(ctx context.Context, channel string) (allowed bool, reason string, err error)
}

// ContentFilter is the anti-corruption port for the content review (reach must
// not import contentfilter). Do is a pure function over the rendered body text.
type ContentFilter interface {
	Do(text string) ContentResult
}

// ContentResult is the outcome of one ContentFilter.Do call (mirrors
// contentfilter.Result, adapted at the composition root).
type ContentResult struct {
	Text     string
	Passed   bool
	HitWords []string
	Replaced []string
}

// ReachRepo persists ReachTask rows, subscription authorizations, and
// unsubscribe records. Every method treats a record owned by another tenant as
// absent (ErrNotFound). The real implementation will be pgx-backed with
// tenant_id row isolation; an in-memory implementation covers dev and tests.
//
// GrantSubscribe / ConsumeSubscribe and Unsubscribe / Resubscribe are the atomic
// critical sections: the in-memory implementation serialises them under a single
// mutex; the pgx implementation will wrap them in real transactions.
type ReachRepo interface {
	CreateTask(ctx context.Context, t *ReachTask) (*ReachTask, error)
	GetTask(ctx context.Context, tenantID, id int64) (*ReachTask, error)
	ListTasks(ctx context.Context, tenantID, storeID int64, status string) ([]*ReachTask, error)
	UpdateTaskStatus(ctx context.Context, tenantID, id int64, status, blockedReason string, sentAt time.Time) (*ReachTask, error)

	// GrantSubscribe idempotently accumulates quota: an existing auth for the
	// same (tenant, member, tpl) gets GrantedQuota += quota; otherwise a new auth
	// is created with GrantedQuota = quota, UsedQuota = 0.
	GrantSubscribe(ctx context.Context, tenantID, memberID int64, tplID string, quota int, now time.Time) (*SubscribeAuth, error)
	// ConsumeSubscribe atomically increments UsedQuota iff used<granted, else
	// returns ErrSubscribeExhausted with no state change. Absent auth ⇒ exhausted.
	ConsumeSubscribe(ctx context.Context, tenantID, memberID int64, tplID string, now time.Time) (*SubscribeAuth, error)
	GetSubscribe(ctx context.Context, tenantID, memberID int64, tplID string) (*SubscribeAuth, error)

	// Unsubscribe upserts the one (tenant, member, channel) opt-out row.
	Unsubscribe(ctx context.Context, tenantID, memberID int64, channel, reason string, now time.Time) (*UnsubscribeRecord, error)
	// IsUnsubscribed reports whether an active opt-out row exists.
	IsUnsubscribed(ctx context.Context, tenantID, memberID int64, channel string) (bool, error)
	// Resubscribe clears the opt-out row (idempotent: missing row ⇒ nil).
	Resubscribe(ctx context.Context, tenantID, memberID int64, channel string) error
}

// Option customises a Service.
type Option func(*Service)

// New constructs a reach Service over repo. A nil clock falls back to time.Now;
// nil Sender / FreqGate / ContentFilter mean the corresponding pipeline step is
// skipped (dev renders + audits only).
func New(repo ReachRepo, opts ...Option) *Service {
	s := &Service{
		repo: repo,
		now:  time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// WithSender wires the channel adapter. Without it, Send skips the actual
// delivery (status ends at sent after the guards pass, no payload emitted).
func WithSender(sender Sender) Option {
	return func(s *Service) { s.sender = sender }
}

// WithFreqGate wires the frequency-circuit port (main thread adapts
// *freqcircuit.Service to it). Without it, Send skips the frequency check.
func WithFreqGate(gate FreqGate) Option {
	return func(s *Service) { s.freq = gate }
}

// WithContentFilter wires the content-review port (main thread adapts
// *contentfilter.Filter to it). Without it, Send skips the content check.
func WithContentFilter(cf ContentFilter) Option {
	return func(s *Service) { s.cf = cf }
}

// WithNowFunc overrides the service clock (deterministic time-based tests).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// WithScheduledProducer wires the async deferred-send port (CONTRACT §19). When
// set, a future SendInput.ScheduledAt enqueues the send instead of delivering
// it inline. Without it, scheduled sends fall back to immediate delivery.
func WithScheduledProducer(p ScheduledSendProducer) Option {
	return func(s *Service) { s.scheduledProducer = p }
}

// RenderInput carries the fields a template needs. MemberName / StoreName are
// the primary values; Vars is the elastic bag for template-specific keys
// (birthday / recall_days / balance / points / product_name). Missing values
// fall back to sensible defaults inside Render.
type RenderInput struct {
	Code       string
	MemberName string
	StoreName  string
	Vars       map[string]string // 弹性: birthday/recall_days/balance/points/product_name 等
}

// Rendered is one rendered template: the code it came from plus title + body.
type Rendered struct {
	Code, Title, Body string
}

// SendInput is one reach attempt: which member on which channel with which
// template, attributed to a strategy. Vars carries render variables (member_name
// / store_name / birthday / recall_days / balance / points / product_name).
//
// ScheduledAt defers delivery (CONTRACT §19): when set to a future time AND a
// ScheduledSendProducer is wired, Send enqueues the attempt onto the async job
// queue and returns a transient pending task WITHOUT delivering or persisting a
// row; the worker delivers it when the window opens (at-least-once via the
// queue's pending redelivery). Zero / past ⇒ immediate send (legacy behaviour).
type SendInput struct {
	StoreID, MemberID, StrategyID int64
	Channel, TemplateCode         string
	Vars                          map[string]string
	ScheduledAt                   time.Time
}

// ScheduledSendProducer defers a future reach send to the async job queue
// (CONTRACT §19). Satisfied by a jobqueue-backed adapter wired at the
// composition root; reach never imports the queue directly. The adapter reads
// the tenant from ctx (Send has already enforced it) so the worker can rebuild
// a tenant-scoped context on delivery.
type ScheduledSendProducer interface {
	EnqueueScheduledSend(ctx context.Context, in SendInput, sendAt time.Time) error
}

// ScheduledSendPayload is the JSON job body enqueued for a deferred send
// (jobqueue.JobReachDueSend). The worker decodes it and re-drives Service.Send
// with ScheduledAt cleared. Defined here so the producer (bootstrap) and the
// consumer (cmd/worker) share one definition.
type ScheduledSendPayload struct {
	In SendInput `json:"in"`
}
