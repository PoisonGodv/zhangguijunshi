// Package bandit implements AI-12 — epsilon-greedy rule-weight self-learning
// (CONTRACT §14.3.3 / ch07 §7.9.2). It maintains one Arm per rule key
// (rule_id = reason_type, the 7-value enum, chosen because it is
// cold-start-safe: every business vertical hits at least one reason_type even
// on day one, whereas a sparse rule.ID trigger may never fire).
//
// The Learner picks the highest-Weight arm with probability 1-epsilon
// (exploit) and a uniformly random arm with probability epsilon (explore),
// and applies each observed reward as an incremental-mean + soft-update
// (Weight = Weight*(1-LR) + q*LR, q = RewardSum/Trials). Rewards are clipped
// to [0,1]; an unseen rule is seeded with Weight 0.5 (a neutral prior) so a
// cold store never panics on its first Update.
//
// The package is dependency-free with respect to other internal packages
// (§14.3.0 acyclic DAG): the reward and the item→rule resolution arrive via
// the self-declared narrow ports RewardSource (satisfied by
// attribution.Computer) and ItemResolver (adapted from strategy.Service by the
// main thread). Arms persist through the Repo port with two implementations —
// MemoryRepo (dev/test) and PGRepo (production, ai_rule_weight table,
// migration 000010).
package bandit

import (
	"context"
	"errors"
	"math"
	"math/rand"
)

// ErrParam is returned when a caller passes an unusable argument (e.g. an
// empty ruleID to Update). Cross-tenant access is NOT an error: it is
// reported as "not found" (zero-value Arm / empty slice) by the Repo, never
// by leaking another tenant's data.
var ErrParam = errors.New("bandit: invalid param")

// Arm is the learned weight of one rule (CONTRACT §14.3.3). RuleID is the
// arm key — a reason_type (7 values) in M3-S3. Weight is the soft-updated Q
// estimate; it starts at 0.5 (neutral prior) for a rule that has never been
// seen. Trials counts applied rewards; RewardSum is their running total, so
// the incremental mean is RewardSum/Trials.
type Arm struct {
	RuleID    string  `json:"rule_id"`
	Weight    float64 `json:"weight"`     // soft-updated Q value, initial 0.5
	Trials    int     `json:"trials"`     // number of applied rewards
	RewardSum float64 `json:"reward_sum"` // running sum of clipped rewards
}

// neutralPrior is the Weight assigned to a rule that has never been seen.
// 0.5 is the midpoint of the [0,1] reward range, so a cold arm is neither
// favoured nor penalised until real feedback arrives.
const neutralPrior = 0.5

// Config holds the epsilon-greedy hyper-parameters (CONTRACT §14.3.3 /
// ch07 §7.11.1).
type Config struct {
	Epsilon float64 // exploration rate, default 0.1
	LR      float64 // learning rate for the soft update, default 0.1
}

// DefaultConfig returns the production hyper-parameters (§14.3.3):
// Epsilon 0.1, LR 0.1.
func DefaultConfig() Config {
	return Config{Epsilon: 0.1, LR: 0.1}
}

// Repo persists Arms (the ai_rule_weight table, migration 000010). Memory
// dev/test + pgx production (pgxmock-tested, §14.5.4 dual implementation).
// An unseen rule Get returns a zero-value Arm{Weight:0.5} neutral prior
// (cold-start-safe, §14.5.8); cross-tenant access is treated as "not found"
// (zero-value / empty slice), never leaking another tenant's weights.
type Repo interface {
	Get(ctx context.Context, tenantID int64, ruleID string) (*Arm, error)
	Put(ctx context.Context, tenantID int64, arm *Arm) error
	All(ctx context.Context, tenantID int64) ([]Arm, error)
}

// RewardSource is the anti-corruption port to attribution (§14.3.3 /
// §14.3.0). It returns the normalised [0,1] reward for one strategy item.
// attribution.Computer.Reward satisfies this exact signature; the main thread
// passes the Computer (or a nil to skip crediting safely in dev).
type RewardSource interface {
	Reward(ctx context.Context, tenantID, itemID, strategyID, storeID int64) (float64, error)
}

// ItemResolver is the anti-corruption port to strategy (§14.3.3 / §14.3.0).
// Resolve maps a strategy item to its arm key (ruleID = item.ReasonType),
// owning strategyID, and storeID. The main thread adapts strategy.Service to
// this interface; bandit never imports strategy.
type ItemResolver interface {
	Resolve(ctx context.Context, tenantID, itemID int64) (ruleID string, strategyID int64, storeID int64, err error)
}

// clipReward clamps r into [0,1] (CONTRACT §14.5.8: math.Max/Min). NaN is
// treated as 0 (math.Min/Max propagate NaN, so guard explicitly) — a NaN
// reward would poison RewardSum and the Q estimate forever.
func clipReward(r float64) float64 {
	if math.IsNaN(r) {
		return 0
	}
	return math.Max(0, math.Min(1, r))
}

// Learner is the epsilon-greedy weight learner (CONTRACT §14.3.3 /
// ch07 §7.9.2). It is safe for concurrent use: all repo access goes through
// the Repo (MemoryRepo is mutex-guarded; PGRepo serialises at the DB), and the
// random source is only read inside Select, which is single-flight per call.
type Learner struct {
	repo         Repo
	cfg          Config
	rand         func() float64
	rewardSource RewardSource
	resolver     ItemResolver
}

// Option configures a Learner (functional options, §4 / §14.3.3).
type Option func(*Learner)

// WithRand injects a deterministic random source (CONTRACT §14.3.3 rand
// injection). The default is math/rand.Float64; tests pass a fixed-sequence
// function so Select's exploit/explore branch is reproducible. The function
// must return values in [0,1) (the same contract as rand.Float64).
func WithRand(f func() float64) Option {
	return func(l *Learner) {
		if f != nil {
			l.rand = f
		}
	}
}

// WithRewardSource wires the attribution port used by CreditItem. nil is
// allowed: CreditItem then returns (nil, nil) without error so a dev build
// with no attribution data skips crediting safely.
func WithRewardSource(rs RewardSource) Option {
	return func(l *Learner) { l.rewardSource = rs }
}

// WithItemResolver wires the strategy port used by CreditItem. nil is
// allowed for the same reason as WithRewardSource.
func WithItemResolver(ir ItemResolver) Option {
	return func(l *Learner) { l.resolver = ir }
}

// NewLearner constructs a Learner over repo with hyper-parameters cfg. A nil
// repo is tolerated at construction (the main thread always passes one) but
// any Select/Update will then report ErrParam rather than panic. opts apply
// WithRand / WithRewardSource / WithItemResolver.
func NewLearner(repo Repo, cfg Config, opts ...Option) *Learner {
	l := &Learner{
		repo: repo,
		cfg:  cfg,
		rand: rand.Float64,
	}
	for _, o := range opts {
		if o != nil {
			o(l)
		}
	}
	return l
}

// Select picks one arm (CONTRACT §14.3.3): with probability 1-Epsilon the
// highest-Weight arm (ties broken by first occurrence), with probability
// Epsilon a uniformly random arm. An empty arms slice returns (nil, nil) —
// there is nothing to pick and the caller (route bias) treats nil as "no
// bias". Select does not touch the repo (the caller already loaded the arms);
// it is a pure function of arms + the injected random source.
func (l *Learner) Select(ctx context.Context, tenantID int64, arms []Arm) (*Arm, error) {
	if len(arms) == 0 {
		return nil, nil
	}
	if l.rand == nil {
		l.rand = rand.Float64
	}

	if l.rand() < l.cfg.Epsilon {
		// Explore: uniformly random index. Use a second draw scaled to
		// len(arms); clamp the top edge so rand()==1-epsilon never overflows.
		idx := int(l.rand() * float64(len(arms)))
		if idx >= len(arms) {
			idx = len(arms) - 1
		}
		a := arms[idx]
		return &a, nil
	}

	// Exploit: argmax Weight, first occurrence wins on ties.
	best := 0
	for i := 1; i < len(arms); i++ {
		if arms[i].Weight > arms[best].Weight {
			best = i
		}
	}
	a := arms[best]
	return &a, nil
}

// Update applies one observed reward to ruleID (CONTRACT §14.3.3): clip the
// reward to [0,1], seed an unseen arm at Weight 0.5, increment Trials, add to
// RewardSum, then soft-update Weight toward the incremental mean
// q = RewardSum/Trials (Weight = Weight*(1-LR) + q*LR). The updated Arm is
// persisted via Put and returned. A nil repo reports ErrParam; an empty
// ruleID reports ErrParam (an arm key is mandatory).
func (l *Learner) Update(ctx context.Context, tenantID int64, ruleID string, reward float64) (*Arm, error) {
	if l.repo == nil {
		return nil, ErrParam
	}
	if ruleID == "" {
		return nil, ErrParam
	}

	r := clipReward(reward)

	arm, err := l.repo.Get(ctx, tenantID, ruleID)
	if err != nil {
		return nil, err
	}
	if arm == nil {
		// Defensive: the Repo contract guarantees a zero-value Arm{Weight:0.5}
		// for an unseen rule, but guard against a non-conforming implementation.
		arm = &Arm{RuleID: ruleID, Weight: neutralPrior}
	}
	if arm.RuleID == "" {
		arm.RuleID = ruleID
	}

	arm.Trials++
	arm.RewardSum += r
	q := arm.RewardSum / float64(arm.Trials)
	arm.Weight = arm.Weight*(1-l.cfg.LR) + q*l.cfg.LR

	if err := l.repo.Put(ctx, tenantID, arm); err != nil {
		return nil, err
	}
	return arm, nil
}

// CreditItem is the one-stop feedback path (CONTRACT §14.3.3): Resolve the
// item to (ruleID, strategyID, storeID), read its normalised reward from the
// RewardSource, then Update that arm. It is shared by the pregen
// bandit-update cron step and the strategy_feedback loop (the main thread
// wires both). If either port is nil (dev with no attribution data), it
// returns (nil, nil) without error — crediting is skipped safely.
func (l *Learner) CreditItem(ctx context.Context, tenantID, itemID int64) (*Arm, error) {
	if l.resolver == nil || l.rewardSource == nil {
		return nil, nil
	}

	ruleID, strategyID, storeID, err := l.resolver.Resolve(ctx, tenantID, itemID)
	if err != nil {
		return nil, err
	}
	reward, err := l.rewardSource.Reward(ctx, tenantID, itemID, strategyID, storeID)
	if err != nil {
		return nil, err
	}
	return l.Update(ctx, tenantID, ruleID, reward)
}

// Weights returns the current arm weights for one tenant as a ruleID→Weight
// map (M3-S3 §14.7 integration: the pipeline priority boost reads this to
// re-rank routed items by learned Q value). The map reflects every persisted
// arm; unseen rules are absent (the caller treats absence as the 0.5 neutral
// prior). A nil repo returns (nil, nil) so a dev build with no bandit storage
// skips boosting safely. This is a read-only operation — it never calls
// Select's exploration path.
func (l *Learner) Weights(ctx context.Context, tenantID int64) (map[string]float64, error) {
	if l == nil || l.repo == nil {
		return nil, nil
	}
	arms, err := l.repo.All(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(arms))
	for _, a := range arms {
		out[a.RuleID] = a.Weight
	}
	return out, nil
}
