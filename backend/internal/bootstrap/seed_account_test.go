package bootstrap

// seed_account_test.go 锁定冷启动引导流程(cmd/migrate seed-account 的核心):
// 首租户+首 boss 账号创建 / 弱密码拒绝 / 幂等跳过(默认不覆盖密码) /
// --reset-password 覆盖+version+1 / 已有租户复用。账号侧用真 MemoryAccountRepo
// (接口同 PG), 租户侧用内存 fake —— 全程无 PG。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/auth"
)

// fakeTenantStore 是 TenantStore 的内存实现(引导流程单测用)。
type fakeTenantStore struct {
	ids   []int64
	names map[int64]string
	next  int64
}

func newFakeTenantStore() *fakeTenantStore {
	return &fakeTenantStore{names: map[int64]string{}}
}

func (f *fakeTenantStore) FirstTenantID(context.Context) (int64, error) {
	if len(f.ids) == 0 {
		return 0, nil
	}
	return f.ids[0], nil
}

func (f *fakeTenantStore) CreateTenant(_ context.Context, name string) (int64, error) {
	f.next++
	f.ids = append(f.ids, f.next)
	f.names[f.next] = name
	return f.next, nil
}

const (
	seedPhone = "13911112222"
	seedPwOld = "Str0ng#Passw0rd"
	seedPwNew = "N3w#StrongPass"
)

// TestSeedFirstBoss_CreatesTenantAndAccount 全空库 → 建租户+建 owner 账号,
// 且登录链路(FindByPhone + VerifyPassword)闭环可验。
func TestSeedFirstBoss_CreatesTenantAndAccount(t *testing.T) {
	tenants := newFakeTenantStore()
	accounts := auth.NewMemoryAccountRepo()

	res, err := SeedFirstBoss(context.Background(), tenants, accounts, SeedAccountOptions{
		Phone: seedPhone, Password: seedPwOld,
	})
	require.NoError(t, err)
	require.True(t, res.TenantCreated)
	require.True(t, res.AccountCreated)
	require.False(t, res.Skipped)
	require.False(t, res.PasswordReset)
	require.Equal(t, int64(1), res.TenantID)
	require.Equal(t, "owner", res.Role)
	require.Equal(t, seedPhone, res.Phone)
	// 默认租户名。
	require.Equal(t, defaultBootstrapTenantName, tenants.names[res.TenantID])

	acc, err := accounts.FindByPhone(context.Background(), res.TenantID, seedPhone)
	require.NoError(t, err)
	require.Equal(t, res.AccountID, acc.ID)
	require.NoError(t, auth.VerifyPassword(acc.PasswordHash, seedPwOld))
}

// TestSeedFirstBoss_IdempotentSkipKeepsPassword 重跑默认跳过: 即使 env 换了新
// 密码也【不覆盖】, 旧密码仍有效, id 不变。
func TestSeedFirstBoss_IdempotentSkipKeepsPassword(t *testing.T) {
	tenants := newFakeTenantStore()
	accounts := auth.NewMemoryAccountRepo()
	ctx := context.Background()

	first, err := SeedFirstBoss(ctx, tenants, accounts, SeedAccountOptions{Phone: seedPhone, Password: seedPwOld})
	require.NoError(t, err)

	second, err := SeedFirstBoss(ctx, tenants, accounts, SeedAccountOptions{Phone: seedPhone, Password: seedPwNew})
	require.NoError(t, err)
	require.True(t, second.Skipped)
	require.False(t, second.TenantCreated)
	require.False(t, second.AccountCreated)
	require.Equal(t, first.TenantID, second.TenantID)
	require.Equal(t, first.AccountID, second.AccountID)

	acc, err := accounts.FindByPhone(ctx, first.TenantID, seedPhone)
	require.NoError(t, err)
	require.NoError(t, auth.VerifyPassword(acc.PasswordHash, seedPwOld)) // 旧密码仍有效
	require.Error(t, auth.VerifyPassword(acc.PasswordHash, seedPwNew))   // 新密码未生效
}

// TestSeedFirstBoss_ResetPasswordOverwrites 显式重置: hash 被覆盖 + version+1
// (旧 JWT 失效钩子), 旧密码死新密码活。
func TestSeedFirstBoss_ResetPasswordOverwrites(t *testing.T) {
	tenants := newFakeTenantStore()
	accounts := auth.NewMemoryAccountRepo()
	ctx := context.Background()

	_, err := SeedFirstBoss(ctx, tenants, accounts, SeedAccountOptions{Phone: seedPhone, Password: seedPwOld})
	require.NoError(t, err)
	before, err := accounts.FindByPhone(ctx, 1, seedPhone)
	require.NoError(t, err)

	res, err := SeedFirstBoss(ctx, tenants, accounts, SeedAccountOptions{
		Phone: seedPhone, Password: seedPwNew, ResetPassword: true,
	})
	require.NoError(t, err)
	require.True(t, res.PasswordReset)
	require.False(t, res.Skipped)

	after, err := accounts.FindByPhone(ctx, 1, seedPhone)
	require.NoError(t, err)
	require.NoError(t, auth.VerifyPassword(after.PasswordHash, seedPwNew))
	require.Error(t, auth.VerifyPassword(after.PasswordHash, seedPwOld))
	require.Equal(t, before.Version+1, after.Version) // revoke-on-password-change
}

// TestSeedFirstBoss_RejectsWeakPassword 建号路径弱密码 → 报错且零副作用
// (账号未建, 租户也不留孤儿)。
func TestSeedFirstBoss_RejectsWeakPassword(t *testing.T) {
	tenants := newFakeTenantStore()
	accounts := auth.NewMemoryAccountRepo()

	_, err := SeedFirstBoss(context.Background(), tenants, accounts, SeedAccountOptions{
		Phone: seedPhone, Password: "123456",
	})
	require.Error(t, err)

	_, ferr := accounts.FindByPhone(context.Background(), 1, seedPhone)
	require.ErrorIs(t, ferr, auth.ErrAccountNotFound)
	require.Empty(t, tenants.ids, "弱密码不应留下孤儿租户")
}

// TestSeedFirstBoss_ResetRejectsWeakPassword 重置路径弱密码 → 报错且旧密码不动。
func TestSeedFirstBoss_ResetRejectsWeakPassword(t *testing.T) {
	tenants := newFakeTenantStore()
	accounts := auth.NewMemoryAccountRepo()
	ctx := context.Background()

	_, err := SeedFirstBoss(ctx, tenants, accounts, SeedAccountOptions{Phone: seedPhone, Password: seedPwOld})
	require.NoError(t, err)

	_, err = SeedFirstBoss(ctx, tenants, accounts, SeedAccountOptions{
		Phone: seedPhone, Password: "password123", ResetPassword: true,
	})
	require.Error(t, err)

	acc, ferr := accounts.FindByPhone(ctx, 1, seedPhone)
	require.NoError(t, ferr)
	require.NoError(t, auth.VerifyPassword(acc.PasswordHash, seedPwOld)) // 旧密码未被覆盖
}

// TestSeedFirstBoss_ReusesExistingTenant tenants 非空 → 复用首个租户(不新建),
// TenantName 不生效。
func TestSeedFirstBoss_ReusesExistingTenant(t *testing.T) {
	tenants := newFakeTenantStore()
	existingID, err := tenants.CreateTenant(context.Background(), "已有店")
	require.NoError(t, err)
	accounts := auth.NewMemoryAccountRepo()

	res, err := SeedFirstBoss(context.Background(), tenants, accounts, SeedAccountOptions{
		TenantName: "不应生效的名字", Phone: seedPhone, Password: seedPwOld,
	})
	require.NoError(t, err)
	require.False(t, res.TenantCreated)
	require.Equal(t, existingID, res.TenantID)
	require.Len(t, tenants.ids, 1)
}

// TestSeedFirstBoss_RequiresPhone 缺手机号直接报错(账号定位键)。
func TestSeedFirstBoss_RequiresPhone(t *testing.T) {
	_, err := SeedFirstBoss(context.Background(), newFakeTenantStore(), auth.NewMemoryAccountRepo(), SeedAccountOptions{
		Password: seedPwOld,
	})
	require.Error(t, err)
}
