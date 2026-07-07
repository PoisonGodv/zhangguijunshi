package gateway

// Pricing holds the per-million-token RMB rates used to estimate call cost and
// drive the per-tenant budget circuit-breaker (tech plan §7.8.4). The single
// most important property is that Lite-tier calls stay under ¥0.001 each so the
// ¥2/tenant/month ceiling leaves headroom (tech plan §7.8.3).
//
// ⚠️ All rates are PLACEHOLDERS reflecting approximate 2026 public list prices.
// They MUST be re-verified against each provider's current rate card before any
// billing or hard circuit-breaking depends on them. See VerifyPricing.

// Pricing is per 1,000,000 tokens, in RMB.
type Pricing struct {
	InputPerMillion  float64
	OutputPerMillion float64
}

// CostCNY estimates the RMB cost of a call given token usage.
func (p Pricing) CostCNY(promptTokens, completionTokens int) float64 {
	return p.InputPerMillion*float64(promptTokens)/1e6 +
		p.OutputPerMillion*float64(completionTokens)/1e6
}

// defaultPricing maps model -> approximate pricing. Verified-against-official
// entries belong in a versioned config; these are conservative placeholders.
//
// Sources to verify (as of knowledge cutoff, confirm before prod):
//   - DeepSeek: https://api-docs.deepseek.com/quick_start/pricing
//   - Doubao (Volcano Ark): https://www.volcengine.com/docs/82379
//   - Qwen (DashScope): https://help.aliyun.com/zh/dashscope/product-overview/billing
var defaultPricing = map[string]Pricing{
	// Doubao lite — cheapest, primary Lite tier.
	"doubao-lite-32k": {InputPerMillion: 0.3, OutputPerMillion: 0.6},
	"doubao-pro-32k":  {InputPerMillion: 0.8, OutputPerMillion: 2.0},
	// DeepSeek.
	"deepseek-chat":     {InputPerMillion: 2.0, OutputPerMillion: 8.0},  // V3
	"deepseek-reasoner": {InputPerMillion: 4.0, OutputPerMillion: 16.0}, // R1
	// Qwen (DashScope compatible mode).
	"qwen-turbo": {InputPerMillion: 0.3, OutputPerMillion: 0.6},
	"qwen-plus":  {InputPerMillion: 0.8, OutputPerMillion: 2.0},
}

// pricingFor returns the Pricing for model, falling back to a safe default
// (treat unknown models as mid-tier so cost is never under-counted).
func pricingFor(model string) Pricing {
	if p, ok := defaultPricing[model]; ok {
		return p
	}
	return Pricing{InputPerMillion: 1.0, OutputPerMillion: 4.0}
}
