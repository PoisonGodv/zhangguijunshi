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
