package main

import (
	"context"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/internal/bootstrap"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/dataquality"
	"github.com/zhangguijunshi/backend/internal/inventory"
	"github.com/zhangguijunshi/backend/pkg/config"
)

// testConfig loads a defaults-only config from an empty temp dir (no yaml layer).
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(config.WithConfigDir(t.TempDir()))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// testServices builds the in-memory/dev graph. We pass an EMPTY non-nil
// &bootstrap.Infra{} so BuildServices skips BuildInfra entirely → DB/RDB/JWT stay
// nil → every repo falls back to in-memory and the tenant enumerator is the
// dev-empty stub. This is deterministic and side-effect-free even on a host with
// a live dev Postgres/Redis (empty ZG_*_HOST env does NOT override viper's
// 127.0.0.1 defaults, so passing nil infra would wire the real dev DB).
func testServices(t *testing.T) *bootstrap.Services {
	t.Helper()
	cfg := testConfig(t)
	svc, err := bootstrap.BuildServices(context.Background(), cfg, zap.NewNop(), &bootstrap.Infra{})
	if err != nil {
		t.Fatalf("BuildServices: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

func TestEnvOr(t *testing.T) {
	t.Setenv("ZG_CRON_TEST_KEY", "override")
	if got := envOr("ZG_CRON_TEST_KEY", "dflt"); got != "override" {
		t.Fatalf("envOr set = %q, want override", got)
	}
	if got := envOr("ZG_CRON_TEST_UNSET_KEY", "dflt"); got != "dflt" {
		t.Fatalf("envOr unset = %q, want dflt", got)
	}
	// Empty value is treated as unset (falls back to default).
	t.Setenv("ZG_CRON_TEST_EMPTY", "")
	if got := envOr("ZG_CRON_TEST_EMPTY", "dflt"); got != "dflt" {
		t.Fatalf("envOr empty = %q, want dflt", got)
	}
}

// TestRegisterJobs asserts the 3 non-pregen jobs register cleanly on a fresh
// cron, WITHOUT running any job body.
func TestRegisterJobs(t *testing.T) {
	svc := testServices(t)
	c := cron.New()
	if err := registerJobs(c, svc, zap.NewNop()); err != nil {
		t.Fatalf("registerJobs: %v", err)
	}
	if got := len(c.Entries()); got != 3 {
		t.Fatalf("registered entries = %d, want 3 (groupbuy+subscription+dataquality)", got)
	}
}

// TestRegisterJobs_NilServices: registration must not touch svc, so even a nil
// services graph registers all 3 specs without error.
func TestRegisterJobs_NilServices(t *testing.T) {
	c := cron.New()
	if err := registerJobs(c, nil, zap.NewNop()); err != nil {
		t.Fatalf("registerJobs(nil svc): %v", err)
	}
	if got := len(c.Entries()); got != 3 {
		t.Fatalf("registered entries = %d, want 3", got)
	}
}

// TestJobBodies_NilSafe invokes each job body against the in-memory graph (dev
// enumerates zero tenants) and against a nil graph; neither may panic.
func TestJobBodies_NilSafe(t *testing.T) {
	svc := testServices(t)
	j := &jobs{svc: svc, log: zap.NewNop(), now: time.Now}
	j.groupbuyFinalize()
	j.subscriptionExpireDue()
	j.dataquality()

	// nil graph: every body must guard and return without panicking.
	jn := &jobs{svc: nil, log: zap.NewNop(), now: time.Now}
	jn.groupbuyFinalize()
	jn.subscriptionExpireDue()
	jn.dataquality()
}

// TestSubscriptionExpireDue_ReturnsCount verifies the global batch op runs
// against the in-memory store and reports a non-negative count with no error.
func TestSubscriptionExpireDue_ReturnsCount(t *testing.T) {
	svc := testServices(t)
	if svc.Subscription == nil {
		t.Fatal("Subscription service is nil")
	}
	n, err := svc.Subscription.ExpireDue(context.Background())
	if err != nil {
		t.Fatalf("ExpireDue: %v", err)
	}
	if n < 0 {
		t.Fatalf("expired count = %d, want >= 0", n)
	}
}

// TestSalesReverser_AbsoluteValue exercises the real adapter + ReverseDailySales
// aggregation: a sale_out movement is recorded as a NEGATIVE quantity (正入负出),
// so the reversed per-product qty must be its absolute value.
func TestSalesReverser_AbsoluteValue(t *testing.T) {
	svc := testServices(t)
	ctx := tenantCtx(context.Background(), 1)
	const storeID int64 = 1

	// Seed a product (catalog) so inventory accepts stock for it.
	p, err := svc.Catalog.Create(ctx, catalog.CreateInput{
		StoreID:    storeID,
		Name:       "可乐",
		Unit:       "瓶",
		CostCents:  100,
		PriceCents: 300,
	})
	if err != nil {
		t.Fatalf("catalog.Create: %v", err)
	}

	// Purchase-in 10 units, then sell 3 → one sale_out movement with qty -3 today.
	if _, err := svc.Inventory.PurchaseIn(ctx, inventory.PurchaseInInput{
		StoreID: storeID, ProductID: p.ID, Quantity: 10, InPriceCents: 100,
	}); err != nil {
		t.Fatalf("PurchaseIn: %v", err)
	}
	if _, err := svc.Inventory.ConsumeFIFO(ctx, storeID, p.ID, 3, 0); err != nil {
		t.Fatalf("ConsumeFIFO: %v", err)
	}

	// Reverse today's sales across all stores (storeID=0), like the cron job.
	rev := &inventorySalesReverser{inv: svc.Inventory}
	sales, err := dataquality.ReverseDailySales(ctx, rev, 1, 0, time.Now())
	if err != nil {
		t.Fatalf("ReverseDailySales: %v", err)
	}
	if len(sales) != 1 {
		t.Fatalf("sales rows = %d, want 1 (%+v)", len(sales), sales)
	}
	if sales[0].ProductID != p.ID || sales[0].Qty != 3 {
		t.Fatalf("sale = %+v, want {ProductID:%d Qty:3}", sales[0], p.ID)
	}
}

// TestComputeTags_HighMargin sanity-checks the pure tagging rule the dataquality
// job uses (only 高毛利 is meaningful until the follow-ups land, since
// DaysSinceLastSale=0 and EarliestExpiry=nil suppress 滞销/临期).
func TestComputeTags_HighMargin(t *testing.T) {
	rule := dataquality.TagRule{NearingDays: 3, StaleDays: 30, HighMarginMinPct: 40}
	tags := dataquality.ComputeTags(dataquality.TagProductView{
		ProductID: 1, CostCents: 100, PriceCents: 1000, // 90% margin
		DaysSinceLastSale: 0, EarliestExpiry: nil,
	}, rule, time.Now())
	if len(tags) != 1 || tags[0] != dataquality.TagHighMargin {
		t.Fatalf("tags = %v, want [%s]", tags, dataquality.TagHighMargin)
	}
}
