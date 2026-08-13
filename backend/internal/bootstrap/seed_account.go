package bootstrap

// seed_account.go — 首次部署冷启动引导: 建首个租户 + 首个 boss(owner) 账号
// (cmd/migrate seed-account 子命令)。
//
// 背景(已实证): 全新 PG 上所有 migration 只建表, 没有任何 INSERT INTO
// tenants/users, 也没有注册/建租户端点或 CLI; seed_demo.sql 要求 tenants 非空
// 才能跑。结果: 部署后无法登录、连演示数据都灌不进。本文件补这个闭环。
//
// 密码安全红线(本机制核心验收点):
//   - 明文密码【只】从 env ZG_BOOTSTRAP_BOSS_PASSWORD 读(cmd/migrate 侧),
//     绝不走 CLI 参数(ps 可见)、绝无默认值;
//   - 明文只在 校验强度 → auth.HashPassword 之间短暂停留, 绝不打印/日志;
//   - SeedAccountResult 只携带 tenant_id / account_id / phone 等可安全输出字段。

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zhangguijunshi/backend/internal/auth"
)

// defaultBootstrapTenantName 首个租户的默认名(env ZG_BOOTSTRAP_TENANT_NAME 未设时)。
const defaultBootstrapTenantName = "演示店"

// TenantStore 是引导流程对 tenants 表的最小持久化端口。tenants 不属于 auth 域,
// 故独立于 auth.AccountRepo 单设。
type TenantStore interface {
	// FirstTenantID 返回库中最早(未软删)的租户 id; 无租户返回 (0, nil)。
	FirstTenantID(ctx context.Context) (int64, error)
	// CreateTenant 新建租户并返回其 id。
	CreateTenant(ctx context.Context, name string) (int64, error)
}

// SQL for tenants (migration 000001)。tenants.id 即 tenant_id(全局租户隔离键);
// license_type/status/plan 等列走 DB 默认(individual/active/free)。
const (
	sqlFirstTenantID = `SELECT id FROM tenants WHERE deleted_at IS NULL ORDER BY id LIMIT 1`
	sqlCreateTenant  = `INSERT INTO tenants (name) VALUES ($1) RETURNING id`
)

// pgTenantQueryRower 是 PGTenantStore 需要的最小连接面(与 auth.PGPool 同风格,
// *pgxpool.Pool 直接满足)。
type pgTenantQueryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PGTenantStore 是 pgx-backed TenantStore。
type PGTenantStore struct{ pool pgTenantQueryRower }

// NewPGTenantStore constructs a PGTenantStore over pool.
func NewPGTenantStore(pool pgTenantQueryRower) *PGTenantStore { return &PGTenantStore{pool: pool} }

// FirstTenantID implements TenantStore; 空库 (pgx.ErrNoRows) 降级为 (0, nil)。
func (s *PGTenantStore) FirstTenantID(ctx context.Context) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("bootstrap: tenant store 未配置 (nil pool)")
	}
	var id int64
	if err := s.pool.QueryRow(ctx, sqlFirstTenantID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("bootstrap: pg first tenant: %w", err)
	}
	return id, nil
}

// CreateTenant implements TenantStore.
func (s *PGTenantStore) CreateTenant(ctx context.Context, name string) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("bootstrap: tenant store 未配置 (nil pool)")
	}
	var id int64
	if err := s.pool.QueryRow(ctx, sqlCreateTenant, name).Scan(&id); err != nil {
		return 0, fmt.Errorf("bootstrap: pg create tenant: %w", err)
	}
	return id, nil
}

// SeedAccountOptions 是 SeedFirstBoss 的输入。Password 是明文, 只能来自 env,
// 函数内绝不打印/日志。
type SeedAccountOptions struct {
	TenantName    string // 首个租户名; 空用 defaultBootstrapTenantName; 仅建租户时使用
	Phone         string // boss 手机号(登录账号), 必填
	Password      string // boss 明文密码; 仅建号或 ResetPassword 时必需
	ResetPassword bool   // 账号已存在时是否覆盖密码(默认否 → 幂等跳过)
}

// SeedAccountResult 只携带可安全输出的字段(绝无任何密码材料)。
type SeedAccountResult struct {
	TenantID       int64
	TenantCreated  bool // 本次是否新建了租户(false = 复用首个租户)
	AccountID      int64
	AccountCreated bool // 本次是否新建了账号
	Skipped        bool // 账号已存在且未指定重置 → 幂等跳过(未碰密码)
	PasswordReset  bool // 已存在账号的密码被覆盖(且 version+1)
	Phone          string
	Role           string
}

// SeedFirstBoss 保证「首个租户 + 该租户下 phone 对应的 owner 账号」存在。
//
// 幂等: tenants 非空则复用首个租户; 同 phone 账号已存在则【默认不覆盖密码】,
// 返回 Skipped=true; 仅 opts.ResetPassword=true 才更新 hash(并 BumpVersion
// 使旧 JWT 立即失效, revoke-on-password-change, CONTRACT §19)。重跑安全。
//
// 副作用排序: 先查租户/账号 → 需要动密码时先校验强度+哈希(失败则零副作用
// 退出) → 需要建租户才建(弱密码重跑不会留下孤儿租户) → 建号/重置。
func SeedFirstBoss(ctx context.Context, tenants TenantStore, accounts auth.AccountRepo, opts SeedAccountOptions) (*SeedAccountResult, error) {
	if tenants == nil || accounts == nil {
		return nil, errors.New("bootstrap: seed-account 存储未配置")
	}
	if opts.Phone == "" {
		return nil, errors.New("bootstrap: 缺少 boss 手机号 (env ZG_BOOTSTRAP_BOSS_PHONE)")
	}
	tenantName := opts.TenantName
	if tenantName == "" {
		tenantName = defaultBootstrapTenantName
	}

	tenantID, err := tenants.FirstTenantID(ctx)
	if err != nil {
		return nil, err
	}

	// 空库时 tenantID=0, FindByPhone 在任何实现下都安全返回 ErrAccountNotFound。
	existing, err := accounts.FindByPhone(ctx, tenantID, opts.Phone)
	if err != nil && !errors.Is(err, auth.ErrAccountNotFound) {
		return nil, fmt.Errorf("bootstrap: 查询 boss 账号: %w", err)
	}

	// 账号已存在且未要求重置 → 幂等跳过, 无需密码, 零副作用。
	if existing != nil && !opts.ResetPassword {
		return &SeedAccountResult{
			TenantID: tenantID, AccountID: existing.ID,
			Skipped: true, Phone: opts.Phone, Role: existing.Role,
		}, nil
	}

	// 走到这里一定要动密码(建号或重置): 先校验强度 + 算哈希, 失败零副作用。
	hash, err := hashBootstrapPassword(opts.Password)
	if err != nil {
		return nil, err
	}

	// 需要建租户才建。
	tenantCreated := false
	if tenantID == 0 {
		if tenantID, err = tenants.CreateTenant(ctx, tenantName); err != nil {
			return nil, err
		}
		tenantCreated = true
	}

	if existing != nil {
		// 显式重置: 覆盖 hash + version+1 失效旧 token(与 ChangePassword 同序:
		// 写 hash → bump version)。
		if err := accounts.UpdatePassword(ctx, existing.ID, hash); err != nil {
			return nil, fmt.Errorf("bootstrap: 重置 boss 密码: %w", err)
		}
		if _, err := accounts.BumpVersion(ctx, existing.ID); err != nil {
			return nil, fmt.Errorf("bootstrap: 重置后提升版本号: %w", err)
		}
		return &SeedAccountResult{
			TenantID: tenantID, TenantCreated: tenantCreated,
			AccountID: existing.ID, PasswordReset: true,
			Phone: opts.Phone, Role: existing.Role,
		}, nil
	}

	acc, err := accounts.Create(ctx, &auth.Account{
		TenantID:     tenantID,
		Name:         "店主",
		Phone:        opts.Phone,
		Role:         "owner",
		Status:       "active",
		PasswordHash: hash,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap: 创建 boss 账号: %w", err)
	}
	return &SeedAccountResult{
		TenantID: tenantID, TenantCreated: tenantCreated,
		AccountID: acc.ID, AccountCreated: true,
		Phone: opts.Phone, Role: "owner",
	}, nil
}

// hashBootstrapPassword 校验强度后返回 bcrypt hash; 明文只进不出。
func hashBootstrapPassword(pw string) (string, error) {
	if err := auth.ValidatePasswordStrength(pw); err != nil {
		return "", err
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return "", fmt.Errorf("bootstrap: 密码哈希: %w", err)
	}
	return hash, nil
}
