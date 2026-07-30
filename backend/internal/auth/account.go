package auth

// account.go defines the staff Account model (users table) and the AccountRepo
// interface for the boss/admin password-login flow (CONTRACT §19 / BE-A-01).
//
// The users table (migration 000001) already holds role / phone_hash / status /
// last_login_at; migration 000017 adds the nullable password_hash bcrypt
// column. Real login (boss phone+password, admin username+password) reads that
// column. Plaintext phone is stored encrypted (phone_enc BYTEA) with phone_hash
// for equality lookup, so the PG path does NOT read the plaintext phone back —
// Account.Phone is populated only by the memory repo's dev seed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
)

// Account is a staff record from the users table (only the login-relevant
// fields). Account.Phone is empty on the PG path (no decryption); the memory
// seed fills it for dev readability.
type Account struct {
	ID           int64
	TenantID     int64
	StoreID      int64
	Name         string
	Phone        string // dev seed only; PG path leaves empty (no KMS decryption)
	Role         string
	Status       string
	PasswordHash string
	// Version is the token-revocation version. The users table has NO version
	// column today (CONTRACT §19 note); the PG repo selects a constant 0 and
	// TouchLogin does not bump it. A future migration adds users.version; until
	// then Version is always 0 and the JWT ver check is a no-op.
	Version     int64
	LastLoginAt time.Time
}

// Sentinel errors for the password-login flow. The login flow collapses
// ErrAccountNotFound into ErrLoginFailed so the HTTP response is constant and
// never reveals whether an account exists (user-enumeration defence). All three
// are errcode.Err-typed so httpx.FailError maps them to the right HTTP status
// without a type switch at the handler layer.
var (
	// ErrAccountNotFound: no active account matched the lookup. Returned by
	// AccountRepo; the login flow maps this to ErrLoginFailed before responding.
	ErrAccountNotFound = errcode.Err{Code: 40404, Message: "账号不存在", HTTP: http.StatusNotFound}
	// ErrLoginFailed: single login-rejection error. Wrong phone/username, wrong
	// password, or no such account all collapse here. HTTP 401.
	ErrLoginFailed = errcode.Err{Code: 40102, Message: "账号或密码错误", HTTP: http.StatusUnauthorized}
	// ErrAccountNotConfigured: password login is not wired (no AccountRepo
	// injected). A 500 so ops notice the misconfiguration rather than silently
	// rejecting every login.
	ErrAccountNotConfigured = errcode.Err{Code: 50001, Message: "登录服务未配置", HTTP: http.StatusInternalServerError}
)

// AccountRepo persists staff accounts (users table). Both lookups are scoped by
// tenant_id (ARCH-01 row-level isolation). Implementations return
// ErrAccountNotFound when no active account matches; other errors are wrapped.
type AccountRepo interface {
	// FindByPhone looks up an active account by phone within tenantID. The
	// phone is hashed with PhoneHash and matched against users.phone_hash.
	FindByPhone(ctx context.Context, tenantID int64, phone string) (*Account, error)

	// FindByUsername looks up an active platform account by name within
	// tenantID. The users table has no dedicated username column, so the name
	// column is reused; name is NOT globally unique, so this is only fit for
	// the small, curated set of platform_ops accounts (see admin_login.go).
	FindByUsername(ctx context.Context, tenantID int64, username string) (*Account, error)

	// TouchLogin sets last_login_at = now() for the account. Best-effort: the
	// login flow ignores the returned error.
	TouchLogin(ctx context.Context, id int64) error
}

// phonePepper is the fixed app-level salt folded into phone_hash. The users
// table stores SHA256(pepper+phone) in phone_hash (migration 000001 COMMENT:
// "手机号 SHA256+盐"). There is no per-row salt column, so this is an app-global
// pepper (not a true per-record salt); the lookup-side hash MUST use the same
// value. This is NOT a password hash: phones are matched by exact equality for
// login, so the goal is a deterministic, non-reversible index key.
const phonePepper = "zgjs-phone-v1"

// PhoneHash returns the deterministic phone_hash stored in users.phone_hash:
// hex-encoded SHA256(phonePepper + phone). Used by both the memory seed and the
// PG repo so the write and read paths agree byte-for-byte.
func PhoneHash(phone string) string {
	h := sha256.Sum256([]byte(phonePepper + phone))
	return hex.EncodeToString(h[:])
}
