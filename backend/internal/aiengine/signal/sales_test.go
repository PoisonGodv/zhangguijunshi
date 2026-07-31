package signal

// sales_test.go covers the M7-B historical-sales signal: the pure aggregateSales
// / markSlowMoving helpers and the Collect integration (a scripted SalesProvider
// feeds real per-product daily sales into the Bundle, the inventory SlowMoving
// flag is merged from history, and the DataQuality bonus applies). The
// nil-provider / provider-error paths assert the cold-start-safe degrade.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSalesProvider implements SalesProvider for tests; it returns a scripted
// row set and/or error and records the range it was asked for.
type fakeSalesProvider struct {
	rows     []DailySale
	err      error
	gotFrom  time.Time
	gotTo    time.Time
	gotStore int64
}

func (f *fakeSalesProvider) DailySales(_ context.Context, _, storeID int64, from, to time.Time) ([]DailySale, error) {
	f.gotFrom, f.gotTo, f.gotStore = from, to, storeID
	return f.rows, f.err
}

func day(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

func TestAggregateSales_TrendSlowBestSeller(t *testing.T) {
	from, to := day(2026, 7, 14), day(2026, 7, 27) // 14-day window, midpoint 07-20 12:00
	cfg := defaultSalesConfig()
	rows := []DailySale{
		{ProductID: 10, Day: day(2026, 7, 15), Qty: 5}, // prior half
		{ProductID: 10, Day: day(2026, 7, 25), Qty: 8}, // recent half
		{ProductID: 20, Day: day(2026, 7, 16), Qty: 1}, // prior half, barely sold
	}
	got := aggregateSales(rows, from, to, cfg)
	if len(got) != 2 {
		t.Fatalf("aggregateSales len: got %d want 2 (%+v)", len(got), got)
	}
	// Sorted by product id: product 10 first.
	p10, p20 := got[0], got[1]
	if p10.ProductID != 10 || p10.TotalQty != 13 || p10.RecentQty != 8 || p10.PriorQty != 5 {
		t.Errorf("p10 qty split wrong: %+v", p10)
	}
	if p10.Trend != TrendUp {
		t.Errorf("p10 trend: got %q want up", p10.Trend)
	}
	if p10.ActiveDays != 2 {
		t.Errorf("p10 active days: got %d want 2", p10.ActiveDays)
	}
	if p10.SlowMoving {
		t.Errorf("p10 should not be slow-moving (13 units)")
	}
	if !p10.BestSeller {
		t.Errorf("p10 should be the best seller (highest total)")
	}
	// Product 20: 1 unit, prior half → trend down, slow-moving, not best.
	if p20.ProductID != 20 || p20.TotalQty != 1 || p20.Trend != TrendDown {
		t.Errorf("p20 wrong: %+v", p20)
	}
	if !p20.SlowMoving {
		t.Errorf("p20 should be slow-moving (1 unit <= threshold 2)")
	}
	if p20.BestSeller {
		t.Errorf("p20 should not be the best seller")
	}
}

func TestAggregateSales_EmptyAndFlat(t *testing.T) {
	from, to := day(2026, 7, 14), day(2026, 7, 27)
	cfg := defaultSalesConfig()

	if got := aggregateSales(nil, from, to, cfg); got != nil {
		t.Errorf("empty rows should yield nil, got %+v", got)
	}

	// Equal recent/prior → flat trend.
	rows := []DailySale{
		{ProductID: 5, Day: day(2026, 7, 15), Qty: 4}, // prior
		{ProductID: 5, Day: day(2026, 7, 25), Qty: 4}, // recent
	}
	got := aggregateSales(rows, from, to, cfg)
	if len(got) != 1 || got[0].Trend != TrendFlat {
		t.Fatalf("flat trend: %+v", got)
	}
	if got[0].RecentQty != 4 || got[0].PriorQty != 4 {
		t.Errorf("flat split: %+v", got[0])
	}
}

func TestMarkSlowMoving_OnlyProductsWithHistory(t *testing.T) {
	inv := []InventorySig{
		{ProductID: 10, Name: "有历史-滞销"},
		{ProductID: 20, Name: "有历史-畅销"},
		{ProductID: 30, Name: "无历史-不动"},
	}
	sales := []SalesSig{
		{ProductID: 10, TotalQty: 1, SlowMoving: true},
		{ProductID: 20, TotalQty: 50, SlowMoving: false},
	}
	out := markSlowMoving(inv, sales, 2)
	if !out[0].SlowMoving {
		t.Errorf("product 10 (1 unit) should be flagged slow-moving")
	}
	if out[1].SlowMoving {
		t.Errorf("product 20 (50 units) should NOT be flagged")
	}
	if out[2].SlowMoving {
		t.Errorf("product 30 (no history) must NOT be flagged (no data yet)")
	}
}

func TestCollect_SalesProvider_FeedsBundle(t *testing.T) {
	fixed := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	// Window (default 14d): to = 07-27, from = 07-14.
	prov := &fakeSalesProvider{rows: []DailySale{
		{ProductID: 10, Day: day(2026, 7, 15), Qty: 5},
		{ProductID: 10, Day: day(2026, 7, 25), Qty: 8},
		{ProductID: 20, Day: day(2026, 7, 16), Qty: 1},
	}}
	lk := &fakeLookuper{inv: []InventorySig{{ProductID: 20, Name: "奶油草莓"}}}
	svc := NewCollector("fruit", "临沂", lk,
		WithNowFunc(func() time.Time { return fixed }),
		WithSalesProvider(prov),
	)

	b, err := svc.Collect(context.Background(), 50021, 7)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// Provider asked for the [07-14, 07-27] window, store-scoped.
	if !prov.gotFrom.Equal(day(2026, 7, 14)) || !prov.gotTo.Equal(day(2026, 7, 27)) {
		t.Errorf("window: got [%v, %v], want [07-14, 07-27]", prov.gotFrom, prov.gotTo)
	}
	if prov.gotStore != 7 {
		t.Errorf("store scope: got %d want 7", prov.gotStore)
	}
	// Bundle.Sales populated + sorted.
	if len(b.Sales) != 2 {
		t.Fatalf("bundle sales len: got %d want 2 (%+v)", len(b.Sales), b.Sales)
	}
	if b.Sales[0].ProductID != 10 || b.Sales[0].TotalQty != 13 || !b.Sales[0].BestSeller {
		t.Errorf("sales[0]: %+v", b.Sales[0])
	}
	// Inventory SlowMoving merged from history: product 20 (1 unit) flagged.
	if len(b.Inventory) != 1 || !b.Inventory[0].SlowMoving {
		t.Errorf("inventory slow-moving merge failed: %+v", b.Inventory)
	}
	// DataQuality: inventory present (40) + sales present (+10) = 50.
	if b.DataQuality != 50 {
		t.Errorf("data_quality: got %d want 50", b.DataQuality)
	}
}

func TestCollect_NilSalesProvider_BackwardCompat(t *testing.T) {
	fixed := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	lk := &fakeLookuper{inv: []InventorySig{{ProductID: 20, Name: "x"}}}
	svc := NewCollector("fruit", "", lk, WithNowFunc(func() time.Time { return fixed }))

	b, err := svc.Collect(context.Background(), 1, 0)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if b.Sales != nil {
		t.Errorf("nil provider must leave Sales nil, got %+v", b.Sales)
	}
	if b.Inventory[0].SlowMoving {
		t.Errorf("nil provider must not touch inventory SlowMoving")
	}
	// DataQuality unchanged from the pre-M7-B contract: inventory only → 40.
	if b.DataQuality != 40 {
		t.Errorf("data_quality: got %d want 40 (backward-compat)", b.DataQuality)
	}
}

func TestCollect_SalesProviderError_Degrades(t *testing.T) {
	fixed := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	prov := &fakeSalesProvider{err: errors.New("sales boom")}
	svc := NewCollector("fruit", "", &fakeLookuper{},
		WithNowFunc(func() time.Time { return fixed }),
		WithSalesProvider(prov),
	)
	b, err := svc.Collect(context.Background(), 1, 0)
	if err != nil {
		t.Fatalf("Collect must not surface provider error: %v", err)
	}
	if b == nil {
		t.Fatal("nil Bundle")
	}
	if b.Sales != nil {
		t.Errorf("provider error must degrade to nil Sales, got %+v", b.Sales)
	}
	if b.DataQuality != 0 {
		t.Errorf("data_quality: got %d want 0 (no signal present)", b.DataQuality)
	}
}

func TestCollect_SalesWindowDaysOption(t *testing.T) {
	fixed := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	prov := &fakeSalesProvider{}
	svc := NewCollector("fruit", "", nil,
		WithNowFunc(func() time.Time { return fixed }),
		WithSalesProvider(prov),
		WithSalesWindowDays(7),
	)
	if _, err := svc.Collect(context.Background(), 1, 0); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// 7-day window: to = 07-27, from = 07-21.
	if !prov.gotFrom.Equal(day(2026, 7, 21)) || !prov.gotTo.Equal(day(2026, 7, 27)) {
		t.Errorf("7-day window: got [%v, %v], want [07-21, 07-27]", prov.gotFrom, prov.gotTo)
	}
}
