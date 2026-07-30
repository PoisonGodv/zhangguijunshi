package auth

// boss_login.go adds the boss (merchant) phone+password login flow to
// AuthService (CONTRACT §19 / BE-A-01). It is a separate file so service.go's
// existing method bodies and the auth.New signature stay untouched; Phase 2's
// bootstrap calls WithAccountRepo after auth.New.

import (
	"context"
	"errors"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/jwt"
)

// WithAccountRepo injects the staff-account repo used by BossLogin/AdminLogin.
// Phase 2's bootstrap calls this after auth.New (auth.New's signature is frozen
// for the consumer flow). nil leaves password login disabled, in which case
// BossLogin/AdminLogin return ErrAccountNotConfigured.
func (s *AuthService) WithAccountRepo(r AccountRepo) *AuthService {
	s.accountRepo = r
	return s
}

// BossLogin authenticates a staff member by phone + password within tenantID
// and returns a fresh access+refresh pair. tenantID is taken from the request
// body here because no authenticated identity exists yet (standard credential
// triple); every subsequent business request takes tenant from the JWT, never
// the body (ARCH-01).
func (s *AuthService) BossLogin(ctx context.Context, tenantID int64, phone, password string) (*LoginResult, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrAccountNotConfigured
	}
	if tenantID <= 0 || phone == "" || password == "" {
		return nil, errcode.ErrParam
	}

	acc, err := s.accountRepo.FindByPhone(ctx, tenantID, phone)
	if err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			// Collapse not-found into login-failed so the response cannot
			// distinguish "no such account" from "wrong password".
			return nil, ErrLoginFailed
		}
		return nil, errcode.ErrInternal
	}
	// Empty hash (account never given a password) or verify failure both map to
	// the same ErrLoginFailed — constant response, no user enumeration.
	if acc.PasswordHash == "" || VerifyPassword(acc.PasswordHash, password) != nil {
		return nil, ErrLoginFailed
	}

	// Best-effort: a failed last_login_at touch must not fail an otherwise
	// valid login (the timestamp is telemetry, not a security control).
	_ = s.accountRepo.TouchLogin(ctx, acc.ID)

	return s.signStaffPair(ctx, acc, jwt.SceneBoss)
}

// signStaffPair signs the access+refresh pair for a staff account in the given
// scene (boss|admin). Shared by BossLogin and AdminLogin. The refresh carries
// enough data to re-sign an access token without a DB hit (same contract as the
// consumer issuePair in service.go).
func (s *AuthService) signStaffPair(ctx context.Context, acc *Account, scene string) (*LoginResult, error) {
	subject := "u_" + itoa(acc.ID)
	access, _, err := s.jwt.Sign(ctx, jwt.IssueInput{
		Subject:  subject,
		Scene:    scene,
		TenantID: acc.TenantID,
		StoreID:  acc.StoreID,
		Role:     acc.Role,
		Version:  acc.Version,
	})
	if err != nil {
		return nil, errcode.ErrInternal
	}
	refresh, err := s.store.IssueRefresh(ctx, jwt.RefreshData{
		TenantID: acc.TenantID,
		StoreID:  acc.StoreID,
		UserID:   acc.ID,
		Subject:  subject,
		Scene:    scene,
		Role:     acc.Role,
		Version:  acc.Version,
	}, s.refreshTTL)
	if err != nil {
		return nil, errcode.ErrInternal
	}
	return &LoginResult{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int(s.accessTTL.Seconds()),
		TokenType:    "Bearer",
	}, nil
}
