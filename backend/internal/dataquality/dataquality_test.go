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

// ---- ExpiryLookuper port + EarliestExpiryFor (M7-A) ------------------------

// fakeExpiryLookuper is a hand-rolled fake implementing ExpiryLookuper. It
// records the tenantID/storeID it was called with so tests can assert the job
// passes them straight through, and returns a canned map or error.
type fakeExpiryLookuper struct {
	m   map[int64]time.Time
	err error

	gotTenantID int64
	gotStoreID  int64
	calls       int
}

func (f *fakeExpiryLookuper) EarliestExpiryByProduct(_ context.Context, tenantID, storeID int64) (map[int64]time.Time, error) {
	f.calls++
	f.gotTenantID = tenantID
	f.gotStoreID = storeID
	if f.err != nil {
		return nil, f.err
	}
	return f.m, nil
}

// The fake must satisfy the port (compile-time guard on the adapter contract).
var _ ExpiryLookuper = (*fakeExpiryLookuper)(nil)

func TestEarliestExpiryFor(t *testing.T) {
	soon := fixedNow.Add(2 * 24 * time.Hour)
	m := map[int64]time.Time{1: soon}

	// Present product -> its expiry, as a fresh copy (mutating the returned
	// pointer must NOT alias the map's stored value).
	got := EarliestExpiryFor(m, 1)
	if got == nil || !got.Equal(soon) {
		t.Fatalf("EarliestExpiryFor(1) = %v, want %v", got, soon)
	}
	*got = fixedNow // mutate the copy
	if !m[1].Equal(soon) {
		t.Fatalf("returned pointer aliased the map value: map[1]=%v", m[1])
	}

	// Absent product -> nil (临期 must not fire).
	if EarliestExpiryFor(m, 999) != nil {
		t.Fatalf("absent product must yield nil")
	}
	// nil map -> nil (nil-safe degradation).
	if EarliestExpiryFor(nil, 1) != nil {
		t.Fatalf("nil map must yield nil")
	}
	// empty map -> nil.
	if EarliestExpiryFor(map[int64]time.Time{}, 1) != nil {
		t.Fatalf("empty map must yield nil")
	}
}

// TestComputeTags_NearingFromLookuper proves the end-to-end 临期 path the M7-A
// port unlocks: a whole-tenant earliest-expiry map feeds EarliestExpiryFor, whose
// result drives ComputeTags. This is the exact wiring the BE-A-09 job performs.
func TestComputeTags_NearingFromLookuper(t *testing.T) {
	rule := TagRule{NearingDays: 3, StaleDays: 30, HighMarginMinPct: 40}
	f := &fakeExpiryLookuper{m: map[int64]time.Time{
		1: fixedNow.AddDate(0, 0, 2),  // within 3 days -> 临期
		2: fixedNow.AddDate(0, 0, 30), // beyond 3 days -> not 临期
	}}

	expiries, err := f.EarliestExpiryByProduct(context.Background(), 42, 0)
	if err != nil {
		t.Fatalf("EarliestExpiryByProduct: %v", err)
	}
	// Pass-through: the job's tenantID/storeID reach the lookuper untouched.
	if f.gotTenantID != 42 || f.gotStoreID != 0 {
		t.Fatalf("args not passed through: tenant=%d store=%d", f.gotTenantID, f.gotStoreID)
	}

	// Product 1 -> 临期 fires.
	v1 := TagProductView{ProductID: 1, EarliestExpiry: EarliestExpiryFor(expiries, 1)}
	if got := ComputeTags(v1, rule, fixedNow); !reflect.DeepEqual(got, []string{TagNearingExpiry}) {
		t.Fatalf("product 1: got %v, want [临期]", got)
	}
	// Product 2 -> no tag (expiry beyond window).
	v2 := TagProductView{ProductID: 2, EarliestExpiry: EarliestExpiryFor(expiries, 2)}
	if got := ComputeTags(v2, rule, fixedNow); len(got) != 0 {
		t.Fatalf("product 2: got %v, want []", got)
	}
	// Product 3 -> absent from map -> EarliestExpiry nil -> no 临期.
	v3 := TagProductView{ProductID: 3, EarliestExpiry: EarliestExpiryFor(expiries, 3)}
	if got := ComputeTags(v3, rule, fixedNow); len(got) != 0 {
		t.Fatalf("product 3 (absent): got %v, want []", got)
	}
}
