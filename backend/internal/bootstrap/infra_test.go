package bootstrap

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// jwtCfg returns a config whose JWT section has NO key_file, so the DevKey
// fallback branch is exercised. Postgres/Redis hosts are left empty so
// BuildInfra skips both pools (no services needed on a dev box). AccessTTL is
// set because jwt.New rejects a non-positive TTL.
func jwtCfg(env string) *config.Config {
	return &config.Config{
		App: config.AppConfig{Env: env},
		JWT: config.JWTConfig{KeyID: "test-kid", Issuer: "zhanggui-test", AccessTTL: 15 * time.Minute},
	}
}

// TestBuildJWTManager_FailClosedNonDev is the regression test for the
// high-severity authz hole: a non-dev env without jwt.key_file must NOT
// silently fall back to the publicly known DevSeed-derived key (which would let
// an attacker forge arbitrary tenant/admin tokens). It must fail closed.
func TestBuildJWTManager_FailClosedNonDev(t *testing.T) {
	core, _ := observer.New(zap.DebugLevel)
	log := zap.New(core)

	for _, env := range []string{"prod", "staging", "production", "anything-else"} {
		t.Run(env, func(t *testing.T) {
			mgr, err := buildJWTManager(jwtCfg(env), log)
			if err == nil {
				t.Fatalf("env=%q without key_file: expected error, got manager %v", env, mgr)
			}
			if mgr != nil {
				t.Fatalf("env=%q: expected nil manager on error, got %v", env, mgr)
			}
			if !strings.Contains(err.Error(), "jwt.key_file") {
				t.Errorf("env=%q: error should point operators at jwt.key_file, got: %v", env, err)
			}
			if !strings.Contains(err.Error(), env) {
				t.Errorf("env=%q: error should name the offending env, got: %v", env, err)
			}
		})
	}
}

// TestBuildJWTManager_DevStillUsesDevKey guards the dev convenience path: the
// dev environment (and the empty/zero-value env used by existing tests) may
// still derive the DevKey so a local server issues verifiable tokens with no
// configuration.
func TestBuildJWTManager_DevStillUsesDevKey(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	log := zap.New(core)

	for _, env := range []string{"dev", ""} {
		t.Run("env="+env, func(t *testing.T) {
			mgr, err := buildJWTManager(jwtCfg(env), log)
			if err != nil {
				t.Fatalf("env=%q: DevKey fallback must succeed, got error: %v", env, err)
			}
			if mgr == nil {
				t.Fatalf("env=%q: expected a working manager, got nil", env)
			}
		})
	}

	// The dev path must still log the loud "do not use in prod" warning.
	if logs.FilterMessage("using derived dev JWT key — DO NOT USE IN PRODUCTION").Len() == 0 {
		t.Error("dev fallback should emit the loud dev-key warning")
	}
}

// TestBuildInfra_FailClosedProdNoKeyFile proves the fail-closed behaviour
// surfaces all the way up through BuildInfra (which wraps the error), so the
// process refuses to boot in prod without a mounted key. No PG/Redis needed:
// empty hosts skip both pools.
func TestBuildInfra_FailClosedProdNoKeyFile(t *testing.T) {
	core, _ := observer.New(zap.DebugLevel)
	log := zap.New(core)

	infra, err := BuildInfra(context.Background(), jwtCfg("prod"), log)
	if err == nil {
		t.Fatalf("prod without key_file: BuildInfra must fail closed, got infra %+v", infra)
	}
	if infra != nil {
		t.Fatalf("prod without key_file: expected nil infra on error, got %+v", infra)
	}
	if !strings.Contains(err.Error(), "jwt") {
		t.Errorf("BuildInfra error should be wrapped as a jwt init failure, got: %v", err)
	}
}

// TestBuildInfra_DevBootsWithoutKeyFile is the positive counterpart: a dev
// config with no key_file boots fine (JWT manager present) so local dev and the
// existing test-suite fixtures are unaffected by the fail-closed change.
func TestBuildInfra_DevBootsWithoutKeyFile(t *testing.T) {
	core, _ := observer.New(zap.DebugLevel)
	log := zap.New(core)

	infra, err := BuildInfra(context.Background(), jwtCfg("dev"), log)
	if err != nil {
		t.Fatalf("dev without key_file: BuildInfra must succeed, got error: %v", err)
	}
	if infra == nil || infra.JWT == nil {
		t.Fatalf("dev: expected infra with a JWT manager, got %+v", infra)
	}
	if infra.Sess == nil {
		t.Fatal("dev: expected a (memory) session store even without Redis")
	}
}
