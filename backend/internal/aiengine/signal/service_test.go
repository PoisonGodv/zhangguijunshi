package signal

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeLookuper implements Lookuper for tests; each path can be scripted with a
// value and/or an error.
type fakeLookuper struct {
	inv    []InventorySig
	mem    MemberStats
	txn    TxnSig
	invErr error
	memErr error
	txnErr error
}

func (f *fakeLookuper) Inventory(_ context.Context, _, _ int64) ([]InventorySig, error) {
	return f.inv, f.invErr
}
func (f *fakeLookuper) Members(_ context.Context, _ int64) (MemberStats, error) {
	return f.mem, f.memErr
}
func (f *fakeLookuper) DailyTxn(_ context.Context, _, _ int64, _ string) (TxnSig, error) {
	return f.txn, f.txnErr
}

func TestCollect_FakeLookuper_FieldMapping(t *testing.T) {
	fixed := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	lk := &fakeLookuper{
		inv: []InventorySig{
			{ProductID: 1001, Name: "阳光玫瑰葡萄", CostCents: 1200, PriceCents: 2580, Stock: 50, DaysToExpiry: 2, HighMargin: true},
			{ProductID: 1002, Name: "奶油草莓", CostCents: 800, PriceCents: 1990, Stock: 30, DaysToExpiry: 1},
		},
		mem: MemberStats{Total: 120, Sleep30: 18, HighValue: []int64{101, 102, 103}, BirthdayToday: []int64{}},
		txn: TxnSig{RevenueCents: 888800, OrderCount: 56, NewMembers: 3},
	}
	svc := NewCollector("fruit", "临沂", lk, WithNowFunc(func() time.Time { return fixed }))

	b, err := svc.Collect(context.Background(), 50021, 7)
	if err != nil {
		t.Fatalf("Collect err: %v", err)
	}
	if b == nil {
		t.Fatal("Collect returned nil Bundle")
	}

	// Identity / context fields.
	if b.TenantID != 50021 || b.StoreID != 7 {
		t.Errorf("ids: got tenant=%d store=%d", b.TenantID, b.StoreID)
	}
	if b.BusinessType != "fruit" || b.City != "临沂" {
		t.Errorf("ctx: got business=%q city=%q", b.BusinessType, b.City)
	}
	if !b.Date.Equal(fixed) {
		t.Errorf("date: got %v want %v", b.Date, fixed)
	}

	// Inventory mapping.
	if len(b.Inventory) != 2 {
		t.Fatalf("inventory len: got %d want 2", len(b.Inventory))
	}
	if b.Inventory[0].ProductID != 1001 || b.Inventory[0].CostCents != 1200 || b.Inventory[0].PriceCents != 2580 {
		t.Errorf("inv[0] mapping wrong: %+v", b.Inventory[0])
	}
	if b.Inventory[0].DaysToExpiry != 2 || !b.Inventory[0].HighMargin {
		t.Errorf("inv[0] flags wrong: %+v", b.Inventory[0])
	}

	// Members mapping.
	if b.Members.Total != 120 || b.Members.Sleep30 != 18 {
		t.Errorf("members scalar wrong: %+v", b.Members)
	}
	if len(b.Members.HighValue) != 3 || b.Members.HighValue[0] != 101 {
		t.Errorf("members high_value wrong: %+v", b.Members)
	}

	// DataQuality: all three paths present → 40+30+30 = 100.
	if b.DataQuality != 100 {
		t.Errorf("data_quality: got %d want 100", b.DataQuality)
	}
}

func TestCollect_EmptyLookuper_ZeroBundle(t *testing.T) {
	// Empty Lookuper: all paths return zero/empty + nil error.
	lk := &fakeLookuper{}
	svc := NewCollector("", "", lk) // business defaults to "fruit"

	b, err := svc.Collect(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Collect err: %v", err)
	}
	if b == nil {
		t.Fatal("Collect returned nil Bundle (must never be nil)")
	}
	if len(b.Inventory) != 0 {
		t.Errorf("empty lookuper: inventory len got %d want 0", len(b.Inventory))
	}
	if b.Members.Total != 0 || b.Members.Sleep30 != 0 || len(b.Members.HighValue) != 0 {
		t.Errorf("empty lookuper: members not zero: %+v", b.Members)
	}
	if b.DataQuality != 0 {
		t.Errorf("empty lookuper: data_quality got %d want 0 (cold-start)", b.DataQuality)
	}
	if b.BusinessType != "fruit" {
		t.Errorf("default business: got %q want fruit", b.BusinessType)
	}
}

func TestCollect_PathErrors_DoNotBlock(t *testing.T) {
	// Each path fails; Collect still returns a non-nil zero Bundle.
	lk := &fakeLookuper{
		invErr: errors.New("inventory boom"),
		memErr: errors.New("members boom"),
		txnErr: errors.New("txn boom"),
	}
	svc := NewCollector("fresh", "徐州", lk)

	b, err := svc.Collect(context.Background(), 50021, 1)
	if err != nil {
		t.Fatalf("Collect must not surface path errors: %v", err)
	}
	if b == nil {
		t.Fatal("nil Bundle")
	}
	if len(b.Inventory) != 0 || b.DataQuality != 0 {
		t.Errorf("failed paths should yield zero bundle: inv=%d dq=%d", len(b.Inventory), b.DataQuality)
	}
}

func TestCollect_NilLookuper_NeverNil(t *testing.T) {
	// Nil Lookuper must not panic; returns a zero Bundle for cold-start.
	svc := NewCollector("apparel", "杭州", nil)

	b, err := svc.Collect(context.Background(), 50021, 9)
	if err != nil {
		t.Fatalf("Collect err: %v", err)
	}
	if b == nil {
		t.Fatal("nil Lookuper must still yield non-nil Bundle")
	}
	if len(b.Inventory) != 0 {
		t.Errorf("nil lookuper: inventory len got %d want 0", len(b.Inventory))
	}
	// Weather/holiday mocks still populated.
	if b.Weather.Condition != "normal" || b.Holiday.Name != "无" {
		t.Errorf("mocks wrong: weather=%+v holiday=%+v", b.Weather, b.Holiday)
	}
}
