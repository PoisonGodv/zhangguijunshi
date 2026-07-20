package catalog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

const (
	tenantA = 50021
	tenantB = 99999
	store1  = 1
)

// ctxFor builds a tenant-scoped context.
func ctxFor(tenantID, storeID int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tenantID,
		StoreID:  storeID,
		Source:   multitenancy.SourceMerchant,
	})
}

func newSvc() *Service { return NewService(NewMemoryProductRepo()) }

// --- CRUD 闭环 ---------------------------------------------------------------

func TestCreateGetUpdatePrice_ClosedLoop(t *testing.T) {
	svc := newSvc()
	ctx := ctxFor(tenantA, store1)

	p, err := svc.Create(ctx, CreateInput{
		Barcode:    "6900001",
		Name:       "农夫山泉",
		Category:   "饮料",
		Unit:       "瓶",
		CostCents:  100,
		PriceCents: 200,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.ID == 0 || p.TenantID != tenantA || p.StoreID != store1 {
		t.Fatalf("bad persisted product: %+v", p)
	}
	if p.Status != "active" {
		t.Fatalf("expected default status active, got %q", p.Status)
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatal("timestamps not stamped")
	}

	got, err := svc.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "农夫山泉" || got.PriceCents != 200 || got.CostCents != 100 {
		t.Fatalf("GetByID mismatch: %+v", got)
	}

	up, err := svc.UpdatePrice(ctx, p.ID, 120, 180)
	if err != nil {
		t.Fatalf("UpdatePrice: %v", err)
	}
	if up.CostCents != 120 || up.PriceCents != 180 {
		t.Fatalf("UpdatePrice mismatch: %+v", up)
	}
	// Re-read to confirm persistence.
	again, err := svc.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if again.PriceCents != 180 {
		t.Fatalf("price not persisted: %+v", again)
	}
}

// --- 条码查 ------------------------------------------------------------------

func TestGetByBarcode(t *testing.T) {
	svc := newSvc()
	ctx := ctxFor(tenantA, store1)

	p, err := svc.Create(ctx, CreateInput{
		Barcode: "6901234567890", Name: "可口可乐", Unit: "瓶",
		CostCents: 150, PriceCents: 300,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := svc.GetByBarcode(ctx, "6901234567890")
	if err != nil {
		t.Fatalf("GetByBarcode: %v", err)
	}
	if got.ID != p.ID {
		t.Fatalf("barcode hit wrong product: %+v", got)
	}

	if _, err := svc.GetByBarcode(ctx, "no-such-barcode"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// --- 串户 --------------------------------------------------------------------

func TestCrossTenant_Isolated(t *testing.T) {
	svc := newSvc()
	ctxA := ctxFor(tenantA, store1)
	ctxB := ctxFor(tenantB, store1)

	p, err := svc.Create(ctxA, CreateInput{
		Barcode: "6900002", Name: "康师傅", Unit: "桶",
		CostCents: 200, PriceCents: 400,
	})
	if err != nil {
		t.Fatalf("Create(A): %v", err)
	}

	if _, err := svc.GetByID(ctxB, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B read tenant A product by id: got %v", err)
	}
	if _, err := svc.GetByBarcode(ctxB, "6900002"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B read tenant A product by barcode: got %v", err)
	}
	list, err := svc.List(ctxB, ProductFilter{})
	if err != nil {
		t.Fatalf("List(B): %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("tenant B listed %d of tenant A products", len(list))
	}
	// UpdatePrice cross-tenant must also fail.
	if _, err := svc.UpdatePrice(ctxB, p.ID, 1, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant B update tenant A product: got %v", err)
	}
}

// --- 价格校验 ----------------------------------------------------------------

func TestPriceValidation_Rejected(t *testing.T) {
	svc := newSvc()
	ctx := ctxFor(tenantA, store1)

	for _, tc := range []struct {
		name  string
		cost  int64
		price int64
	}{
		{"price<cost", 300, 200},
		{"negative cost", -1, 100},
		{"negative price", 0, -5},
	} {
		if _, err := svc.Create(ctx, CreateInput{
			Name: "X" + tc.name, Unit: "个", CostCents: tc.cost, PriceCents: tc.price,
		}); !errors.Is(err, errcode.ErrParam) {
			t.Fatalf("Create %s: expected ErrParam, got %v", tc.name, err)
		}
	}

	p, err := svc.Create(ctx, CreateInput{Name: "正价", Unit: "个", CostCents: 10, PriceCents: 20})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.UpdatePrice(ctx, p.ID, 50, 40); !errors.Is(err, errcode.ErrParam) {
		t.Fatalf("UpdatePrice price<cost: expected ErrParam, got %v", err)
	}
}

// --- 缺省值 ------------------------------------------------------------------

func TestDefaults(t *testing.T) {
	svc := newSvc()
	ctx := ctxFor(tenantA, store1)

	p, err := svc.Create(ctx, CreateInput{Name: "散装糖", CostCents: 0, PriceCents: 0})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.Unit != "个" {
		t.Fatalf("expected default unit 个, got %q", p.Unit)
	}
	if p.Status != "active" {
		t.Fatalf("expected default status active, got %q", p.Status)
	}
}

func TestCreate_NameRequired(t *testing.T) {
	svc := newSvc()
	ctx := ctxFor(tenantA, store1)
	if _, err := svc.Create(ctx, CreateInput{Name: "", Unit: "个"}); !errors.Is(err, errcode.ErrParam) {
		t.Fatalf("expected ErrParam for empty name, got %v", err)
	}
}

func TestNoTenant_Rejected(t *testing.T) {
	svc := newSvc()
	if _, err := svc.Create(context.Background(), CreateInput{Name: "x", Unit: "个"}); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("expected ErrNoTenant, got %v", err)
	}
}

// --- List 过滤 ---------------------------------------------------------------

func TestList_Filters(t *testing.T) {
	svc := newSvc()
	ctx := ctxFor(tenantA, store1)
	ctxStore2 := ctxFor(tenantA, 2)

	mk := func(c context.Context, in CreateInput) {
		if _, err := svc.Create(c, in); err != nil {
			t.Fatalf("Create(%s): %v", in.Name, err)
		}
	}
	mk(ctx, CreateInput{Barcode: "B001", Name: "可口可乐", Category: "饮料", Unit: "瓶", CostCents: 1, PriceCents: 2})
	mk(ctx, CreateInput{Barcode: "B002", Name: "雪碧", Category: "饮料", Unit: "瓶", CostCents: 1, PriceCents: 2})
	mk(ctx, CreateInput{Barcode: "B003", Name: "乐事薯片", Category: "零食", Unit: "包", CostCents: 1, PriceCents: 2})
	mk(ctxStore2, CreateInput{Barcode: "B004", Name: "可口可乐", Category: "饮料", Unit: "瓶", CostCents: 1, PriceCents: 2})

	// StoreID filter: store1 has 3, store2 has 1.
	if got, _ := svc.List(ctx, ProductFilter{StoreID: store1}); len(got) != 3 {
		t.Fatalf("store1 list: want 3 got %d", len(got))
	}
	if got, _ := svc.List(ctxStore2, ProductFilter{StoreID: 2}); len(got) != 1 {
		t.Fatalf("store2 list: want 1 got %d", len(got))
	}

	// Query by name substring.
	if got, _ := svc.List(ctx, ProductFilter{StoreID: store1, Query: "可乐"}); len(got) != 1 {
		t.Fatalf("query 可乐: want 1 got %d", len(got))
	}
	// Query by barcode substring.
	if got, _ := svc.List(ctx, ProductFilter{StoreID: store1, Query: "B003"}); len(got) != 1 {
		t.Fatalf("query B003: want 1 got %d", len(got))
	}
	// Category filter.
	if got, _ := svc.List(ctx, ProductFilter{StoreID: store1, Category: "饮料"}); len(got) != 2 {
		t.Fatalf("category 饮料: want 2 got %d", len(got))
	}

	// Pagination.
	page1, _ := svc.List(ctx, ProductFilter{StoreID: store1, Limit: 2, Offset: 0})
	page2, _ := svc.List(ctx, ProductFilter{StoreID: store1, Limit: 2, Offset: 2})
	if len(page1) != 2 || len(page2) != 1 {
		t.Fatalf("pagination: want 2+1 got %d+%d", len(page1), len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Fatal("pagination returned overlapping ids")
	}
}

// --- 唯一性 ------------------------------------------------------------------

func TestUniqueness(t *testing.T) {
	svc := newSvc()
	ctx := ctxFor(tenantA, store1)

	if _, err := svc.Create(ctx, CreateInput{Barcode: "DUP", Name: "可乐", Unit: "瓶", CostCents: 1, PriceCents: 2}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Duplicate barcode in same store -> conflict mapped to ErrParam.
	if _, err := svc.Create(ctx, CreateInput{Barcode: "DUP", Name: "雪碧", Unit: "瓶", CostCents: 1, PriceCents: 2}); !errors.Is(err, errcode.ErrParam) {
		t.Fatalf("dup barcode: expected ErrParam, got %v", err)
	}
	// Duplicate (name, unit) in same store -> conflict.
	if _, err := svc.Create(ctx, CreateInput{Barcode: "OTHER", Name: "可乐", Unit: "瓶", CostCents: 1, PriceCents: 2}); !errors.Is(err, errcode.ErrParam) {
		t.Fatalf("dup name+unit: expected ErrParam, got %v", err)
	}
	// Same barcode in a DIFFERENT tenant is allowed (isolation scope).
	if _, err := svc.Create(ctxFor(tenantB, store1), CreateInput{Barcode: "DUP", Name: "可乐", Unit: "瓶", CostCents: 1, PriceCents: 2}); err != nil {
		t.Fatalf("same barcode other tenant should succeed: %v", err)
	}
}

// --- 防御性拷贝 + 并发 ---------------------------------------------------------

func TestDefensiveCopy(t *testing.T) {
	repo := NewMemoryProductRepo()
	ctx := context.Background()
	p, err := repo.Create(ctx, &Product{TenantID: tenantA, StoreID: store1, Barcode: "C1", Name: "X", Unit: "个", Tags: []string{"a"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	p.Name = "MUTATED"
	p.Tags[0] = "MUTATED"
	got, err := repo.GetByID(ctx, tenantA, p.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "X" || got.Tags[0] != "a" {
		t.Fatalf("stored record mutated through returned pointer: %+v", got)
	}
}

func TestConcurrentAccess(t *testing.T) {
	repo := NewMemoryProductRepo()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = repo.Create(ctx, &Product{TenantID: tenantA, StoreID: store1, Barcode: fmt.Sprintf("BC%03d", i), Name: fmt.Sprintf("N%03d", i), Unit: "个"})
			_, _ = repo.List(ctx, tenantA, ProductFilter{})
		}(i)
	}
	wg.Wait()
	list, err := repo.List(ctx, tenantA, ProductFilter{Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 50 {
		t.Fatalf("want 50 products, got %d", len(list))
	}
}
