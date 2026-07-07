package jwt

// This file defines the SessionStore: the server-side state behind opaque
// refresh tokens and the access-token jti blacklist (tech plan §6.2.5).
//
// Why an interface with two implementations:
//   - memory — stdlib only, used in local dev and every unit test (the box has
//     no Docker/Redis, mirroring how pkg/db tolerates a missing PostgreSQL).
//   - redis   — go-redis backed, used in staging/prod where refresh tokens and
//     the blacklist must survive restarts and be visible to all replicas.
//
// The wiring layer picks one at boot (see cmd/api): if a Redis pool is
// available it constructs NewRedisSessionStore; otherwise NewMemorySessionStore.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// RefreshData is the server-side record an opaque refresh token resolves to.
// It carries everything needed to re-sign an access token without a DB hit.
type RefreshData struct {
	TenantID int64  `json:"t"`
	StoreID  int64  `json:"s"`
	UserID   int64  `json:"u"`
	Subject  string `json:"sub"`
	Scene    string `json:"sc"`
	Role     string `json:"r"`
	OpenID   string `json:"oid"`
	Version  int64  `json:"v"`
	DeviceID string `json:"d"`
}

// ErrRefreshNotFound is returned by GetRefresh when the token is absent or
// expired (revoked/evicted). Callers map this to errcode.ErrTokenInvalid.
var ErrRefreshNotFound = errors.New("jwt: refresh token not found or expired")

// SessionStore holds the server-side state for refresh tokens and the jti
// blacklist. Both must be available across replicas in production, hence the
// Redis implementation; the memory implementation is for dev/test only.
type SessionStore interface {
	// IssueRefresh mints a fresh opaque refresh token, stores data under it with
	// ttl, and returns the token string to hand to the client.
	IssueRefresh(ctx context.Context, data RefreshData, ttl time.Duration) (string, error)

	// GetRefresh resolves an opaque refresh token to its stored data.
	GetRefresh(ctx context.Context, token string) (RefreshData, error)

	// RevokeRefresh deletes a refresh token (logout / single-device kick).
	RevokeRefresh(ctx context.Context, token string) error

	// BlockJTI adds an access token's jti to the blacklist for ttl (the access
	// token's remaining lifetime). Subsequent IsBlocked calls return true.
	BlockJTI(ctx context.Context, jti string, ttl time.Duration) error

	// IsBlocked reports whether jti is on the blacklist.
	IsBlocked(ctx context.Context, jti string) (bool, error)

	// Close releases background resources. It is a no-op for the Redis impl.
	Close() error
}

// generateRefreshToken returns 32 cryptographically random bytes as base64url
// (43 chars). Centralised here so both implementations mint identically.
func generateRefreshToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// marshalRefresh / unmarshalRefresh isolate the JSON encoding so the Redis
// implementation and tests share one wire format.
func marshalRefresh(d RefreshData) ([]byte, error) {
	return json.Marshal(d)
}

func unmarshalRefresh(b []byte) (RefreshData, error) {
	var d RefreshData
	if err := json.Unmarshal(b, &d); err != nil {
		return RefreshData{}, err
	}
	return d, nil
}
