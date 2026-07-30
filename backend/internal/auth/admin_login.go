package auth

// admin_login.go adds the platform-admin (运营后台) username+password login flow
// to AuthService (CONTRACT §19 / BE-A-01). AdminLogin looks up the account
// within the configured PlatformTenantID and requires role == platform_ops; any
// other outcome collapses into ErrLoginFailed (no enumeration).
//
// Username trade-off (CONTRACT §19 note): the users table has no dedicated
// username column, so the name column is reused for platform account lookup.
// name is NOT globally unique; this is acceptable ONLY because the set of
// platform_ops accounts is small and curated by ops. A future migration may add
// a unique username column; the AccountRepo interface (FindByUsername) is the
// seam that absorbs that change.

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
			return nil, ErrLoginFailed
		}
		return nil, errcode.ErrInternal
	}
	// platform_ops only: a non-platform_ops account that happens to share the
	// name cannot log in here. Combined with the password check below, the
	// response is constant.
	if acc.Role != "platform_ops" {
		return nil, ErrLoginFailed
	}
	if acc.PasswordHash == "" || VerifyPassword(acc.PasswordHash, password) != nil {
		return nil, ErrLoginFailed
	}

	_ = s.accountRepo.TouchLogin(ctx, acc.ID)

	// Platform accounts are not store-scoped: force StoreID 0 in the issued
	// token regardless of the row (platform rows should already have store_id
	// NULL, this is defence-in-depth).
	acc.StoreID = 0
	return s.signStaffPair(ctx, acc, jwt.SceneAdmin)
}
