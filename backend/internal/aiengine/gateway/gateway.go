package gateway

// gateway.go ties providers together with tier routing and the per-tenant
// budget circuit-breaker (tech plan §7.8.2–7.8.4). It is the surface the AI
// pipeline calls; the pipeline itself (Signal→Rule→LLM→Converge→Schema) lives
// in the Python demo for now and will be ported into this monolith in M3.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// Route selects which provider+model serves a task tier.
type Route struct {
	Provider string // provider name (qwen/deepseek/doubao)
	Model    string // model id; empty -> provider default
}

// Gateway is the LLM entry point. All methods are safe for concurrent use.
type Gateway struct {
	providers    map[string]Provider
	routes       map[TaskTier]Route
	defaultRoute Route
	budget       Budget
	timeout      time.Duration
}

// Option configures a Gateway.
type Option func(*Gateway)

// WithRoute binds a task tier to a provider+model.
func WithRoute(tier TaskTier, r Route) Option {
	return func(g *Gateway) { g.routes[tier] = r }
}

// WithBudget injects a Budget (memory or redis). Defaults to a memory budget.
func WithBudget(b Budget) Option {
	return func(g *Gateway) { g.budget = b }
}

// WithTimeout sets the default per-call timeout.
func WithTimeout(d time.Duration) Option {
	return func(g *Gateway) {
		if d > 0 {
			g.timeout = d
		}
	}
}

// New builds a Gateway from one or more providers plus Options.
func New(providers []Provider, defaultRoute Route, opts ...Option) (*Gateway, error) {
	if len(providers) == 0 {
		return nil, errors.New("gateway: at least one provider is required")
	}
	g := &Gateway{
		providers:    make(map[string]Provider, len(providers)),
		routes:       make(map[TaskTier]Route),
		defaultRoute: defaultRoute,
		budget:       NewMemoryBudget(0),
		timeout:      8 * time.Second,
	}
	for _, p := range providers {
		g.providers[p.Name()] = p
	}
	if _, ok := g.providers[defaultRoute.Provider]; !ok {
		return nil, fmt.Errorf("gateway: default provider %q not registered", defaultRoute.Provider)
	}
	for _, opt := range opts {
		opt(g)
	}
	return g, nil
}

// Chat issues a single completion for a tenant + tier, enforcing the budget.
// On budget exhaustion it returns ErrBudgetExceeded (caller MUST degrade to
// rules+cache). On upstream error it returns the wrapped error so the caller
// can retry or fall back.
func (g *Gateway) Chat(ctx context.Context, tenantID int64, tier TaskTier, msgs []Message, opts Opts) (Resp, error) {
	provider, model, err := g.resolve(tier)
	if err != nil {
		return Resp{}, err
	}
	exceeded, err := g.budget.Exceeded(ctx, tenantID)
	if err != nil {
		return Resp{}, err
	}
	if exceeded {
		return Resp{}, ErrBudgetExceeded
	}
	if opts.Timeout <= 0 && g.timeout > 0 {
		opts.Timeout = g.timeout
	}
	if opts.Model == "" {
		opts.Model = model
	}
	resp, err := provider.Chat(ctx, msgs, opts)
	if err != nil {
		return Resp{}, err
	}
	// Account cost only on success (failed calls cost nothing to the tenant).
	if resp.CostCNY > 0 {
		if aerr := g.budget.AddCost(ctx, tenantID, resp.CostCNY); aerr != nil {
			// Cost accounting failure must NOT block the result; log upstream.
			_ = aerr
		}
	}
	return resp, nil
}

// ChatWithFallback tries routes in order; the first non-budget, non-timeout
// error moves to the next route (tech plan §7.8.3: lite → V3 → degrade). It
// returns the first success, or the last error if all routes fail. Budget
// exhaustion short-circuits immediately (degrade to rules is the caller's job).
func (g *Gateway) ChatWithFallback(ctx context.Context, tenantID int64, tiers []TaskTier, msgs []Message, opts Opts) (Resp, error) {
	if len(tiers) == 0 {
		return Resp{}, errors.New("gateway: no tiers to try")
	}
	var lastErr error
	for _, tier := range tiers {
		// Budget check before each attempt (a prior call may have tipped over).
		if exceeded, err := g.budget.Exceeded(ctx, tenantID); err != nil {
			return Resp{}, err
		} else if exceeded {
			return Resp{}, ErrBudgetExceeded
		}
		resp, err := g.Chat(ctx, tenantID, tier, msgs, opts)
		if err == nil {
			return resp, nil
		}
		if errors.Is(err, ErrBudgetExceeded) {
			return Resp{}, err
		}
		lastErr = err
	}
	return Resp{}, lastErr
}

// resolve picks the provider+model for a tier, falling back to defaultRoute.
func (g *Gateway) resolve(tier TaskTier) (Provider, string, error) {
	r := g.defaultRoute
	if tier != "" {
		if routed, ok := g.routes[tier]; ok {
			r = routed
		}
	}
	p, ok := g.providers[r.Provider]
	if !ok {
		return nil, "", fmt.Errorf("gateway: %w for tier %q (provider %q)", ErrNoProvider, tier, r.Provider)
	}
	return p, r.Model, nil
}

// NewFromConfig builds a Gateway from the AI section of app config. Providers
// with an empty APIKey are skipped (so the gateway still builds in dev without
// keys; callers get ErrNoProvider when they hit an unconfigured tier).
func NewFromConfig(cfg config.AIConfig, opts ...Option) (*Gateway, error) {
	var providers []Provider
	for name, pc := range cfg.Providers {
		if pc.APIKey == "" {
			continue
		}
		p, err := buildProvider(name, pc)
		if err != nil {
			return nil, fmt.Errorf("gateway: provider %s: %w", name, err)
		}
		providers = append(providers, p)
	}
	def := cfg.DefaultProvider
	if def == "" && len(providers) > 0 {
		def = providers[0].Name()
	}
	// Default model: take from the configured provider, else empty.
	var defModel string
	if pc, ok := cfg.Providers[def]; ok {
		defModel = pc.Model
	}
	if def == "" {
		// No providers configured: build a placeholder gateway so the binary
		// still boots; Chat will return ErrNoProvider.
		return &Gateway{
			providers: map[string]Provider{},
			routes:    map[TaskTier]Route{},
			budget:    NewMemoryBudget(cfg.MonthlyBudgetCNY),
			timeout:   timeoutOr(cfg.RequestTimeout, 8*time.Second),
		}, nil
	}
	extra := append([]Option{WithBudget(NewMemoryBudget(cfg.MonthlyBudgetCNY)), WithTimeout(timeoutOr(cfg.RequestTimeout, 8*time.Second))}, opts...)
	return New(providers, Route{Provider: def, Model: defModel}, extra...)
}

func buildProvider(name string, pc config.AIProviderConfig) (Provider, error) {
	cfg := ProviderCfg{APIKey: pc.APIKey, Model: pc.Model, BaseURL: pc.BaseURL}
	switch name {
	case "qwen":
		return NewQwen(cfg), nil
	case "deepseek":
		return NewDeepSeek(cfg), nil
	case "doubao":
		return NewDoubao(cfg), nil
	default:
		// Generic OpenAI-compatible provider for anything else.
		return newOpenAIProvider(name, pc.BaseURL, cfg), nil
	}
}

func timeoutOr(d time.Duration, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// DifyStub documents the deliberate M0-S2 deferral of Dify (tech plan §7.5).
// The plan decided NOT to introduce Dify as a 0→1 dependency: the Go pipeline
// (in-process, low latency, margin guard embedded directly) beats a separate
// Dify container with cross-process guard placement. Dify remains available as
// a remote experiment for complex multi-step agents; once a stable flow is
// found it is re-cast into the Go pipeline. Do not remove this symbol until a
// concrete Dify experiment is actually built — it carries the architectural
// rationale for reviewers.
type DifyStub struct{}
