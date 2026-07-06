// Command migrate applies SQL migrations from backend/migrations using
// golang-migrate as a library. Run from the backend/ directory (so the default
// migrations dir resolves), or set ZG_MIGRATIONS_DIR.
//
// Usage:
//
//	migrate up            apply all pending migrations
//	migrate down [N]      revert N migrations (default 1)
//	migrate version       print the current schema version
//	migrate force <v>     force-set the schema version (unstick a dirty state)
//
// It reads the same layered config as the API (ZG_ env / config.<env>.yaml) to
// locate the database, so the same ZG_ENV + ZG_POSTGRES_* variables apply.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // postgres driver for migrate
	_ "github.com/golang-migrate/migrate/v4/source/file"       // file:// source for migrate

	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/db"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "migrate %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	cfg, err := config.Load(config.WithEnv(envOr("ZG_ENV", "dev")))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	migrationsDir := envOr("ZG_MIGRATIONS_DIR", "migrations")

	m, err := migrate.New("file://"+migrationsDir, db.DSNFromConfig(cfg.Postgres))
	if err != nil {
		return fmt.Errorf("init migrate: %w", err)
	}
	defer m.Close()

	switch cmd {
	case "up":
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return err
		}
	case "down":
		n := 1
		if len(args) > 0 {
			if n, err = strconv.Atoi(args[0]); err != nil {
				return fmt.Errorf("down: count must be an integer, got %q", args[0])
			}
		}
		if err := m.Steps(-n); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return err
		}
	case "version":
		v, dirty, err := m.Version()
		if err != nil {
			return err
		}
		fmt.Printf("version=%d dirty=%v\n", v, dirty)
	case "force":
		if len(args) < 1 {
			return errors.New("force requires a version argument")
		}
		v, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("force: version must be an integer, got %q", args[0])
		}
		if err := m.Force(v); err != nil {
			return err
		}
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: migrate [up|down [N]|version|force <v>]")
}

func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}
