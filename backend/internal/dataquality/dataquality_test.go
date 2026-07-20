package dataquality

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// fakeSalesReverser is a hand-rolled fake implementing SalesReverser. It records
// the tenantID/storeID/day it was called with so tests can assert the job passes
// them straight through, and returns a canned result or error.
type fakeSalesReverser struct {
	rows []SaleQty
	err  error

	gotTenantID int64
	gotStoreID  int64
	gotDay      time.Time
	calls       int
}

func (f *fakeSalesReverser) ListSaleOut(_ context.Context, tenantID, storeID int64, day time.Time) ([]SaleQty, error) {
	f.calls++
	f.gotTenantID = tenantID
	f.gotStoreID = storeID
	f.gotDay = day
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func TestReverseDailySales_AggregatesAndSorts(t *testing.T) {
	day := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	f := &fakeSalesReverser{rows: []SaleQty{
		{ProductID: 30, Qty: 5},
		{ProductID: 10, Qty: 2},
		{ProductID: 30, Qty: 7}, // duplicate ProductID 30 → merged
		{ProductID: 20, Qty: 1},
		{ProductID: 10, Qty: 3}, // duplicate ProductID 10 → merged
	}}

	got, err := ReverseDailySales(context.Background(), f, 42, 9, day)
	if err != nil {
		t.Fatalf("ReverseDailySales: %v", err)
	}

	want := []SaleQty{
		{ProductID: 10, Qty: 5}, // 2+3
		{ProductID: 20, Qty: 1},
		{ProductID: 30, Qty: 12}, // 5+7
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// Pass-through: tenantID/storeID/day reach the reverser untouched.
	if f.calls != 1 {
		t.Fatalf("ListSaleOut called %d times, want 1", f.calls)
	}
	if f.gotTenantID != 42 || f.gotStoreID != 9 || !f.gotDay.Equal(day) {
		t.Fatalf("args not passed through: tenant=%d store=%d day=%v", f.gotTenantID, f.gotStoreID, f.gotDay)
	}
}

func TestReverseDailySales_Empty(t *testing.T) {
	f := &fakeSalesReverser{rows: nil}
	got, err := ReverseDailySales(context.Background(), f, 1, 1, time.Now())
	if err != nil {
		t.Fatalf("ReverseDailySales: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d rows, want 0", len(got))
	}
}

func TestReverseDailySales_PropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	f := &fakeSalesReverser{err: sentinel}
	if _, err := ReverseDailySales(context.Background(), f, 1, 1, time.Now()); !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want wrapped %v", err, sentinel)
	}
}

// ---- ComputeTags -----------------------------------------------------------

var fixedNow = time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

func ptr(t time.Time) *time.Time { return &t }

func TestComputeTags_NearingExpiry(t *testing.T) {
	rule := TagRule{NearingDays: 7, StaleDays: 30, HighMarginMinPct: 50}

	// Within window → tagged.
	v := TagProductView{ProductID: 1, EarliestExpiry: ptr(fixedNow.Add(3 * 24 * time.Hour))}
	if got := ComputeTags(v, rule, fixedNow); !reflect.DeepEqual(got, []string{TagNearingExpiry}) {
		t.Fatalf("within window: got %v", got)
	}

	// Exactly N days away → boundary hits (<= N).
	v.EarliestExpiry = ptr(fixedNow.Add(7 * 24 * time.Hour))
	if got := ComputeTags(v, rule, fixedNow); !reflect.DeepEqual(got, []string{TagNearingExpiry}) {
		t.Fatalf("exactly N days: got %v, want [临期]", got)
	}

	// Already expired → still 临期 (diff <= N).
	v.EarliestExpiry = ptr(fixedNow.Add(-2 * 24 * time.Hour))
	if got := ComputeTags(v, rule, fixedNow); !reflect.DeepEqual(got, []string{TagNearingExpiry}) {
		t.Fatalf("already expired: got %v, want [临期]", got)
	}

	// Outside window → not tagged.
	v.EarliestExpiry = ptr(fixedNow.Add(8 * 24 * time.Hour))
	if got := ComputeTags(v, rule, fixedNow); len(got) != 0 {
		t.Fatalf("outside window: got %v, want []", got)
	}

	// nil expiry → never 临期.
	v.EarliestExpiry = nil
	if got := ComputeTags(v, rule, fixedNow); len(got) != 0 {
		t.Fatalf("nil expiry: got %v, want []", got)
	}
}

func TestComputeTags_Stale(t *testing.T) {
	rule := TagRule{NearingDays: 7, StaleDays: 30, HighMarginMinPct: 50}

	v := TagProductView{ProductID: 1, DaysSinceLastSale: 30} // exactly N → boundary hits
	if got := ComputeTags(v, rule, fixedNow); !reflect.DeepEqual(got, []string{TagStale}) {
		t.Fatalf("exactly StaleDays: got %v, want [滞销]", got)
	}

	v.DaysSinceLastSale = 45
	if got := ComputeTags(v, rule, fixedNow); !reflect.DeepEqual(got, []string{TagStale}) {
		t.Fatalf("beyond StaleDays: got %v, want [滞销]", got)
	}

	v.DaysSinceLastSale = 29 // below threshold
	if got := ComputeTags(v, rule, fixedNow); len(got) != 0 {
		t.Fatalf("below StaleDays: got %v, want []", got)
	}
}

func TestComputeTags_HighMargin(t *testing.T) {
	rule := TagRule{NearingDays: 7, StaleDays: 30, HighMarginMinPct: 50}

	// price=200 cost=100 → margin 50% → boundary hits.
	v := TagProductView{ProductID: 1, CostCents: 100, PriceCents: 200}
	if got := ComputeTags(v, rule, fixedNow); !reflect.DeepEqual(got, []string{TagHighMargin}) {
		t.Fatalf("margin 50%%: got %v, want [高毛利]", got)
	}

	// price=300 cost=100 → margin 66% → tagged.
	v = TagProductView{ProductID: 1, CostCents: 100, PriceCents: 300}
	if got := ComputeTags(v, rule, fixedNow); !reflect.DeepEqual(got, []string{TagHighMargin}) {
		t.Fatalf("margin 66%%: got %v, want [高毛利]", got)
	}

	// price=200 cost=120 → margin 40% < 50 → not tagged.
	v = TagProductView{ProductID: 1, CostCents: 120, PriceCents: 200}
	if got := ComputeTags(v, rule, fixedNow); len(got) != 0 {
		t.Fatalf("margin 40%%: got %v, want []", got)
	}

	// price=0 → never 高毛利 (guards division by zero).
	v = TagProductView{ProductID: 1, CostCents: 0, PriceCents: 0}
	if got := ComputeTags(v, rule, fixedNow); len(got) != 0 {
		t.Fatalf("price=0: got %v, want []", got)
	}
}

func TestComputeTags_CombinationAndOrder(t *testing.T) {
	rule := TagRule{NearingDays: 7, StaleDays: 30, HighMarginMinPct: 50}

	// Hits all three; result must be de-duplicated and in fixed order
	// 临期/滞销/高毛利.
	v := TagProductView{
		ProductID:         1,
		EarliestExpiry:    ptr(fixedNow.Add(1 * 24 * time.Hour)), // 临期
		DaysSinceLastSale: 90,                                    // 滞销
		CostCents:         10,
		PriceCents:        100, // 90% margin → 高毛利
	}
	want := []string{TagNearingExpiry, TagStale, TagHighMargin}
	if got := ComputeTags(v, rule, fixedNow); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestComputeTags_NoneHit(t *testing.T) {
	rule := TagRule{NearingDays: 7, StaleDays: 30, HighMarginMinPct: 50}
	v := TagProductView{
		ProductID:         1,
		EarliestExpiry:    ptr(fixedNow.Add(60 * 24 * time.Hour)),
		DaysSinceLastSale: 3,
		CostCents:         180,
		PriceCents:        200, // 10% margin
	}
	if got := ComputeTags(v, rule, fixedNow); len(got) != 0 {
		t.Fatalf("got %v, want []", got)
	}
}
