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

	// BE-A-08/BE-A-09 persistence (migration 000019). dailySales lands the fused
	// per-product daily sales; tagWriter writes the computed tags back onto the
	// product. Both are wired in registerJobs from svc and nil-guarded in
	// dataqualityTenant, so the job degrades to a clean compute+log pass when
	// either is unset.
	dailySales dataquality.SalesRepo
	tagWriter  dataquality.TagWriter
	// expiry feeds the 临期 (nearing-expiry) tag with a whole-tenant per-product
	// earliest batch expiry (M7-A). nil-guarded in dataqualityTenant: unset keeps
	// EarliestExpiry nil so 临期 does not fire (pre-M7-A behaviour).
	expiry dataquality.ExpiryLookuper
}

// registerJobs wires the 3 non-pregen jobs onto c, logging each registration and
// returning the first AddFunc error (the specs are hard-coded + valid, so an
// error is effectively impossible — the path is kept for contract fidelity).
func registerJobs(c *cron.Cron, svc *bootstrap.Services, log *zap.Logger) error {
	// BE-A-08 fused daily-sales persistence: PG when a database is wired, else
	// the in-memory repo (dev). Mirrors bootstrap's memory↔pg repo convention so
	// the cron binary is self-sufficient (no extra Services field required).
	var salesRepo dataquality.SalesRepo = dataquality.NewMemorySalesRepo()
	if svc != nil && svc.DB != nil {
		salesRepo = dataquality.NewPGSalesRepo(svc.DB.Pool())
	}
	// BE-A-09 tag write-back: catalogSvc satisfies dataquality.TagWriter directly
	// (its SetProductTags method), so no adapter is needed.
	var tagWriter dataquality.TagWriter
	if svc != nil && svc.Catalog != nil {
		tagWriter = svc.Catalog
	}
	// M7-A 临期 tag input: inventory.Service implements the whole-tenant
	// per-product earliest-expiry lookup; the thin adapter re-injects the
	// explicit tenantID contract (tenant already rides in ctx, same convention
	// as inventorySalesReverser below).
	var expiry dataquality.ExpiryLookuper
	if svc != nil && svc.Inventory != nil {
		expiry = &inventoryExpiryLookuper{inv: svc.Inventory}
	}

	j := &jobs{svc: svc, log: log, now: time.Now, dailySales: salesRepo, tagWriter: tagWriter, expiry: expiry}

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
// SCOPE: this job COMPUTES the fused per-product daily sales and the per-product
// tags, then PERSISTS both: the fused sales land in daily_product_sales
// (migration 000019) via SalesRepo, and the computed tags are written back onto
// the product via TagWriter (catalog.Service). Feeding the persisted results to
// the AI pipeline is a documented FOLLOW-UP. When either persistence dep is nil
// the job degrades to compute+log only.

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
			zap.Int("products", len(sales)))
		// Persist the fused daily sales (migration 000019). The upsert is
		// idempotent on the unique (tenant, store, product, date) cell, so
		// re-running a day overwrites rather than duplicates.
		if j.dailySales != nil {
			n, uerr := j.dailySales.UpsertDailySales(tctx, tenantID, 0, day, sales)
			if uerr != nil {
				j.log.Warn("dataquality: persist daily sales failed",
					zap.Int64("tenant_id", tenantID), zap.Error(uerr))
			} else {
				j.log.Info("dataquality: daily sales persisted (BE-A-08)",
					zap.Int64("tenant_id", tenantID),
					zap.String("day", day.Format("2006-01-02")),
					zap.Int("rows", n))
			}
		}
	}

	// BE-A-09 标签标记: compute 临期/滞销/高毛利 tags for each product.
	products, err := j.svc.Catalog.List(tctx, catalog.ProductFilter{Limit: dataqualityProductLimit})
	if err != nil {
		j.log.Warn("dataquality: list products failed",
			zap.Int64("tenant_id", tenantID), zap.Error(err))
		return
	}
	rule := dataquality.TagRule{NearingDays: 3, StaleDays: 30, HighMarginMinPct: 40}
	// M7-A 临期 input: one whole-tenant per-product earliest-expiry map (storeID=0
	// covers all of the tenant's stores), fetched once and reused per product.
	// Any error / nil port degrades to a nil map → EarliestExpiryFor returns nil
	// → 临期 does not fire (pre-M7-A behaviour), never a crash (§0).
	var expiries map[int64]time.Time
	if j.expiry != nil {
		expiries, _ = j.expiry.EarliestExpiryByProduct(tctx, tenantID, 0)
	}
	tagged := 0
	written := 0
	for _, p := range products {
		view := dataquality.TagProductView{
			ProductID:     int(p.ID),
			CostCents:     p.CostCents,
			PriceCents:    p.PriceCents,
			ShelfLifeDays: p.ShelfLifeDays,
			// M7-A: real per-product earliest batch expiry (whole-tenant). nil when
			// the product has no dated batch / the port is unwired → 临期 not fired.
			EarliestExpiry: dataquality.EarliestExpiryFor(expiries, p.ID),
		}
		// DaysSinceLastSale comes from the freshly-persisted fused daily sales
		// (BE-A-08): the product's most recent sale_date. A product with no
		// recorded sale keeps DaysSinceLastSale=0 so 滞销 does NOT fire
		// spuriously on products we simply have no sales history for yet.
		if j.dailySales != nil {
			last, ok, lerr := j.dailySales.LastSaleDate(tctx, tenantID, 0, p.ID)
			if lerr != nil {
				j.log.Warn("dataquality: last sale date failed",
					zap.Int64("tenant_id", tenantID), zap.Int64("product_id", p.ID), zap.Error(lerr))
			} else if ok {
				days := int(now.Sub(last).Hours() / 24)
				if days < 0 {
					days = 0
				}
				view.DaysSinceLastSale = days
			}
		}
		tags := dataquality.ComputeTags(view, rule, now)
		// Write the tags back onto the product whenever the computed set differs
		// from the stored set — including the transition to EMPTY. The previous
		// `if len(tags)==0 { continue }` early-return meant a product that lost
		// all its tags (e.g. margin dropped below threshold) kept its stale tags
		// forever, because the clearing write-back never happened. tagsEqual
		// gates the write so an unchanged product (including the steady-state
		// tagsEqual([],[])==true case) is never re-written / re-stamped.
		if j.tagWriter != nil && !tagsEqual(p.Tags, tags) {
			if werr := j.tagWriter.SetProductTags(tctx, tenantID, p.ID, tags); werr != nil {
				j.log.Warn("dataquality: set product tags failed",
					zap.Int64("tenant_id", tenantID), zap.Int64("product_id", p.ID), zap.Error(werr))
			} else {
				written++
			}
		}
		if len(tags) > 0 {
			tagged++
			j.log.Info("dataquality: tag (BE-A-09)",
				zap.Int64("tenant_id", tenantID),
				zap.Int64("product_id", p.ID),
				zap.Strings("tags", tags))
		}
	}
	j.log.Info("dataquality: tenant done",
		zap.Int64("tenant_id", tenantID),
		zap.Int("products", len(products)),
		zap.Int("tagged", tagged),
		zap.Int("tags_written", written))
}

// tagsEqual reports whether two tag sets are identical. The comparison is
// order-sensitive on purpose: dataquality.ComputeTags returns tags in a fixed
// canonical order, so a plain element-wise compare also detects (and lets the
// job normalise) any out-of-order / stale stored set.
func tagsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// ---- ExpiryLookuper adapter over inventory (M7-A) ---------------------------

// inventoryExpiryLookuper adapts inventory.Service onto dataquality's
// ExpiryLookuper port (防腐层). inventory.Service.EarliestExpiryByProduct reads
// the tenant from ctx (ARCH-01 EnforceTenant); the cron's tctx already carries
// it, so the explicit tenantID param is port-contract only (mirrors
// inventorySalesReverser). storeID=0 = whole tenant.
type inventoryExpiryLookuper struct {
	inv *inventory.Service
}

func (a *inventoryExpiryLookuper) EarliestExpiryByProduct(ctx context.Context, tenantID, storeID int64) (map[int64]time.Time, error) {
	if a == nil || a.inv == nil {
		return nil, nil
	}
	return a.inv.EarliestExpiryByProduct(ctx, storeID)
}
