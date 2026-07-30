package main

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/internal/bootstrap"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/dataquality"
	"github.com/zhangguijunshi/backend/internal/inventory"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Cron specs for the non-pregen jobs (the 4 pregen crons live inside
// svc.Scheduler and are NOT registered here). All run in Asia/Shanghai.
const (
	// groupbuy 成团超时: every 5 min, finalize under-subscribed squads past EndAt.
	groupbuyFinalizeSpec = "*/5 * * * *"
	// subscription 到期: daily 06:00, expire active subscriptions past period end.
	subscriptionExpireDueSpec = "0 6 * * *"
	// dataquality: daily 01:00, BE-A-08 销售反推 + BE-A-09 标签标记.
	dataqualitySpec = "0 1 * * *"

	// movementQueryLimit caps a single day's sale_out read. One natural day of a
	// single tenant's stores is far below this; it only guards against an
	// unbounded repo scan.
	movementQueryLimit = 100000

	// dataqualityProductLimit bounds the BE-A-09 tag sweep per tenant.
	dataqualityProductLimit = 500
)

// jobs holds the dependencies the per-job bodies need. Factoring registration
// (registerJobs) apart from the bodies lets a test build a cron.Cron, register,
// and assert the entry count WITHOUT running any job.
type jobs struct {
	svc *bootstrap.Services
	log *zap.Logger
	now func() time.Time
}

// registerJobs wires the 3 non-pregen jobs onto c, logging each registration and
// returning the first AddFunc error (the specs are hard-coded + valid, so an
// error is effectively impossible — the path is kept for contract fidelity).
func registerJobs(c *cron.Cron, svc *bootstrap.Services, log *zap.Logger) error {
	j := &jobs{svc: svc, log: log, now: time.Now}

	specs := []struct {
		spec, name string
		fn         func()
	}{
		{groupbuyFinalizeSpec, "groupbuy.finalize_expired", j.groupbuyFinalize},
		{subscriptionExpireDueSpec, "subscription.expire_due", j.subscriptionExpireDue},
		{dataqualitySpec, "dataquality.daily", j.dataquality},
	}

	for _, s := range specs {
		if _, err := c.AddFunc(s.spec, s.fn); err != nil {
			log.Error("cron register failed",
				zap.String("job", s.name), zap.String("spec", s.spec), zap.Error(err))
			return fmt.Errorf("cron: register %s (%q): %w", s.name, s.spec, err)
		}
		log.Info("cron registered", zap.String("job", s.name), zap.String("spec", s.spec))
	}
	return nil
}

// tenantCtx builds a system-source, tenant-scoped context for a background job.
// The tenant id comes from the enumerator (never a request body); Source=system
// marks it as a cron-originated call.
func tenantCtx(ctx context.Context, tenantID int64) context.Context {
	return multitenancy.WithContext(ctx, multitenancy.Context{
		TenantID: tenantID,
		Source:   multitenancy.SourceSystem,
	})
}

// ---- groupbuy 成团超时 ("*/5 * * * *") --------------------------------------

func (j *jobs) groupbuyFinalize() {
	if j.svc == nil || j.svc.Groupbuy == nil || j.svc.Tenants == nil {
		j.log.Warn("groupbuy.finalize skipped: nil dependency")
		return
	}
	ctx := context.Background()
	ids, err := j.svc.Tenants.ActiveTenantIDs(ctx)
	if err != nil {
		j.log.Warn("groupbuy.finalize: enumerate tenants", zap.Error(err))
		return
	}
	now := j.now()
	total := 0
	for _, id := range ids {
		n, err := j.svc.Groupbuy.FinalizeExpired(tenantCtx(ctx, id), now)
		if err != nil {
			j.log.Warn("groupbuy.finalize: tenant failed",
				zap.Int64("tenant_id", id), zap.Error(err))
			continue
		}
		total += n
	}
	j.log.Info("groupbuy.finalize done",
		zap.Int("tenants", len(ids)), zap.Int("finalized", total))
}

// ---- subscription 到期 ("0 6 * * *") ----------------------------------------

func (j *jobs) subscriptionExpireDue() {
	if j.svc == nil || j.svc.Subscription == nil {
		j.log.Warn("subscription.expire_due skipped: nil dependency")
		return
	}
	// ExpireDue is a GLOBAL batch op: it does not resolve/enforce a tenant from
	// ctx (unlike the per-tenant methods), so a bare background ctx is correct.
	n, err := j.svc.Subscription.ExpireDue(context.Background())
	if err != nil {
		j.log.Warn("subscription.expire_due failed", zap.Error(err))
		return
	}
	j.log.Info("subscription.expire_due done", zap.Int("expired", n))
}

// ---- dataquality ("0 1 * * *") — BE-A-08 销售反推 + BE-A-09 标签 --------------
//
// BEST-EFFORT SCOPE: this job COMPUTES and LOGS the fused per-product daily
// sales and the per-product tags. Persisting the fused daily-sales rows and the
// computed tags back onto the product (and feeding them to the AI pipeline) is a
// documented FOLLOW-UP — see the TODO notes in dataqualityTenant.

func (j *jobs) dataquality() {
	if j.svc == nil || j.svc.Inventory == nil || j.svc.Catalog == nil || j.svc.Tenants == nil {
		j.log.Warn("dataquality skipped: nil dependency")
		return
	}
	ctx := context.Background()
	ids, err := j.svc.Tenants.ActiveTenantIDs(ctx)
	if err != nil {
		j.log.Warn("dataquality: enumerate tenants", zap.Error(err))
		return
	}
	now := j.now()
	day := now.AddDate(0, 0, -1) // 反推昨日销售
	for _, id := range ids {
		j.dataqualityTenant(tenantCtx(ctx, id), id, day, now)
	}
	j.log.Info("dataquality sweep done",
		zap.Int("tenants", len(ids)), zap.String("day", day.Format("2006-01-02")))
}

func (j *jobs) dataqualityTenant(tctx context.Context, tenantID int64, day, now time.Time) {
	// BE-A-08 销售反推: aggregate yesterday's sale_out movements per product.
	// storeID=0 covers all of the tenant's stores in one call.
	reverser := &inventorySalesReverser{inv: j.svc.Inventory}
	sales, err := dataquality.ReverseDailySales(tctx, reverser, tenantID, 0, day)
	if err != nil {
		j.log.Warn("dataquality: reverse daily sales failed",
			zap.Int64("tenant_id", tenantID), zap.Error(err))
	} else {
		j.log.Info("dataquality: daily sales (BE-A-08)",
			zap.Int64("tenant_id", tenantID),
			zap.String("day", day.Format("2006-01-02")),
			zap.Int("products", len(sales)),
			zap.Any("sales", sales))
		// FOLLOW-UP (persistence): write `sales` into the fused daily-sales
		// store (one of the three BE-A-08 sources) once that table/repo lands.
	}

	// BE-A-09 标签标记: compute 临期/滞销/高毛利 tags for each product.
	products, err := j.svc.Catalog.List(tctx, catalog.ProductFilter{Limit: dataqualityProductLimit})
	if err != nil {
		j.log.Warn("dataquality: list products failed",
			zap.Int64("tenant_id", tenantID), zap.Error(err))
		return
	}
	rule := dataquality.TagRule{NearingDays: 3, StaleDays: 30, HighMarginMinPct: 40}
	tagged := 0
	for _, p := range products {
		view := dataquality.TagProductView{
			ProductID:     int(p.ID),
			CostCents:     p.CostCents,
			PriceCents:    p.PriceCents,
			ShelfLifeDays: p.ShelfLifeDays,
			// FOLLOW-UP (accuracy): DaysSinceLastSale should come from the fused
			// daily-sales (BE-A-08) once persisted; EarliestExpiry should come
			// from inventory.ListNearingBatches / the soonest active batch. Both
			// are left at their zero values here (0 / nil) so 滞销 and 临期 do not
			// fire spuriously — only the pure 高毛利 tag is meaningful today.
			DaysSinceLastSale: 0,
			EarliestExpiry:    nil,
		}
		tags := dataquality.ComputeTags(view, rule, now)
		if len(tags) == 0 {
			continue
		}
		tagged++
		j.log.Info("dataquality: tag (BE-A-09)",
			zap.Int64("tenant_id", tenantID),
			zap.Int64("product_id", p.ID),
			zap.Strings("tags", tags))
		// FOLLOW-UP (persistence): write `tags` onto the product once the tag
		// write-back path lands.
	}
	j.log.Info("dataquality: tenant done",
		zap.Int64("tenant_id", tenantID),
		zap.Int("products", len(products)),
		zap.Int("tagged", tagged))
}

// ---- SalesReverser adapter over inventory -----------------------------------

// inventorySalesReverser adapts inventory.Service onto dataquality's narrow
// SalesReverser port (防腐层). The tenant is already resolved from ctx by
// inventory.ListMovements (ARCH-01); the explicit tenantID/storeID params are
// the port's contract and storeID is passed through as the filter scope (0 = all
// of the tenant's stores).
type inventorySalesReverser struct {
	inv *inventory.Service
}

// ListSaleOut returns the day's sale_out quantities per product. Movement.Quantity
// is 正入负出 (sale_out is negative), so the absolute value is the sold amount.
func (a *inventorySalesReverser) ListSaleOut(ctx context.Context, tenantID, storeID int64, day time.Time) ([]dataquality.SaleQty, error) {
	movements, err := a.inv.ListMovements(ctx, inventory.MovementFilter{
		StoreID: storeID,
		Type:    inventory.MovementTypeSaleOut,
		Day:     &day,
		Limit:   movementQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]dataquality.SaleQty, 0, len(movements))
	for _, m := range movements {
		qty := m.Quantity
		if qty < 0 {
			qty = -qty
		}
		out = append(out, dataquality.SaleQty{ProductID: m.ProductID, Qty: qty})
	}
	return out, nil
}
