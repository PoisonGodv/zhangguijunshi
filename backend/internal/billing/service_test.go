package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// fixedNow is the deterministic clock used across all tests.
var fixedNow = time.Date(2026, 7, 20, 15, 4, 5, 0, time.Local)

func ctxFor(tenantID, storeID, userID int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tenantID,
		StoreID:  storeID,
		UserID:   userID,
		Source:   multitenancy.SourceMerchant,
	})
}

// fakePricer is an in-memory ProductPricer keyed by product id. It does not
// itself enforce tenancy (the service must), so tests can hand the service a
// product belonging to another tenant to prove the cross-tenant guard fires.
type fakePricer struct {
	byID map[int64]*PricedProduct
}

func (f *fakePricer) GetByID(_ context.Context, _ int64, id int64) (*PricedProduct, error) {
	p, ok := f.byID[id]
	if !ok {
		return nil, errors.New("fake pricer: not found")
	}
	cp := *p
	return &cp, nil
}

func newService(pricer ProductPricer) (*Service, *MemoryOrderRepo) {
	repo := NewMemoryOrderRepo()
	svc := New(repo, pricer,
		WithNowFunc(func() time.Time { return fixedNow }),
		WithOrderNoFunc(func(tenantID int64, now time.Time) string {
			return "ZG-TEST-" + now.Format("20060102")
		}),
	)
	return svc, repo
}

func activeProduct(id, tenantID, storeID int64, name, unit string, price, cost int64) *PricedProduct {
	return &PricedProduct{
		ID: id, TenantID: tenantID, StoreID: storeID,
		Name: name, Unit: unit,
		PriceCents: price, CostCents: cost,
		Status: productStatusActive,
	}
}

func TestCreateOrder_Success(t *testing.T) {
	pricer := &fakePricer{byID: map[int64]*PricedProduct{
		1: activeProduct(1, 100, 10, "可乐", "瓶", 300, 200),
		2: activeProduct(2, 100, 10, "薯片", "包", 500, 350),
	}}
	svc, _ := newService(pricer)

	order, err := svc.CreateOrder(ctxFor(100, 10, 7), CreateOrderInput{
		StoreID: 10,
		Items: []CreateOrderItem{
			{ProductID: 1, Qty: 2},
			{ProductID: 2, Qty: 3},
		},
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	// total = 2*300 + 3*500 = 600 + 1500 = 2100
	if order.TotalCents != 2100 {
		t.Errorf("TotalCents = %d, want 2100", order.TotalCents)
	}
	if order.PaidCents != 2100 || order.DiscountCents != 0 {
		t.Errorf("Paid/Discount = %d/%d, want 2100/0", order.PaidCents, order.DiscountCents)
	}
	if order.Status != orderStatusPaid {
		t.Errorf("Status = %q, want paid", order.Status)
	}
	if order.PaidAt == nil || !order.PaidAt.Equal(fixedNow) {
		t.Errorf("PaidAt = %v, want %v", order.PaidAt, fixedNow)
	}
	if order.Channel != defaultChannel || order.PaymentMethod != defaultPaymentMethod {
		t.Errorf("defaults Channel/Payment = %q/%q", order.Channel, order.PaymentMethod)
	}
	if order.OrderNo != "ZG-TEST-20260720" {
		t.Errorf("OrderNo = %q", order.OrderNo)
	}
	if order.TenantID != 100 || order.StoreID != 10 || order.OperatorID != 7 {
		t.Errorf("tenant/store/operator = %d/%d/%d", order.TenantID, order.StoreID, order.OperatorID)
	}

	// Snapshot fidelity: name/unit/price/cost frozen from the pricer.
	if len(order.Items) != 2 {
		t.Fatalf("Items len = %d", len(order.Items))
	}
	it0 := order.Items[0]
	if it0.NameSnapshot != "可乐" || it0.Unit != "瓶" || it0.PriceCents != 300 || it0.CostCents != 200 || it0.Qty != 2 {
		t.Errorf("item0 snapshot wrong: %+v", it0)
	}
}

func TestCreateOrder_EmptyItemsRejected(t *testing.T) {
	svc, _ := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	if _, err := svc.CreateOrder(ctxFor(100, 10, 7), CreateOrderInput{StoreID: 10}); err != errcode.ErrParam {
		t.Fatalf("empty items err = %v, want ErrParam", err)
	}
}

func TestCreateOrder_NonPositiveQtyRejected(t *testing.T) {
	pricer := &fakePricer{byID: map[int64]*PricedProduct{
		1: activeProduct(1, 100, 10, "可乐", "瓶", 300, 200),
	}}
	svc, _ := newService(pricer)
	for _, qty := range []int64{0, -1} {
		_, err := svc.CreateOrder(ctxFor(100, 10, 7), CreateOrderInput{
			StoreID: 10,
			Items:   []CreateOrderItem{{ProductID: 1, Qty: qty}},
		})
		if err != errcode.ErrParam {
			t.Errorf("qty=%d err = %v, want ErrParam", qty, err)
		}
	}
}

func TestCreateOrder_NonActiveProductRejected(t *testing.T) {
	p := activeProduct(1, 100, 10, "下架品", "个", 300, 200)
	p.Status = "archived"
	pricer := &fakePricer{byID: map[int64]*PricedProduct{1: p}}
	svc, _ := newService(pricer)
	_, err := svc.CreateOrder(ctxFor(100, 10, 7), CreateOrderInput{
		StoreID: 10,
		Items:   []CreateOrderItem{{ProductID: 1, Qty: 1}},
	})
	if err != errcode.ErrParam {
		t.Fatalf("archived product err = %v, want ErrParam", err)
	}
}

func TestCreateOrder_CrossTenantProductRejected(t *testing.T) {
	// Product 9 belongs to tenant B(200); tenant A(100) tries to bill it.
	pricer := &fakePricer{byID: map[int64]*PricedProduct{
		9: activeProduct(9, 200, 20, "别家商品", "个", 100, 50),
	}}
	svc, _ := newService(pricer)
	_, err := svc.CreateOrder(ctxFor(100, 10, 7), CreateOrderInput{
		StoreID: 10,
		Items:   []CreateOrderItem{{ProductID: 9, Qty: 1}},
	})
	if err != errcode.ErrForbidden {
		t.Fatalf("cross-tenant product err = %v, want ErrForbidden", err)
	}
}

func TestCreateOrder_NoTenantRejected(t *testing.T) {
	svc, _ := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	_, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		StoreID: 10,
		Items:   []CreateOrderItem{{ProductID: 1, Qty: 1}},
	})
	if !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant err = %v, want ErrNoTenant", err)
	}
}

func TestGetOrder_CrossTenantIsolated(t *testing.T) {
	pricer := &fakePricer{byID: map[int64]*PricedProduct{
		1: activeProduct(1, 100, 10, "可乐", "瓶", 300, 200),
	}}
	svc, _ := newService(pricer)

	order, err := svc.CreateOrder(ctxFor(100, 10, 7), CreateOrderInput{
		StoreID: 10,
		Items:   []CreateOrderItem{{ProductID: 1, Qty: 1}},
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	// Same tenant reads it back.
	got, err := svc.GetOrder(ctxFor(100, 10, 7), order.ID)
	if err != nil || got.ID != order.ID {
		t.Fatalf("same-tenant GetOrder err=%v got=%v", err, got)
	}
	// Tenant B must NOT see tenant A's order.
	if _, err := svc.GetOrder(ctxFor(200, 20, 8), order.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant GetOrder err = %v, want ErrNotFound", err)
	}
}

func TestDailySummary_Numbers(t *testing.T) {
	pricer := &fakePricer{byID: map[int64]*PricedProduct{
		1: activeProduct(1, 100, 10, "可乐", "瓶", 300, 200),
		2: activeProduct(2, 100, 10, "薯片", "包", 500, 350),
	}}
	svc, _ := newService(pricer)
	ctx := ctxFor(100, 10, 7)

	// Order 1: 2 可乐 + 1 薯片 -> total 1100, cost 2*200+1*350=750
	if _, err := svc.CreateOrder(ctx, CreateOrderInput{StoreID: 10, Items: []CreateOrderItem{
		{ProductID: 1, Qty: 2}, {ProductID: 2, Qty: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	// Order 2: 3 可乐 -> total 900, cost 600
	if _, err := svc.CreateOrder(ctx, CreateOrderInput{StoreID: 10, Items: []CreateOrderItem{
		{ProductID: 1, Qty: 3},
	}}); err != nil {
		t.Fatal(err)
	}

	sum, err := svc.DailySummary(ctx, 10, fixedNow)
	if err != nil {
		t.Fatalf("DailySummary: %v", err)
	}
	if sum.Date != "2026-07-20" {
		t.Errorf("Date = %q", sum.Date)
	}
	if sum.OrderCount != 2 {
		t.Errorf("OrderCount = %d, want 2", sum.OrderCount)
	}
	if sum.GrossCents != 2000 || sum.NetCents != 2000 {
		t.Errorf("Gross/Net = %d/%d, want 2000/2000", sum.GrossCents, sum.NetCents)
	}
	if sum.CostCents != 1350 {
		t.Errorf("CostCents = %d, want 1350", sum.CostCents)
	}
	if sum.ProfitCents != 650 {
		t.Errorf("ProfitCents = %d, want 650 (net-cost)", sum.ProfitCents)
	}

	// TopProducts: 可乐 qty 5 first, 薯片 qty 1 second.
	if len(sum.TopProducts) != 2 {
		t.Fatalf("TopProducts len = %d", len(sum.TopProducts))
	}
	if sum.TopProducts[0].ProductID != 1 || sum.TopProducts[0].Qty != 5 {
		t.Errorf("top[0] = %+v, want product 1 qty 5", sum.TopProducts[0])
	}
	if sum.TopProducts[1].ProductID != 2 || sum.TopProducts[1].Qty != 1 {
		t.Errorf("top[1] = %+v, want product 2 qty 1", sum.TopProducts[1])
	}
}

func TestDailySummary_TopProductsCappedAtFive(t *testing.T) {
	pricer := &fakePricer{byID: map[int64]*PricedProduct{}}
	// 7 distinct active products; product i sells qty (10-i) so ordering is stable.
	items := make([]CreateOrderItem, 0, 7)
	for i := int64(1); i <= 7; i++ {
		pricer.byID[i] = activeProduct(i, 100, 10, "品"+string(rune('A'+i-1)), "个", 100, 10)
		items = append(items, CreateOrderItem{ProductID: i, Qty: 10 - i})
	}
	svc, _ := newService(pricer)
	ctx := ctxFor(100, 10, 7)
	if _, err := svc.CreateOrder(ctx, CreateOrderInput{StoreID: 10, Items: items}); err != nil {
		t.Fatal(err)
	}
	sum, err := svc.DailySummary(ctx, 10, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.TopProducts) != maxTopProducts {
		t.Fatalf("TopProducts len = %d, want %d", len(sum.TopProducts), maxTopProducts)
	}
	// Descending qty: 9,8,7,6,5 for products 1..5.
	for idx, tp := range sum.TopProducts {
		wantQty := int64(9 - idx)
		if tp.Qty != wantQty {
			t.Errorf("top[%d].Qty = %d, want %d", idx, tp.Qty, wantQty)
		}
	}
}

func TestDailySummary_DateFilterAndTenantIsolation(t *testing.T) {
	pricer := &fakePricer{byID: map[int64]*PricedProduct{
		1: activeProduct(1, 100, 10, "可乐", "瓶", 300, 200),
	}}
	svc, repo := newService(pricer)
	ctx := ctxFor(100, 10, 7)
	if _, err := svc.CreateOrder(ctx, CreateOrderInput{StoreID: 10, Items: []CreateOrderItem{{ProductID: 1, Qty: 1}}}); err != nil {
		t.Fatal(err)
	}

	// A different natural day yields an empty summary.
	yesterday := fixedNow.AddDate(0, 0, -1)
	sum, err := svc.DailySummary(ctx, 10, yesterday)
	if err != nil {
		t.Fatal(err)
	}
	if sum.OrderCount != 0 || sum.GrossCents != 0 || len(sum.TopProducts) != 0 {
		t.Errorf("yesterday summary not empty: %+v", sum)
	}

	// Tenant B sees nothing on the real day either.
	sumB, err := svc.DailySummary(ctxFor(200, 10, 8), 10, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if sumB.OrderCount != 0 {
		t.Errorf("tenant B OrderCount = %d, want 0", sumB.OrderCount)
	}
	_ = repo
}

func TestMemoryOrderRepo_DefensiveCopy(t *testing.T) {
	repo := NewMemoryOrderRepo()
	in := &Order{
		TenantID: 100, StoreID: 10, Status: orderStatusPaid,
		TotalCents: 100, PaidCents: 100,
		Items: []OrderItem{{ProductID: 1, Qty: 1, PriceCents: 100, CostCents: 50}},
	}
	created, err := repo.Create(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the input and the returned record must not corrupt the store.
	in.Items[0].Qty = 999
	created.Items[0].Qty = 888
	created.TotalCents = 777

	got, err := repo.GetByID(context.Background(), 100, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Items[0].Qty != 1 || got.TotalCents != 100 {
		t.Errorf("stored record was mutated: qty=%d total=%d", got.Items[0].Qty, got.TotalCents)
	}
}

func TestDefaultOrderNoFunc(t *testing.T) {
	n1 := DefaultOrderNoFunc(100, fixedNow)
	n2 := DefaultOrderNoFunc(100, fixedNow)
	if n1 == n2 {
		t.Errorf("DefaultOrderNoFunc produced duplicate %q", n1)
	}
	if got := n1[:2]; got != "ZG" {
		t.Errorf("OrderNo prefix = %q, want ZG", got)
	}
}

// seedPaidOrderWithMember writes a PAID order for tenantID+memberID directly
// into repo (bypassing CreateOrder's tenant-from-ctx path) so a test can
// place orders in arbitrary tenants and members. PaidAt is set to paidAt.
func seedPaidOrderWithMember(repo *MemoryOrderRepo, tenantID, memberID int64, total int64, paidAt time.Time) {
	o := &Order{
		TenantID:   tenantID,
		MemberID:   memberID,
		Status:     orderStatusPaid,
		TotalCents: total,
		PaidCents:  total,
		PaidAt:     &paidAt,
	}
	if _, err := repo.Create(context.Background(), o); err != nil {
		panic(err)
	}
}

// seedOrderAt writes a PAID order for tenantID+memberID with a controlled
// CreatedAt so ListByMember ordering is deterministic. Items are populated to
// prove the deep-copied Items travel with the order.
func seedOrderAt(repo *MemoryOrderRepo, tenantID, memberID, total int64, createdAt time.Time) {
	o := &Order{
		TenantID:   tenantID,
		MemberID:   memberID,
		Status:     orderStatusPaid,
		TotalCents: total,
		PaidCents:  total,
		PaidAt:     &createdAt,
		CreatedAt:  createdAt,
		Items:      []OrderItem{{ProductID: 1, NameSnapshot: "品", Unit: "个", Qty: 1, PriceCents: total, CostCents: total / 2}},
	}
	if _, err := repo.Create(context.Background(), o); err != nil {
		panic(err)
	}
}

func TestListByMember_TenantAndMemberScoped(t *testing.T) {
	svc, repo := newService(&fakePricer{byID: map[int64]*PricedProduct{}})

	// Tenant A member 7 has two orders; member 8 has one; a walk-in
	// (MemberID=0) order must NOT appear in any member list.
	seedPaidOrderWithMember(repo, 100, 7, 300, fixedNow)
	seedPaidOrderWithMember(repo, 100, 7, 600, fixedNow)
	seedPaidOrderWithMember(repo, 100, 8, 900, fixedNow)
	seedPaidOrderWithMember(repo, 100, 0, 9999, fixedNow) // 散客, never listed
	// Tenant B, SAME member id 7 — must be invisible to tenant A.
	seedPaidOrderWithMember(repo, 200, 7, 7777, fixedNow)

	got, err := svc.ListByMember(ctxFor(100, 10, 7), 7)
	if err != nil {
		t.Fatalf("ListByMember: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("member 7 orders = %d, want 2", len(got))
	}
	for _, o := range got {
		if o.TenantID != 100 || o.MemberID != 7 {
			t.Errorf("leaked foreign/walk-in order: %+v", o)
		}
	}

	// Member 8 sees exactly one.
	got8, err := svc.ListByMember(ctxFor(100, 10, 7), 8)
	if err != nil {
		t.Fatalf("ListByMember m8: %v", err)
	}
	if len(got8) != 1 || got8[0].TotalCents != 900 {
		t.Errorf("member 8 = %+v, want 1 order total 900", got8)
	}

	// Unknown member → empty (non-nil) slice.
	gotEmpty, err := svc.ListByMember(ctxFor(100, 10, 7), 999)
	if err != nil {
		t.Fatalf("ListByMember unknown: %v", err)
	}
	if gotEmpty == nil || len(gotEmpty) != 0 {
		t.Errorf("unknown member orders = %#v, want non-nil empty", gotEmpty)
	}
}

func TestListByMember_CrossTenantIsolated(t *testing.T) {
	svc, repo := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	// Same member id 7 in two tenants; each tenant must see ONLY its own order.
	seedPaidOrderWithMember(repo, 100, 7, 1000, fixedNow)
	seedPaidOrderWithMember(repo, 200, 7, 2000, fixedNow)

	a, err := svc.ListByMember(ctxFor(100, 10, 7), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || a[0].TotalCents != 1000 || a[0].TenantID != 100 {
		t.Errorf("tenant A member 7 = %+v, want 1 order total 1000 tenant 100", a)
	}
	b, err := svc.ListByMember(ctxFor(200, 10, 7), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 1 || b[0].TotalCents != 2000 || b[0].TenantID != 200 {
		t.Errorf("tenant B member 7 = %+v, want 1 order total 2000 tenant 200", b)
	}
}

func TestListByMember_DescOrderAndItems(t *testing.T) {
	svc, repo := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	t1 := fixedNow
	t2 := fixedNow.Add(time.Minute)
	t3 := fixedNow.Add(2 * time.Minute)
	seedOrderAt(repo, 100, 7, 100, t1)
	seedOrderAt(repo, 100, 7, 200, t2)
	seedOrderAt(repo, 100, 7, 300, t3)

	got, err := svc.ListByMember(ctxFor(100, 10, 7), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// newest-first (created_at DESC): 300, 200, 100.
	if got[0].TotalCents != 300 || got[1].TotalCents != 200 || got[2].TotalCents != 100 {
		t.Errorf("not DESC by created_at: %+v", []int64{got[0].TotalCents, got[1].TotalCents, got[2].TotalCents})
	}
	// Items travel with the order (defensive copy).
	if len(got[0].Items) != 1 || got[0].Items[0].PriceCents != 300 {
		t.Errorf("items wrong on newest order: %+v", got[0].Items)
	}
}

func TestListByMember_NoTenant(t *testing.T) {
	svc, _ := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	_, err := svc.ListByMember(context.Background(), 7)
	if !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant err = %v, want ErrNoTenant", err)
	}
}

func TestOrderTotalByMembers_Numbers(t *testing.T) {
	svc, repo := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	ctx := ctxFor(100, 10, 7)

	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.Local)

	// Members 1 and 2 each have paid orders inside the window.
	seedPaidOrderWithMember(repo, 100, 1, 1000, fixedNow)
	seedPaidOrderWithMember(repo, 100, 2, 500, fixedNow)
	seedPaidOrderWithMember(repo, 100, 1, 250, fixedNow) // member 1 twice -> 1250 total
	// Member 3 outside the window -> excluded.
	seedPaidOrderWithMember(repo, 100, 3, 9999, time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local))

	got, err := svc.OrderTotalByMembers(ctx, []int64{1, 2, 3}, since, until)
	if err != nil {
		t.Fatalf("OrderTotalByMembers: %v", err)
	}
	if want := int64(1750); got != want { // 1000+500+250
		t.Errorf("total = %d, want %d", got, want)
	}

	// Subsetting members still works: only member 1 -> 1250.
	got, err = svc.OrderTotalByMembers(ctx, []int64{1}, since, until)
	if err != nil {
		t.Fatalf("OrderTotalByMembers subset: %v", err)
	}
	if want := int64(1250); got != want {
		t.Errorf("subset total = %d, want %d", got, want)
	}
}

func TestOrderTotalByMembers_CrossTenantExcluded(t *testing.T) {
	svc, repo := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	ctx := ctxFor(100, 10, 7)

	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.Local)

	// Tenant A member 1 + a tenant B order for the SAME member id must not be
	// counted when tenant A asks.
	seedPaidOrderWithMember(repo, 100, 1, 1000, fixedNow)
	seedPaidOrderWithMember(repo, 200, 1, 9999, fixedNow) // different tenant

	got, err := svc.OrderTotalByMembers(ctx, []int64{1}, since, until)
	if err != nil {
		t.Fatalf("OrderTotalByMembers: %v", err)
	}
	if want := int64(1000); got != want {
		t.Errorf("total = %d, want %d (tenant B order leaked)", got, want)
	}
}

func TestOrderTotalByMembers_NonPaidExcluded(t *testing.T) {
	svc, repo := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	ctx := ctxFor(100, 10, 7)

	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.Local)

	// A pending order (same member, same window) must not add to total.
	pending := &Order{
		TenantID:   100,
		MemberID:   1,
		Status:     "pending",
		TotalCents: 9999,
		PaidAt:     &fixedNow,
	}
	if _, err := repo.Create(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	seedPaidOrderWithMember(repo, 100, 1, 1000, fixedNow)

	got, err := svc.OrderTotalByMembers(ctx, []int64{1}, since, until)
	if err != nil {
		t.Fatalf("OrderTotalByMembers: %v", err)
	}
	if want := int64(1000); got != want {
		t.Errorf("total = %d, want %d (pending leaked)", got, want)
	}
}

func TestOrderTotalByMembers_EmptyAndReversedRange(t *testing.T) {
	svc, repo := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	ctx := ctxFor(100, 10, 7)

	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.Local)
	seedPaidOrderWithMember(repo, 100, 1, 1000, fixedNow)

	// Empty memberIDs -> 0.
	got, err := svc.OrderTotalByMembers(ctx, nil, since, until)
	if err != nil || got != 0 {
		t.Errorf("empty memberIDs: got=%d err=%v, want 0/nil", got, err)
	}
	// since > until -> 0.
	got, err = svc.OrderTotalByMembers(ctx, []int64{1}, until, since)
	if err != nil || got != 0 {
		t.Errorf("reversed range: got=%d err=%v, want 0/nil", got, err)
	}
}

func TestOrderTotalByMembers_NoTenant(t *testing.T) {
	svc, _ := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.Local)
	_, err := svc.OrderTotalByMembers(context.Background(), []int64{1}, since, until)
	if !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant err = %v, want ErrNoTenant", err)
	}
}

func TestRepurchaseMembers_Count(t *testing.T) {
	svc, repo := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	ctx := ctxFor(100, 10, 7)

	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.Local)

	// Members 1 and 2 have paid orders; member 3 does not; member 4 has a
	// pending order (not a repurchase). Member 1 has TWO paid orders -> still
	// counts once (de-duped).
	seedPaidOrderWithMember(repo, 100, 1, 1000, fixedNow)
	seedPaidOrderWithMember(repo, 100, 1, 500, fixedNow) // dup -> counts once
	seedPaidOrderWithMember(repo, 100, 2, 200, fixedNow)
	pending := &Order{TenantID: 100, MemberID: 4, Status: "pending", TotalCents: 100, PaidAt: &fixedNow}
	if _, err := repo.Create(context.Background(), pending); err != nil {
		t.Fatal(err)
	}

	got, err := svc.RepurchaseMembers(ctx, []int64{1, 2, 3, 4}, since, until)
	if err != nil {
		t.Fatalf("RepurchaseMembers: %v", err)
	}
	if want := 2; got != want { // members 1 and 2 only
		t.Errorf("count = %d, want %d", got, want)
	}
}

func TestRepurchaseMembers_CrossTenantExcluded(t *testing.T) {
	svc, repo := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	ctx := ctxFor(100, 10, 7)

	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.Local)

	// Same member id, two tenants; tenant A must see only its own repurchaser.
	seedPaidOrderWithMember(repo, 100, 1, 1000, fixedNow)
	seedPaidOrderWithMember(repo, 200, 1, 1000, fixedNow) // tenant B

	got, err := svc.RepurchaseMembers(ctx, []int64{1}, since, until)
	if err != nil {
		t.Fatalf("RepurchaseMembers: %v", err)
	}
	if want := 1; got != want {
		t.Errorf("count = %d, want %d (tenant B leaked)", got, want)
	}
}

func TestRepurchaseMembers_RangeExcludesOutside(t *testing.T) {
	svc, repo := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	ctx := ctxFor(100, 10, 7)

	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.Local)

	// Member 1 paid before the window -> not a repurchaser in this window.
	seedPaidOrderWithMember(repo, 100, 1, 1000, time.Date(2026, 6, 1, 0, 0, 0, 0, time.Local))
	// Member 2 paid exactly on since boundary -> in range (inclusive).
	seedPaidOrderWithMember(repo, 100, 2, 100, since)

	got, err := svc.RepurchaseMembers(ctx, []int64{1, 2}, since, until)
	if err != nil {
		t.Fatalf("RepurchaseMembers: %v", err)
	}
	if want := 1; got != want {
		t.Errorf("count = %d, want %d", got, want)
	}
}

func TestRepurchaseMembers_EmptyAndReversedRange(t *testing.T) {
	svc, repo := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	ctx := ctxFor(100, 10, 7)

	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.Local)
	seedPaidOrderWithMember(repo, 100, 1, 1000, fixedNow)

	got, err := svc.RepurchaseMembers(ctx, nil, since, until)
	if err != nil || got != 0 {
		t.Errorf("empty memberIDs: got=%d err=%v, want 0/nil", got, err)
	}
	got, err = svc.RepurchaseMembers(ctx, []int64{1}, until, since)
	if err != nil || got != 0 {
		t.Errorf("reversed range: got=%d err=%v, want 0/nil", got, err)
	}
}

func TestRepurchaseMembers_NoTenant(t *testing.T) {
	svc, _ := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.Local)
	_, err := svc.RepurchaseMembers(context.Background(), []int64{1}, since, until)
	if !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant err = %v, want ErrNoTenant", err)
	}
}

// ===== M4-S1 (BE-A-10): pending orders + MarkOrderPaid =====

// paidAt is the deterministic payment moment for MarkOrderPaid tests, kept
// distinct from fixedNow so we can assert the field was actually written.
var paidAt = time.Date(2026, 7, 20, 16, 0, 0, 0, time.Local)

// stockCall records one ConsumeFIFO invocation.
type stockCall struct {
	storeID, productID, qty, refOrderID int64
}

// recordingStock is a StockConsumer that records every ConsumeFIFO call so
// tests can assert whether the pending vs paid path deducted stock (and how
// many times — idempotency must not double-deduct).
type recordingStock struct {
	calls []stockCall
}

func (r *recordingStock) ConsumeFIFO(_ context.Context, storeID, productID, qty, refOrderID int64) error {
	r.calls = append(r.calls, stockCall{storeID, productID, qty, refOrderID})
	return nil
}

// newServiceWithStock is newService plus a wired StockConsumer.
func newServiceWithStock(pricer ProductPricer, stock StockConsumer) (*Service, *MemoryOrderRepo) {
	repo := NewMemoryOrderRepo()
	svc := New(repo, pricer,
		WithNowFunc(func() time.Time { return fixedNow }),
		WithOrderNoFunc(func(tenantID int64, now time.Time) string {
			return "ZG-TEST-" + now.Format("20060102")
		}),
		WithStockConsumer(stock),
	)
	return svc, repo
}

func TestCreatePendingOrder_Success(t *testing.T) {
	pricer := &fakePricer{byID: map[int64]*PricedProduct{
		1: activeProduct(1, 100, 10, "可乐", "瓶", 300, 200),
		2: activeProduct(2, 100, 10, "薯片", "包", 500, 350),
	}}
	svc, _ := newService(pricer)

	order, err := svc.CreatePendingOrder(ctxFor(100, 10, 7), CreateOrderInput{
		StoreID: 10,
		Items: []CreateOrderItem{
			{ProductID: 1, Qty: 2},
			{ProductID: 2, Qty: 3},
		},
	})
	if err != nil {
		t.Fatalf("CreatePendingOrder: %v", err)
	}

	// total = 2*300 + 3*500 = 2100, but unpaid.
	if order.TotalCents != 2100 {
		t.Errorf("TotalCents = %d, want 2100", order.TotalCents)
	}
	if order.PaidCents != 0 || order.DiscountCents != 0 {
		t.Errorf("Paid/Discount = %d/%d, want 0/0 (pending is unpaid)", order.PaidCents, order.DiscountCents)
	}
	if order.Status != orderStatusPending {
		t.Errorf("Status = %q, want pending", order.Status)
	}
	if order.PaidAt != nil {
		t.Errorf("PaidAt = %v, want nil for pending", order.PaidAt)
	}
	if order.PaymentRef != "" {
		t.Errorf("PaymentRef = %q, want empty for pending", order.PaymentRef)
	}
	if order.TenantID != 100 || order.StoreID != 10 || order.OperatorID != 7 {
		t.Errorf("tenant/store/operator = %d/%d/%d", order.TenantID, order.StoreID, order.OperatorID)
	}
	// Snapshot fidelity preserved (same as CreateOrder).
	if len(order.Items) != 2 || order.Items[0].PriceCents != 300 || order.Items[0].CostCents != 200 {
		t.Errorf("items snapshot wrong: %+v", order.Items)
	}
}

func TestCreatePendingOrder_StockNotDeducted(t *testing.T) {
	pricer := &fakePricer{byID: map[int64]*PricedProduct{
		1: activeProduct(1, 100, 10, "可乐", "瓶", 300, 200),
	}}
	stock := &recordingStock{}
	svc, _ := newServiceWithStock(pricer, stock)

	pending, err := svc.CreatePendingOrder(ctxFor(100, 10, 7), CreateOrderInput{
		StoreID: 10,
		Items:   []CreateOrderItem{{ProductID: 1, Qty: 2}},
	})
	if err != nil {
		t.Fatalf("CreatePendingOrder: %v", err)
	}
	// A pending order must NOT reserve inventory.
	if len(stock.calls) != 0 {
		t.Errorf("pending stage deducted stock %d times, want 0", len(stock.calls))
	}

	// Promoting to paid deducts exactly once per line.
	if _, err := svc.MarkOrderPaid(ctxFor(100, 10, 7), pending.ID, "wxpay-OTN-1", paidAt); err != nil {
		t.Fatalf("MarkOrderPaid: %v", err)
	}
	if len(stock.calls) != 1 {
		t.Fatalf("paid stage stock calls = %d, want 1", len(stock.calls))
	}
	c := stock.calls[0]
	if c.productID != 1 || c.qty != 2 || c.refOrderID != pending.ID || c.storeID != 10 {
		t.Errorf("stock call = %+v, want {product 1 qty 2 order %d store 10}", c, pending.ID)
	}
}

func TestMarkOrderPaid_PendingToPaid(t *testing.T) {
	pricer := &fakePricer{byID: map[int64]*PricedProduct{
		1: activeProduct(1, 100, 10, "可乐", "瓶", 300, 200),
	}}
	svc, _ := newService(pricer)

	pending, err := svc.CreatePendingOrder(ctxFor(100, 10, 7), CreateOrderInput{
		StoreID: 10,
		Items:   []CreateOrderItem{{ProductID: 1, Qty: 2}}, // total 600
	})
	if err != nil {
		t.Fatalf("CreatePendingOrder: %v", err)
	}

	got, err := svc.MarkOrderPaid(ctxFor(100, 10, 7), pending.ID, "wxpay-OTN-9", paidAt)
	if err != nil {
		t.Fatalf("MarkOrderPaid: %v", err)
	}
	if got.Status != orderStatusPaid {
		t.Errorf("Status = %q, want paid", got.Status)
	}
	if got.PaidCents != got.TotalCents || got.PaidCents != 600 {
		t.Errorf("PaidCents = %d, want total 600", got.PaidCents)
	}
	if got.PaymentRef != "wxpay-OTN-9" {
		t.Errorf("PaymentRef = %q, want wxpay-OTN-9", got.PaymentRef)
	}
	if got.PaidAt == nil || !got.PaidAt.Equal(paidAt) {
		t.Errorf("PaidAt = %v, want %v", got.PaidAt, paidAt)
	}

	// Persisted: a fresh GetOrder observes the paid state.
	reloaded, err := svc.GetOrder(ctxFor(100, 10, 7), pending.ID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if reloaded.Status != orderStatusPaid || reloaded.PaymentRef != "wxpay-OTN-9" || reloaded.PaidCents != 600 {
		t.Errorf("persisted order wrong: %+v", reloaded)
	}
}

func TestMarkOrderPaid_Idempotent(t *testing.T) {
	pricer := &fakePricer{byID: map[int64]*PricedProduct{
		1: activeProduct(1, 100, 10, "可乐", "瓶", 300, 200),
	}}
	stock := &recordingStock{}
	svc, _ := newServiceWithStock(pricer, stock)

	pending, err := svc.CreatePendingOrder(ctxFor(100, 10, 7), CreateOrderInput{
		StoreID: 10,
		Items:   []CreateOrderItem{{ProductID: 1, Qty: 1}},
	})
	if err != nil {
		t.Fatalf("CreatePendingOrder: %v", err)
	}

	first, err := svc.MarkOrderPaid(ctxFor(100, 10, 7), pending.ID, "ref-A", paidAt)
	if err != nil {
		t.Fatalf("first MarkOrderPaid: %v", err)
	}

	// A duplicate notify arrives with a different ref/time. It must be a no-op:
	// no error, no stock re-deduction, no PaymentRef overwrite.
	laterPaidAt := paidAt.Add(time.Hour)
	second, err := svc.MarkOrderPaid(ctxFor(100, 10, 7), pending.ID, "ref-B", laterPaidAt)
	if err != nil {
		t.Fatalf("second (idempotent) MarkOrderPaid: %v, want nil", err)
	}
	if second.Status != orderStatusPaid {
		t.Errorf("second status = %q, want paid", second.Status)
	}
	if second.PaymentRef != "ref-A" {
		t.Errorf("second PaymentRef = %q, want ref-A (must not be overwritten)", second.PaymentRef)
	}
	if second.PaidAt == nil || !second.PaidAt.Equal(paidAt) {
		t.Errorf("second PaidAt = %v, want original %v (must not be overwritten)", second.PaidAt, paidAt)
	}
	if len(stock.calls) != 1 {
		t.Errorf("stock calls = %d after duplicate notify, want 1 (no re-deduction)", len(stock.calls))
	}
	// Idempotent return value equals the first committed state.
	if first.PaymentRef != second.PaymentRef || first.PaidCents != second.PaidCents {
		t.Errorf("idempotent return diverged: first=%+v second=%+v", first, second)
	}
}

func TestMarkOrderPaid_StatusConflict(t *testing.T) {
	pricer := &fakePricer{byID: map[int64]*PricedProduct{
		1: activeProduct(1, 100, 10, "可乐", "瓶", 300, 200),
	}}
	svc, repo := newService(pricer)

	for _, status := range []string{"refunded", "voided", "weird-unknown"} {
		// Seed an order already past the payable state directly in the repo.
		seeded := &Order{
			TenantID:   100,
			StoreID:    10,
			Status:     status,
			TotalCents: 500,
			PaidCents:  500,
			Items:      []OrderItem{{ProductID: 1, Qty: 1, PriceCents: 500, CostCents: 200}},
		}
		created, err := repo.Create(context.Background(), seeded)
		if err != nil {
			t.Fatalf("seed %q: %v", status, err)
		}
		_, err = svc.MarkOrderPaid(ctxFor(100, 10, 7), created.ID, "ref", paidAt)
		if !errors.Is(err, ErrStatusConflict) {
			t.Errorf("status=%q err = %v, want ErrStatusConflict", status, err)
		}
		// State must be untouched on conflict.
		got, err := svc.GetOrder(ctxFor(100, 10, 7), created.ID)
		if err != nil {
			t.Fatalf("GetOrder: %v", err)
		}
		if got.Status != status {
			t.Errorf("status=%q was mutated to %q", status, got.Status)
		}
	}
}

func TestMarkOrderPaid_CrossTenantAndNotFound(t *testing.T) {
	pricer := &fakePricer{byID: map[int64]*PricedProduct{
		1: activeProduct(1, 100, 10, "可乐", "瓶", 300, 200),
	}}
	svc, _ := newService(pricer)

	pending, err := svc.CreatePendingOrder(ctxFor(100, 10, 7), CreateOrderInput{
		StoreID: 10,
		Items:   []CreateOrderItem{{ProductID: 1, Qty: 1}},
	})
	if err != nil {
		t.Fatalf("CreatePendingOrder: %v", err)
	}

	// Tenant B cannot pay tenant A's order — indistinguishable from absence.
	_, err = svc.MarkOrderPaid(ctxFor(200, 20, 8), pending.ID, "ref", paidAt)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant err = %v, want ErrNotFound", err)
	}
	// Tenant A's order is still pending (the cross-tenant call changed nothing).
	got, err := svc.GetOrder(ctxFor(100, 10, 7), pending.ID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got.Status != orderStatusPending {
		t.Errorf("after cross-tenant attempt, status = %q, want pending", got.Status)
	}

	// A non-existent id is also ErrNotFound (not a panic / not conflict).
	_, err = svc.MarkOrderPaid(ctxFor(100, 10, 7), 999999, "ref", paidAt)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing id err = %v, want ErrNotFound", err)
	}
}

func TestMarkOrderPaid_NoTenant(t *testing.T) {
	svc, _ := newService(&fakePricer{byID: map[int64]*PricedProduct{}})
	_, err := svc.MarkOrderPaid(context.Background(), 5001, "ref", paidAt)
	if !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant err = %v, want ErrNoTenant", err)
	}
}

// TestUpdateStatus_Repo exercises the repo method directly: paid fields are
// filled on status=paid, untouched on other statuses, and cross-tenant ids are
// rejected (ARCH-01 row isolation).
func TestUpdateStatus_Repo(t *testing.T) {
	repo := NewMemoryOrderRepo()
	pending := &Order{
		TenantID:   100,
		StoreID:    10,
		Status:     orderStatusPending,
		TotalCents: 800,
		Items:      []OrderItem{{ProductID: 1, Qty: 2, PriceCents: 400, CostCents: 150}},
	}
	created, err := repo.Create(context.Background(), pending)
	if err != nil {
		t.Fatal(err)
	}

	// pending → paid fills payment fields + bumps UpdatedAt to paidAt.
	upd, err := repo.UpdateStatus(context.Background(), 100, created.ID, orderStatusPaid, 800, &paidAt, "OTN-1")
	if err != nil {
		t.Fatalf("UpdateStatus paid: %v", err)
	}
	if upd.Status != orderStatusPaid || upd.PaidCents != 800 || upd.PaymentRef != "OTN-1" {
		t.Errorf("paid update wrong: %+v", upd)
	}
	if upd.PaidAt == nil || !upd.PaidAt.Equal(paidAt) || !upd.UpdatedAt.Equal(paidAt) {
		t.Errorf("PaidAt/UpdatedAt = %v/%v, want %v", upd.PaidAt, upd.UpdatedAt, paidAt)
	}

	// Cross-tenant UpdateStatus is rejected; the paid record is unchanged.
	if _, err := repo.UpdateStatus(context.Background(), 200, created.ID, orderStatusPaid, 1, &paidAt, "hack"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant err = %v, want ErrNotFound", err)
	}
	got, err := repo.GetByID(context.Background(), 100, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PaymentRef != "OTN-1" || got.PaidCents != 800 {
		t.Errorf("cross-tenant attempt mutated record: %+v", got)
	}

	// A non-paid target status must NOT touch the payment fields.
	voided, err := repo.UpdateStatus(context.Background(), 100, created.ID, "voided", 0, nil, "")
	if err != nil {
		t.Fatalf("UpdateStatus voided: %v", err)
	}
	if voided.Status != "voided" {
		t.Errorf("status = %q, want voided", voided.Status)
	}
	if voided.PaidCents != 800 || voided.PaymentRef != "OTN-1" || voided.PaidAt == nil {
		t.Errorf("non-paid target cleared payment fields: %+v", voided)
	}
}
