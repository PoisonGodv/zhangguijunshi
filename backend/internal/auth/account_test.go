package auth

// account_test.go verifies the Account model sentinels, the PhoneHash
// convention (write == read), and the rbac menu/MeResult builders
// (CONTRACT §19 / BE-A-01).

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/jwt"
)

func TestPhoneHash_Deterministic(t *testing.T) {
	a := PhoneHash("13800000001")
	b := PhoneHash("13800000001")
	require.Equal(t, a, b)
	require.NotEmpty(t, a)
	// Different phones hash differently.
	require.NotEqual(t, a, PhoneHash("13800000002"))
}

// TestMemoryRepo_PhoneHashMatchesLogin proves the seed's phone_hash (computed
// via PhoneHash on write) matches the hash computed on lookup — i.e. the boss
// login phone resolves to the seeded account. This is the load-bearing
// cross-check that the write and read paths agree.
func TestMemoryRepo_PhoneHashWriteReadAgree(t *testing.T) {
	repo := NewMemoryAccountRepo()
	acc, err := repo.FindByPhone(t.Context(), 1, "13800000001")
	require.NoError(t, err)
	require.Equal(t, "owner", acc.Role)
	require.NotEmpty(t, acc.PasswordHash)
}

// TestMemoryRepo_FindByUsernameByUsername proves FindByUsername matches the
// dedicated username column (migration 000018), not the display name. The dev
// seed gives the platform ops account username "ops"; looking it up by its
// display name ("ops" is also its name, so use a fresh account to disambiguate)
// and by username both resolve.
func TestMemoryRepo_FindByUsername(t *testing.T) {
	repo := NewMemoryAccountRepo()

	// Seeded platform ops: username "ops".
	acc, err := repo.FindByUsername(t.Context(), 1, "ops")
	require.NoError(t, err)
	require.Equal(t, "platform_ops", acc.Role)
	require.Equal(t, "ops", acc.Username)

	// Seed an account whose display name differs from its username; the lookup
	// must succeed by username and fail by the (non-unique) display name.
	seeded, err := repo.SeedAccountWithUsername(1, 1, "Alice Display", "alice_admin", "13900000099", "manager", "pw12345")
	require.NoError(t, err)
	got, err := repo.FindByUsername(t.Context(), 1, "alice_admin")
	require.NoError(t, err)
	require.Equal(t, seeded.ID, got.ID)
	require.Equal(t, "Alice Display", got.Name)

	// Display name is NOT a username: lookup by name fails.
	_, err = repo.FindByUsername(t.Context(), 1, "Alice Display")
	require.ErrorIs(t, err, ErrAccountNotFound)

	// Empty username never matches.
	_, err = repo.FindByUsername(t.Context(), 1, "")
	require.ErrorIs(t, err, ErrAccountNotFound)
}

// TestMemoryRepo_UsernameUniquePerTenant proves the username index is scoped per
// tenant: the same username in two tenants resolves independently, and a
// cross-tenant lookup misses (row-level isolation).
func TestMemoryRepo_UsernameUniquePerTenant(t *testing.T) {
	repo := NewMemoryAccountRepo()
	a1, err := repo.SeedAccountWithUsername(1, 0, "ops t1", "shared", "", "platform_ops", "pw12345")
	require.NoError(t, err)
	a2, err := repo.SeedAccountWithUsername(2, 0, "ops t2", "shared", "", "platform_ops", "pw12345")
	require.NoError(t, err)
	require.NotEqual(t, a1.ID, a2.ID)

	got1, err := repo.FindByUsername(t.Context(), 1, "shared")
	require.NoError(t, err)
	require.Equal(t, a1.ID, got1.ID)
	require.Equal(t, int64(1), got1.TenantID)

	got2, err := repo.FindByUsername(t.Context(), 2, "shared")
	require.NoError(t, err)
	require.Equal(t, a2.ID, got2.ID)
	require.Equal(t, int64(2), got2.TenantID)

	// Tenant 3 never saw "shared".
	_, err = repo.FindByUsername(t.Context(), 3, "shared")
	require.ErrorIs(t, err, ErrAccountNotFound)
}

// TestMemoryRepo_BumpVersion proves BumpVersion increments the stored version
// monotonically and returns the new value (revoke-on-password-change hook).
func TestMemoryRepo_BumpVersion(t *testing.T) {
	repo := NewMemoryAccountRepo()
	seeded, err := repo.SeedAccountWithUsername(1, 0, "v ops", "vops", "", "platform_ops", "pw12345")
	require.NoError(t, err)
	require.Equal(t, int64(0), seeded.Version)

	v1, err := repo.BumpVersion(t.Context(), seeded.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), v1)

	v2, err := repo.BumpVersion(t.Context(), seeded.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), v2)

	// The bump is visible through a fresh lookup (version is live, not constant).
	got, err := repo.FindByUsername(t.Context(), 1, "vops")
	require.NoError(t, err)
	require.Equal(t, int64(2), got.Version)

	// Unknown id degrades to ErrAccountNotFound.
	_, err = repo.BumpVersion(t.Context(), 999999)
	require.ErrorIs(t, err, ErrAccountNotFound)
}

// TestMemoryRepo_FindByID proves FindByID is tenant-scoped: same-tenant loads,
// cross-tenant / unknown / soft-deleted all miss (ARCH-01).
func TestMemoryRepo_FindByID(t *testing.T) {
	repo := NewMemoryAccountRepo()
	seeded, err := repo.SeedAccount(1, 1, "find me", "13700000000", "manager", "pw12345")
	require.NoError(t, err)

	got, err := repo.FindByID(t.Context(), 1, seeded.ID)
	require.NoError(t, err)
	require.Equal(t, seeded.ID, got.ID)
	require.Equal(t, int64(1), got.TenantID)
	require.Equal(t, "manager", got.Role)

	// Cross-tenant id behaves as absent.
	_, err = repo.FindByID(t.Context(), 2, seeded.ID)
	require.ErrorIs(t, err, ErrAccountNotFound)
	// Unknown id.
	_, err = repo.FindByID(t.Context(), 1, 999999)
	require.ErrorIs(t, err, ErrAccountNotFound)
}

// TestMemoryRepo_UpdatePassword proves UpdatePassword overwrites the hash
// (without bumping version) and the new hash verifies on the next read.
func TestMemoryRepo_UpdatePassword(t *testing.T) {
	repo := NewMemoryAccountRepo()
	seeded, err := repo.SeedAccount(1, 1, "pw ops", "13800000077", "owner", "oldpw12345")
	require.NoError(t, err)

	before, err := repo.FindByID(t.Context(), 1, seeded.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), before.Version) // version untouched by UpdatePassword
	require.NoError(t, VerifyPassword(before.PasswordHash, "oldpw12345"))

	newHash, err := HashPassword("newpw12345")
	require.NoError(t, err)
	require.NoError(t, repo.UpdatePassword(t.Context(), seeded.ID, newHash))

	after, err := repo.FindByID(t.Context(), 1, seeded.ID)
	require.NoError(t, err)
	require.Equal(t, newHash, after.PasswordHash)
	require.Equal(t, int64(0), after.Version) // still 0 — UpdatePassword does NOT bump
	require.NoError(t, VerifyPassword(after.PasswordHash, "newpw12345"))
	require.Error(t, VerifyPassword(after.PasswordHash, "oldpw12345")) // old pw dead

	// Unknown id.
	require.ErrorIs(t, repo.UpdatePassword(t.Context(), 999999, newHash), ErrAccountNotFound)
}

// TestMemoryRepo_Create proves Create persists a caller-hashed account (the
// bootstrap path): the stored hash verifies via FindByPhone, the allocated id
// comes back, and an empty status defaults to active.
func TestMemoryRepo_Create(t *testing.T) {
	repo := NewMemoryAccountRepo()
	hash, err := HashPassword("Str0ng#Passw0rd")
	require.NoError(t, err)

	created, err := repo.Create(t.Context(), &Account{
		TenantID: 7, Name: "店主", Phone: "13911112222",
		Role: "owner", Status: "active", PasswordHash: hash,
	})
	require.NoError(t, err)
	require.NotZero(t, created.ID)
	require.Equal(t, int64(7), created.TenantID)

	// 登录链路闭环: FindByPhone 取回 + VerifyPassword 验过。
	got, err := repo.FindByPhone(t.Context(), 7, "13911112222")
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.Equal(t, "owner", got.Role)
	require.NoError(t, VerifyPassword(got.PasswordHash, "Str0ng#Passw0rd"))

	// status 空默认 active。
	c2, err := repo.Create(t.Context(), &Account{
		TenantID: 7, Name: "二店员", Phone: "13911113333",
		Role: "clerk", PasswordHash: hash,
	})
	require.NoError(t, err)
	require.Equal(t, "active", c2.Status)
}

// TestMemoryRepo_Create_RejectsEmptyHash proves Create refuses to persist an
// account without a bcrypt hash (such an account could never log in), before
// any state changes.
func TestMemoryRepo_Create_RejectsEmptyHash(t *testing.T) {
	repo := NewMemoryAccountRepo()
	_, err := repo.Create(t.Context(), &Account{TenantID: 1, Name: "x", Phone: "13900000000", Role: "owner"})
	require.ErrorIs(t, err, errEmptyPasswordHash)
	_, err = repo.Create(t.Context(), nil)
	require.ErrorIs(t, err, errEmptyPasswordHash)
	// 未落任何账号。
	_, err = repo.FindByPhone(t.Context(), 1, "13900000000")
	require.ErrorIs(t, err, ErrAccountNotFound)
}

func TestSentinels_AreErrcodeTyped(t *testing.T) {
	// All three must satisfy errcode.Err so httpx.FailError maps them without a
	// handler type switch, and must carry distinct, sensible codes.
	cases := []struct {
		name string
		err  error
		code int
		http int
	}{
		{"not found", ErrAccountNotFound, 40404, 404},
		{"login failed", ErrLoginFailed, 40102, 401},
		{"not configured", ErrAccountNotConfigured, 50001, 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e errcode.Err
			require.ErrorAs(t, tc.err, &e)
			require.Equal(t, tc.code, e.Code)
			require.Equal(t, tc.http, e.HTTP)
		})
	}
}

func TestMenusFor(t *testing.T) {
	require.Equal(t, []string{"dashboard", "catalog", "orders", "members", "marketing", "finance", "reach", "settings"}, MenusFor("owner"))
	require.Equal(t, []string{"dashboard", "catalog", "orders", "members", "marketing", "finance", "reach", "settings"}, MenusFor("manager"))
	require.Equal(t, []string{"dashboard", "orders", "members"}, MenusFor("clerk"))
	require.Equal(t, []string{"dashboard", "orders", "members"}, MenusFor("cashier"))
	require.Equal(t, []string{"dashboard", "catalog", "orders", "marketing"}, MenusFor("regional_mgr"))
	require.Equal(t, []string{"platform/merchants", "platform/audit", "platform/billing", "platform/tenants"}, MenusFor("platform_ops"))
	// Unknown role -> empty, non-nil slice (stable response shape).
	unknown := MenusFor("nope")
	require.NotNil(t, unknown)
	require.Empty(t, unknown)
}

func TestBuildMeResult(t *testing.T) {
	claims := &jwt.Claims{
		Scene:    jwt.SceneAdmin,
		TenantID: 1,
		StoreID:  0,
		Role:     "platform_ops",
	}
	me := BuildMeResult(claims)
	require.Equal(t, "platform_ops", me.Role)
	require.Equal(t, "platform_ops", me.User.Role)
	require.Equal(t, int64(1), me.User.TenantID)
	require.Equal(t, int64(0), me.User.StoreID)
	require.Equal(t, []string{"platform/merchants", "platform/audit", "platform/billing", "platform/tenants"}, me.Permissions.Menus)
}
