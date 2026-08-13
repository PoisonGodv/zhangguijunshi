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
	"errors"
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
	Username     string // dedicated login name (users.username, migration 000018); per-tenant unique. Distinct from Name (display name, not unique). PG path leaves it empty (not selected) unless a lookup-by-username path needs it.
	Phone        string // dev seed only; PG path leaves empty (no KMS decryption)
	Role         string
	Status       string
	PasswordHash string
	// Version is the token-revocation version. The users table has a real
	// version BIGINT NOT NULL DEFAULT 0 column (migration 000018); the PG repo
	// selects it and BumpVersion increments it. It is the hook for
	// revoke-on-password-change (bump on password change so old tokens' ver no
	// longer match). The JWT ver check is still a no-op until the middleware
	// starts comparing it, but Version is now a live value rather than a
	// constant 0.
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

	// FindByUsername looks up an active platform account by its dedicated
	// username within tenantID, matching users.username (migration 000018).
	// username is per-tenant unique (partial unique index excluding soft-deleted
	// and NULL rows), so at most one active account matches. This is the seam the
	// admin username+password login flow relies on (see admin_login.go).
	FindByUsername(ctx context.Context, tenantID int64, username string) (*Account, error)

	// TouchLogin sets last_login_at = now() for the account. Best-effort: the
	// login flow ignores the returned error.
	TouchLogin(ctx context.Context, id int64) error

	// BumpVersion atomically increments users.version for the account and returns
	// the new value. This is the hook for revoke-on-password-change: bumping
	// version invalidates every previously issued token (their ver no longer
	// matches). Returns ErrAccountNotFound when no row was updated.
	BumpVersion(ctx context.Context, id int64) (int64, error)

	// FindByID loads an active account by primary key within tenantID. Used by
	// the password-change flow to re-load the account after identifying it from
	// the verified JWT — the tenant scope (ARCH-01) means a cross-tenant id can
	// never be loaded, so a forged account_id in a token from another tenant
	// resolves to ErrAccountNotFound rather than to someone else's row.
	FindByID(ctx context.Context, tenantID, id int64) (*Account, error)

	// UpdatePassword writes the new bcrypt hash for the account. It does NOT bump
	// version — that is a separate explicit BumpVersion call so ChangePassword
	// controls ordering (write hash → bump version → invalidate cached tokens).
	// Scoped by id (the caller has already tenant-verified the account via
	// FindByID). Returns ErrAccountNotFound when no row was updated (concurrent
	// delete / unknown id) so the caller can distinguish a missing account from a
	// real DB fault.
	UpdatePassword(ctx context.Context, id int64, newHash string) error

	// Create 持久化一个新员工账号(冷启动引导路径: 首个租户 + 首个 boss 账号,
	// 见 cmd/migrate seed-account)。bcrypt 哈希由调用方算好(acc.PasswordHash
	// 必须是 HashPassword 的产物), repo 只做持久化、绝不接触明文。PG 路径写
	// phone_hash = PhoneHash(acc.Phone)(无 KMS, 不写 phone_enc); store_id /
	// username / phone 为空时落 NULL(部分唯一索引排除 NULL)。空 PasswordHash
	// 拒绝(errEmptyPasswordHash), 防止造出永远无法登录的账号。返回带分配 ID
	// 的账号。
	Create(ctx context.Context, acc *Account) (*Account, error)
}

// errEmptyPasswordHash 守住 Create 路径: 持久化空哈希会造出永远无法登录的
// 账号(登录流拒绝空 hash)。与 errEmptyPassword 同为内部错误, 不映射 HTTP。
var errEmptyPasswordHash = errors.New("auth: empty password hash")

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
