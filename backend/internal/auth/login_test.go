package auth

// login_test.go exercises the boss/admin password-login flow end-to-end against
// a real (dev-key) JWT stack + memory SessionStore + MemoryAccountRepo. Covers
// success, wrong-password, not-found, param, not-configured, and the
// user-enumeration defence (wrong-password and not-found collapse to the same
// ErrLoginFailed) (CONTRACT §19 / BE-A-01).

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/jwt"
)

// buildLoginSvc wires an AuthService with a dev-key JWT manager, memory session
// store, and a seeded MemoryAccountRepo. The returned manager verifies the
// issued access tokens in-test. wx / consumer repo are nil (staff login does
// not touch them; auth.New only stores them).
func buildLoginSvc(t *testing.T, platformTenantID int64) (*AuthService, *jwt.Manager) {
	t.Helper()
	priv, err := jwt.DevKey("login-test")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, KeyID: "k1", Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })
	repo := NewMemoryAccountRepo()
	svc := New(Config{
		AppID:            "A",
		Secret:           "S",
		AccessTTL:        time.Minute,
		RefreshTTL:       time.Hour,
		PlatformTenantID: platformTenantID,
	}, nil, mgr, store, nil)
	svc.WithAccountRepo(repo)
	return svc, mgr
}

// errcodeOf extracts the errcode.Err from err's chain (httpx.FailError relies
// on errors.As finding exactly this).
func errcodeOf(t *testing.T, err error) errcode.Err {
	t.Helper()
	var e errcode.Err
	require.ErrorAs(t, err, &e)
	return e
}

func TestBossLogin_Success(t *testing.T) {
	svc, mgr := buildLoginSvc(t, 1)
	res, err := svc.BossLogin(t.Context(), 1, "13800000001", "boss12345")
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "Bearer", res.TokenType)
	require.Equal(t, int(time.Minute.Seconds()), res.ExpiresIn)
	require.NotEmpty(t, res.AccessToken)
	require.NotEmpty(t, res.RefreshToken)

	claims, verr := mgr.Verify(res.AccessToken)
	require.NoError(t, verr)
	require.Equal(t, jwt.SceneBoss, claims.Scene)
	require.Equal(t, "owner", claims.Role)
	require.Equal(t, int64(1), claims.TenantID)
	require.Equal(t, int64(1), claims.StoreID)
	require.True(t, strings.HasPrefix(claims.Subject, "u_"), "subject should be u_<id>, got %q", claims.Subject)
}

func TestBossLogin_WrongPassword(t *testing.T) {
	svc, _ := buildLoginSvc(t, 1)
	_, err := svc.BossLogin(t.Context(), 1, "13800000001", "nope")
	require.ErrorIs(t, err, ErrLoginFailed)
	require.Equal(t, 40102, errcodeOf(t, err).Code)
}

func TestBossLogin_NotFound(t *testing.T) {
	svc, _ := buildLoginSvc(t, 1)
	_, err := svc.BossLogin(t.Context(), 1, "13900000000", "boss12345")
	require.ErrorIs(t, err, ErrLoginFailed)
}

// TestBossLogin_NoUserEnumeration: wrong-password and not-found must be
// indistinguishable — same errcode.Code and Message.
func TestBossLogin_NoUserEnumeration(t *testing.T) {
	svc, _ := buildLoginSvc(t, 1)
	_, wrongPwd := svc.BossLogin(t.Context(), 1, "13800000001", "bad")
	_, notFound := svc.BossLogin(t.Context(), 1, "13900000000", "boss12345")
	e1 := errcodeOf(t, wrongPwd)
	e2 := errcodeOf(t, notFound)
	require.Equal(t, e1.Code, e2.Code)
	require.Equal(t, e1.Message, e2.Message)
}

func TestBossLogin_CrossTenantNotFound(t *testing.T) {
	svc, _ := buildLoginSvc(t, 1)
	// The seeded owner is in tenant 1; tenant 2 must not see it.
	_, err := svc.BossLogin(t.Context(), 2, "13800000001", "boss12345")
	require.ErrorIs(t, err, ErrLoginFailed)
}

func TestBossLogin_NotConfigured(t *testing.T) {
	// Service built without WithAccountRepo -> accountRepo is nil.
	priv, err := jwt.DevKey("nc")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })
	svc := New(Config{AccessTTL: time.Minute, RefreshTTL: time.Hour}, nil, mgr, store, nil)
	_, err = svc.BossLogin(t.Context(), 1, "13800000001", "boss12345")
	require.ErrorIs(t, err, ErrAccountNotConfigured)
	require.Equal(t, 50001, errcodeOf(t, err).Code)
}

func TestBossLogin_ParamValidation(t *testing.T) {
	svc, _ := buildLoginSvc(t, 1)
	cases := []struct {
		name     string
		tenantID int64
		phone    string
		password string
	}{
		{"zero tenant", 0, "13800000001", "boss12345"},
		{"empty phone", 1, "", "boss12345"},
		{"empty password", 1, "13800000001", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.BossLogin(t.Context(), tc.tenantID, tc.phone, tc.password)
			require.ErrorIs(t, err, errcode.ErrParam)
		})
	}
}

func TestAdminLogin_Success(t *testing.T) {
	svc, mgr := buildLoginSvc(t, 1)
	res, err := svc.AdminLogin(t.Context(), "ops", "ops12345")
	require.NoError(t, err)
	require.Equal(t, "Bearer", res.TokenType)

	claims, verr := mgr.Verify(res.AccessToken)
	require.NoError(t, verr)
	require.Equal(t, jwt.SceneAdmin, claims.Scene)
	require.Equal(t, "platform_ops", claims.Role)
	require.Equal(t, int64(1), claims.TenantID)
	require.Equal(t, int64(0), claims.StoreID, "platform tokens must be store-agnostic")
	require.True(t, strings.HasPrefix(claims.Subject, "u_"))
}

func TestAdminLogin_WrongPassword(t *testing.T) {
	svc, _ := buildLoginSvc(t, 1)
	_, err := svc.AdminLogin(t.Context(), "ops", "bad")
	require.ErrorIs(t, err, ErrLoginFailed)
}

func TestAdminLogin_NotFound(t *testing.T) {
	svc, _ := buildLoginSvc(t, 1)
	_, err := svc.AdminLogin(t.Context(), "nobody", "ops12345")
	require.ErrorIs(t, err, ErrLoginFailed)
}

func TestAdminLogin_RejectsNonPlatformRole(t *testing.T) {
	// Seed a manager-staff account in the platform tenant; AdminLogin must
	// reject it even with the right password (role != platform_ops).
	priv, err := jwt.DevKey("mgr")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })
	repo := NewMemoryAccountRepo()
	_, _ = repo.SeedAccount(1, 1, "mgr1", "13800000002", "manager", "m12345")
	svc := New(Config{AccessTTL: time.Minute, RefreshTTL: time.Hour, PlatformTenantID: 1}, nil, mgr, store, nil)
	svc.WithAccountRepo(repo)

	_, err = svc.AdminLogin(t.Context(), "mgr1", "m12345")
	require.ErrorIs(t, err, ErrLoginFailed)
}

func TestAdminLogin_NotConfigured(t *testing.T) {
	priv, err := jwt.DevKey("nc2")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })
	svc := New(Config{AccessTTL: time.Minute, RefreshTTL: time.Hour, PlatformTenantID: 1}, nil, mgr, store, nil)
	_, err = svc.AdminLogin(t.Context(), "ops", "ops12345")
	require.ErrorIs(t, err, ErrAccountNotConfigured)
}

func TestAdminLogin_ParamValidation(t *testing.T) {
	svc, _ := buildLoginSvc(t, 1)
	_, err := svc.AdminLogin(t.Context(), "", "ops12345")
	require.ErrorIs(t, err, errcode.ErrParam)
	_, err = svc.AdminLogin(t.Context(), "ops", "")
	require.ErrorIs(t, err, errcode.ErrParam)
}
