package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/wxsdk"
)

type stubWX struct {
	sess *wxsdk.Session
	err  error
}

func (s stubWX) Code2Session(_ context.Context, _, _, _ string) (*wxsdk.Session, error) {
	return s.sess, s.err
}

func newTestService(t *testing.T, wx Code2Sessioner) (*AuthService, *jwt.Manager, *MemoryConsumerRepo) {
	t.Helper()
	priv, err := jwt.DevKey("auth-test")
	if err != nil {
		t.Fatalf("DevKey: %v", err)
	}
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	if err != nil {
		t.Fatalf("jwt.New: %v", err)
	}
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })
	repo := NewMemoryConsumerRepo()
	svc := New(Config{
		AppID: "APP", Secret: "SEC", AccessTTL: time.Minute, RefreshTTL: time.Hour,
	}, wx, mgr, store, repo)
	return svc, mgr, repo
}

func TestWxLogin_HappyPath(t *testing.T) {
	wx := stubWX{sess: &wxsdk.Session{OpenID: "oABC", SessionKey: "sk", UnionID: "u1"}}
	svc, mgr, repo := newTestService(t, wx)

	res, err := svc.WxLogin(context.Background(), 50021, "CODE")
	if err != nil {
		t.Fatalf("WxLogin: %v", err)
	}
	if res.AccessToken == "" || res.RefreshToken == "" || res.ExpiresIn != 60 || res.TokenType != "Bearer" {
		t.Fatalf("bad result: %+v", res)
	}

	claims, err := mgr.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("verify access: %v", err)
	}
	if claims.TenantID != 50021 || claims.OpenID != "oABC" || claims.Scene != jwt.SceneConsumer {
		t.Fatalf("claims mismatch: %+v", claims)
	}
	if claims.Subject == "" {
		t.Fatal("empty sub")
	}

	// Consumer persisted under the id encoded in the access token's subject.
	id := parseConsumerID(t, claims.Subject)
	got, err := repo.GetByID(context.Background(), 50021, id)
	if err != nil {
		t.Fatalf("GetByID(%d): %v", id, err)
	}
	if got.SessionKey != "sk" {
		t.Fatalf("session_key not cached server-side: %q", got.SessionKey)
	}
	if got.OpenID != "oABC" {
		t.Fatalf("openid mismatch: %q", got.OpenID)
	}
	// Result must NEVER leak session_key or openid.
	if res.AccessToken == "sk" {
		t.Fatal("session_key leaked into token")
	}
}

func parseConsumerID(t *testing.T, sub string) int64 {
	t.Helper()
	if len(sub) < 3 || sub[:2] != "c_" {
		t.Fatalf("unexpected subject %q", sub)
	}
	var n int64
	for _, ch := range sub[2:] {
		if ch < '0' || ch > '9' {
			t.Fatalf("non-digit subject %q", sub)
		}
		n = n*10 + int64(ch-'0')
	}
	return n
}

func TestWxLogin_BadParams(t *testing.T) {
	svc, _, _ := newTestService(t, stubWX{})
	for _, tc := range []struct {
		name     string
		tenantID int64
		code     string
	}{
		{"empty code", 1, ""},
		{"zero tenant", 0, "CODE"},
	} {
		if _, err := svc.WxLogin(context.Background(), tc.tenantID, tc.code); !errors.Is(err, errcode.ErrParam) {
			t.Fatalf("%s: expected ErrParam, got %v", tc.name, err)
		}
	}
}

func TestWxLogin_WxErrorMapping(t *testing.T) {
	svc, _, _ := newTestService(t, stubWX{err: &wxsdk.Code2SessionError{ErrCode: 40029, ErrMsg: "invalid code"}})
	if _, err := svc.WxLogin(context.Background(), 1, "bad"); !errors.Is(err, errcode.ErrParam) {
		t.Fatalf("expected ErrParam for invalid code, got %v", err)
	}

	svc2, _, _ := newTestService(t, stubWX{err: errors.New("network down")})
	if _, err := svc2.WxLogin(context.Background(), 1, "c"); !errors.Is(err, errcode.ErrUpstream) {
		t.Fatalf("expected ErrUpstream for transport error, got %v", err)
	}
}

func TestRefresh_HappyAndRotation(t *testing.T) {
	wx := stubWX{sess: &wxsdk.Session{OpenID: "oA", SessionKey: "sk"}}
	svc, mgr, _ := newTestService(t, wx)

	first, err := svc.WxLogin(context.Background(), 7, "CODE")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	second, err := svc.Refresh(context.Background(), first.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if second.AccessToken == "" {
		t.Fatal("refresh returned no access token")
	}
	if _, err := mgr.Verify(second.AccessToken); err != nil {
		t.Fatalf("verify refreshed access: %v", err)
	}
	// Sliding renewal rotates the refresh token.
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh token not rotated")
	}
	// Old refresh revoked -> cannot refresh again with it.
	if _, err := svc.Refresh(context.Background(), first.RefreshToken); !errors.Is(err, errcode.ErrTokenInvalid) {
		t.Fatalf("expected ErrTokenInvalid for revoked refresh, got %v", err)
	}
}

func TestRefresh_BadToken(t *testing.T) {
	svc, _, _ := newTestService(t, stubWX{})
	if _, err := svc.Refresh(context.Background(), ""); !errors.Is(err, errcode.ErrTokenInvalid) {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}
	if _, err := svc.Refresh(context.Background(), "never-issued"); !errors.Is(err, errcode.ErrTokenInvalid) {
		t.Fatalf("expected ErrTokenInvalid, got %v", err)
	}
}

func TestLogout(t *testing.T) {
	wx := stubWX{sess: &wxsdk.Session{OpenID: "oA", SessionKey: "sk"}}
	svc, _, _ := newTestService(t, wx)

	res, err := svc.WxLogin(context.Background(), 9, "CODE")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := svc.Logout(context.Background(), res.AccessToken, res.RefreshToken); err != nil {
		t.Fatalf("logout: %v", err)
	}
	// Refresh now dead.
	if _, err := svc.Refresh(context.Background(), res.RefreshToken); !errors.Is(err, errcode.ErrTokenInvalid) {
		t.Fatalf("expected ErrTokenInvalid after logout, got %v", err)
	}
}
