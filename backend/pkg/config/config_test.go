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
