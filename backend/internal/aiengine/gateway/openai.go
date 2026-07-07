package gateway

// openai.go implements the shared OpenAI-compatible chat client used by every
// concrete provider. Qwen/DeepSeek/Doubao all expose /chat/completions and
// differ only in BaseURL, APIKey and default model — so one client + three
// constructors cover the lot (tech plan §7.8.1).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// chatCompletionsPath is appended to each provider's BaseURL.
const chatCompletionsPath = "/chat/completions"

// OpenAIProvider is a single OpenAI-compatible LLM provider.
type OpenAIProvider struct {
	name         string
	baseURL      string
	apiKey       string
	defaultModel string
	pricing      Pricing
	http         *http.Client
	timeout      time.Duration
}

// ProviderCfg is the caller-supplied config for a provider constructor.
type ProviderCfg struct {
	APIKey  string
	Model   string
	BaseURL string // optional override; constructor fills a provider default
	HTTP    *http.Client
	Timeout time.Duration
}

func newOpenAIProvider(name, defaultBase string, cfg ProviderCfg) *OpenAIProvider {
	base := cfg.BaseURL
	if base == "" {
		base = defaultBase
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	h := cfg.HTTP
	if h == nil {
		h = &http.Client{Timeout: timeout}
	}
	return &OpenAIProvider{
		name:         name,
		baseURL:      base,
		apiKey:       cfg.APIKey,
		defaultModel: cfg.Model,
		pricing:      pricingFor(cfg.Model),
		http:         h,
		timeout:      timeout,
	}
}

// NewQwen builds the Alibaba DashScope (通义/百炼) provider in OpenAI-compat mode.
func NewQwen(cfg ProviderCfg) *OpenAIProvider {
	return newOpenAIProvider("qwen",
		"https://dashscope.aliyuncs.com/compatible-mode/v1", cfg) // 需核实
}

// NewDeepSeek builds the DeepSeek provider (V3/R1).
func NewDeepSeek(cfg ProviderCfg) *OpenAIProvider {
	return newOpenAIProvider("deepseek", "https://api.deepseek.com/v1", cfg) // 需核实
}

// NewDoubao builds the Volcano Ark (豆包) provider.
func NewDoubao(cfg ProviderCfg) *OpenAIProvider {
	return newOpenAIProvider("doubao",
		"https://ark.cn-beijing.volces.com/api/v3", cfg) // 需核实; model is an endpoint id
}

// Name implements Provider.
func (p *OpenAIProvider) Name() string { return p.name }

// Chat implements Provider.
func (p *OpenAIProvider) Chat(ctx context.Context, msgs []Message, opts Opts) (Resp, error) {
	if p.apiKey == "" {
		return Resp{}, fmt.Errorf("gateway: provider %s has no api key", p.name)
	}
	model := opts.Model
	if model == "" {
		model = p.defaultModel
	}
	if model == "" {
		return Resp{}, fmt.Errorf("gateway: provider %s has no model", p.name)
	}

	body := chatRequest{
		Model:    model,
		Messages: msgs,
	}
	if opts.Temperature > 0 {
		body.Temperature = opts.Temperature
	}
	if opts.MaxTokens > 0 {
		body.MaxTokens = opts.MaxTokens
	}
	if opts.ResponseFormat != nil {
		body.ResponseFormat = opts.ResponseFormat
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return Resp{}, fmt.Errorf("gateway: marshal request: %w", err)
	}

	// Bind the timeout to the context so cancellation propagates upstream.
	reqCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.baseURL+chatCompletionsPath, bytes.NewReader(buf))
	if err != nil {
		return Resp{}, fmt.Errorf("gateway: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Resp{}, fmt.Errorf("%w: %v", ErrUpstreamTimeout, err)
		}
		return Resp{}, fmt.Errorf("gateway: call %s: %w", p.name, err)
	}
	defer resp.Body.Close()

	var parsed chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Resp{}, fmt.Errorf("gateway: decode %s response (HTTP %d): %w", p.name, resp.StatusCode, err)
	}
	if resp.StatusCode >= 400 {
		// 5xx / 429 are retryable upstream faults; surface distinctly.
		return Resp{}, fmt.Errorf("gateway: %s HTTP %d: %s",
			p.name, resp.StatusCode, parsed.ErrorString())
	}
	if len(parsed.Choices) == 0 {
		return Resp{}, fmt.Errorf("gateway: %s returned no choices", p.name)
	}
	pr := pricingFor(model)
	cost := pr.CostCNY(parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens)
	return Resp{
		Content:          parsed.Choices[0].Message.Content,
		FinishReason:     parsed.Choices[0].FinishReason,
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
		TotalTokens:      parsed.Usage.TotalTokens,
		CostCNY:          cost,
	}, nil
}

// chatRequest is the OpenAI /chat/completions body.
type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Temperature    float32         `json:"temperature,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// chatResponse is the subset of the OpenAI response the gateway needs.
type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

// ErrorString returns a human-readable error fragment if the provider returned
// an error object, else a generic note.
func (r chatResponse) ErrorString() string {
	if r.Error != nil {
		return fmt.Sprintf("%s: %v", r.Error.Type, r.Error.Message)
	}
	return "no error object"
}
