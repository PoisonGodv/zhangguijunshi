// Package dataquality implements the M1-S2 data-quality background jobs
// (BE-A-08 销售反推 / BE-A-09 标签标记, CONTRACT §9.3).
//
// This is a PURE-FUNCTION job package: it imports NO domain packages
// (catalog / billing / inventory). All cross-domain access goes through the
// narrow interfaces defined here (SalesReverser), which the main thread adapts
// to real repositories and schedules on a cron. Tenant is never taken from a
// request body; in the job scenario the main thread resolves tenantID from ctx
// and passes it explicitly.
//
// Amounts are always int64 cents (分); floating point is forbidden.
package dataquality

import (
	"context"
	"sort"
	"time"
)

// Tag label constants. ComputeTags returns tags drawn only from this set, in
// this fixed order (临期 / 滞销 / 高毛利), de-duplicated.
const (
	TagNearingExpiry = "临期"
	TagStale         = "滞销"
	TagHighMargin    = "高毛利"
)

// ---- BE-A-08 销售反推 -------------------------------------------------------

// SalesReverser is the minimal read port the reverse-sales job needs: the
// store's sale_out quantities for one natural day, already tenant-scoped.
// The main thread adapts an inventory/billing repository to this interface.
type SalesReverser interface {
	ListSaleOut(ctx context.Context, tenantID, storeID int64, day time.Time) ([]SaleQty, error)
}

// SaleQty is the aggregated sold quantity of one product for a day.
type SaleQty struct {
	ProductID, Qty int64
}

// ReverseDailySales aggregates the day's sale_out records into per-product
// quantities (BE-A-08, the inventory source of the three-source daily-sales
// fusion). It delegates the read to r.ListSaleOut, merges multiple records
// that share a ProductID by summing their Qty, and returns the result sorted
// by ProductID ascending so the output is deterministic.
//
// tenantID/storeID are passed explicitly (resolved from ctx by the main thread
// in the job scenario); this function performs no tenant enforcement itself.
func ReverseDailySales(ctx context.Context, r SalesReverser, tenantID, storeID int64, day time.Time) ([]SaleQty, error) {
	rows, err := r.ListSaleOut(ctx, tenantID, storeID, day)
	if err != nil {
		return nil, err
	}

	merged := make(map[int64]int64, len(rows))
	for _, row := range rows {
		merged[row.ProductID] += row.Qty
	}

	out := make([]SaleQty, 0, len(merged))
	for productID, qty := range merged {
		out = append(out, SaleQty{ProductID: productID, Qty: qty})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProductID < out[j].ProductID })
	return out, nil
}

// ---- BE-A-09 标签标记 -------------------------------------------------------

// TagProductView is the per-product view the tagging rule consumes: the union
// of the product view and its earliest-expiry batch view, prepared by the main
// thread. All money fields are int64 cents.
type TagProductView struct {
	ProductID, ShelfLifeDays int
	CostCents, PriceCents    int64
	DaysSinceLastSale        int        // 滞销判定: days since the product last sold
	EarliestExpiry           *time.Time // 临期判定: expiry of the soonest-expiring batch; nil = unset
}

// TagRule holds the thresholds for each tag.
type TagRule struct {
	NearingDays      int // 距到期 <= N 天打「临期」
	StaleDays        int // >= N 天未售打「滞销」
	HighMarginMinPct int // (price-cost)/price*100 >= N 打「高毛利」
}

// ComputeTags is a PURE function (no side effects): given one product view, a
// rule, and the evaluation instant now, it returns the set of tags the product
// should carry — de-duplicated and in the fixed order 临期 / 滞销 / 高毛利.
//
// Rules:
//   - 临期: EarliestExpiry != nil and EarliestExpiry is within NearingDays of
//     now (expired already also counts, since diff <= NearingDays).
//   - 滞销: DaysSinceLastSale >= StaleDays.
//   - 高毛利: PriceCents > 0 and (price-cost)*100/price >= HighMarginMinPct.
func ComputeTags(v TagProductView, rule TagRule, now time.Time) []string {
	tags := make([]string, 0, 3)

	// 临期: earliest expiry within (or past) the nearing window.
	if v.EarliestExpiry != nil {
		if v.EarliestExpiry.Sub(now) <= time.Duration(rule.NearingDays)*24*time.Hour {
			tags = append(tags, TagNearingExpiry)
		}
	}

	// 滞销: no sale for at least StaleDays.
	if v.DaysSinceLastSale >= rule.StaleDays {
		tags = append(tags, TagStale)
	}

	// 高毛利: integer margin percentage meets the threshold. Guard price>0 to
	// avoid division by zero; computed in int64 cents, never floating point.
	if v.PriceCents > 0 {
		marginPct := (v.PriceCents - v.CostCents) * 100 / v.PriceCents
		if marginPct >= int64(rule.HighMarginMinPct) {
			tags = append(tags, TagHighMargin)
		}
	}

	return tags
}
