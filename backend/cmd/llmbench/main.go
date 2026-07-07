// Command llmbench is the AI-01 cost/latency benchmark CLI (tech plan §7.8.3:
// "单次生成 <¥0.001 验证"). It fires N chat completions at a configured provider
// and prints per-call cost, latency distribution and a projected per-tenant
// monthly cost — the artifact that proves the ¥2/tenant/month ceiling has
// headroom before the pipeline depends on it.
//
// It needs a real API key and network, so it is NOT run from CI. Usage:
//
//	ZG_AI_QWEN_API_KEY=sk-... llmbench -provider qwen -model qwen-turbo -n 20
//	llmbench -provider doubao -base-url http://localhost:9999 -api-key KEY -model doubao-lite-32k
//
// Exit code is non-zero if the projected per-tenant monthly cost exceeds ¥2
// (the plan-book circuit-breaker threshold), so this can gate a release decision.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
)

func main() {
	provider := flag.String("provider", "doubao", "provider: qwen|deepseek|doubao")
	baseURL := flag.String("base-url", "", "override OpenAI-compatible base URL")
	apiKey := flag.String("api-key", "", "API key (or env ZG_AI_<PROVIDER>_API_KEY)")
	model := flag.String("model", "", "model id (provider default if empty)")
	n := flag.Int("n", 10, "number of iterations")
	maxTokens := flag.Int("max-tokens", 256, "max output tokens per call")
	timeout := flag.Duration("timeout", 15*time.Second, "per-call timeout")
	flag.Parse()

	key := *apiKey
	if key == "" {
		key = os.Getenv("ZG_AI_" + upper(*provider) + "_API_KEY")
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "fatal: no api key (pass -api-key or set ZG_AI_<PROVIDER>_API_KEY)")
		os.Exit(2)
	}
	if *n <= 0 {
		fatal("n must be positive")
	}

	p, err := buildProvider(*provider, gateway.ProviderCfg{APIKey: key, Model: *model, BaseURL: *baseURL})
	if err != nil {
		fatal("build provider: %v", err)
	}

	// Representative "今日营销建议" prompt (~2K input tokens after the plan's sizing).
	msgs := []gateway.Message{
		{Role: gateway.RoleSystem, Content: "你是零售营销文案助手,输出一段简洁的今日促销文案(JSON: {title,text})."},
		{Role: gateway.RoleUser, Content: samplePrompt},
	}
	opts := gateway.Opts{Model: *model, MaxTokens: *maxTokens, Timeout: *timeout}

	latencies := make([]time.Duration, 0, *n)
	var totalCost float64
	var totalIn, totalOut int
	failed := 0

	fmt.Printf("benchmarking %s/%s n=%d ...\n", *provider, orDefault(*model, p.Name()), *n)
	for i := 0; i < *n; i++ {
		start := time.Now()
		resp, err := p.Chat(context.Background(), msgs, opts)
		dur := time.Since(start)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  [%d] FAIL (%v): %v\n", i+1, dur.Truncate(time.Millisecond), err)
			continue
		}
		latencies = append(latencies, dur)
		totalCost += resp.CostCNY
		totalIn += resp.PromptTokens
		totalOut += resp.CompletionTokens
	}

	if len(latencies) == 0 {
		fatal("all calls failed")
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	avgCost := totalCost / float64(len(latencies))
	// Plan-book usage assumption: ~30 generations per active tenant per day.
	projectedMonthly := avgCost * 30 * 30

	fmt.Println()
	fmt.Printf("successful:    %d / %d  (failed %d)\n", len(latencies), *n, failed)
	fmt.Printf("tokens total:  in=%d out=%d\n", totalIn, totalOut)
	fmt.Printf("latency:       p50=%s p95=%s max=%s\n",
		pct(latencies, 50), pct(latencies, 95), latencies[len(latencies)-1])
	fmt.Printf("cost per call: ¥%.6f\n", avgCost)
	fmt.Printf("total cost:    ¥%.4f\n", totalCost)
	fmt.Printf("projected per-tenant monthly (30/day): ¥%.2f  (ceiling ¥2.00)\n", projectedMonthly)
	if avgCost >= 0.001 {
		fmt.Fprintln(os.Stderr, "WARN: per-call cost >= ¥0.001 (plan §7.8.3 target not met)")
	}
	if projectedMonthly > gateway.MonthlyLimitCNY {
		fmt.Fprintln(os.Stderr, "FAIL: projected monthly cost exceeds ¥2/tenant ceiling")
		os.Exit(1)
	}
}

func buildProvider(name string, cfg gateway.ProviderCfg) (gateway.Provider, error) {
	switch name {
	case "qwen":
		return gateway.NewQwen(cfg), nil
	case "deepseek":
		return gateway.NewDeepSeek(cfg), nil
	case "doubao":
		return gateway.NewDoubao(cfg), nil
	default:
		return nil, errors.New("unknown provider: " + name)
	}
}

func pct(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := len(sorted) * p / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx].Truncate(time.Millisecond)
}

func upper(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		out[i] = c
	}
	return string(out)
}

func orDefault(model, def string) string {
	if model != "" {
		return model
	}
	return def
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...)
	os.Exit(2)
}

const samplePrompt = `店铺:水果店;商品:芒果(成本¥4/斤,库存200斤,临期3天);
情境:本地连续雨天,客流偏低;目标:清库存且保毛利(下限5%)。
请给一条今日促销建议。`
