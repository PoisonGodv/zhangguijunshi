// Command migrate applies SQL migrations from backend/migrations using
// golang-migrate as a library. Run from the backend/ directory (so the default
// migrations dir resolves), or set ZG_MIGRATIONS_DIR.
//
// Usage:
//
//	migrate up            apply all pending migrations
//	migrate down [N]      revert N migrations (default 1)
//	migrate version       print the current schema version
//	migrate force <v>     force-set the schema version (unstick a dirty state)
//	migrate seed-account [--reset-password]
//	                      冷启动引导: 建首个租户 + 首个 boss(owner) 账号
//
// seed-account 从 env 读引导参数(绝不走 CLI 参数): ZG_BOOTSTRAP_TENANT_NAME
// (默认"演示店") / ZG_BOOTSTRAP_BOSS_PHONE / ZG_BOOTSTRAP_BOSS_PASSWORD
// (明文只从 env 读, 无默认值, 绝不打印/日志)。详见 docs/runbooks/deploy.md §2.4。
//
// It reads the same layered config as the API (ZG_ env / config.<env>.yaml) to
// locate the database, so the same ZG_ENV + ZG_POSTGRES_* variables apply.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // postgres driver for migrate
	_ "github.com/golang-migrate/migrate/v4/source/file"       // file:// source for migrate

	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/internal/bootstrap"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/db"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "migrate %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	cfg, err := config.Load(config.WithEnv(envOr("ZG_ENV", "dev")))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// seed-account 不走 golang-migrate(不需要 migrations 目录), 提前分支。
	if cmd == "seed-account" {
		return runSeedAccount(cfg, args)
	}

	migrationsDir := envOr("ZG_MIGRATIONS_DIR", "migrations")

	m, err := migrate.New("file://"+migrationsDir, db.DSNFromConfig(cfg.Postgres))
	if err != nil {
		return fmt.Errorf("init migrate: %w", err)
	}
	defer m.Close()

	switch cmd {
	case "up":
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return err
		}
	case "down":
		n := 1
		if len(args) > 0 {
			if n, err = strconv.Atoi(args[0]); err != nil {
				return fmt.Errorf("down: count must be an integer, got %q", args[0])
			}
		}
		if err := m.Steps(-n); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return err
		}
	case "version":
		v, dirty, err := m.Version()
		if err != nil {
			return err
		}
		fmt.Printf("version=%d dirty=%v\n", v, dirty)
	case "force":
		if len(args) < 1 {
			return errors.New("force requires a version argument")
		}
		v, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("force: version must be an integer, got %q", args[0])
		}
		if err := m.Force(v); err != nil {
			return err
		}
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: migrate [up|down [N]|version|force <v>|seed-account [--reset-password]]")
}

// runSeedAccount 实现「建首个租户 + 首个 boss 账号」冷启动引导。
//
// 密码安全红线(本任务核心验收点): 明文密码【只】从 env ZG_BOOTSTRAP_BOSS_PASSWORD
// 读, 绝不接受 CLI 参数(ps 可见)、绝无默认值、绝不打印/日志; 输出只有
// tenant_id / account_id / phone。账号已存在且未指定 --reset-password 时根本不
// 读密码 env(幂等重跑不要求它在场)。
func runSeedAccount(cfg *config.Config, args []string) error {
	reset, err := parseSeedAccountArgs(args)
	if err != nil {
		return err
	}
	phone := os.Getenv("ZG_BOOTSTRAP_BOSS_PHONE")
	if phone == "" {
		return errors.New("seed-account: 缺少 env ZG_BOOTSTRAP_BOSS_PHONE (boss 手机号)")
	}
	// 明文密码只在内存短暂停留; 绝不打印/日志。
	password := os.Getenv("ZG_BOOTSTRAP_BOSS_PASSWORD")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	database, err := db.New(ctx, cfg.Postgres)
	if err != nil {
		return fmt.Errorf("seed-account: 连接 PG: %w", err)
	}
	defer database.Close()
	if err := database.Ping(ctx); err != nil {
		return fmt.Errorf("seed-account: PG 不可达: %w", err)
	}

	res, err := bootstrap.SeedFirstBoss(ctx,
		bootstrap.NewPGTenantStore(database.Pool()),
		auth.NewPGAccountRepo(database.Pool()),
		bootstrap.SeedAccountOptions{
			TenantName:    envOr("ZG_BOOTSTRAP_TENANT_NAME", "演示店"),
			Phone:         phone,
			Password:      password,
			ResetPassword: reset,
		})
	if err != nil {
		return err
	}

	switch {
	case res.Skipped:
		fmt.Printf("seed-account: 账号已存在, 跳过 (tenant_id=%d account_id=%d phone=%s); 如需重置密码请显式加 --reset-password\n",
			res.TenantID, res.AccountID, res.Phone)
	case res.PasswordReset:
		fmt.Printf("seed-account: 密码已重置 (tenant_id=%d account_id=%d phone=%s)\n",
			res.TenantID, res.AccountID, res.Phone)
	default:
		fmt.Printf("seed-account: 引导完成 tenant_id=%d(新建=%v) account_id=%d phone=%s role=%s\n",
			res.TenantID, res.TenantCreated, res.AccountID, res.Phone, res.Role)
	}
	return nil
}

// parseSeedAccountArgs 只认 --reset-password; 其它任何参数一律拒绝——密码绝不
// 允许以 CLI 参数形式出现(ps / shell history 会留痕)。
func parseSeedAccountArgs(args []string) (reset bool, err error) {
	for _, a := range args {
		if a == "--reset-password" {
			reset = true
			continue
		}
		return false, fmt.Errorf("seed-account: 未知参数 %q (密码只走 env ZG_BOOTSTRAP_BOSS_PASSWORD, 不接受命令行参数)", a)
	}
	return reset, nil
}

func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}
