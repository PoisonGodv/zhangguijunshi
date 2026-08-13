package bootstrap

import (
	"errors"
	"strings"

	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/db"
)

var errProdDatabaseRequired = errors.New("production requires an available PostgreSQL database")

// validateDatabaseRequirement prevents production binaries from silently
// constructing memory repositories when PostgreSQL is missing or unavailable.
// BuildInfra intentionally keeps the dev experience resilient by returning a
// nil DB after configuration, pool, or ping failures; this startup-boundary
// guard turns that degraded state into a hard error only for prod.
//
// Keep the error generic: connection details and credentials must not be
// exposed through the process's fatal error output.
func validateDatabaseRequirement(cfg *config.Config, database *db.DB) error {
	if cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.App.Env), "prod") && database == nil {
		return errProdDatabaseRequired
	}
	return nil
}
