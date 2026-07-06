// Package db wraps pgxpool to provide a lazily-connected PostgreSQL
// connection pool and a transaction helper with deterministic commit/rollback
// semantics.
//
// The pool is created without contacting the database (pgxpool establishes
// connections lazily), which lets the package build and unit-test in
// environments that have no live PostgreSQL (see CONTRACT.md §0). Call
// (*DB).Ping explicitly at startup to verify connectivity on demand.
package db

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// DB is a thin wrapper around a pgxpool.Pool plus the masked DSN captured at
// construction time (for safe logging).
type DB struct {
	pool *pgxpool.Pool

	// dsn holds the connection string with the password masked; it is safe
	// to emit to logs. The plaintext password is never retained here.
	dsn string
}

// New parses cfg into a DSN, applies pool sizing/lifetime parameters, and
// returns a lazily-connected pool. It does NOT ping the database — pgxpool
// only establishes connections on first use (or in the background up to
// MinConns), so New succeeds even when no PostgreSQL is reachable. Use Ping
// to verify connectivity when a fast failure is desired.
func New(ctx context.Context, cfg config.PostgresConfig) (*DB, error) {
	dsn := buildDSN(cfg)

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse pool config: %w", err)
	}

	// Apply caller-provided sizing only when set; zero values fall back to
	// pgxpool defaults so missing config fields do not shrink the pool to 0.
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}

	return &DB{
		pool: pool,
		dsn:  maskDSN(dsn),
	}, nil
}

// Pool returns the underlying *pgxpool.Pool for direct use by repositories.
func (db *DB) Pool() *pgxpool.Pool {
	return db.pool
}

// Ping verifies connectivity to the database. Call this at startup when you
// want to fail fast on a misconfigured DSN; New deliberately does not ping.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}

// Close releases all pool resources. Safe to call multiple times.
func (db *DB) Close() {
	if db.pool != nil {
		db.pool.Close()
	}
}

// DSN returns the connection string with the password masked, suitable for
// logging. The real password is never exposed.
func (db *DB) DSN() string {
	return db.dsn
}

// DSNFromConfig renders the real (unmasked) connection string for a config,
// for tooling such as the migrate command that needs to open its own
// connection. Production code should prefer New + Pool.
func DSNFromConfig(cfg config.PostgresConfig) string {
	return buildDSN(cfg)
}

// buildDSN renders a PostgreSQL URL from the layered PostgresConfig. Special
// characters in user/password are percent-encoded via net/url so the result is
// always parseable by pgxpool.ParseConfig. sslmode is always emitted, defaulting
// to pgx's "prefer" when unset.
func buildDSN(cfg config.PostgresConfig) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.User, cfg.Password),
		Host:   fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Path:   cfg.Database,
	}
	q := u.Query()
	q.Set("sslmode", sslModeOrDefault(cfg.SSLMode))
	u.RawQuery = q.Encode()
	return u.String()
}

// sslModeOrDefault returns s when non-empty, else pgx's default "prefer".
func sslModeOrDefault(s string) string {
	if s == "" {
		return "prefer"
	}
	return s
}

// maskDSN replaces the password in a URL-form DSN with a literal "***" so the
// mask stays human-readable in logs. url.URL.String() would percent-encode the
// asterisks to %2A%2A%2A, obscuring the mask; operating on the raw string keeps
// the result both readable and round-trippable through url.Parse (which still
// exposes password "***"). If the input is not a URL DSN or has no userinfo,
// it is returned unchanged.
func maskDSN(dsn string) string {
	schemeIdx := strings.Index(dsn, "://")
	if schemeIdx < 0 {
		return dsn
	}
	head := dsn[:schemeIdx+3] // e.g. "postgres://"
	rest := dsn[schemeIdx+3:]
	atIdx := strings.IndexByte(rest, '@')
	if atIdx < 0 {
		return dsn // no userinfo segment to mask
	}
	userinfo := rest[:atIdx] // "user:password" or "user"
	tail := rest[atIdx:]     // "@host:port/db?..."
	// Drop everything after the first ':' so the real password is removed.
	user := userinfo
	if c := strings.IndexByte(userinfo, ':'); c >= 0 {
		user = userinfo[:c]
	}
	if user == "" {
		return head + ":***" + tail
	}
	return head + user + ":***" + tail
}
