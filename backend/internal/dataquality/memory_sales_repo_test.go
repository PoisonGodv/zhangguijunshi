package dataquality

// memory_sales_repo_test.go covers the in-memory SalesRepo (idempotent upsert on
// the unique cell, most-recent-sale lookup, store/tenant scoping, nil safety)
// and an integration-style test that runs the same flow the cron uses: persist
// fused sales → derive DaysSinceLastSale → ComputeTags → SetProductTags.

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestMemorySalesRepo_UpsertIdempotentOnUniqueKey(t *testing.T) {
	r := NewMemorySalesRepo()
	ctx := context.Background()
	day := time.Date(2026, 7, 29, 1, 0, 0, 0, time.UTC)

	n, err := r.UpsertDailySales(ctx, 1, 0, day, []SaleQty{{ProductID: 10, Qty: 5}, {ProductID: 20, Qty: 3}})
	if err != nil || n != 2 {
		t.Fatalf("first upsert: n=%d err=%v, want 2/nil", n, err)
	}

	// Re-running the SAME day overwrites the cell (idempotent on the unique key
	// (tenant, store, product, date)); it does NOT create a duplicate.
	n, err = r.UpsertDailySales(ctx, 1, 0, day, []SaleQty{{ProductID: 10, Qty: 9}})
	if err != nil || n != 1 {
		t.Fatalf("re-upsert: n=%d err=%v, want 1/nil", n, err)
	}
	if got := len(r.m); got != 2 {
		t.Fatalf("cells after re-upsert: got %d, want 2 (idempotent on unique key)", got)
	}

	last, ok, err := r.LastSaleDate(ctx, 1, 0, 10)
	if err != nil || !ok || !last.Equal(day) {
		t.Fatalf("LastSaleDate(10): last=%v ok=%v err=%v, want %v/true/nil", last, ok, err, day)
	}
}

func TestMemorySalesRepo_LastSaleDateMostRecent(t *testing.T) {
	r := NewMemorySalesRepo()
	ctx := context.Background()
	early := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	_, _ = r.UpsertDailySales(ctx, 1, 0, early, []SaleQty{{ProductID: 10, Qty: 1}})
	_, _ = r.UpsertDailySales(ctx, 1, 0, late, []SaleQty{{ProductID: 10, Qty: 2}})

	last, ok, err := r.LastSaleDate(ctx, 1, 0, 10)
	if err != nil || !ok || !last.Equal(late) {
		t.Fatalf("want most recent %v, got %v ok=%v err=%v", late, last, ok, err)
	}

	// No record → ok=false, not an error.
	if _, ok, _ := r.LastSaleDate(ctx, 1, 0, 999); ok {
		t.Fatal("unknown product should be ok=false")
	}
	// Tenant isolation: another tenant cannot see tenant 1's sales.
	if _, ok, _ := r.LastSaleDate(ctx, 2, 0, 10); ok {
		t.Fatal("cross-tenant read should be ok=false")
	}
}

func TestMemorySalesRepo_StoreScope(t *testing.T) {
	r := NewMemorySalesRepo()
	ctx := context.Background()
	day := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	_, _ = r.UpsertDailySales(ctx, 1, 7, day, []SaleQty{{ProductID: 10, Qty: 1}})

	if _, ok, _ := r.LastSaleDate(ctx, 1, 0, 10); !ok {
		t.Fatal("storeID=0 should span all stores")
	}
	if _, ok, _ := r.LastSaleDate(ctx, 1, 7, 10); !ok {
		t.Fatal("storeID=7 should see its own sale")
	}
	if _, ok, _ := r.LastSaleDate(ctx, 1, 8, 10); ok {
		t.Fatal("storeID=8 should not see store 7's sale")
	}
}

func TestMemorySalesRepo_NilSafe(t *testing.T) {
	var r *MemorySalesRepo
	if n, err := r.UpsertDailySales(context.Background(), 1, 0, time.Now(), []SaleQty{{ProductID: 10, Qty: 5}}); err != nil || n != 0 {
		t.Fatalf("nil upsert: n=%d err=%v, want 0/nil", n, err)
	}
	if _, ok, err := r.LastSaleDate(context.Background(), 1, 0, 10); err != nil || ok {
		t.Fatalf("nil LastSaleDate: ok=%v err=%v, want false/nil", ok, err)
	}
	if rows, err := r.ListDailySales(context.Background(), 1, 0, time.Now(), time.Now()); err != nil || len(rows) != 0 {
		t.Fatalf("nil ListDailySales: rows=%d err=%v, want 0/nil", len(rows), err)
	}
}

// TestMemorySalesRepo_ListDailySalesRange covers the M7-B range read: inclusive
// natural-day bounds, per-product-per-day rows, deterministic ordering, and the
// empty/inverted-range no-op contract.
func TestMemorySalesRepo_ListDailySalesRange(t *testing.T) {
	r := NewMemorySalesRepo()
	ctx := context.Background()
	d := func(day int) time.Time { return time.Date(2026, 7, day, 0, 0, 0, 0, time.UTC) }

	// Product 10 sold on days 20/22/24; product 20 sold on day 21; product 30
	// sold on day 29 (outside the [20,25] query window below).
	_, _ = r.UpsertDailySales(ctx, 1, 0, d(20), []SaleQty{{ProductID: 10, Qty: 5}})
	_, _ = r.UpsertDailySales(ctx, 1, 0, d(21), []SaleQty{{ProductID: 20, Qty: 3}})
	_, _ = r.UpsertDailySales(ctx, 1, 0, d(22), []SaleQty{{ProductID: 10, Qty: 2}})
	_, _ = r.UpsertDailySales(ctx, 1, 0, d(24), []SaleQty{{ProductID: 10, Qty: 1}})
	_, _ = r.UpsertDailySales(ctx, 1, 0, d(29), []SaleQty{{ProductID: 30, Qty: 9}})

	rows, err := r.ListDailySales(ctx, 1, 0, d(20), d(25))
	if err != nil {
		t.Fatalf("ListDailySales: %v", err)
	}
	// Expect product 10 (3 days) + product 20 (1 day), ordered product then day;
	// product 30 (day 29) is outside the window.
	if len(rows) != 4 {
		t.Fatalf("rows: got %d want 4 (%+v)", len(rows), rows)
	}
	if rows[0].ProductID != 10 || !rows[0].Day.Equal(d(20)) || rows[0].Qty != 5 {
		t.Errorf("rows[0]: %+v, want product 10 day 20 qty 5", rows[0])
	}
	if rows[1].ProductID != 10 || !rows[1].Day.Equal(d(22)) || rows[1].Qty != 2 {
		t.Errorf("rows[1]: %+v, want product 10 day 22 qty 2", rows[1])
	}
	if rows[2].ProductID != 10 || !rows[2].Day.Equal(d(24)) || rows[2].Qty != 1 {
		t.Errorf("rows[2]: %+v, want product 10 day 24 qty 1", rows[2])
	}
	if rows[3].ProductID != 20 || !rows[3].Day.Equal(d(21)) || rows[3].Qty != 3 {
		t.Errorf("rows[3]: %+v, want product 20 day 21 qty 3", rows[3])
	}

	// Inclusive bounds: a window that exactly matches one day returns just it.
	exact, _ := r.ListDailySales(ctx, 1, 0, d(21), d(21))
	if len(exact) != 1 || exact[0].ProductID != 20 {
		t.Errorf("exact-day window: %+v, want only product 20", exact)
	}

	// Inverted range → empty, not an error.
	if inv, err := r.ListDailySales(ctx, 1, 0, d(25), d(20)); err != nil || len(inv) != 0 {
		t.Errorf("inverted range: rows=%d err=%v, want 0/nil", len(inv), err)
	}

	// Tenant isolation: another tenant sees nothing.
	if cross, _ := r.ListDailySales(ctx, 2, 0, d(20), d(25)); len(cross) != 0 {
		t.Errorf("cross-tenant: rows=%d, want 0", len(cross))
	}
}

// TestMemorySalesRepo_ListDailySalesStoreScope covers the storeID filter on the
// range read (0 = all stores; a concrete id scopes to that store).
func TestMemorySalesRepo_ListDailySalesStoreScope(t *testing.T) {
	r := NewMemorySalesRepo()
	ctx := context.Background()
	day := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	_, _ = r.UpsertDailySales(ctx, 1, 7, day, []SaleQty{{ProductID: 10, Qty: 1}})

	if rows, _ := r.ListDailySales(ctx, 1, 0, from, to); len(rows) != 1 {
		t.Errorf("storeID=0 should span all stores, got %d rows", len(rows))
	}
	if rows, _ := r.ListDailySales(ctx, 1, 7, from, to); len(rows) != 1 {
		t.Errorf("storeID=7 should see its own sale, got %d rows", len(rows))
	}
	if rows, _ := r.ListDailySales(ctx, 1, 8, from, to); len(rows) != 0 {
		t.Errorf("storeID=8 should not see store 7's sale, got %d rows", len(rows))
	}
}

// fakeTagWriter is a hand-rolled TagWriter that records the tags written per
// product so the integration test can assert the tagging outcome.
type fakeTagWriter struct {
	calls map[int64][]string
	err   error
}

func (f *fakeTagWriter) SetProductTags(_ context.Context, _, productID int64, tags []string) error {
	if f.err != nil {
		return f.err
	}
	if f.calls == nil {
		f.calls = make(map[int64][]string)
	}
	f.calls[productID] = append([]string(nil), tags...)
	return nil
}

// TestComputeAndPersistTags_Integration runs the exact flow dataqualityTenant
// uses: persist the fused daily sales (MemorySalesRepo), derive
// DaysSinceLastSale from the most recent sale_date, ComputeTags, then write the
// changed tag set through the TagWriter port.
func TestComputeAndPersistTags_Integration(t *testing.T) {
	ctx := context.Background()
	sales := NewMemorySalesRepo()
	tw := &fakeTagWriter{}
	now := time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC)

	// Product 10 last sold 45 days ago → 滞销 (StaleDays=30). Product 20 sold
	// yesterday → fresh. Both are 高毛利 (price 200 / cost 100 = 50% >= 40).
	stale := now.AddDate(0, 0, -45)
	fresh := now.AddDate(0, 0, -1)
	if _, err := sales.UpsertDailySales(ctx, 1, 0, stale, []SaleQty{{ProductID: 10, Qty: 1}}); err != nil {
		t.Fatalf("upsert stale: %v", err)
	}
	if _, err := sales.UpsertDailySales(ctx, 1, 0, fresh, []SaleQty{{ProductID: 20, Qty: 4}}); err != nil {
		t.Fatalf("upsert fresh: %v", err)
	}

	rule := TagRule{NearingDays: 3, StaleDays: 30, HighMarginMinPct: 40}
	for _, p := range []struct{ id, cost, price int64 }{{10, 100, 200}, {20, 100, 200}} {
		view := TagProductView{ProductID: int(p.id), CostCents: p.cost, PriceCents: p.price}
		if last, ok, _ := sales.LastSaleDate(ctx, 1, 0, p.id); ok {
			view.DaysSinceLastSale = int(now.Sub(last).Hours() / 24)
		}
		tags := ComputeTags(view, rule, now)
		if len(tags) == 0 {
			continue
		}
		if err := tw.SetProductTags(ctx, 1, p.id, tags); err != nil {
			t.Fatalf("SetProductTags(%d): %v", p.id, err)
		}
	}

	// Product 10: 滞销 + 高毛利 (EarliestExpiry nil → no 临期).
	if got := tw.calls[10]; !reflect.DeepEqual(got, []string{TagStale, TagHighMargin}) {
		t.Fatalf("product 10 tags: got %v, want [滞销 高毛利]", got)
	}
	// Product 20: sold yesterday → only 高毛利.
	if got := tw.calls[20]; !reflect.DeepEqual(got, []string{TagHighMargin}) {
		t.Fatalf("product 20 tags: got %v, want [高毛利]", got)
	}
}
