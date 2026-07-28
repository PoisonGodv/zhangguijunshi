package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// ---- FakeEmbedder tests ----

func TestFakeEmbedder_Dim(t *testing.T) {
	emb := FakeEmbedder{}
	if got := emb.Dim(); got != 1024 {
		t.Fatalf("Dim() = %d, want 1024", got)
	}
}

func TestFakeEmbedder_Deterministic(t *testing.T) {
	emb := FakeEmbedder{}
	ctx := context.Background()
	a, err := emb.Embed(ctx, []string{"hello world"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	b, err := emb.Embed(ctx, []string{"hello world"})
	if err != nil {
		t.Fatalf("Embed second: %v", err)
	}
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected 1 vector each, got %d / %d", len(a), len(b))
	}
	if !vectorsEqual(a[0], b[0]) {
		t.Fatal("same text must yield identical vector (deterministic)")
	}
}

func TestFakeEmbedder_DifferentTextsDiffer(t *testing.T) {
	emb := FakeEmbedder{}
	ctx := context.Background()
	out, err := emb.Embed(ctx, []string{"apple", "banana"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(out))
	}
	if vectorsEqual(out[0], out[1]) {
		t.Fatal("different texts must yield different vectors")
	}
}

func TestFakeEmbedder_Normalized(t *testing.T) {
	emb := FakeEmbedder{}
	ctx := context.Background()
	out, err := emb.Embed(ctx, []string{"normalization check", "another text", ""})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i, v := range out {
		if len(v) != EmbedDim {
			t.Fatalf("vector %d has dim %d, want %d", i, len(v), EmbedDim)
		}
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		norm := math.Sqrt(sum)
		if math.Abs(norm-1.0) > 1e-5 {
			t.Fatalf("vector %d L2 norm = %v, want 1.0", i, norm)
		}
	}
}

func TestFakeEmbedder_EmptyInput(t *testing.T) {
	emb := FakeEmbedder{}
	out, err := emb.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed(nil): %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty slice, got %d", len(out))
	}
}

// ---- HTTPEmbedder tests ----

func TestHTTPEmbedder_HappyPath(t *testing.T) {
	var gotBody embeddingRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, embeddingsPath) {
			t.Errorf("path = %q, want suffix %q", r.URL.Path, embeddingsPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer KEY" {
			t.Errorf("auth = %q", got)
		}
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &gotBody)
		// Return 2 vectors of dim 1024.
		vectors := make([][]float32, len(gotBody.Input))
		for i := range vectors {
			vectors[i] = make([]float32, EmbedDim)
			vectors[i][0] = float32(i + 1)
		}
		resp := embeddingResponse{}
		resp.Data = make([]struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}, len(vectors))
		for i, v := range vectors {
			resp.Data[i].Embedding = v
			resp.Data[i].Index = i
		}
		out, _ := json.Marshal(resp)
		_, _ = w.Write(out)
	}))
	defer srv.Close()

	e := NewHTTPEmbedder(srv.URL, "KEY", "text-embedding-v3")
	out, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(out))
	}
	if len(out[0]) != EmbedDim || len(out[1]) != EmbedDim {
		t.Fatalf("dim mismatch: %d / %d", len(out[0]), len(out[1]))
	}
	if gotBody.Model != "text-embedding-v3" {
		t.Fatalf("model = %q", gotBody.Model)
	}
	if len(gotBody.Input) != 2 || gotBody.Input[0] != "a" || gotBody.Input[1] != "b" {
		t.Fatalf("input = %+v", gotBody.Input)
	}
}

func TestHTTPEmbedder_4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad model","type":"invalid_request","code":400}}`))
	}))
	defer srv.Close()

	e := NewHTTPEmbedder(srv.URL, "KEY", "x")
	_, err := e.Embed(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("expected HTTP 400 error, got %v", err)
	}
}

func TestHTTPEmbedder_DimMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Return a vector with wrong dimension (3 instead of 1024).
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3],"index":0}]}`))
	}))
	defer srv.Close()

	e := NewHTTPEmbedder(srv.URL, "KEY", "x")
	_, err := e.Embed(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "dim") {
		t.Fatalf("expected dim mismatch error, got %v", err)
	}
}

func TestHTTPEmbedder_NoAPIKey(t *testing.T) {
	e := NewHTTPEmbedder("http://localhost", "", "x")
	_, err := e.Embed(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "no api key") {
		t.Fatalf("expected no api key error, got %v", err)
	}
}

func TestHTTPEmbedder_CountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":` + zerosJSON(EmbedDim) + `,"index":0}]}`))
	}))
	defer srv.Close()

	e := NewHTTPEmbedder(srv.URL, "KEY", "x")
	_, err := e.Embed(context.Background(), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "want 2") {
		t.Fatalf("expected count mismatch error, got %v", err)
	}
}

func TestHTTPEmbedder_Dim(t *testing.T) {
	e := NewHTTPEmbedder("http://localhost", "KEY", "x")
	if e.Dim() != EmbedDim {
		t.Fatalf("Dim() = %d, want %d", e.Dim(), EmbedDim)
	}
}

func TestHTTPEmbedder_DefaultModel(t *testing.T) {
	var gotBody embeddingRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(buf, &gotBody)
		v := make([]float32, EmbedDim)
		resp := embeddingResponse{}
		resp.Data = []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}{{Embedding: v, Index: 0}}
		buf, _ = json.Marshal(resp)
		_, _ = w.Write(buf)
	}))
	defer srv.Close()

	e := NewHTTPEmbedder(srv.URL, "KEY", "") // empty model → default
	_, _ = e.Embed(context.Background(), []string{"a"})
	if gotBody.Model != defaultEmbeddingModel {
		t.Fatalf("model = %q, want %q", gotBody.Model, defaultEmbeddingModel)
	}
}

// ---- NewEmbedderFromConfig tests ----

func TestNewEmbedderFromConfig_NoKey_FakeEmbedder(t *testing.T) {
	// No providers → FakeEmbedder.
	emb := NewEmbedderFromConfig(config.AIConfig{})
	if _, ok := emb.(FakeEmbedder); !ok {
		t.Fatalf("expected FakeEmbedder, got %T", emb)
	}
	// Providers present but none has a key → FakeEmbedder.
	emb = NewEmbedderFromConfig(config.AIConfig{
		Providers: map[string]config.AIProviderConfig{
			"qwen": {BaseURL: "http://x", APIKey: ""},
		},
	})
	if _, ok := emb.(FakeEmbedder); !ok {
		t.Fatalf("expected FakeEmbedder when no key, got %T", emb)
	}
}

func TestNewEmbedderFromConfig_WithKey_HTTPEmbedder(t *testing.T) {
	emb := NewEmbedderFromConfig(config.AIConfig{
		DefaultProvider: "qwen",
		Providers: map[string]config.AIProviderConfig{
			"qwen": {BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", APIKey: "KEY", Model: "qwen-turbo"},
		},
	})
	he, ok := emb.(*HTTPEmbedder)
	if !ok {
		t.Fatalf("expected *HTTPEmbedder, got %T", emb)
	}
	if he.apiKey != "KEY" {
		t.Fatalf("apiKey = %q", he.apiKey)
	}
	if he.baseURL != "https://dashscope.aliyuncs.com/compatible-mode/v1" {
		t.Fatalf("baseURL = %q", he.baseURL)
	}
	// qwen-turbo is a chat model → falls back to defaultEmbeddingModel.
	if he.model != defaultEmbeddingModel {
		t.Fatalf("model = %q, want %q", he.model, defaultEmbeddingModel)
	}
}

func TestNewEmbedderFromConfig_EmbeddingModelDetected(t *testing.T) {
	emb := NewEmbedderFromConfig(config.AIConfig{
		DefaultProvider: "qwen",
		Providers: map[string]config.AIProviderConfig{
			"qwen": {BaseURL: "http://x", APIKey: "KEY", Model: "text-embedding-v3"},
		},
	})
	he, ok := emb.(*HTTPEmbedder)
	if !ok {
		t.Fatalf("expected *HTTPEmbedder, got %T", emb)
	}
	if he.model != "text-embedding-v3" {
		t.Fatalf("model = %q, want text-embedding-v3", he.model)
	}
}

func TestNewEmbedderFromConfig_BgeModelDetected(t *testing.T) {
	emb := NewEmbedderFromConfig(config.AIConfig{
		Providers: map[string]config.AIProviderConfig{
			"local": {BaseURL: "http://x", APIKey: "KEY", Model: "bge-m3"},
		},
	})
	he, ok := emb.(*HTTPEmbedder)
	if !ok {
		t.Fatalf("expected *HTTPEmbedder, got %T", emb)
	}
	if he.model != "bge-m3" {
		t.Fatalf("model = %q, want bge-m3", he.model)
	}
}

// ---- NewFromConfig Routes tests ----

func TestNewFromConfig_Routes(t *testing.T) {
	cfg := config.AIConfig{
		DefaultProvider: "doubao",
		Providers: map[string]config.AIProviderConfig{
			"doubao":   {BaseURL: "http://x", APIKey: "KEY", Model: "doubao-lite-32k"},
			"deepseek": {BaseURL: "http://y", APIKey: "KEY", Model: "deepseek-chat"},
		},
		MonthlyBudgetCNY: 100,
		Routes: map[string]config.AIRouteConfig{
			"lite": {Provider: "doubao", Model: "doubao-lite-32k"},
			"v3":   {Provider: "deepseek", Model: "deepseek-chat"},
			"r1":   {Provider: "deepseek", Model: "deepseek-reasoner"},
		},
	}
	g, err := NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	wantLite := Route{Provider: "doubao", Model: "doubao-lite-32k"}
	wantV3 := Route{Provider: "deepseek", Model: "deepseek-chat"}
	wantR1 := Route{Provider: "deepseek", Model: "deepseek-reasoner"}
	if g.routes[TierLite] != wantLite {
		t.Fatalf("lite route = %+v, want %+v", g.routes[TierLite], wantLite)
	}
	if g.routes[TierV3] != wantV3 {
		t.Fatalf("v3 route = %+v, want %+v", g.routes[TierV3], wantV3)
	}
	if g.routes[TierR1] != wantR1 {
		t.Fatalf("r1 route = %+v, want %+v", g.routes[TierR1], wantR1)
	}

	// resolve returns the right provider+model for each tier.
	p, model, err := g.resolve(TierLite)
	if err != nil {
		t.Fatalf("resolve lite: %v", err)
	}
	if p.Name() != "doubao" || model != "doubao-lite-32k" {
		t.Fatalf("lite resolve = %s/%s", p.Name(), model)
	}
	p, model, err = g.resolve(TierV3)
	if err != nil {
		t.Fatalf("resolve v3: %v", err)
	}
	if p.Name() != "deepseek" || model != "deepseek-chat" {
		t.Fatalf("v3 resolve = %s/%s", p.Name(), model)
	}
	p, model, err = g.resolve(TierR1)
	if err != nil {
		t.Fatalf("resolve r1: %v", err)
	}
	if p.Name() != "deepseek" || model != "deepseek-reasoner" {
		t.Fatalf("r1 resolve = %s/%s", p.Name(), model)
	}
}

func TestNewFromConfig_RoutesUnknownKeySkipped(t *testing.T) {
	cfg := config.AIConfig{
		DefaultProvider: "doubao",
		Providers: map[string]config.AIProviderConfig{
			"doubao": {BaseURL: "http://x", APIKey: "KEY", Model: "doubao-lite-32k"},
		},
		MonthlyBudgetCNY: 100,
		Routes: map[string]config.AIRouteConfig{
			"lite":   {Provider: "doubao", Model: "doubao-lite-32k"},
			"future": {Provider: "doubao", Model: "mystery"}, // unknown key → skipped
		},
	}
	g, err := NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if _, ok := g.routes[TierLite]; !ok {
		t.Fatal("lite route should be configured")
	}
	// Unknown key must not create a route.
	if len(g.routes) != 1 {
		t.Fatalf("expected exactly 1 route (lite), got %d: %+v", len(g.routes), g.routes)
	}
}

func TestNewFromConfig_NoRoutes_BackwardCompatible(t *testing.T) {
	// No Routes → M3-S1 behavior: routes map is empty, defaultRoute used.
	cfg := config.AIConfig{
		DefaultProvider: "doubao",
		Providers: map[string]config.AIProviderConfig{
			"doubao": {BaseURL: "http://x", APIKey: "KEY", Model: "doubao-lite-32k"},
		},
		MonthlyBudgetCNY: 100,
	}
	g, err := NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if len(g.routes) != 0 {
		t.Fatalf("expected 0 routes, got %d", len(g.routes))
	}
	// Default tier resolves to defaultRoute.
	p, model, err := g.resolve("")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if p.Name() != "doubao" || model != "doubao-lite-32k" {
		t.Fatalf("default resolve = %s/%s", p.Name(), model)
	}
	// TierLite also falls back to defaultRoute (no explicit route).
	p, model, err = g.resolve(TierLite)
	if err != nil {
		t.Fatalf("resolve lite (no route): %v", err)
	}
	if p.Name() != "doubao" {
		t.Fatalf("lite fallback resolve = %s, want doubao", p.Name())
	}
}

func TestNewFromConfig_RoutesPlaceholderGateway(t *testing.T) {
	// No providers but Routes set: placeholder gateway still wires routes
	// (Chat returns ErrNoProvider, but routes map is populated).
	cfg := config.AIConfig{
		MonthlyBudgetCNY: 100,
		Routes: map[string]config.AIRouteConfig{
			"lite": {Provider: "doubao", Model: "doubao-lite-32k"},
			"v3":   {Provider: "deepseek", Model: "deepseek-chat"},
		},
	}
	g, err := NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if len(g.routes) != 2 {
		t.Fatalf("expected 2 routes on placeholder, got %d", len(g.routes))
	}
	if g.routes[TierLite] != (Route{Provider: "doubao", Model: "doubao-lite-32k"}) {
		t.Fatalf("lite route = %+v", g.routes[TierLite])
	}
	if g.routes[TierV3] != (Route{Provider: "deepseek", Model: "deepseek-chat"}) {
		t.Fatalf("v3 route = %+v", g.routes[TierV3])
	}
	// Chat still returns ErrNoProvider (no providers registered).
	_, err = g.Chat(context.Background(), 1, TierLite, nil, Opts{})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("expected ErrNoProvider, got %v", err)
	}
}

// ---- helpers ----

func vectorsEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// zerosJSON returns a JSON array of n zero floats, e.g. "[0,0,0]".
func zerosJSON(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "0"
	}
	return "[" + strings.Join(parts, ",") + "]"
}
