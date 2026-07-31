package signal

// sales.go wires the fused per-product daily sales (dataquality BE-A-08,
// daily_product_sales / migration 000019) into the Signal Collect stage (M7-B).
// Before M7-B the signal stage had no real historical-sales input — the
// inventory.slow_moving axis the rule router branches on was always false and
// the Bundle carried no sales trend. This file defines the SalesProvider
// anti-corruption port (the main thread adapts dataquality.SalesRepo to it, so
// signal imports NO domain package), the per-product SalesSig the Bundle now
// carries, and the pure aggregation that turns a raw daily-sales window into
// those signals (近N天销量 / 趋势 / 滞销·畅销).
//
// Everything here is nil-safe and best-effort: a nil provider, a provider
// error, or an empty window all degrade to "no sales signal" (Sales left nil,
// the Inventory SlowMoving flag untouched) so the rule-only / cold-start
// philosophy (CONTRACT §0) holds.

import (
	"context"
	"sort"
	"time"
)

// DefaultSalesWindowDays is the lookback the collector reads when no explicit
// window is configured (WithSalesWindowDays). 14 days is long enough for a
// stable recent-vs-prior trend split yet short enough to bound the scan.
const DefaultSalesWindowDays = 14

// DefaultSlowMovingThresholdQty is the default 滞销 cutoff: a product whose
// total sales over the window is <= this many units is slow-moving. 2 units in
// two weeks ≈ effectively no movement for a fresh-goods SKU.
const DefaultSlowMovingThresholdQty = 2

// SalesSig is one product's historical-sales signal derived from the fused
// daily sales over the lookback window. Qty fields are sold counts (non-amount,
// never floating point). Trend is the direction of RecentQty vs PriorQty.
type SalesSig struct {
	ProductID  int64  `json:"product_id"`
	TotalQty   int64  `json:"total_qty"`   // 近N天总销量
	RecentQty  int64  `json:"recent_qty"`  // 近半窗销量
	PriorQty   int64  `json:"prior_qty"`   // 前半窗销量
	ActiveDays int    `json:"active_days"` // 有销量的天数
	Trend      string `json:"trend"`       // up|down|flat
	SlowMoving bool   `json:"slow_moving"` // 滞销: TotalQty <= 阈值
	BestSeller bool   `json:"best_seller"` // 畅销: 窗口内销量最高
}

// Trend direction constants for SalesSig.Trend.
const (
	TrendUp   = "up"
	TrendDown = "down"
	TrendFlat = "flat"
)

// DailySale is one product's fused sales on one natural day — the value the
// SalesProvider returns. It deliberately mirrors dataquality.DailyProductSales
// field-for-field so the main thread's adapter is a trivial copy, but it is
// declared here so signal imports no domain package (anti-corruption port).
type DailySale struct {
	ProductID int64
	Day       time.Time
	Qty       int64
}

// SalesProvider is the anti-corruption port (defined in signal; the main thread
// adapts dataquality.SalesRepo.ListDailySales to it) that supplies real
// historical per-product daily sales to the Signal Collect stage (M7-B). It is
// best-effort: an implementation that cannot enumerate returns an empty slice +
// nil error so the collector degrades to "no sales signal" (cold-start safe).
// from/to are an inclusive natural-day range; storeID=0 means all of the
// tenant's stores.
type SalesProvider interface {
	DailySales(ctx context.Context, tenantID, storeID int64, from, to time.Time) ([]DailySale, error)
}

// salesConfig holds the tunables for the sales-signal derivation.
type salesConfig struct {
	windowDays    int   // lookback window length (>=1)
	slowThreshold int64 // TotalQty <= threshold → slow-moving
}

// defaultSalesConfig returns the default tunables.
func defaultSalesConfig() salesConfig {
	return salesConfig{windowDays: DefaultSalesWindowDays, slowThreshold: DefaultSlowMovingThresholdQty}
}

// aggregateSales folds the raw daily-sales rows over [from, to] into one
// SalesSig per product. It is a PURE function (no clock, no side effects): the
// recent/prior split is around the window midpoint, so the result is fully
// determined by (rows, from, to, cfg).
//
//   - TotalQty / ActiveDays: sum of qty / count of distinct days with qty>0.
//   - RecentQty / PriorQty: the window is split at its midpoint; days at or
//     after the midpoint are "recent", earlier days are "prior".
//   - Trend: up when RecentQty > PriorQty, down when <, flat when equal.
//   - SlowMoving: TotalQty <= cfg.slowThreshold (a product we have history for
//     but that barely sold).
//   - BestSeller: the single product with the highest TotalQty (> 0); ties go
//     to the lowest ProductID for determinism.
//
// Rows outside [from, to] are ignored defensively. An empty rows slice yields a
// nil result. The returned slice is sorted by ProductID ascending.
func aggregateSales(rows []DailySale, from, to time.Time, cfg salesConfig) []SalesSig {
	if len(rows) == 0 {
		return nil
	}
	// midpoint is the first "recent" day: from + half the window length.
	midpoint := from.Add(to.Sub(from) / 2)

	type acc struct {
		total, recent, prior int64
		days                 map[string]struct{}
	}
	byProduct := make(map[int64]*acc)
	for _, r := range rows {
		if r.Day.Before(from) || r.Day.After(to) {
			continue // defensive: ignore out-of-window rows
		}
		a := byProduct[r.ProductID]
		if a == nil {
			a = &acc{days: make(map[string]struct{})}
			byProduct[r.ProductID] = a
		}
		if r.Qty <= 0 {
			continue // a zero/negative cell contributes no volume nor an active day
		}
		a.total += r.Qty
		if !r.Day.Before(midpoint) {
			a.recent += r.Qty
		} else {
			a.prior += r.Qty
		}
		a.days[r.Day.Format("2006-01-02")] = struct{}{}
	}
	if len(byProduct) == 0 {
		return nil
	}

	out := make([]SalesSig, 0, len(byProduct))
	bestID, bestQty := int64(0), int64(0)
	for productID, a := range byProduct {
		trend := TrendFlat
		switch {
		case a.recent > a.prior:
			trend = TrendUp
		case a.recent < a.prior:
			trend = TrendDown
		}
		out = append(out, SalesSig{
			ProductID:  productID,
			TotalQty:   a.total,
			RecentQty:  a.recent,
			PriorQty:   a.prior,
			ActiveDays: len(a.days),
			Trend:      trend,
			SlowMoving: a.total <= cfg.slowThreshold,
		})
		if a.total > bestQty || (a.total == bestQty && a.total > 0 && productID < bestID) {
			bestID, bestQty = productID, a.total
		}
	}
	// Mark the single best seller (highest TotalQty>0; tie → lowest ProductID).
	if bestQty > 0 {
		for i := range out {
			if out[i].ProductID == bestID {
				out[i].BestSeller = true
			}
		}
	}
	sortSalesByProduct(out)
	return out
}

// sortSalesByProduct orders sales by ProductID ascending for deterministic
// downstream consumption.
func sortSalesByProduct(sales []SalesSig) {
	sort.Slice(sales, func(i, j int) bool { return sales[i].ProductID < sales[j].ProductID })
}

// salesQtyByProduct maps productID → total window qty, for merging the
// slow-moving flag onto inventory signals.
func salesQtyByProduct(sales []SalesSig) map[int64]int64 {
	if len(sales) == 0 {
		return nil
	}
	m := make(map[int64]int64, len(sales))
	for _, s := range sales {
		m[s.ProductID] = s.TotalQty
	}
	return m
}

// markSlowMoving sets InventorySig.SlowMoving=true for any inventory item whose
// product has a sales history in the window AND is slow-moving (TotalQty <=
// threshold). Products with NO sales history are left untouched — we cannot
// distinguish "滞销" from "no data yet" for them, so we do not fire the flag
// spuriously (mirrors the dataquality job's DaysSinceLastSale guard). The merge
// is in-place on inv and returns it.
func markSlowMoving(inv []InventorySig, sales []SalesSig, threshold int64) []InventorySig {
	qty := salesQtyByProduct(sales)
	if len(qty) == 0 {
		return inv
	}
	for i := range inv {
		total, ok := qty[inv[i].ProductID]
		if ok && total <= threshold {
			inv[i].SlowMoving = true
		}
	}
	return inv
}
