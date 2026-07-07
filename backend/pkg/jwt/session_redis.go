package jwt

// Redis SessionStore: production-grade, backed by go-redis. Refresh tokens and
// the jti blacklist live in Redis so they survive restarts and are visible to
// every replica. Keys:
//
//	rt:<token>  -> JSON(RefreshData)   TTL = refresh lifetime
//	bl:<jti>    -> "1"                 TTL = access token remaining lifetime
//
// The implementation depends only on redis.Cmdable so both a real *redis.Client
// (from pkg/redisx) and an in-process miniredis client (tests) satisfy it.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	refreshPrefix  = "rt:"
	blacklistKeyPF = "bl:"
)

// RedisSessionStore is the Redis-backed SessionStore.
type RedisSessionStore struct {
	c redis.Cmdable
}

// NewRedisSessionStore wraps a redis.Cmdable (typically *redis.Client). The
// caller owns the client lifecycle; Close is a no-op.
func NewRedisSessionStore(c redis.Cmdable) *RedisSessionStore {
	return &RedisSessionStore{c: c}
}

// IssueRefresh implements SessionStore.
func (s *RedisSessionStore) IssueRefresh(ctx context.Context, data RefreshData, ttl time.Duration) (string, error) {
	tok, err := generateRefreshToken()
	if err != nil {
		return "", err
	}
	buf, err := marshalRefresh(data)
	if err != nil {
		return "", err
	}
	if err := s.c.Set(ctx, refreshPrefix+tok, buf, ttl).Err(); err != nil {
		return "", fmt.Errorf("jwt: store refresh: %w", err)
	}
	return tok, nil
}

// GetRefresh implements SessionStore.
func (s *RedisSessionStore) GetRefresh(ctx context.Context, token string) (RefreshData, error) {
	buf, err := s.c.Get(ctx, refreshPrefix+token).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return RefreshData{}, ErrRefreshNotFound
		}
		return RefreshData{}, fmt.Errorf("jwt: get refresh: %w", err)
	}
	return unmarshalRefresh(buf)
}

// RevokeRefresh implements SessionStore.
func (s *RedisSessionStore) RevokeRefresh(ctx context.Context, token string) error {
	if err := s.c.Del(ctx, refreshPrefix+token).Err(); err != nil {
		return fmt.Errorf("jwt: revoke refresh: %w", err)
	}
	return nil
}

// BlockJTI implements SessionStore.
func (s *RedisSessionStore) BlockJTI(ctx context.Context, jti string, ttl time.Duration) error {
	if err := s.c.Set(ctx, blacklistKeyPF+jti, "1", ttl).Err(); err != nil {
		return fmt.Errorf("jwt: block jti: %w", err)
	}
	return nil
}

// IsBlocked implements SessionStore.
func (s *RedisSessionStore) IsBlocked(ctx context.Context, jti string) (bool, error) {
	n, err := s.c.Exists(ctx, blacklistKeyPF+jti).Result()
	if err != nil {
		return false, fmt.Errorf("jwt: check jti: %w", err)
	}
	return n > 0, nil
}

// Close is a no-op: the caller owns the Redis client.
func (s *RedisSessionStore) Close() error { return nil }
