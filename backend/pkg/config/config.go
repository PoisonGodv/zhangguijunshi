// Package config loads the backend's layered configuration into the shared
// Config type defined in types.go.
//
// Loading precedence (lowest → highest):
//
//  1. SetDefaults — safe built-in values (ports, timeouts, etc.).
//  2. <configDir>/config.<env>.yaml — an optional file; a missing file is
//     NOT an error so pure defaults+env deployments work.
//  3. Environment variables prefixed with ZG_, underscore-nested
//     (ZG_POSTGRES_HOST → postgres.host, ZG_HTTP_READ_TIMEOUT → http.read_timeout).
//
// Sources are merged by viper and unmarshaled into *Config with a decode hook
// that parses human-readable durations ("30s", "720h") into time.Duration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

// findConfigFile locates the first existing "config.<env>.{yaml,yml}" inside
// dir and returns its bytes. found is false when no file exists, which Load
// treats as "no yaml layer" (pure defaults + env).
func findConfigFile(dir, env string) ([]byte, bool) {
	for _, name := range []string{"config." + env + ".yaml", "config." + env + ".yml"} {
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if err == nil {
			return b, true
		}
	}
	return nil, false
}

// envPrefix is the environment-variable prefix for source 3 (see package doc).
const envPrefix = "ZG"

// loadConfig carries the options applied via Option funcs.
type loadConfig struct {
	configDir string
	env       string
}

// Option configures Load behaviour.
type Option func(*loadConfig)

// WithConfigDir overrides the directory searched for config.<env>.yaml.
// An empty argument is ignored. Defaults to "configs".
func WithConfigDir(dir string) Option {
	return func(lc *loadConfig) {
		if dir != "" {
			lc.configDir = dir
		}
	}
}

// WithEnv overrides the deployment environment (dev | staging | prod) that
// selects the config.<env>.yaml file. An empty argument is ignored.
// Defaults to "dev".
func WithEnv(env string) Option {
	return func(lc *loadConfig) {
		if env != "" {
			lc.env = env
		}
	}
}

// Load builds a *Config from the layered sources described in the package doc.
// Missing config file is treated as "no yaml layer" rather than an error;
// any other read or decode failure is returned wrapped with %w.
func Load(opts ...Option) (*Config, error) {
	lc := &loadConfig{
		configDir: "configs",
		env:       "dev",
	}
	for _, opt := range opts {
		opt(lc)
	}

	v := viper.New()

	// Layer 1: built-in defaults. Setting defaults also registers every key
	// with viper so that AutomaticEnv (layer 3) can override each one.
	SetDefaults(v)
	v.SetDefault("app.env", lc.env)

	// Layer 2: optional yaml with ${VAR} env expansion. We locate
	// "config.<env>.{yaml,yml}" ourselves, ExpandEnv its bytes (so staging/prod
	// files can reference SSM/env-injected values), then hand the expanded text
	// to viper. A missing file is NOT an error — pure defaults+env still work.
	if raw, found := findConfigFile(lc.configDir, lc.env); found {
		expanded := os.ExpandEnv(string(raw))
		v.SetConfigType("yaml")
		if err := v.ReadConfig(strings.NewReader(expanded)); err != nil {
			return nil, fmt.Errorf("config: parse config.%s.yaml: %w", lc.env, err)
		}
	}

	// Layer 3: environment variables. EnvPrefix ZG + "." → "_" gives the
	// documented mapping, e.g. ZG_POSTGRES_HOST → postgres.host.
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	var cfg Config
	if err := v.Unmarshal(&cfg, withDurationHook()); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}
	// Layer 3b: env-injected map fields. viper's AutomaticEnv cannot populate
	// map[string]struct{} fields, so the prod config's "providers injected via
	// ZG_AI_PROVIDERS_* env" promise (config.prod.yaml) was an unimplemented gap
	// until this pass (CONTRACT §19 / plan B3). Scan the real env here.
	applyAIProvidersEnv(&cfg)
	return &cfg, nil
}

// SetDefaults registers safe built-in defaults on a viper instance. It is
// exported so callers (and tests) can reuse the same baseline; Load invokes it
// automatically as layer 1.
//
// Defaults follow the fail-safe principle: multitenancy is strict by default,
// TLS is off only for local dev infra, and production-only knobs are inert.
func SetDefaults(v *viper.Viper) {
	// App
	v.SetDefault("app.name", "zhanggui-junshi-backend")
	v.SetDefault("app.version", "0.0.0-dev")

	// HTTP
	v.SetDefault("http.host", "0.0.0.0")
	v.SetDefault("http.port", 8080)
	v.SetDefault("http.read_timeout", 15*time.Second)
	v.SetDefault("http.write_timeout", 15*time.Second)
	v.SetDefault("http.shutdown_timeout", 10*time.Second)

	// Postgres
	v.SetDefault("postgres.host", "127.0.0.1")
	v.SetDefault("postgres.port", 5432)
	v.SetDefault("postgres.user", "zgjs")
	v.SetDefault("postgres.database", "zgjs")
	v.SetDefault("postgres.ssl_mode", "disable")
	v.SetDefault("postgres.max_conns", int32(10))
	v.SetDefault("postgres.min_conns", int32(2))
	v.SetDefault("postgres.max_conn_lifetime", time.Hour)
	v.SetDefault("postgres.max_conn_idle_time", 15*time.Minute)

	// Redis
	v.SetDefault("redis.host", "127.0.0.1")
	v.SetDefault("redis.port", 6379)
	v.SetDefault("redis.db", 0)
	v.SetDefault("redis.pool_size", 10)

	// Log
	v.SetDefault("log.level", "info")
	v.SetDefault("log.encoding", "json")
	v.SetDefault("log.output_paths", []string{"stdout"})

	// JWT. KeyFile empty -> wiring derives a DevKey; never default a real key.
	v.SetDefault("jwt.access_ttl", 15*time.Minute)
	v.SetDefault("jwt.refresh_ttl", 720*time.Hour)
	v.SetDefault("jwt.issuer", "zhanggui-junshi")

	// WeChat mini-program: no credentials by default (POC supplies via env).
	v.SetDefault("wx.timeout", 5*time.Second)

	// AI gateway defaults (tech plan §7.8). Default monthly budget = ¥2/tenant.
	v.SetDefault("ai.request_timeout", 8*time.Second)
	v.SetDefault("ai.monthly_budget_cny", 2.0)

	// OpenTelemetry
	v.SetDefault("otel.enable", false)
	v.SetDefault("otel.endpoint", "")
	v.SetDefault("otel.service_name", "zhanggui-junshi-backend")
	v.SetDefault("otel.sample_ratio", 0.0)
	v.SetDefault("otel.insecure", true)

	// Multitenancy: fail-safe strict; dev mock header OFF by default (opt-in).
	v.SetDefault("multitenancy.strict", true)
	v.SetDefault("multitenancy.dev_mock_header", false)
	// Platform tenant (default 1) hosts platform_ops accounts for admin login.
	v.SetDefault("multitenancy.platform_tenant_id", int64(1))

	// Payment (WeChat Pay V3): no creds by default → wiring keeps FakeWxPay
	// (dev/test never network). Only a timeout has a safe built-in.
	// Enabled gates the whole payment surface (CONTRACT §25.1): MVP runs false
	// (计划书 §4.2.5 砍在线支付) — routes are not registered though the domain
	// code stays assembled. V1 flips it true once the 资质 is in.
	v.SetDefault("payment.enabled", false)
	v.SetDefault("payment.timeout", 10*time.Second)

	// Subscription paywall (CONTRACT §25.2): MVP runs false (全免费 L1,
	// 计划书 §4.2.5 付费墙不启用) — FeatureGate passes every feature through
	// and /subscription routes are not registered. V1 flips true with billing.
	v.SetDefault("subscription.enabled", false)

	// Job queue (Redis Stream): sane built-ins; the queue is effectively
	// disabled when the Redis client is nil (dev without Redis).
	v.SetDefault("job_queue.stream", "zg:jobs")
	v.SetDefault("job_queue.group", "zg-workers")
	v.SetDefault("job_queue.consumer", "worker-1")
	v.SetDefault("job_queue.block", 5*time.Second)
	v.SetDefault("job_queue.max_len", int64(10000))

	// Scheduler: dev runs the pregen cron inside the api process (single-binary
	// convenience); prod sets run_in_api=false so the cron binary owns it.
	v.SetDefault("scheduler.run_in_api", true)
}

// withDurationHook returns a viper DecoderConfigOption that wires a decode-hook
// chain capable of parsing human-readable durations ("30s", "720h") into
// time.Duration and comma-separated strings into []string.
//
// Although viper's own default decoder already registers these hooks,
// declaring the chain explicitly guarantees duration parsing regardless of the
// viper version's defaults (the task contract requires the duration hook).
func withDurationHook() viper.DecoderConfigOption {
	return func(dc *mapstructure.DecoderConfig) {
		dc.DecodeHook = mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		)
	}
}

// Env prefixes for the map-backed AI config (CONTRACT §19 / plan B3). Viper's
// AutomaticEnv populates scalar keys but cannot materialise map fields, so we
// scan os.Environ ourselves.
const (
	aiProviderEnvPrefix = "ZG_AI_PROVIDERS_" // + <NAME>_<base_url|api_key|model>
	aiRouteEnvPrefix    = "ZG_AI_ROUTES_"    // + <TIER>_<provider|model>
)

// applyAIProvidersEnv backfills cfg.AI.Providers and cfg.AI.Routes from the
// ZG_AI_PROVIDERS_<NAME>_<FIELD> and ZG_AI_ROUTES_<TIER>_<FIELD> environment
// variables. Names and tiers are lower-cased to match the map keys consumed by
// the gateway (qwen/deepseek/doubao, lite/v3/r1). Values already present (from
// yaml) are preserved; env only fills or overrides individual fields.
//
// Examples:
//
//	ZG_AI_PROVIDERS_QWEN_API_KEY=sk-...      → providers["qwen"].APIKey
//	ZG_AI_PROVIDERS_QWEN_BASE_URL=https://..  → providers["qwen"].BaseURL
//	ZG_AI_ROUTES_V3_PROVIDER=deepseek         → routes["v3"].Provider
func applyAIProvidersEnv(cfg *Config) {
	if cfg == nil {
		return
	}
	// Providers.
	if cfg.AI.Providers == nil {
		cfg.AI.Providers = map[string]AIProviderConfig{}
	}
	for _, kv := range os.Environ() {
		body, ok := envBody(kv, aiProviderEnvPrefix)
		if !ok {
			continue
		}
		name, field, val, ok := splitNameField(body) // "QWEN", "API_KEY", val
		if !ok {
			continue
		}
		p := cfg.AI.Providers[strings.ToLower(name)]
		switch strings.ToLower(field) {
		case "base_url":
			p.BaseURL = val
		case "api_key":
			p.APIKey = val
		case "model":
			p.Model = val
		}
		cfg.AI.Providers[strings.ToLower(name)] = p
	}
	// Routes.
	if cfg.AI.Routes == nil {
		cfg.AI.Routes = map[string]AIRouteConfig{}
	}
	for _, kv := range os.Environ() {
		body, ok := envBody(kv, aiRouteEnvPrefix)
		if !ok {
			continue
		}
		tier, field, val, ok := splitNameField(body) // "V3", "PROVIDER", val
		if !ok {
			continue
		}
		r := cfg.AI.Routes[strings.ToLower(tier)]
		switch strings.ToLower(field) {
		case "provider":
			r.Provider = val
		case "model":
			r.Model = val
		}
		cfg.AI.Routes[strings.ToLower(tier)] = r
	}
}

// envBody strips prefix and returns the part after the first '=' when kv has the
// given prefix (and an '='). ok is false otherwise.
func envBody(kv, prefix string) (body string, ok bool) {
	if !strings.HasPrefix(kv, prefix) {
		return "", false
	}
	rest := kv[len(prefix):]
	eq := strings.IndexByte(rest, '=')
	if eq < 0 {
		return "", false
	}
	return rest, true
}

// splitNameField parses "NAME_FIELD=value" → (name, field, value). name is up to
// the first '_' (provider/tier names never contain '_'); field is everything
// between that '_' and '=' (fields like base_url/api_key may contain '_'). The
// 4th return is false when no '_' separates a name from a field.
func splitNameField(body string) (name, field, val string, ok bool) {
	eq := strings.IndexByte(body, '=')
	if eq < 0 {
		return "", "", "", false
	}
	nameField := body[:eq]
	val = body[eq+1:]
	us := strings.IndexByte(nameField, '_')
	if us < 0 {
		return "", "", "", false
	}
	return nameField[:us], nameField[us+1:], val, true
}
