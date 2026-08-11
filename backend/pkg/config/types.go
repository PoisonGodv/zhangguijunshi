// Package config defines the layered configuration model for the backend.
//
// The struct surface below is the SHARED CONTRACT consumed by every package
// (logger, db, http, multitenancy). Only the type definitions live here; the
// loading logic (viper) lives in config.go, authored separately. Do NOT change
// these struct shapes without updating CONTRACT.md and downstream packages.
package config

import "time"

// Config is the root configuration loaded from layered sources
// (defaults → configs/<env>.yaml → env vars prefixed ZG_, underscore-nested).
type Config struct {
	App          AppConfig          `mapstructure:"app"`
	HTTP         HTTPConfig         `mapstructure:"http"`
	Postgres     PostgresConfig     `mapstructure:"postgres"`
	Redis        RedisConfig        `mapstructure:"redis"`
	Log          LogConfig          `mapstructure:"log"`
	JWT          JWTConfig          `mapstructure:"jwt"`
	WX           WXConfig           `mapstructure:"wx"`
	AI           AIConfig           `mapstructure:"ai"`
	OTel         OTelConfig         `mapstructure:"otel"`
	COS          COSConfig          `mapstructure:"cos"`
	Payment      PaymentConfig      `mapstructure:"payment"`
	JobQueue     JobQueueConfig     `mapstructure:"job_queue"`
	Scheduler    SchedulerConfig    `mapstructure:"scheduler"`
	Multitenancy MultitenancyConfig `mapstructure:"multitenancy"`
	Subscription SubscriptionConfig `mapstructure:"subscription"`
}

// SubscriptionConfig configures the plan paywall (CONTRACT §25.2). MVP runs
// with Enabled=false (计划书 §4.2.5: 订阅付费墙不启用, 全免费 L1) — the
// FeatureGate passes every feature through, and the /subscription routes are
// not registered. Flip Enabled=true in V1 (with real billing) to revive.
type SubscriptionConfig struct {
	Enabled bool `mapstructure:"enabled"` // MVP: false (paywall off, all-free)
}

type AppConfig struct {
	Name    string `mapstructure:"name"`
	Env     string `mapstructure:"env"` // dev | staging | prod
	Version string `mapstructure:"version"`
}

type HTTPConfig struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

type PostgresConfig struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	User            string        `mapstructure:"user"`
	Password        string        `mapstructure:"password"`
	Database        string        `mapstructure:"database"`
	SSLMode         string        `mapstructure:"ssl_mode"`
	MaxConns        int32         `mapstructure:"max_conns"`
	MinConns        int32         `mapstructure:"min_conns"`
	MaxConnLifetime time.Duration `mapstructure:"max_conn_lifetime"`
	MaxConnIdleTime time.Duration `mapstructure:"max_conn_idle_time"`
}

type RedisConfig struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
	PoolSize int    `mapstructure:"pool_size"`
}

type LogConfig struct {
	Level       string   `mapstructure:"level"`    // debug | info | warn | error
	Encoding    string   `mapstructure:"encoding"` // json | console
	OutputPaths []string `mapstructure:"output_paths"`
}

type JWTConfig struct {
	Secret     string        `mapstructure:"secret"`      // legacy/dev convenience; ES256 keys come from KeyFile
	KeyFile    string        `mapstructure:"key_file"`    // path to ECDSA P-256 private key PEM; empty -> DevKey
	KeyID      string        `mapstructure:"key_id"`      // kid header for key rotation
	AccessTTL  time.Duration `mapstructure:"access_ttl"`  // access-token lifetime
	RefreshTTL time.Duration `mapstructure:"refresh_ttl"` // refresh-token lifetime (opaque, store-backed)
	Issuer     string        `mapstructure:"issuer"`      // iss claim
}

// WXConfig holds the WeChat Mini Program credentials used by code2session.
// In production each merchant's mini-program has its own AppID/Secret; the
// multi-tenant mapping (tenant_id -> AppID) is resolved at the auth layer. For
// M0-S2 POC a single default pair is sufficient (tech plan §6.2.1 / §A.1.1).
type WXConfig struct {
	AppID  string `mapstructure:"app_id"`
	Secret string `mapstructure:"secret"`
	// Code2SessionURL overrides the jscode2session endpoint (mainly for tests
	// / local POC against a fake server). Empty -> official endpoint.
	Code2SessionURL string `mapstructure:"code2session_url"`
	// Timeout for upstream WeChat calls.
	Timeout time.Duration `mapstructure:"timeout"`
	// SubscribeTemplates maps a reach template code (e.g. "recall_30",
	// "birthday") to the WeChat-side subscribe-message template_id registered in
	// the Mini Program console (tech plan §4.3). When AppID+Secret AND this map
	// are non-empty the reach domain wires the real WxSender (subscribe_msg
	// channel); otherwise it keeps the in-memory dev stub (no network).
	SubscribeTemplates map[string]string `mapstructure:"subscribe_templates"`
	// SubscribePage is the default mini-program page a tapped subscribe
	// notification opens (optional; empty -> home page).
	SubscribePage string `mapstructure:"subscribe_page"`
}

// AIProviderConfig describes one OpenAI-compatible LLM provider (tech plan
// §7.8.1). Qwen/DeepSeek/Doubao all expose /chat/completions and differ only
// in BaseURL, APIKey and model name.
type AIProviderConfig struct {
	BaseURL string `mapstructure:"base_url"`
	APIKey  string `mapstructure:"api_key"`
	Model   string `mapstructure:"model"`
}

// AIRouteConfig binds one task tier (lite|v3|r1) to a provider+model pair
// (M3-S2 §13.3.7). key in AIConfig.Routes is the tier name string; gateway
// maps it to gateway.TaskTier (TierLite/TierV3/TierR1) in NewFromConfig.
type AIRouteConfig struct {
	Provider string `mapstructure:"provider"` // qwen|deepseek|doubao
	Model    string `mapstructure:"model"`
}

// AIConfig configures the AI gateway (tech plan §7.8).
type AIConfig struct {
	// DefaultProvider selects which provider name under Providers is active.
	DefaultProvider string `mapstructure:"default_provider"`
	// Providers maps a short name (qwen/deepseek/doubao) to its config.
	Providers map[string]AIProviderConfig `mapstructure:"providers"`
	// RequestTimeout bounds every upstream LLM call.
	RequestTimeout time.Duration `mapstructure:"request_timeout"`
	// MonthlyBudgetCNY is the per-tenant monthly AI cost ceiling; exceeding it
	// forces a full fallback to rules+cache (tech plan §7.8.4, plan book 6.7).
	MonthlyBudgetCNY float64 `mapstructure:"monthly_budget_cny"`
	// Routes binds task tiers (key "lite"|"v3"|"r1") to provider+model pairs
	// (M3-S2 §13.3.7). Empty → gateway uses defaultRoute for every tier
	// (M3-S1 behavior, backward compatible).
	Routes map[string]AIRouteConfig `mapstructure:"routes"`
}

type OTelConfig struct {
	Enable      bool    `mapstructure:"enable"`
	Endpoint    string  `mapstructure:"endpoint"`
	ServiceName string  `mapstructure:"service_name"`
	SampleRatio float64 `mapstructure:"sample_ratio"`
	Insecure    bool    `mapstructure:"insecure"`
}

type COSConfig struct {
	SecretID  string `mapstructure:"secret_id"`
	SecretKey string `mapstructure:"secret_key"`
	Region    string `mapstructure:"region"`
	Bucket    string `mapstructure:"bucket"`
	Endpoint  string `mapstructure:"endpoint"`
}

// PaymentConfig holds the WeChat Pay V3 service-provider credentials (CONTRACT
// §15.5.8). All fields are env-injected in prod (never committed). When the
// required fields are absent the wiring falls back to FakeWxPay (dev/test); the
// real V3 client (internal/payment/v3*.go) is used otherwise. PrivateKeyPEM
// (inline) and PrivateKeyFile (path) are mutually exclusive; PEM wins when both
// are set.
type PaymentConfig struct {
	// Enabled gates the payment surface (CONTRACT §25.1). MVP runs with false
	// (计划书 §4.2.5: 砍在线支付, 资质重阻塞 PMF) — the /payment routes and the
	// wxshop payment-notify endpoint are NOT registered, even though the domain
	// code/repos/FakeWxPay stay assembled (保留待用). Flip to true in V1 once the
	// 微信支付服务商 资质 is in and the V3 credentials below are populated.
	Enabled         bool          `mapstructure:"enabled"`
	MchID           string        `mapstructure:"mch_id"`            // 服务商商户号
	APiv3Key        string        `mapstructure:"api_v3_key"`        // APIv3 密钥 (AES-GCM 解密 resource + 证书)
	CertSerial      string        `mapstructure:"cert_serial"`       // 商户 API 证书序列号
	PrivateKeyPEM   string        `mapstructure:"private_key_pem"`   // 商户 API 私钥 PEM (内联)
	PrivateKeyFile  string        `mapstructure:"private_key_file"`  // 商户 API 私钥 PEM (文件路径)
	PlatformCertURL string        `mapstructure:"platform_cert_url"` // 平台证书拉取端点; 空 -> 官方 /v3/certificates
	NotifyURL       string        `mapstructure:"notify_url"`        // 支付回调通知地址
	BaseURL         string        `mapstructure:"base_url"`          // V3 网关; 空 -> api.mch.weixin.qq.com
	Timeout         time.Duration `mapstructure:"timeout"`
}

// Configured reports whether the minimum V3 fields are present to build the real
// client. Missing any → wiring keeps FakeWxPay (dev/test never networks).
func (c PaymentConfig) Configured() bool {
	return c.MchID != "" && c.APiv3Key != "" && c.CertSerial != "" &&
		(c.PrivateKeyPEM != "" || c.PrivateKeyFile != "")
}

// JobQueueConfig configures the Redis Stream job queue shared by the worker and
// the in-process producers (reach scheduled-send / pregen incremental). When
// Redis is unavailable the producer degrades to a no-op log and the consumer
// does not start (CONTRACT §19).
type JobQueueConfig struct {
	Stream   string        `mapstructure:"stream"`   // Redis Stream 名 (XADD/XREADGROUP)
	Group    string        `mapstructure:"group"`    // consumer group
	Consumer string        `mapstructure:"consumer"` // consumer 名 (区分 worker 实例)
	Block    time.Duration `mapstructure:"block"`    // XREADGROUP block 时长
	MaxLen   int64         `mapstructure:"max_len"`  // stream 近似裁剪长度 (XADD MAXLEN ~ n)
}

// SchedulerConfig governs where the pregen cron runs. dev runs it inside the api
// process (single-binary convenience); prod runs it in the dedicated cron binary
// and sets RunInAPI=false so the two never double-tick (CONTRACT §19).
type SchedulerConfig struct {
	RunInAPI bool `mapstructure:"run_in_api"` // true: api 进程内嵌 pregen; false: 仅 cron 二进制
}

// MultitenancyConfig governs the row-level isolation guard (ARCH-01).
type MultitenancyConfig struct {
	Strict bool `mapstructure:"strict"` // hard-enforce tenant on every repo call
	// DevMockHeader enables the DEV-ONLY MockTenant middleware (X-Mock-Tenant
	// header) so the isolation guard can be exercised through HTTP before/aside
	// from real JWT auth. MUST be false in staging/prod. Default false; dev
	// config sets it true.
	DevMockHeader bool `mapstructure:"dev_mock_header"`
	// PlatformTenantID is the tenant that hosts platform_ops accounts (admin
	// login, CONTRACT §19). AdminLogin resolves the username under this tenant
	// and signs a Scene=admin JWT. Default 1; override via
	// multitenancy.platform_tenant_id for non-default platform tenants.
	PlatformTenantID int64 `mapstructure:"platform_tenant_id"`
}
