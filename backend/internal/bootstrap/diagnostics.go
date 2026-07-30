package bootstrap

// diagnostics.go emits an operator-visible startup summary of which external
// integrations are running with REAL credentials versus degraded to their
// dev/test fallbacks (CONTRACT §19 real-key bring-up). Every dependency
// degrades gracefully (CONTRACT §0), so a boot that "succeeds" can still be
// silently running on FakeWxPay / placeholder LLM / in-memory repos. These
// helpers make that state explicit: one structured info line per integration
// with active=true/false and a human-readable reason.
//
// No secrets are ever logged — only presence/absence of credentials and
// provider names. LogClientDiagnostics is wired into BuildServices by the
// composition root (it is NOT called from this package).

import (
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/db"
	"github.com/zhangguijunshi/backend/pkg/redisx"
)

// Integration identifiers reported by ClientStatus / LogClientDiagnostics. The
// keys are stable so operators and dashboards can grep for them.
const (
	integrationPostgres = "postgres"
	integrationRedis    = "redis"
	integrationWXMini   = "wx_miniprogram"
	integrationCOS      = "cos"
	integrationLLM      = "llm"
	integrationWxPayV3  = "wxpay_v3"
)

// ClientStatus reports, per integration, whether a real client is active given
// the loaded config and whether the Postgres / Redis infra came up. It is pure
// (no logging, no I/O) so the truth table is unit-testable. The returned map
// always contains every integration key. A nil cfg yields all-false.
//
// Semantics (mirror the real-vs-fake gates in the composition root):
//   - postgres:        a live pool was built (hasDB).
//   - redis:           a live client was built (hasRedis).
//   - wx_miniprogram:  WX.AppID AND WX.Secret present.
//   - cos:             COS.SecretID AND COS.SecretKey AND COS.Bucket present.
//   - llm:             at least one AI provider has a non-empty APIKey.
//   - wxpay_v3:        PaymentConfig.Configured() (mch_id + api_v3_key +
//     cert_serial + a private key).
func ClientStatus(cfg *config.Config, hasDB, hasRedis bool) map[string]bool {
	status := map[string]bool{
		integrationPostgres: hasDB,
		integrationRedis:    hasRedis,
		integrationWXMini:   false,
		integrationCOS:      false,
		integrationLLM:      false,
		integrationWxPayV3:  false,
	}
	if cfg == nil {
		return status
	}
	status[integrationWXMini] = cfg.WX.AppID != "" && cfg.WX.Secret != ""
	status[integrationCOS] = cfg.COS.SecretID != "" && cfg.COS.SecretKey != "" && cfg.COS.Bucket != ""
	status[integrationLLM] = len(activeLLMProviders(cfg)) > 0
	status[integrationWxPayV3] = cfg.Payment.Configured()
	return status
}

// activeLLMProviders returns the sorted names of AI providers that carry a
// non-empty APIKey — exactly the set gateway.NewFromConfig will actually build
// (it skips providers with an empty key). Sorting keeps log output and tests
// deterministic despite Go's randomised map iteration.
func activeLLMProviders(cfg *config.Config) []string {
	if cfg == nil || len(cfg.AI.Providers) == 0 {
		return nil
	}
	names := make([]string, 0, len(cfg.AI.Providers))
	for name, pc := range cfg.AI.Providers {
		if pc.APIKey != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// LogClientDiagnostics logs ONE structured info line per integration with
// active=true/false plus a human-readable detail (no secrets). It is nil-safe
// on every argument: a nil logger is a no-op, a nil cfg reports everything as
// degraded, and nil database / rdb mean the corresponding infra is absent.
func LogClientDiagnostics(log *zap.Logger, cfg *config.Config, database *db.DB, rdb *redisx.Client) {
	if log == nil {
		return
	}
	hasDB := database != nil
	hasRedis := rdb != nil
	status := ClientStatus(cfg, hasDB, hasRedis)

	logIntegration(log, integrationPostgres, status[integrationPostgres], postgresDetail(cfg, hasDB))
	logIntegration(log, integrationRedis, status[integrationRedis], redisDetail(cfg, hasRedis))
	logIntegration(log, integrationWXMini, status[integrationWXMini], wxDetail(cfg))
	logIntegration(log, integrationCOS, status[integrationCOS], cosDetail(cfg))
	logIntegration(log, integrationLLM, status[integrationLLM], llmDetail(cfg))
	logIntegration(log, integrationWxPayV3, status[integrationWxPayV3], wxpayDetail(cfg))
}

// logIntegration emits the single structured line per integration. The
// integration name, the active flag and the detail are all top-level fields so
// they are directly queryable in structured log backends.
func logIntegration(log *zap.Logger, name string, active bool, detail string) {
	log.Info("client diagnostic",
		zap.String("integration", name),
		zap.Bool("active", active),
		zap.String("detail", detail),
	)
}

func postgresDetail(cfg *config.Config, hasDB bool) string {
	if hasDB {
		return "pgxpool connected; pg repos active"
	}
	if cfg != nil && cfg.Postgres.Host != "" {
		return "postgres configured but no live pool; repos fall back to in-memory"
	}
	return "postgres not configured; repos fall back to in-memory"
}

func redisDetail(cfg *config.Config, hasRedis bool) string {
	if hasRedis {
		return "redis client connected; session store + job queue active"
	}
	if cfg != nil && cfg.Redis.Host != "" {
		return "redis configured but not reachable; session store in-memory, job queue no-op"
	}
	return "redis not configured; session store in-memory, job queue no-op"
}

func wxDetail(cfg *config.Config) string {
	if cfg != nil && cfg.WX.AppID != "" && cfg.WX.Secret != "" {
		return "wx mini-program AppID + Secret present; real code2session enabled"
	}
	var missing []string
	if cfg == nil || cfg.WX.AppID == "" {
		missing = append(missing, "app_id")
	}
	if cfg == nil || cfg.WX.Secret == "" {
		missing = append(missing, "secret")
	}
	return "wx mini-program missing " + strings.Join(missing, ", ") + "; consumer login degraded"
}

func cosDetail(cfg *config.Config) string {
	if cfg != nil && cfg.COS.SecretID != "" && cfg.COS.SecretKey != "" && cfg.COS.Bucket != "" {
		return "COS SecretID + SecretKey + Bucket present; uploads enabled"
	}
	return "COS SecretID/SecretKey/Bucket incomplete; uploads disabled (clean error)"
}

func llmDetail(cfg *config.Config) string {
	providers := activeLLMProviders(cfg)
	if len(providers) > 0 {
		return "LLM providers with API key: " + strings.Join(providers, ", ")
	}
	return "no AI provider has an API key; gateway is a placeholder (rules + cache only)"
}

func wxpayDetail(cfg *config.Config) string {
	if cfg != nil && cfg.Payment.Configured() {
		return "WeChat Pay V3 credentials present; real V3 client enabled"
	}
	return "WeChat Pay V3 credentials incomplete; using FakeWxPay (dev/test, no network)"
}
