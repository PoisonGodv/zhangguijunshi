// Package redisx wraps go-redis to provide a lazily-connected Redis client with
// the same "tolerate a missing dependency" philosophy as pkg/db (CONTRACT.md
// §0): the client is constructed without contacting Redis, and a boot-time
// Ping decides availability. When Redis is absent (local dev), the wiring layer
// simply constructs a nil client and falls back to in-memory implementations
// of the stores (jwt.SessionStore, wx session cache, ai Budget).
package redisx

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/zhangguijunshi/backend/pkg/config"
)

// Client is a thin wrapper around *redis.Client. The embedded *redis.Client
// satisfies redis.Cmdable, so it can be passed directly to any store that
// accepts a Cmdable (e.g. jwt.NewRedisSessionStore).
type Client struct {
	*redis.Client
	addr string
}

// New builds a lazily-connected client from cfg. It does NOT ping Redis —
// go-redis only connects on first command — so New succeeds even with no live
// Redis. Use Ping at startup to verify connectivity.
func New(_ context.Context, cfg config.RedisConfig) (*Client, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("redisx: redis host is empty")
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	opts := &redis.Options{
		Addr:     addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	}
	if cfg.PoolSize > 0 {
		opts.PoolSize = cfg.PoolSize
	}
	return &Client{Client: redis.NewClient(opts), addr: addr}, nil
}

// Ping verifies connectivity. A non-nil error means Redis is not reachable and
// the caller should fall back to in-memory stores.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.Client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redisx: ping %s: %w", c.addr, err)
	}
	return nil
}

// Addr returns the configured address (for safe logging).
func (c *Client) Addr() string { return c.addr }
