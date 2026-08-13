package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeYAML writes a minimal config.<env>.yaml into dir.
func writeYAML(t *testing.T, dir, env, content string) {
	t.Helper()
	name := "config." + env + ".yaml"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
}

// newDir returns a fresh temp dir scoped to the test.
func newDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestLoad_DefaultsOnly(t *testing.T) {
	// No yaml, no env: pure built-in defaults. Missing file must NOT error.
	cfg, err := Load(WithConfigDir(newDir(t)))
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load: nil config")
	}
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"default http.port", cfg.HTTP.Port, 8080},
		{"default http.host", cfg.HTTP.Host, "0.0.0.0"},
		{"default http.read_timeout", cfg.HTTP.ReadTimeout, 15 * time.Second},
		{"default postgres.host", cfg.Postgres.Host, "127.0.0.1"},
		{"default postgres.port", cfg.Postgres.Port, 5432},
		{"default log.level", cfg.Log.Level, "info"},
		{"default log.encoding", cfg.Log.Encoding, "json"},
		{"default jwt.refresh_ttl", cfg.JWT.RefreshTTL, 720 * time.Hour},
		{"default app.env", cfg.App.Env, "dev"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	if !cfg.Multitenancy.Strict {
		t.Error("multitenancy.strict: got false, want true (fail-safe default)")
	}
}

func TestLoad_YamlOverridesDefaults(t *testing.T) {
	dir := newDir(t)
	writeYAML(t, dir, "dev", `
http:
  port: 9090
  read_timeout: "30s"
postgres:
  host: pg-yaml
  port: 6543
log:
  level: debug
  encoding: console
`)

	cfg, err := Load(WithConfigDir(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// yaml wins over defaults
	if cfg.HTTP.Port != 9090 {
		t.Errorf("http.port: got %d, want 9090", cfg.HTTP.Port)
	}
	if cfg.Postgres.Host != "pg-yaml" {
		t.Errorf("postgres.host: got %q, want %q", cfg.Postgres.Host, "pg-yaml")
	}
	if cfg.Postgres.Port != 6543 {
		t.Errorf("postgres.port: got %d, want 6543", cfg.Postgres.Port)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level: got %q, want %q", cfg.Log.Level, "debug")
	}
	if cfg.Log.Encoding != "console" {
		t.Errorf("log.encoding: got %q, want %q", cfg.Log.Encoding, "console")
	}
	// duration string from yaml must parse into time.Duration
	if cfg.HTTP.ReadTimeout != 30*time.Second {
		t.Errorf("http.read_timeout: got %v, want 30s (duration hook)", cfg.HTTP.ReadTimeout)
	}
	// untouched keys keep defaults
	if cfg.HTTP.WriteTimeout != 15*time.Second {
		t.Errorf("http.write_timeout: got %v, want default 15s", cfg.HTTP.WriteTimeout)
	}
	if !cfg.Multitenancy.Strict {
		t.Error("multitenancy.strict should remain true")
	}
}

func TestLoad_EnvOverrulesYaml(t *testing.T) {
	dir := newDir(t)
	writeYAML(t, dir, "dev", `
http:
  port: 9090
  read_timeout: "30s"
postgres:
  host: pg-yaml
`)

	// Layer 3: environment variables (prefix ZG_, underscore-nested) must win.
	t.Setenv("ZG_HTTP_PORT", "7777")
	t.Setenv("ZG_POSTGRES_HOST", "pg-env")
	t.Setenv("ZG_HTTP_READ_TIMEOUT", "60s")
	t.Setenv("ZG_LOG_LEVEL", "warn")

	cfg, err := Load(WithConfigDir(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Port != 7777 {
		t.Errorf("http.port: got %d, want 7777 (env)", cfg.HTTP.Port)
	}
	if cfg.Postgres.Host != "pg-env" {
		t.Errorf("postgres.host: got %q, want %q (env)", cfg.Postgres.Host, "pg-env")
	}
	if cfg.HTTP.ReadTimeout != 60*time.Second {
		t.Errorf("http.read_timeout: got %v, want 60s (env duration)", cfg.HTTP.ReadTimeout)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("log.level: got %q, want %q (env)", cfg.Log.Level, "warn")
	}
}

func TestLoad_EnvLongDuration(t *testing.T) {
	// "720h" style strings must survive the round-trip as time.Duration.
	t.Setenv("ZG_HTTP_READ_TIMEOUT", "720h")

	cfg, err := Load(WithConfigDir(newDir(t)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.ReadTimeout != 720*time.Hour {
		t.Errorf("http.read_timeout: got %v, want 720h", cfg.HTTP.ReadTimeout)
	}
}

func TestLoad_PriorityChainAllThree(t *testing.T) {
	// Single scenario pinning the full precedence: default < yaml < env.
	dir := newDir(t)
	writeYAML(t, dir, "dev", `
http:
  port: 9090
postgres:
  host: pg-yaml
`)
	t.Setenv("ZG_HTTP_PORT", "7777")
	t.Setenv("ZG_POSTGRES_HOST", "pg-env")

	cfg, err := Load(WithConfigDir(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Port != 7777 {
		t.Errorf("http.port: got %d, want 7777 (env wins)", cfg.HTTP.Port)
	}
	if cfg.Postgres.Host != "pg-env" {
		t.Errorf("postgres.host: got %q, want pg-env (env wins)", cfg.Postgres.Host)
	}
	// a field untouched by yaml/env keeps its default
	if cfg.HTTP.WriteTimeout != 15*time.Second {
		t.Errorf("http.write_timeout: got %v, want default 15s", cfg.HTTP.WriteTimeout)
	}
}

func TestLoad_WithEnvSelectsFile(t *testing.T) {
	// WithEnv("staging") must read config.staging.yaml, not config.dev.yaml.
	dir := newDir(t)
	writeYAML(t, dir, "dev", `http: { port: 8001 }`)
	writeYAML(t, dir, "staging", `http: { port: 9001 }`)

	cfg, err := Load(WithConfigDir(dir), WithEnv("staging"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Port != 9001 {
		t.Errorf("http.port: got %d, want 9001 (staging file)", cfg.HTTP.Port)
	}
	if cfg.App.Env != "staging" {
		t.Errorf("app.env: got %q, want %q", cfg.App.Env, "staging")
	}
}

func TestLoad_MissingDirIsNotError(t *testing.T) {
	// A non-existent directory yields ConfigFileNotFoundError which Load ignores.
	cfg, err := Load(WithConfigDir(filepath.Join(t.TempDir(), "does-not-exist")))
	if err != nil {
		t.Fatalf("Load on missing dir should not error: %v", err)
	}
	if cfg == nil {
		t.Fatal("nil config")
	}
	if cfg.HTTP.Port != 8080 {
		t.Errorf("http.port: got %d, want default 8080", cfg.HTTP.Port)
	}
}

func TestSetDefaults_StrictBaseline(t *testing.T) {
	// SetDefaults is the documented layer-1 contract; strict stays on by default.
	cfg, err := Load(WithConfigDir(newDir(t)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Multitenancy.Strict {
		t.Error("multitenancy.strict: got false, want true")
	}
	if cfg.Postgres.MaxConns <= 0 {
		t.Errorf("postgres.max_conns: got %d, want > 0", cfg.Postgres.MaxConns)
	}
}

// TestLoad_AIProvidersEnv verifies the ZG_AI_PROVIDERS_<NAME>_<FIELD> /
// ZG_AI_ROUTES_<TIER>_<FIELD> env→map injection that viper's AutomaticEnv cannot
// perform (CONTRACT §19 / plan B3). Names and tiers are lower-cased.
func TestLoad_AIProvidersEnv(t *testing.T) {
	t.Setenv("ZG_AI_PROVIDERS_QWEN_BASE_URL", "https://dashscope.aliyuncs.com")
	t.Setenv("ZG_AI_PROVIDERS_QWEN_API_KEY", "sk-qwen")
	t.Setenv("ZG_AI_PROVIDERS_DEEPSEEK_MODEL", "deepseek-chat")
	// Mixed-case name should still map to a lower-cased key.
	t.Setenv("ZG_AI_PROVIDERS_Doubao_API_KEY", "sk-doubao")

	t.Setenv("ZG_AI_ROUTES_V3_PROVIDER", "deepseek")
	t.Setenv("ZG_AI_ROUTES_V3_MODEL", "deepseek-chat")
	t.Setenv("ZG_AI_ROUTES_LITE_PROVIDER", "doubao")

	cfg, err := Load(WithConfigDir(newDir(t)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	qwen := cfg.AI.Providers["qwen"]
	if qwen.BaseURL != "https://dashscope.aliyuncs.com" {
		t.Errorf("providers[qwen].base_url: got %q", qwen.BaseURL)
	}
	if qwen.APIKey != "sk-qwen" {
		t.Errorf("providers[qwen].api_key: got %q", qwen.APIKey)
	}
	if cfg.AI.Providers["deepseek"].Model != "deepseek-chat" {
		t.Errorf("providers[deepseek].model: got %q", cfg.AI.Providers["deepseek"].Model)
	}
	if cfg.AI.Providers["doubao"].APIKey != "sk-doubao" {
		t.Errorf("providers[doubao].api_key: got %q (mixed-case name not lower-cased)", cfg.AI.Providers["doubao"].APIKey)
	}

	v3 := cfg.AI.Routes["v3"]
	if v3.Provider != "deepseek" || v3.Model != "deepseek-chat" {
		t.Errorf("routes[v3]: got %+v", v3)
	}
	if cfg.AI.Routes["lite"].Provider != "doubao" {
		t.Errorf("routes[lite].provider: got %q", cfg.AI.Routes["lite"].Provider)
	}
}

// TestLoad_AIProvidersEnvPreservesYaml confirms env only fills/overrides
// individual fields and does not wipe providers already set by the yaml layer.
func TestLoad_AIProvidersEnvPreservesYaml(t *testing.T) {
	dir := newDir(t)
	writeYAML(t, dir, "dev", `
ai:
  providers:
    qwen:
      base_url: "https://from-yaml"
      model: "qwen-from-yaml"
`)
	// Env fills api_key and overrides model; base_url from yaml must survive.
	t.Setenv("ZG_AI_PROVIDERS_QWEN_API_KEY", "sk-env")
	t.Setenv("ZG_AI_PROVIDERS_QWEN_MODEL", "qwen-from-env")

	cfg, err := Load(WithConfigDir(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	qwen := cfg.AI.Providers["qwen"]
	if qwen.BaseURL != "https://from-yaml" {
		t.Errorf("base_url should be preserved from yaml: got %q", qwen.BaseURL)
	}
	if qwen.Model != "qwen-from-env" {
		t.Errorf("model should be overridden by env: got %q", qwen.Model)
	}
	if qwen.APIKey != "sk-env" {
		t.Errorf("api_key should be filled from env: got %q", qwen.APIKey)
	}
}

// TestLoad_WXSubscribeTemplatesEnv verifies the ZG_WX_SUBSCRIBE_TEMPLATES_<CODE>
// env→map injection that viper's AutomaticEnv cannot perform (same gap as the
// AI providers above). The template code is lower-cased into the map key.
func TestLoad_WXSubscribeTemplatesEnv(t *testing.T) {
	t.Setenv("ZG_WX_SUBSCRIBE_TEMPLATES_RECALL_30", "tpl_abc")
	t.Setenv("ZG_WX_SUBSCRIBE_TEMPLATES_BIRTHDAY", "tpl_bday")
	// Mixed-case code should still map to a lower-cased key.
	t.Setenv("ZG_WX_SUBSCRIBE_TEMPLATES_Repurchase", "tpl_rebuy")

	cfg, err := Load(WithConfigDir(newDir(t)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.WX.SubscribeTemplates["recall_30"]; got != "tpl_abc" {
		t.Errorf("subscribe_templates[recall_30]: got %q", got)
	}
	if got := cfg.WX.SubscribeTemplates["birthday"]; got != "tpl_bday" {
		t.Errorf("subscribe_templates[birthday]: got %q", got)
	}
	if got := cfg.WX.SubscribeTemplates["repurchase"]; got != "tpl_rebuy" {
		t.Errorf("subscribe_templates[repurchase]: got %q (mixed-case code not lower-cased)", got)
	}
}

// TestLoad_WXSubscribeTemplatesEnvPreservesYaml confirms env only fills/overrides
// individual entries and does not wipe template mappings already set by yaml.
func TestLoad_WXSubscribeTemplatesEnvPreservesYaml(t *testing.T) {
	dir := newDir(t)
	writeYAML(t, dir, "dev", `
wx:
  subscribe_templates:
    recall_30: "tpl_from_yaml"
    birthday: "tpl_bday_yaml"
`)
	// Env fills points_expire and overrides recall_30; birthday must survive.
	t.Setenv("ZG_WX_SUBSCRIBE_TEMPLATES_POINTS_EXPIRE", "tpl_points_env")
	t.Setenv("ZG_WX_SUBSCRIBE_TEMPLATES_RECALL_30", "tpl_from_env")

	cfg, err := Load(WithConfigDir(dir))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.WX.SubscribeTemplates["birthday"]; got != "tpl_bday_yaml" {
		t.Errorf("birthday should be preserved from yaml: got %q", got)
	}
	if got := cfg.WX.SubscribeTemplates["recall_30"]; got != "tpl_from_env" {
		t.Errorf("recall_30 should be overridden by env: got %q", got)
	}
	if got := cfg.WX.SubscribeTemplates["points_expire"]; got != "tpl_points_env" {
		t.Errorf("points_expire should be filled from env: got %q", got)
	}
}

// TestLoad_NewDefaults covers the payment/jobqueue/scheduler/platform-tenant
// defaults added in plan Phase 0.
func TestLoad_NewDefaults(t *testing.T) {
	cfg, err := Load(WithConfigDir(newDir(t)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Multitenancy.PlatformTenantID != 1 {
		t.Errorf("platform_tenant_id: got %d, want 1", cfg.Multitenancy.PlatformTenantID)
	}
	if cfg.Payment.Configured() {
		t.Error("payment should be unconfigured by default (FakeWxPay path)")
	}
	if cfg.JobQueue.Stream != "zg:jobs" {
		t.Errorf("job_queue.stream: got %q", cfg.JobQueue.Stream)
	}
	if cfg.JobQueue.Group != "zg-workers" {
		t.Errorf("job_queue.group: got %q", cfg.JobQueue.Group)
	}
	if !cfg.Scheduler.RunInAPI {
		t.Error("scheduler.run_in_api should default true (dev in-process)")
	}
}
