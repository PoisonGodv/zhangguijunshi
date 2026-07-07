package auth

// AuthService orchestrates the consumer login flow: wx.login code -> code2session
// -> upsert consumer (caching session_key server-side) -> issue access+refresh.
// It also handles refresh (with sliding renewal) and logout (jti blacklist +
// refresh revoke). Every collaborator is injected so the flow is fully testable
// without Redis/Postgres/WeChat.

import (
	"context"
	"errors"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/wxsdk"
)

// Code2Sessioner is the subset of *wxsdk.Client the service needs.
type Code2Sessioner interface {
	Code2Session(ctx context.Context, appID, secret, code string) (*wxsdk.Session, error)
}

// Config carries the AuthService parameters.
type Config struct {
	AppID        string
	Secret       string
	AccessTTL    time.Duration
	RefreshTTL   time.Duration
	RefreshSlack time.Duration // renew refresh when remaining TTL below this; 0 -> default 7d
}

// AuthService is the consumer-auth orchestrator.
type AuthService struct {
	wx           Code2Sessioner
	jwt          *jwt.Manager
	store        jwt.SessionStore
	repo         ConsumerRepo
	appID        string
	secret       string
	accessTTL    time.Duration
	refreshTTL   time.Duration
	refreshSlack time.Duration
}

// New constructs an AuthService. The same wx credentials are used for every
// tenant in M0-S2 POC; multi-tenant credential mapping lands with the merchant
// domain.
func New(cfg Config, wx Code2Sessioner, mgr *jwt.Manager, store jwt.SessionStore, repo ConsumerRepo) *AuthService {
	slack := cfg.RefreshSlack
	if slack <= 0 {
		slack = 7 * 24 * time.Hour // tech plan §6.2.5: renew refresh when < 7d left
	}
	return &AuthService{
		wx:           wx,
		jwt:          mgr,
		store:        store,
		repo:         repo,
		appID:        cfg.AppID,
		secret:       cfg.Secret,
		accessTTL:    cfg.AccessTTL,
		refreshTTL:   cfg.RefreshTTL,
		refreshSlack: slack,
	}
}

// LoginResult is the JSON returned to the mini-program. It intentionally never
// contains session_key or openid (openid is not needed client-side).
type LoginResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// WxLogin runs the full consumer login flow for a tenant.
func (s *AuthService) WxLogin(ctx context.Context, tenantID int64, code string) (*LoginResult, error) {
	if tenantID <= 0 {
		return nil, errcode.ErrParam
	}
	if code == "" {
		return nil, errcode.ErrParam
	}

	sess, err := s.wx.Code2Session(ctx, s.appID, s.secret, code)
	if err != nil {
		return nil, mapWXErr(err)
	}

	// Upsert caches session_key SERVER-SIDE only (never returned to the client).
	consumer, err := s.repo.UpsertByOpenID(ctx, tenantID, sess.OpenID, sess.UnionID, sess.SessionKey)
	if err != nil {
		return nil, errcode.ErrInternal
	}
	return s.issuePair(ctx, consumer)
}

// Refresh re-signs an access token from a valid opaque refresh token, with
// sliding renewal of the refresh token when its remaining life drops below
// RefreshSlack.
func (s *AuthService) Refresh(ctx context.Context, refreshToken string) (*LoginResult, error) {
	if refreshToken == "" {
		return nil, errcode.ErrTokenInvalid
	}
	data, err := s.store.GetRefresh(ctx, refreshToken)
	if err != nil {
		if errors.Is(err, jwt.ErrRefreshNotFound) {
			return nil, errcode.ErrTokenInvalid
		}
		return nil, errcode.ErrInternal
	}

	access, _, err := s.jwt.Sign(ctx, jwt.IssueInput{
		Subject:  data.Subject,
		Scene:    data.Scene,
		TenantID: data.TenantID,
		StoreID:  data.StoreID,
		Role:     data.Role,
		OpenID:   data.OpenID,
		Version:  data.Version,
	})
	if err != nil {
		return nil, errcode.ErrInternal
	}

	// Sliding renewal: mint a fresh refresh and revoke the old one so each
	// refresh produces at most one live token. (We cannot read the remaining TTL
	// back from the store portably, so we renew on every call for the POC; the
	// <7d sliding rule is wired through RefreshSlack when a TTL-inspecting store
	// is available. See ADR note.)
	out := refreshToken
	if s.refreshSlack > 0 {
		newTok, err := s.store.IssueRefresh(ctx, data, s.refreshTTL)
		if err != nil {
			return nil, errcode.ErrInternal
		}
		_ = s.store.RevokeRefresh(ctx, refreshToken)
		out = newTok
	}

	return &LoginResult{
		AccessToken:  access,
		RefreshToken: out,
		ExpiresIn:    int(s.accessTTL.Seconds()),
		TokenType:    "Bearer",
	}, nil
}

// Logout blocks the access token's jti (for its remaining life) and revokes the
// refresh token. A missing/expired access token is tolerated (its jti is moot).
func (s *AuthService) Logout(ctx context.Context, accessToken, refreshToken string) error {
	if accessToken != "" {
		if claims, err := s.jwt.Verify(accessToken); err == nil && claims.ExpiresAt != nil {
			remaining := time.Until(claims.ExpiresAt.Time)
			if remaining > 0 {
				if berr := s.store.BlockJTI(ctx, claims.ID, remaining); berr != nil {
					return errcode.ErrInternal
				}
			}
		}
	}
	if refreshToken != "" {
		if err := s.store.RevokeRefresh(ctx, refreshToken); err != nil {
			return errcode.ErrInternal
		}
	}
	return nil
}

// issuePair signs an access token and issues an opaque refresh token for a
// consumer, returning the LoginResult. The refresh carries enough data to
// re-sign an access token without a DB hit.
func (s *AuthService) issuePair(ctx context.Context, c *Consumer) (*LoginResult, error) {
	access, _, err := s.jwt.Sign(ctx, jwt.IssueInput{
		Subject:  c.Subject(),
		Audience: []string{jwt.SceneConsumer},
		Scene:    jwt.SceneConsumer,
		TenantID: c.TenantID,
		OpenID:   c.OpenID,
		Version:  c.Version,
	})
	if err != nil {
		return nil, errcode.ErrInternal
	}
	refresh, err := s.store.IssueRefresh(ctx, jwt.RefreshData{
		TenantID: c.TenantID,
		UserID:   c.ID,
		Subject:  c.Subject(),
		Scene:    jwt.SceneConsumer,
		OpenID:   c.OpenID,
		Version:  c.Version,
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

// mapWXErr translates a wxsdk error into an errcode sentinel: caller faults
// (invalid/expired code) -> ErrParam; upstream faults -> ErrUpstream.
func mapWXErr(err error) error {
	var wxErr *wxsdk.Code2SessionError
	if errors.As(err, &wxErr) {
		if wxErr.IsWellKnownInvalid() {
			return errcode.ErrParam
		}
		return errcode.ErrUpstream
	}
	return errcode.ErrUpstream
}
