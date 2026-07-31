package auth

// change_password.go adds the ChangePassword flow + the WithVersionStore option
// to AuthService (CONTRACT §20.5 follow-up). This file lands the two callers
// that §20.5 deferred: the password-change flow that calls BumpVersion, and the
// VersionStore wiring that lets the JWT middleware enforce the `ver` claim.
//
// auth.New's signature is frozen (consumer flow), so — like WithAccountRepo —
// the VersionStore is injected via a WithXxx method-on-pointer after construct.

import (
	"context"
	"errors"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/jwt"
)

// WithVersionStore injects the token-version cache used by ChangePassword (to
// invalidate stale tokens immediately on password change) and read by the JWT
// middleware's ver check. nil leaves the ver check best-effort-only (the
// middleware allows every token whose version is not known-stale) — the
// degraded mode for local dev without Redis. Mirrors WithAccountRepo: called by
// bootstrap after auth.New, returns s for chaining.
func (s *AuthService) WithVersionStore(vs VersionStore) *AuthService {
	s.versionStore = vs
	return s
}

// password policy bounds for the new-password sanity check. Min 6 matches the
// shortest seeded password shape ("ops12345" is 8); max 64 is well under bcrypt's
// 72-byte input cap and keeps pathological inputs out. These are sanity bounds,
// not a full strength policy (no complexity rules) — the goal is to reject
// obviously-bad inputs cheaply before the bcrypt hash.
const (
	minPasswordLen = 6
	maxPasswordLen = 64
)

// ChangePassword verifies oldPassword, writes a new bcrypt hash, bumps the
// account's token version, and (if a VersionStore is wired) Incrs the cached
// version so every previously-issued token fails the middleware's ver check on
// the very next request — no DB hit per request.
//
// Flow (CONTRACT §20.5):
//  1. Param sanity (non-empty, new != old, length bounds) -> ErrParam.
//  2. Load the account by id (tenant-scoped via FindByID: ARCH-01) -> not found
//     returns ErrAccountNotFound (NOT collapsed, because the caller is an
//     already-authenticated identity — there is no enumeration surface here).
//  3. Verify oldPassword via VerifyPassword; ANY failure (empty hash, mismatch,
//     malformed) collapses to ErrLoginFailed — constant response, no enumeration,
//     matching BossLogin.
//  4. Hash newPassword, UpdatePassword (hash only — no version bump here).
//  5. BumpVersion -> new version (this is what invalidates old tokens).
//  6. If versionStore != nil, Incr BOTH staff scenes (boss + admin) for the id.
//     A staff account logs in under exactly one scene in practice (boss phone
//     login requires role != platform_ops; admin username login requires role ==
//     platform_ops), so only one scene has a cached entry; bumping both is
//     defence-in-depth and the stray key is never read. We bump both because
//     ChangePassword takes no scene parameter and cannot tell which applies.
//
// The returned *Account carries the NEW PasswordHash and Version; the caller MAY
// re-sign a fresh token pair from it, but this implementation does NOT — the
// client is expected to re-login (the old session is now invalid by design, and
// forcing a fresh credential check after a password change is the secure
// default). Documented so the handler can evolve to re-sign without changing
// this signature.
func (s *AuthService) ChangePassword(ctx context.Context, tenantID, accountID int64, oldPassword, newPassword string) (*Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrAccountNotConfigured
	}
	// 1. Param sanity. new == old is rejected up-front: allowing it would hash a
	// no-op change and still bump version (logging everyone out for nothing).
	if oldPassword == "" || newPassword == "" {
		return nil, errcode.ErrParam
	}
	if newPassword == oldPassword {
		return nil, errcode.ErrParam
	}
	if len(newPassword) < minPasswordLen || len(newPassword) > maxPasswordLen {
		return nil, errcode.ErrParam
	}

	// 2. Load account (tenant-scoped). Not-found here is a real 404, not a login
	// collapse: the caller already proved identity via JWT, so revealing "no such
	// account" is fine and helps the client surface a meaningful error.
	acc, err := s.accountRepo.FindByID(ctx, tenantID, accountID)
	if err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			return nil, ErrAccountNotFound
		}
		return nil, errcode.ErrInternal
	}

	// 3. Verify old password. Empty hash (never set) or any verify failure both
	// collapse to ErrLoginFailed — constant response, no user enumeration.
	if acc.PasswordHash == "" || VerifyPassword(acc.PasswordHash, oldPassword) != nil {
		return nil, ErrLoginFailed
	}

	// 4. Hash + write. HashPassword failure is a 500 (bcrypt broken / too long).
	newHash, err := HashPassword(newPassword)
	if err != nil {
		return nil, errcode.ErrInternal
	}
	if err := s.accountRepo.UpdatePassword(ctx, accountID, newHash); err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			return nil, ErrAccountNotFound
		}
		return nil, errcode.ErrInternal
	}

	// 5. Bump version -> invalidates every previously-issued token (their ver no
	// longer matches current) once the middleware checks ver.
	newVer, err := s.accountRepo.BumpVersion(ctx, accountID)
	if err != nil {
		// Extremely unlikely (FindByID just succeeded); surface as 500.
		if errors.Is(err, ErrAccountNotFound) {
			return nil, ErrAccountNotFound
		}
		return nil, errcode.ErrInternal
	}
	acc.PasswordHash = newHash
	acc.Version = newVer

	// 6. Invalidate the cached version so the middleware rejects old tokens on
	// the next request without a DB hit. Best-effort: a Redis flap here only
	// delays invalidation until the cache expires / next login re-Sets; it never
	// fails the password change itself.
	if s.versionStore != nil {
		// Best-effort: a Redis flap only delays invalidation; it never fails the
		// password change. Discard the returned version / error.
		_, _ = s.versionStore.Incr(ctx, jwt.SceneBoss, accountID)
		_, _ = s.versionStore.Incr(ctx, jwt.SceneAdmin, accountID)
	}

	return acc, nil
}
