package auth

// change_password_test.go exercises AuthService.ChangePassword end-to-end
// against the memory AccountRepo + a memory VersionStore (CONTRACT §20.5).
// Covers: success (hash rotated, version bumped, both staff scenes Incr'd in the
// version cache), wrong-old (ErrLoginFailed), not-found (ErrAccountNotFound),
// param validation, and not-configured. Mirrors buildLoginSvc but returns the
// repo so tests know the seeded account's id (black-box at the AuthService seam,
// observing repo + store side-effects through public methods).

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/jwt"
)

// buildChangePwdSvc wires an AuthService with a dev-key JWT manager + memory
// session store + a seeded MemoryAccountRepo (the repo is returned so tests can
// resolve the seeded account's id and observe post-change state) + a memory
// VersionStore (returned so tests can assert the Incr).
func buildChangePwdSvc(t *testing.T) (*AuthService, *MemoryAccountRepo, *MemoryVersionStore) {
	t.Helper()
	priv, err := jwt.DevKey("chg-pwd")
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
		PlatformTenantID: 1,
	}, nil, mgr, store, nil)
	svc.WithAccountRepo(repo)
	vs := NewMemoryVersionStore()
	svc.WithVersionStore(vs)
	return svc, repo, vs
}

func TestChangePassword_Success(t *testing.T) {
	svc, repo, vs := buildChangePwdSvc(t)
	acc, err := repo.FindByPhone(t.Context(), 1, "13800000001")
	require.NoError(t, err)
	require.Equal(t, int64(0), acc.Version) // baseline

	updated, err := svc.ChangePassword(t.Context(), 1, acc.ID, "boss12345", "newboss12345")
	require.NoError(t, err)
	require.Equal(t, int64(1), updated.Version) // bumped 0 -> 1

	// Hash rotated: new pw verifies, old pw is dead.
	require.NoError(t, VerifyPassword(updated.PasswordHash, "newboss12345"))
	require.Error(t, VerifyPassword(updated.PasswordHash, "boss12345"))

	// Persisted: a fresh load shows the new hash + bumped version.
	got, err := repo.FindByID(t.Context(), 1, acc.ID)
	require.NoError(t, err)
	require.Equal(t, updated.PasswordHash, got.PasswordHash)
	require.Equal(t, int64(1), got.Version)

	// VersionStore Incr'd for BOTH staff scenes (defence-in-depth; one is the
	// real session scene, the other a stray never read). Boss baseline was 0
	// (no login happened, so Incr-on-fresh-key yields 1) — which still correctly
	// invalidates a hypothetical ver-0 token (cur=1 > 0).
	bossV, _ := vs.Get(t.Context(), jwt.SceneBoss, acc.ID)
	require.Equal(t, int64(1), bossV)
	adminV, _ := vs.Get(t.Context(), jwt.SceneAdmin, acc.ID)
	require.Equal(t, int64(1), adminV)
}

// TestChangePassword_VersionStoreSeededByLogin proves the realistic path: login
// Sets the DB baseline, then ChangePassword Incrs to baseline+1 (matching the DB
// bump). This is the contract the middleware relies on (stale ver < cur).
func TestChangePassword_VersionStoreSeededByLogin(t *testing.T) {
	svc, repo, vs := buildChangePwdSvc(t)
	acc, err := repo.FindByPhone(t.Context(), 1, "13800000001")
	require.NoError(t, err)

	// A real boss login seeds (SceneBoss, id, 0) in the store.
	_, err = svc.BossLogin(t.Context(), 1, "13800000001", "boss12345")
	require.NoError(t, err)
	bossV, _ := vs.Get(t.Context(), jwt.SceneBoss, acc.ID)
	require.Equal(t, int64(0), bossV, "login Sets the DB version baseline")

	_, err = svc.ChangePassword(t.Context(), 1, acc.ID, "boss12345", "newboss12345")
	require.NoError(t, err)
	bossV, _ = vs.Get(t.Context(), jwt.SceneBoss, acc.ID)
	require.Equal(t, int64(1), bossV, "Incr bumps the seeded baseline 0 -> 1")
}

func TestChangePassword_WrongOld(t *testing.T) {
	svc, repo, _ := buildChangePwdSvc(t)
	acc, _ := repo.FindByPhone(t.Context(), 1, "13800000001")
	_, err := svc.ChangePassword(t.Context(), 1, acc.ID, "WRONG", "newboss12345")
	require.ErrorIs(t, err, ErrLoginFailed)
	require.Equal(t, 40102, errcodeOf(t, err).Code)
}

func TestChangePassword_NotFound(t *testing.T) {
	svc, _, _ := buildChangePwdSvc(t)
	// Unknown account id within a real tenant -> ErrAccountNotFound (NOT collapsed
	// to ErrLoginFailed; the caller is already authenticated, so 404 is fine).
	_, err := svc.ChangePassword(t.Context(), 1, 999999, "whatever", "newpw12345")
	require.ErrorIs(t, err, ErrAccountNotFound)
}

func TestChangePassword_CrossTenantNotFound(t *testing.T) {
	svc, repo, _ := buildChangePwdSvc(t)
	acc, _ := repo.FindByPhone(t.Context(), 1, "13800000001")
	// The account is in tenant 1; asking tenant 2 must not load it.
	_, err := svc.ChangePassword(t.Context(), 2, acc.ID, "boss12345", "newboss12345")
	require.ErrorIs(t, err, ErrAccountNotFound)
}

func TestChangePassword_ParamValidation(t *testing.T) {
	svc, repo, _ := buildChangePwdSvc(t)
	acc, _ := repo.FindByPhone(t.Context(), 1, "13800000001")
	cases := []struct {
		name string
		old  string
		new  string
	}{
		{"empty old", "", "newboss12345"},
		{"empty new", "boss12345", ""},
		{"new equals old", "boss12345", "boss12345"},
		{"new too short", "boss12345", "12345"},         // 5 < minPasswordLen(6)
		{"new too long", "boss12345", longPassword(65)}, // 65 > maxPasswordLen(64)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ChangePassword(t.Context(), 1, acc.ID, tc.old, tc.new)
			require.ErrorIs(t, err, errcode.ErrParam)
		})
	}
}

// longPassword returns an n-char password for the max-length boundary test.
func longPassword(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func TestChangePassword_NotConfigured(t *testing.T) {
	// AuthService built without WithAccountRepo -> accountRepo is nil.
	priv, err := jwt.DevKey("nc-chg")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })
	svc := New(Config{AccessTTL: time.Minute, RefreshTTL: time.Hour}, nil, mgr, store, nil)
	_, err = svc.ChangePassword(t.Context(), 1, 2001, "boss12345", "newboss12345")
	require.ErrorIs(t, err, ErrAccountNotConfigured)
	require.Equal(t, 50001, errcodeOf(t, err).Code)
}

// TestChangePassword_NoVersionStoreStillWorks proves that with no VersionStore
// wired, ChangePassword still rotates the hash + bumps version (the Incr step is
// simply skipped). This is the dev-without-Redis path.
func TestChangePassword_NoVersionStoreStillWorks(t *testing.T) {
	svc, repo, _ := buildChangePwdSvc(t)
	svc.WithVersionStore(nil) // explicitly unwire
	acc, _ := repo.FindByPhone(t.Context(), 1, "13800000001")
	updated, err := svc.ChangePassword(t.Context(), 1, acc.ID, "boss12345", "newboss12345")
	require.NoError(t, err)
	require.Equal(t, int64(1), updated.Version)
	require.NoError(t, VerifyPassword(updated.PasswordHash, "newboss12345"))
}
