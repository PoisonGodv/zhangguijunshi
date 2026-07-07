// Package auth implements the three-surface authentication domain (tech plan
// §6.2): wx.login → code2session → self-signed JWT for the consumer mini-program
// (boss/admin login flows land in later milestones). It orchestrates pkg/wxsdk,
// pkg/jwt and a Consumer repository, and is deliberately infrastructure-light:
// every external dependency is an interface, so the whole flow runs in unit
// tests with an in-memory repo, a memory SessionStore and an httptest wx fake.
package auth

import (
	"context"
	"errors"
	"time"
)

// Consumer is a minimal authenticated consumer record. The full membership
// profile (tech plan §04 / §A.1.1 wx_sessions) lands with the membership
// domain; for M0-S2 the auth flow only needs identity + the server-side
// session_key cache.
type Consumer struct {
	ID         int64
	TenantID   int64
	OpenID     string
	UnionID    string
	MemberID   int64
	SessionKey string // SERVER-SIDE ONLY: never serialised into a response.
	Version    int64  // token version; bumped on kick / re-issue
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Subject returns the JWT `sub` claim for this consumer (stable internal id).
func (c *Consumer) Subject() string { return consumerSubjectPrefix + itoa(c.ID) }

const consumerSubjectPrefix = "c_"

// ErrNotFound is returned by repositories when no record matches.
var ErrNotFound = errors.New("auth: consumer not found")

// ConsumerRepo persists consumer identities. The real implementation will be
// pgx-backed (wx_sessions / user_consumer table with tenant_id row isolation);
// an in-memory implementation covers dev and tests until PostgreSQL is wired.
type ConsumerRepo interface {
	// UpsertByOpenID inserts or updates the (tenantID, openid) consumer and
	// caches sessionKey/unionid server-side. Returns the persisted record.
	UpsertByOpenID(ctx context.Context, tenantID int64, openid, unionid, sessionKey string) (*Consumer, error)

	// GetByID loads a consumer by id within tenantID. ErrNotFound if absent.
	GetByID(ctx context.Context, tenantID, id int64) (*Consumer, error)
}

// itoa is a strconv-free int64 -> string (keeps the package import-light).
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
