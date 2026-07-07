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
	Multitenancy MultitenancyConfig `mapstructure:"multitenancy"`
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
}

// AIProviderConfig describes one OpenAI-compatible LLM provider (tech plan
// §7.8.1). Qwen/DeepSeek/Doubao all expose /chat/completions and differ only
// in BaseURL, APIKey and model name.
type AIProviderConfig struct {
	BaseURL string `mapstructure:"base_url"`
	APIKey  string `mapstructure:"api_key"`
	Model   string `mapstructure:"model"`
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

// MultitenancyConfig governs the row-level isolation guard (ARCH-01).
type MultitenancyConfig struct {
	Strict bool `mapstructure:"strict"` // hard-enforce tenant on every repo call
	// DevMockHeader enables the DEV-ONLY MockTenant middleware (X-Mock-Tenant
	// header) so the isolation guard can be exercised through HTTP before/aside
	// from real JWT auth. MUST be false in staging/prod. Default false; dev
	// config sets it true.
	DevMockHeader bool `mapstructure:"dev_mock_header"`
}
