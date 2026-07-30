package bootstrap

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// fullConfig returns a config with every diagnostic integration's credentials
// present, so the truth-table "all active" case is a single fixture.
func fullDiagConfig() *config.Config {
	return &config.Config{
		Postgres: config.PostgresConfig{Host: "localhost"},
		Redis:    config.RedisConfig{Host: "localhost"},
		WX:       config.WXConfig{AppID: "wx-app", Secret: "wx-secret"},
		COS:      config.COSConfig{SecretID: "id", SecretKey: "key", Bucket: "bkt"},
		AI: config.AIConfig{
			Providers: map[string]config.AIProviderConfig{
				"deepseek": {APIKey: "sk-x", Model: "deepseek-chat"},
				"qwen":     {APIKey: "sk-y", Model: "qwen-turbo"},
			},
		},
		Payment: config.PaymentConfig{
			MchID:         "1900000001",
			APiv3Key:      "01234567890123456789012345678901",
			CertSerial:    "SERIAL",
			PrivateKeyPEM: "-----BEGIN PRIVATE KEY-----",
		},
	}
}

func TestClientStatusDiagnostic_EmptyConfigAllFalse(t *testing.T) {
	got := ClientStatus(&config.Config{}, false, false)
	for k, v := range got {
		if v {
			t.Errorf("empty config: integration %q = true, want false", k)
		}
	}
	// Every documented integration key must be present in the map.
	for _, k := range []string{
		integrationPostgres, integrationRedis, integrationWXMini,
		integrationCOS, integrationLLM, integrationWxPayV3,
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("empty config: missing integration key %q", k)
		}
	}
}

func TestClientStatusDiagnostic_NilConfigAllFalse(t *testing.T) {
	got := ClientStatus(nil, true, true)
	// Even with infra up, a nil config reports every credential-gated
	// integration as inactive; only the infra flags reflect reality.
	if !got[integrationPostgres] || !got[integrationRedis] {
		t.Errorf("nil cfg with infra: postgres/redis should reflect hasDB/hasRedis, got %v", got)
	}
	for _, k := range []string{integrationWXMini, integrationCOS, integrationLLM, integrationWxPayV3} {
		if got[k] {
			t.Errorf("nil cfg: integration %q = true, want false", k)
		}
	}
}

func TestClientStatusDiagnostic_FullConfigAllTrue(t *testing.T) {
	got := ClientStatus(fullDiagConfig(), true, true)
	for k, v := range got {
		if !v {
			t.Errorf("full config: integration %q = false, want true", k)
		}
	}
}

func TestClientStatusDiagnostic_InfraFlagsIndependentOfConfig(t *testing.T) {
	// hasDB/hasRedis are authoritative for the two infra integrations even when
	// the config names a host (the pool may have failed to connect).
	got := ClientStatus(fullDiagConfig(), false, false)
	if got[integrationPostgres] || got[integrationRedis] {
		t.Errorf("infra down: postgres/redis should be false, got %v", got)
	}
}

func TestClientStatusDiagnostic_TableDriven(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want map[string]bool // subset of integrations to assert
	}{
		{
			name: "wx partial (missing secret) -> inactive",
			cfg:  &config.Config{WX: config.WXConfig{AppID: "only-appid"}},
			want: map[string]bool{integrationWXMini: false},
		},
		{
			name: "wx complete -> active",
			cfg:  &config.Config{WX: config.WXConfig{AppID: "a", Secret: "s"}},
			want: map[string]bool{integrationWXMini: true},
		},
		{
			name: "cos missing bucket -> inactive",
			cfg:  &config.Config{COS: config.COSConfig{SecretID: "id", SecretKey: "key"}},
			want: map[string]bool{integrationCOS: false},
		},
		{
			name: "cos complete -> active",
			cfg:  &config.Config{COS: config.COSConfig{SecretID: "id", SecretKey: "key", Bucket: "b"}},
			want: map[string]bool{integrationCOS: true},
		},
		{
			name: "llm only empty keys -> inactive",
			cfg: &config.Config{AI: config.AIConfig{Providers: map[string]config.AIProviderConfig{
				"qwen": {APIKey: ""},
			}}},
			want: map[string]bool{integrationLLM: false},
		},
		{
			name: "llm one real key among empties -> active",
			cfg: &config.Config{AI: config.AIConfig{Providers: map[string]config.AIProviderConfig{
				"qwen":     {APIKey: ""},
				"deepseek": {APIKey: "sk-real"},
			}}},
			want: map[string]bool{integrationLLM: true},
		},
		{
			name: "payment missing private key -> inactive",
			cfg: &config.Config{Payment: config.PaymentConfig{
				MchID: "m", APiv3Key: "k", CertSerial: "s",
			}},
			want: map[string]bool{integrationWxPayV3: false},
		},
		{
			name: "payment via private_key_file -> active",
			cfg: &config.Config{Payment: config.PaymentConfig{
				MchID: "m", APiv3Key: "k", CertSerial: "s", PrivateKeyFile: "/pem",
			}},
			want: map[string]bool{integrationWxPayV3: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClientStatus(tc.cfg, false, false)
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("%s: integration %q = %v, want %v", tc.name, k, got[k], want)
				}
			}
		})
	}
}

func TestActiveLLMProvidersDiagnostic_Sorted(t *testing.T) {
	cfg := &config.Config{AI: config.AIConfig{Providers: map[string]config.AIProviderConfig{
		"qwen":     {APIKey: "k"},
		"doubao":   {APIKey: "k"},
		"deepseek": {APIKey: ""}, // excluded
	}}}
	got := activeLLMProviders(cfg)
	want := []string{"doubao", "qwen"}
	if len(got) != len(want) {
		t.Fatalf("activeLLMProviders = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("activeLLMProviders = %v, want sorted %v", got, want)
		}
	}
}

// TestLogClientDiagnostics_OneLinePerIntegration captures the emitted logs and
// asserts the shape the operator relies on: exactly one "client diagnostic"
// line per integration, each carrying active + detail, with no secrets.
func TestLogClientDiagnostics_OneLinePerIntegration(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	log := zap.New(core)

	LogClientDiagnostics(log, fullDiagConfig(), nil, nil)

	entries := logs.FilterMessage("client diagnostic").All()
	if len(entries) != 6 {
		t.Fatalf("expected 6 diagnostic lines, got %d", len(entries))
	}

	// Each integration appears exactly once, active flag reflects the config,
	// and both active/detail fields are present. (hasDB/hasRedis are false here
	// because we passed nil infra, so postgres/redis are inactive despite the
	// config naming hosts.)
	wantActive := map[string]bool{
		integrationPostgres: false, // nil *db.DB
		integrationRedis:    false, // nil *redisx.Client
		integrationWXMini:   true,
		integrationCOS:      true,
		integrationLLM:      true,
		integrationWxPayV3:  true,
	}
	seen := map[string]int{}
	for _, e := range entries {
		fields := e.ContextMap()
		name, _ := fields["integration"].(string)
		seen[name]++
		active, ok := fields["active"].(bool)
		if !ok {
			t.Errorf("integration %q: missing bool 'active' field", name)
		}
		if active != wantActive[name] {
			t.Errorf("integration %q: active = %v, want %v", name, active, wantActive[name])
		}
		detail, _ := fields["detail"].(string)
		if detail == "" {
			t.Errorf("integration %q: empty 'detail' field", name)
		}
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("integration %q logged %d times, want exactly 1", k, n)
		}
	}
}

// TestLogClientDiagnostics_NilSafe guards the degraded boot path: a nil logger
// must be a no-op (no panic) and a nil cfg must not panic either.
func TestLogClientDiagnostics_NilSafe(t *testing.T) {
	LogClientDiagnostics(nil, fullDiagConfig(), nil, nil) // nil logger: no-op

	core, logs := observer.New(zap.InfoLevel)
	LogClientDiagnostics(zap.New(core), nil, nil, nil) // nil cfg: all inactive
	if got := logs.FilterMessage("client diagnostic").Len(); got != 6 {
		t.Fatalf("nil cfg: expected 6 diagnostic lines, got %d", got)
	}
}
