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
