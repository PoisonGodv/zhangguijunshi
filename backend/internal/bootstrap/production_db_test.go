package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/db"
)

func TestValidateDatabaseRequirement_ProdRejectsNilDB(t *testing.T) {
	cfg := &config.Config{}
	cfg.App.Env = "prod"

	err := validateDatabaseRequirement(cfg, nil)
	require.ErrorIs(t, err, errProdDatabaseRequired)
}

func TestValidateDatabaseRequirement_ProdEnvIsNormalized(t *testing.T) {
	for _, env := range []string{"PROD", " prod ", "PrOd"} {
		t.Run(env, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.App.Env = env
			require.ErrorIs(t, validateDatabaseRequirement(cfg, nil), errProdDatabaseRequired)
		})
	}
}

func TestValidateDatabaseRequirement_DevAllowsNilDB(t *testing.T) {
	cfg := &config.Config{}
	cfg.App.Env = "dev"

	require.NoError(t, validateDatabaseRequirement(cfg, nil))
}

func TestValidateDatabaseRequirement_ProdAllowsConfiguredDBObject(t *testing.T) {
	cfg := &config.Config{}
	cfg.App.Env = "prod"

	// The startup guard only distinguishes the successfully initialized real-DB
	// path from the nil/degraded path. Connectivity was already verified by
	// BuildInfra before it stores the object, so no live PostgreSQL is needed here.
	require.NoError(t, validateDatabaseRequirement(cfg, &db.DB{}))
}
