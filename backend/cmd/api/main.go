// Command api is the single HTTP entry point for the modular monolith
// (tech plan §1.2.1 / §6.1.3). It loads layered config, initialises structured
// logging, optionally opens the DB pool, and serves the three BFF groups with
// graceful shutdown on SIGINT/SIGTERM.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	httpserver "github.com/zhangguijunshi/backend/interfaces/http"
	"github.com/zhangguijunshi/backend/internal/auth"
	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/inventory"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/db"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/logger"
	"github.com/zhangguijunshi/backend/pkg/redisx"
	"github.com/zhangguijunshi/backend/pkg/wxsdk"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.WithEnv(envOr("ZG_ENV", "dev")))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log, err := logger.New(cfg.Log)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()
	logger.SetDefault(log)

	log.Info("starting zhanggui backend",
		zap.String("env", cfg.App.Env),
		zap.String("version", cfg.App.Version),
	)

	// The DB pool is optional at boot: in local dev without Docker/Postgres we
	// log a warning and continue with a nil pool; readyz reports it as skipped.
	var database *db.DB
	if cfg.Postgres.Host != "" {
		database, err = db.New(context.Background(), cfg.Postgres)
		if err != nil {
			log.Warn("db pool init failed; continuing without database", zap.Error(err))
			database = nil
		} else {
			pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if perr := database.Ping(pctx); perr != nil {
				log.Warn("db ping failed; continuing without database",
					zap.String("dsn", database.DSN()), zap.Error(perr))
				database.Close()
				database = nil
			} else {
				log.Info("db connected", zap.String("dsn", database.DSN()))
			}
			cancel()
		}
	}

	// Auth stack (tech plan §6.2): ES256 JWT manager + a SessionStore (Redis
	// when available, else in-memory for local dev) + the consumer auth service.
	// All degrade gracefully — a dev box with no Redis still issues real tokens.
	jwtMgr, err := buildJWTManager(cfg, log)
	if err != nil {
		return fmt.Errorf("init jwt: %w", err)
	}
	store := buildSessionStore(context.Background(), cfg, log)

	var authSvc *auth.AuthService
	if jwtMgr != nil {
		wxOpts := []wxsdk.Option{}
		if cfg.WX.Code2SessionURL != "" {
			wxOpts = append(wxOpts, wxsdk.WithBaseURL(cfg.WX.Code2SessionURL))
		}
		if cfg.WX.Timeout > 0 {
			wxOpts = append(wxOpts, wxsdk.WithTimeout(cfg.WX.Timeout))
		}
		authSvc = auth.New(auth.Config{
			AppID:      cfg.WX.AppID,
			Secret:     cfg.WX.Secret,
			AccessTTL:  cfg.JWT.AccessTTL,
			RefreshTTL: cfg.JWT.RefreshTTL,
		}, wxsdk.New(wxOpts...), jwtMgr, store, auth.NewMemoryConsumerRepo())
	}

	// M1-S1 domain services (CONTRACT §8). Repositories are in-memory until
	// PostgreSQL is wired (the services depend only on the repo interfaces, so a
	// pgx implementation drops in later without touching the HTTP layer).
	catalogRepo := catalog.NewMemoryProductRepo()
	catalogSvc := catalog.NewService(catalogRepo)

	// M1-S2 domain services (CONTRACT §9). Inventory enforces product-tenant
	// ownership through the catalog adapter; billing consumes stock FIFO after a
	// committed sale; membership issues/validates dynamic member codes.
	inventorySvc := inventory.New(inventory.NewMemoryBatchRepo(), catalogProductLookuper{catalogSvc})
	billingSvc := billing.New(
		billing.NewMemoryOrderRepo(),
		catalogPricer{catalogSvc},
		billing.WithStockConsumer(inventoryStockConsumer{inventorySvc}),
	)
	membershipSvc := membership.New(membership.NewMemoryMemberRepo(), membership.NewMemoryMemberCoder(nil))

	srv := &http.Server{
		Addr: fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler: httpserver.New(httpserver.Deps{
			Cfg:        cfg,
			DB:         database,
			JWT:        jwtMgr,
			Store:      store,
			AuthSvc:    authSvc,
			Catalog:    catalogSvc,
			Billing:    billingSvc,
			Inventory:  inventorySvc,
			Membership: membershipSvc,
			DevMock:    cfg.Multitenancy.DevMockHeader,
		}),
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
	}

	go func() {
		log.Info("http listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("http server crashed", zap.Error(err))
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	if database != nil {
		database.Close()
	}
	if store != nil {
		_ = store.Close()
	}
	log.Info("shutdown complete")
	return nil
}

func envOr(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}

// catalogPricer adapts *catalog.Service to billing.ProductPricer (CONTRACT §8.3
// anti-corruption layer): billing never imports catalog, so this thin adapter
// translates the priced view at the composition root.
type catalogPricer struct{ svc *catalog.Service }

func (p catalogPricer) GetByID(ctx context.Context, tenantID, id int64) (*billing.PricedProduct, error) {
	prod, err := p.svc.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// Defence-in-depth: the service already scopes to the ctx tenant, but the
	// billing contract requires the returned product to belong to tenantID.
	if prod.TenantID != tenantID {
		return nil, catalog.ErrNotFound
	}
	return &billing.PricedProduct{
		ID:         prod.ID,
		TenantID:   prod.TenantID,
		StoreID:    prod.StoreID,
		Name:       prod.Name,
		Unit:       prod.Unit,
		PriceCents: prod.PriceCents,
		CostCents:  prod.CostCents,
		Status:     prod.Status,
	}, nil
}

// catalogProductLookuper adapts *catalog.Service to inventory.ProductLookuper
// (CONTRACT §9.3 anti-corruption): inventory never imports catalog, so this thin
// adapter answers "does this product belong to the ctx tenant" at the
// composition root. A cross-tenant or unknown product reads as not-exists.
type catalogProductLookuper struct{ svc *catalog.Service }

func (l catalogProductLookuper) Exists(ctx context.Context, tenantID, productID int64) (bool, error) {
	prod, err := l.svc.GetByID(ctx, productID)
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if prod.TenantID != tenantID {
		return false, nil
	}
	return true, nil
}

// inventoryStockConsumer adapts *inventory.Service to billing.StockConsumer
// (CONTRACT §9.3 anti-corruption): billing never imports inventory, so this
// thin adapter forwards FIFO deduction at the composition root. Errors are
// mapped to a plain error (billing treats any failure as a reconciliation gap).
type inventoryStockConsumer struct{ svc *inventory.Service }

func (c inventoryStockConsumer) ConsumeFIFO(ctx context.Context, storeID, productID, qty, refOrderID int64) error {
	_, err := c.svc.ConsumeFIFO(ctx, storeID, productID, qty, refOrderID)
	return err
}

// buildJWTManager constructs the ES256 signer/verifier. It loads a PEM private
// key from cfg.JWT.KeyFile when set; otherwise it derives a deterministic dev
// key (logged loudly) so a dev server still issues verifiable tokens with no
// configuration.
func buildJWTManager(cfg *config.Config, log *zap.Logger) (*jwt.Manager, error) {
	var priv *ecdsa.PrivateKey
	if cfg.JWT.KeyFile != "" {
		p, err := jwt.LoadPrivateKeyFile(cfg.JWT.KeyFile)
		if err != nil {
			return nil, err
		}
		priv = p
		log.Info("jwt key loaded from file", zap.String("key_file", cfg.JWT.KeyFile))
	} else {
		p, err := jwt.DevKey(jwt.DevSeed)
		if err != nil {
			return nil, err
		}
		priv = p
		log.Warn("using derived dev JWT key — DO NOT USE IN PRODUCTION",
			zap.String("kid", cfg.JWT.KeyID),
			zap.String("hint", "set jwt.key_file to a PEM ECDSA P-256 private key in staging/prod"),
		)
	}
	return jwt.New(jwt.Config{
		PrivateKey: priv,
		KeyID:      cfg.JWT.KeyID,
		Issuer:     cfg.JWT.Issuer,
		AccessTTL:  cfg.JWT.AccessTTL,
	})
}

// buildSessionStore returns a Redis-backed SessionStore when Redis is reachable,
// otherwise an in-memory store. Both satisfy jwt.SessionStore.
func buildSessionStore(ctx context.Context, cfg *config.Config, log *zap.Logger) jwt.SessionStore {
	if cfg.Redis.Host == "" {
		return jwt.NewMemorySessionStore()
	}
	rdb, err := redisx.New(ctx, cfg.Redis)
	if err != nil {
		log.Warn("redis client init failed; using memory session store", zap.Error(err))
		return jwt.NewMemorySessionStore()
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := rdb.Ping(pctx); err != nil {
		log.Warn("redis ping failed; using memory session store",
			zap.String("addr", rdb.Addr()), zap.Error(err))
		_ = rdb.Close()
		return jwt.NewMemorySessionStore()
	}
	log.Info("redis connected; using redis session store", zap.String("addr", rdb.Addr()))
	return jwt.NewRedisSessionStore(rdb.Client)
}
