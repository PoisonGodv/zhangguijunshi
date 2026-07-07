package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// testAIConfig builds an AIConfig for tests.
func testAIConfig(def string, providers map[string]config.AIProviderConfig) config.AIConfig {
	return config.AIConfig{
		DefaultProvider:  def,
		Providers:        providers,
		MonthlyBudgetCNY: 100,
	}
}

func TestNewFromConfig_BuildsProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	cfg := testAIConfig("doubao", map[string]config.AIProviderConfig{
		"doubao": {BaseURL: srv.URL, APIKey: "KEY", Model: "doubao-lite-32k"},
	})
	g, err := NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	r, err := g.Chat(context.Background(), 1, "", []Message{{Role: RoleUser, Content: "hi"}}, Opts{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if r.Content != "ok" {
		t.Fatalf("content = %s", r.Content)
	}
}

// fakeProvider is a controllable Provider for gateway-level tests.
type fakeProvider struct {
	name  string
	resp  Resp
	err   error
	calls int
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Chat(_ context.Context, _ []Message, _ Opts) (Resp, error) {
	f.calls++
	return f.resp, f.err
}

func TestGateway_ResolveAndRouting(t *testing.T) {
	lite := &fakeProvider{name: "doubao", resp: Resp{Content: "lite"}}
	v3 := &fakeProvider{name: "deepseek", resp: Resp{Content: "v3"}}
	g, err := New([]Provider{lite, v3}, Route{Provider: "doubao"},
		WithRoute(TierV3, Route{Provider: "deepseek"}),
		WithBudget(NewMemoryBudget(100)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Default tier -> doubao.
	r, err := g.Chat(context.Background(), 1, "", nil, Opts{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if r.Content != "lite" || lite.calls != 1 {
		t.Fatalf("expected lite, calls=%d", lite.calls)
	}
	// V3 tier -> deepseek.
	r, err = g.Chat(context.Background(), 1, TierV3, nil, Opts{})
	if err != nil {
		t.Fatalf("Chat V3: %v", err)
	}
	if r.Content != "v3" {
		t.Fatalf("expected v3, got %s", r.Content)
	}
}

func TestGateway_BudgetExceeded(t *testing.T) {
	p := &fakeProvider{name: "doubao", resp: Resp{CostCNY: 1.0}}
	g, _ := New([]Provider{p}, Route{Provider: "doubao"}, WithBudget(NewMemoryBudget(1.5)))
	ctx := context.Background()

	if _, err := g.Chat(ctx, 7, "", nil, Opts{}); err != nil {
		t.Fatalf("first call: %v", err)
	} // spent 1.0
	if _, err := g.Chat(ctx, 7, "", nil, Opts{}); err != nil {
		t.Fatalf("second call: %v", err)
	} // spent 2.0
	// Third call blocked by budget.
	_, err := g.Chat(ctx, 7, "", nil, Opts{})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got %v", err)
	}
}

func TestGateway_FallbackTriesNextTier(t *testing.T) {
	lite := &fakeProvider{name: "doubao", err: errors.New("upstream 500")}
	v3 := &fakeProvider{name: "deepseek", resp: Resp{Content: "recovered"}}
	g, _ := New([]Provider{lite, v3}, Route{Provider: "doubao"},
		WithRoute(TierV3, Route{Provider: "deepseek"}),
		WithBudget(NewMemoryBudget(100)),
	)

	r, err := g.ChatWithFallback(context.Background(), 1, []TaskTier{TierLite, TierV3}, nil, Opts{})
	if err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	if r.Content != "recovered" || lite.calls != 1 || v3.calls != 1 {
		t.Fatalf("fallback wrong: r=%+v lite=%d v3=%d", r, lite.calls, v3.calls)
	}
}

func TestGateway_FallbackAllFail(t *testing.T) {
	lite := &fakeProvider{name: "doubao", err: errors.New("e1")}
	v3 := &fakeProvider{name: "deepseek", err: errors.New("e2")}
	g, _ := New([]Provider{lite, v3}, Route{Provider: "doubao"},
		WithRoute(TierV3, Route{Provider: "deepseek"}),
		WithBudget(NewMemoryBudget(100)),
	)
	_, err := g.ChatWithFallback(context.Background(), 1, []TaskTier{TierLite, TierV3}, nil, Opts{})
	if err == nil || err.Error() != "e2" {
		t.Fatalf("expected last error e2, got %v", err)
	}
}

func TestNewFromConfig_NoProviders(t *testing.T) {
	// No providers configured -> placeholder gateway; Chat -> ErrNoProvider.
	g, err := NewFromConfig(testAIConfig("", nil))
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	_, err = g.Chat(context.Background(), 1, "", nil, Opts{})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("expected ErrNoProvider, got %v", err)
	}
}
