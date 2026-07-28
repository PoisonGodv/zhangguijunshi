package gateway

// embedder.go implements the Embedder interface (M3-S2 §13.3.3) used by the
// semantic cache (internal/aiengine/semantic). It is intentionally a SEPARATE
// interface from Provider: chat completions and embeddings are different
// upstream endpoints with different request/response shapes, and the semantic
// cache only needs Embed — not Chat. Keeping them separate also means a dev
// box with no LLM key still gets a deterministic FakeEmbedder so the cache is
// exercisable end-to-end.
//
// Two implementations ship in this file:
//   - FakeEmbedder: deterministic FNV-hash → [1024]float32 (L2-normalised);
//     same text always yields the same vector. dev/test only.
//   - HTTPEmbedder: OpenAI-compatible /v1/embeddings client (net/http, no new
//     deps). Used in production with 通义 text-embedding-v3 / bge-m3.
//
// NewEmbedderFromConfig picks HTTPEmbedder when at least one provider has an
// API key, otherwise FakeEmbedder — mirroring the gateway.NewFromConfig
// "boot without keys" philosophy.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// EmbedDim is the fixed embedding dimension (bge-m3 / text-embedding-v3 = 1024).
// M3-S2 §13.5.6: embedding 维度固定 1024.
const EmbedDim = 1024

// embeddingsPath is appended to a provider's BaseURL (which already includes
// the /v1 segment, same convention as chatCompletionsPath in openai.go).
const embeddingsPath = "/embeddings"

// defaultEmbeddingModel is used when the provider config does not carry an
// embedding-specific model. 通义 text-embedding-v3 is the reference; bge-m3
// deployments use the same OpenAI-compatible shape.
const defaultEmbeddingModel = "text-embedding-v3"

// Embedder is the independent embedding interface (M3-S2 §13.3.3). It does NOT
// extend Provider — the semantic cache depends only on this surface.
type Embedder interface {
	// Embed returns one L2-normalised vector per input text. The returned slice
	// has the same length and order as texts; each vector has Dim() elements.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dim returns the embedding dimension (1024 for bge-m3 / text-embedding-v3).
	Dim() int
}

// ---- FakeEmbedder (dev/test) ----

// FakeEmbedder produces deterministic 1024-dim L2-normalised vectors from the
// FNV-1a hash of each text. Same text → same vector, so the semantic cache can
// hit identical signal texts in dev without any upstream dependency.
type FakeEmbedder struct{}

// Dim implements Embedder.
func (FakeEmbedder) Dim() int { return EmbedDim }

// Embed implements Embedder.
func (FakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = fakeVector(t, EmbedDim)
	}
	return out, nil
}

// fakeVector builds a deterministic dim-dim float32 vector from text and
// L2-normalises it. The FNV-1a hash seeds a local rand source (no global
// state, safe for concurrent use).
func fakeVector(text string, dim int) []float32 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	seed := int64(h.Sum64())
	// math/rand.New with a seeded Source is deterministic; the global rand
	// state is untouched, so this is safe under -race.
	r := rand.New(rand.NewSource(seed))
	v := make([]float32, dim)
	var sum float64
	for i := range v {
		// Gaussian samples give a better spread than uniform [0,1); they make
		// cosine similarity between different texts near-orthogonal.
		f := r.NormFloat64()
		v[i] = float32(f)
		sum += f * f
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		// Degenerate: all-zero (astronomically unlikely). Plant a unit basis
		// so the vector is still valid and normalised.
		v[0] = 1
		return v
	}
	inv := float32(1.0 / norm)
	for i := range v {
		v[i] *= inv
	}
	return v
}

// ---- HTTPEmbedder (production) ----

// HTTPEmbedder calls an OpenAI-compatible /v1/embeddings endpoint (通义
// DashScope compatible-mode / Volcano Ark / self-hosted bge-m3).
type HTTPEmbedder struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
	timeout time.Duration
}

// NewHTTPEmbedder builds an HTTPEmbedder. baseURL should include the /v1
// suffix (same convention as ProviderCfg.BaseURL). model empty →
// defaultEmbeddingModel.
func NewHTTPEmbedder(baseURL, apiKey, model string, opts ...HTTPEmbedderOption) *HTTPEmbedder {
	if model == "" {
		model = defaultEmbeddingModel
	}
	timeout := 15 * time.Second
	h := &http.Client{Timeout: timeout}
	e := &HTTPEmbedder{baseURL: baseURL, apiKey: apiKey, model: model, http: h, timeout: timeout}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// HTTPEmbedderOption configures an HTTPEmbedder.
type HTTPEmbedderOption func(*HTTPEmbedder)

// WithHTTPClient injects a custom *http.Client (mainly for tests with
// httptest, or to share a connection pool).
func WithHTTPClient(c *http.Client) HTTPEmbedderOption {
	return func(e *HTTPEmbedder) {
		if c != nil {
			e.http = c
		}
	}
}

// WithEmbedTimeout sets the per-call timeout.
func WithEmbedTimeout(d time.Duration) HTTPEmbedderOption {
	return func(e *HTTPEmbedder) {
		if d > 0 {
			e.timeout = d
		}
	}
}

// Dim implements Embedder.
func (e *HTTPEmbedder) Dim() int { return EmbedDim }

// Embed implements Embedder.
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if e.apiKey == "" {
		return nil, errors.New("gateway: embedder has no api key")
	}
	body := embeddingRequest{Model: e.model, Input: texts}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("gateway: marshal embed request: %w", err)
	}

	reqCtx := ctx
	if e.timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, e.baseURL+embeddingsPath, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("gateway: build embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)

	resp, err := e.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: embed: %v", ErrUpstreamTimeout, err)
		}
		return nil, fmt.Errorf("gateway: call embeddings: %w", err)
	}
	defer resp.Body.Close()

	var parsed embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("gateway: decode embeddings response (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gateway: embeddings HTTP %d: %s", resp.StatusCode, parsed.ErrorString())
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("gateway: embeddings returned %d vectors, want %d", len(parsed.Data), len(texts))
	}
	out := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		if len(d.Embedding) != EmbedDim {
			return nil, fmt.Errorf("gateway: embedding %d has dim %d, want %d", i, len(d.Embedding), EmbedDim)
		}
		out[i] = d.Embedding
	}
	return out, nil
}

// embeddingRequest is the OpenAI-compatible /v1/embeddings body.
type embeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embeddingResponse is the subset of the OpenAI embeddings response we need.
type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

// ErrorString returns a human-readable error fragment if the provider returned
// an error object, else a generic note.
func (r embeddingResponse) ErrorString() string {
	if r.Error != nil {
		return fmt.Sprintf("%s: %v", r.Error.Type, r.Error.Message)
	}
	return "no error object"
}

// ---- NewEmbedderFromConfig ----

// NewEmbedderFromConfig builds an Embedder from the AI config. If at least one
// provider has an API key, an HTTPEmbedder is returned pointed at that
// provider's BaseURL; otherwise a FakeEmbedder is returned so dev/test still
// works without credentials (M3-S2 §13.3.3).
func NewEmbedderFromConfig(cfg config.AIConfig) Embedder {
	name, pc := pickEmbeddingProvider(cfg)
	if name == "" || pc.APIKey == "" {
		return FakeEmbedder{}
	}
	return NewHTTPEmbedder(pc.BaseURL, pc.APIKey, embeddingModelFor(name, pc))
}

// pickEmbeddingProvider selects the provider to serve embeddings: the default
// provider if it has a key, else the first keyed provider. Returns ("", zero)
// when none qualify.
func pickEmbeddingProvider(cfg config.AIConfig) (string, config.AIProviderConfig) {
	if cfg.DefaultProvider != "" {
		if pc, ok := cfg.Providers[cfg.DefaultProvider]; ok && pc.APIKey != "" {
			return cfg.DefaultProvider, pc
		}
	}
	// Map iteration order is non-deterministic, but all keyed providers are
	// equivalent candidates for embeddings in POC; a production deployment
	// should set DefaultProvider to make the choice explicit.
	for name, pc := range cfg.Providers {
		if pc.APIKey != "" {
			return name, pc
		}
	}
	return "", config.AIProviderConfig{}
}

// embeddingModelFor picks the embedding model: if the provider config's Model
// looks like an embedding model (contains "embedding" or "bge"), use it;
// otherwise fall back to the provider's known embedding model or the default.
func embeddingModelFor(name string, pc config.AIProviderConfig) string {
	if pc.Model != "" && looksLikeEmbeddingModel(pc.Model) {
		return pc.Model
	}
	switch name {
	case "qwen":
		// 通义 text-embedding-v3 via DashScope compatible mode.
		return defaultEmbeddingModel
	case "doubao":
		// 豆包 embedding endpoint id is deployment-specific; use default.
		return defaultEmbeddingModel
	default:
		return defaultEmbeddingModel
	}
}

// looksLikeEmbeddingModel reports whether model is plausibly an embedding
// model (not a chat model). Used to avoid sending a chat model id to the
// embeddings endpoint.
func looksLikeEmbeddingModel(model string) bool {
	return strings.Contains(model, "embedding") || strings.Contains(model, "bge")
}
