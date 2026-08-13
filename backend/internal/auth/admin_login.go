package auth

// admin_login.go adds the platform-admin (运营后台) username+password login flow
// to AuthService (CONTRACT §19 / BE-A-01). AdminLogin looks up the account
// within the configured PlatformTenantID and requires role == platform_ops; any
// other outcome collapses into ErrLoginFailed (no enumeration).
//
// Username (migration 000018): users now has a dedicated, per-tenant-unique
// username column (partial unique index excluding soft-deleted/NULL rows).
// AdminLogin resolves accounts through AccountRepo.FindByUsername, which matches
// that column — no longer the non-unique display name. Seeded platform accounts
// must be given a username (the memory seed sets username "ops").

import (
	"context"
	"errors"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/jwt"
)

// AdminLogin authenticates a platform_ops account by username + password and
// returns a fresh access+refresh pair. The tenant is the configured
// PlatformTenantID (platform accounts live in that single tenant); StoreID is 0
// (platform accounts are not store-scoped).
func (s *AuthService) AdminLogin(ctx context.Context, username, password string) (*LoginResult, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrAccountNotConfigured
	}
	if username == "" || password == "" {
		return nil, errcode.ErrParam
	}

	tenant := s.platformTenantID
	acc, err := s.accountRepo.FindByUsername(ctx, tenant, username)
	if err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			_ = VerifyPassword(dummyPasswordHash, password) // timing pad
			return nil, ErrLoginFailed
		}
		return nil, errcode.ErrInternal
	}
	// platform_ops only: a non-platform_ops account that happens to share the
	// name cannot log in here. Both the role mismatch and an empty stored hash
	// run one dummy bcrypt compare so the latency matches the wrong-password
	// path — combined with the constant ErrLoginFailed body there is no
	// enumeration oracle (neither content nor timing).
	if acc.Role != "platform_ops" || acc.PasswordHash == "" {
		_ = VerifyPassword(dummyPasswordHash, password)
		return nil, ErrLoginFailed
	}
	if VerifyPassword(acc.PasswordHash, password) != nil {
		return nil, ErrLoginFailed
	}

	_ = s.accountRepo.TouchLogin(ctx, acc.ID)

	// Platform accounts are not store-scoped: force StoreID 0 in the issued
	// token regardless of the row (platform rows should already have store_id
	// NULL, this is defence-in-depth).
	acc.StoreID = 0
	return s.signStaffPair(ctx, acc, jwt.SceneAdmin)
}
