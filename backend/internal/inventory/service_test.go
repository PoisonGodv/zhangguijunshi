package inventory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// baseNow is the deterministic clock anchor. Each test derives per-batch
// timestamps from it so FIFO order is explicit and independent of wall time.
var baseNow = time.Date(2026, 7, 20, 9, 0, 0, 0, time.Local)

// clock is a mutable injected time source: each purchase advances it so every
// batch gets a strictly increasing CreatedAt (deterministic FIFO).
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: baseNow} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func ctxFor(tenantID, storeID, userID int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tenantID,
		StoreID:  storeID,
		UserID:   userID,
		Source:   multitenancy.SourceMerchant,
	})
}

// memLookuper is a thread-safe ProductLookuper that reports whether a product
// exists for a given tenant. It lets tests hand tenant A a product that only
// tenant B owns to prove the cross-tenant guard fires.
type memLookuper struct {
	// products maps productID -> owning tenantID.
	products map[int64]int64
	err      error // injected upstream failure
}

func (m *memLookuper) Exists(_ context.Context, tenantID, productID int64) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	owner, ok := m.products[productID]
	return ok && owner == tenantID, nil
}

// rejectingLookuper always denies stock: used to prove ConsumeFIFO can be
// blocked mid-flight by an interleaved PurchaseIn being refused.
type rejectingLookuper struct{}

func (rejectingLookuper) Exists(context.Context, int64, int64) (bool, error) { return false, nil }

// newService wires a fresh repo + service with a deterministic clock.
func newService(l ProductLookuper) (*Service, *MemoryBatchRepo, *clock) {
	repo := NewMemoryBatchRepo()
	clk := newClock()
	svc := New(repo, l, WithNowFunc(clk.now))
	return svc, repo, clk
}

func tenantLookuper() *memLookuper {
	// products 1,2 belong to tenant 100; product 9 belongs to tenant 200.
	return &memLookuper{products: map[int64]int64{1: 100, 2: 100, 9: 200}}
}

// purchase is a helper: stock-in qty units of productID for tenant 100 and
// advance the clock so the next batch is newer.
func purchase(t *testing.T, svc *Service, clk *clock, ctx context.Context, productID, qty int64, price int64) *Batch {
	t.Helper()
	b, err := svc.PurchaseIn(ctx, PurchaseInInput{ProductID: productID, Quantity: qty, InPriceCents: price})
	if err != nil {
		t.Fatalf("PurchaseIn(product=%d qty=%d): %v", productID, qty, err)
	}
	clk.advance(time.Minute) // next batch is strictly newer
	return b
}

func TestPurchaseIn_Success(t *testing.T) {
	svc, repo, _ := newService(tenantLookuper())
	ctx := ctxFor(100, 10, 7)
	exp := baseNow.AddDate(0, 1, 0)

	b, err := svc.PurchaseIn(ctx, PurchaseInInput{
		ProductID: 1, Quantity: 50, InPriceCents: 120, ExpiryDate: &exp, Note: "首批",
	})
	if err != nil {
		t.Fatalf("PurchaseIn: %v", err)
	}
	if b.ID == 0 || b.TenantID != 100 || b.StoreID != 10 || b.ProductID != 1 {
		t.Errorf("batch identity wrong: %+v", b)
	}
	if b.Quantity != 50 || b.InPriceCents != 120 {
		t.Errorf("batch qty/price = %d/%d", b.Quantity, b.InPriceCents)
	}
	if b.Status != BatchStatusActive {
		t.Errorf("Status = %q, want active", b.Status)
	}
	if b.ExpiryDate == nil || !b.ExpiryDate.Equal(exp) {
		t.Errorf("ExpiryDate = %v, want %v", b.ExpiryDate, exp)
	}

	// Exactly one purchase_in movement recorded, linked to the batch.
	mvs, err := repo.ListMovements(ctx, 100, MovementFilter{ProductID: 1, Type: MovementTypePurchaseIn})
	if err != nil {
		t.Fatal(err)
	}
	if len(mvs) != 1 {
		t.Fatalf("purchase_in movements = %d, want 1", len(mvs))
	}
	m := mvs[0]
	if m.BatchID != b.ID || m.Quantity != 50 || m.OperatorID != 7 || m.Note != "首批" {
		t.Errorf("movement wrong: %+v", m)
	}
}

func TestPurchaseIn_StoreFallsBackToCtx(t *testing.T) {
	svc, _, _ := newService(tenantLookuper())
	// ctx carries StoreID 33; PurchaseInInput.StoreID is zero -> uses ctx store.
	b, err := svc.PurchaseIn(ctxFor(100, 33, 7), PurchaseInInput{ProductID: 1, Quantity: 5, InPriceCents: 10})
	if err != nil {
		t.Fatal(err)
	}
	if b.StoreID != 33 {
		t.Errorf("StoreID = %d, want 33 (from ctx)", b.StoreID)
	}
}

func TestPurchaseIn_InvalidInputRejected(t *testing.T) {
	svc, _, _ := newService(tenantLookuper())
	ctx := ctxFor(100, 10, 7)
	for _, in := range []PurchaseInInput{
		{ProductID: 1, Quantity: 0, InPriceCents: 10},  // qty 0
		{ProductID: 1, Quantity: -3, InPriceCents: 10}, // negative qty
		{ProductID: 1, Quantity: 5, InPriceCents: -1},  // negative price
		{ProductID: 99, Quantity: 5, InPriceCents: 10}, // unknown product
		{ProductID: 9, Quantity: 5, InPriceCents: 10},  // product owned by tenant 200
	} {
		if _, err := svc.PurchaseIn(ctx, in); err != errcode.ErrParam {
			t.Errorf("PurchaseIn(%+v) err = %v, want ErrParam", in, err)
		}
	}
}

func TestPurchaseIn_LookuperFailureIsInternal(t *testing.T) {
	lk := tenantLookuper()
	lk.err = errors.New("catalog down")
	svc, _, _ := newService(lk)
	_, err := svc.PurchaseIn(ctxFor(100, 10, 7), PurchaseInInput{ProductID: 1, Quantity: 5, InPriceCents: 10})
	if err != errcode.ErrInternal {
		t.Fatalf("lookuper failure err = %v, want ErrInternal", err)
	}
}

func TestPurchaseIn_NoTenantRejected(t *testing.T) {
	svc, _, _ := newService(tenantLookuper())
	_, err := svc.PurchaseIn(context.Background(), PurchaseInInput{ProductID: 1, Quantity: 5})
	if !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant err = %v, want ErrNoTenant", err)
	}
}

func TestConsumeFIFO_OldestFirstAndRemainders(t *testing.T) {
	svc, repo, clk := newService(tenantLookuper())
	// Pin the repo clock to the service clock so batches get the same
	// deterministic, strictly-increasing CreatedAt as the injected now().
	defer setNowForTest(clk.now)()
	ctx := ctxFor(100, 10, 7)

	// Three batches, oldest first: 5, 10, 20.
	b1 := purchase(t, svc, clk, ctx, 1, 5, 100)
	b2 := purchase(t, svc, clk, ctx, 1, 10, 100)
	b3 := purchase(t, svc, clk, ctx, 1, 20, 100)

	// Consume 12 -> drains b1 (5), drains b2 (10 -> but only 7 needed), so b1=0,
	// b2=3, b3=20 untouched. FIFO must touch b1 then b2, never b3.
	updated, err := svc.ConsumeFIFO(ctx, 10, 1, 12, 555)
	if err != nil {
		t.Fatalf("ConsumeFIFO: %v", err)
	}
	if len(updated) != 2 {
		t.Fatalf("updated batches = %d, want 2 (b1,b2)", len(updated))
	}
	if updated[0].ID != b1.ID || updated[0].Quantity != 0 || updated[0].Status != BatchStatusCleared {
		t.Errorf("b1 after = %+v, want qty 0 cleared", updated[0])
	}
	if updated[1].ID != b2.ID || updated[1].Quantity != 3 || updated[1].Status != BatchStatusActive {
		t.Errorf("b2 after = %+v, want qty 3 active", updated[1])
	}

	// Persisted state matches; b3 untouched.
	if got, _ := repo.GetBatch(ctx, 100, b1.ID); got.Quantity != 0 || got.Status != BatchStatusCleared {
		t.Errorf("persisted b1 = %+v", got)
	}
	if got, _ := repo.GetBatch(ctx, 100, b2.ID); got.Quantity != 3 {
		t.Errorf("persisted b2 qty = %d, want 3", got.Quantity)
	}
	if got, _ := repo.GetBatch(ctx, 100, b3.ID); got.Quantity != 20 || got.Status != BatchStatusActive {
		t.Errorf("persisted b3 = %+v, want untouched qty 20", got)
	}

	// sale_out movements: one per touched batch, negative qty, linked to order.
	mvs, err := svc.ListMovements(ctx, MovementFilter{ProductID: 1, Type: MovementTypeSaleOut})
	if err != nil {
		t.Fatal(err)
	}
	if len(mvs) != 2 {
		t.Fatalf("sale_out movements = %d, want 2", len(mvs))
	}
	if mvs[0].BatchID != b1.ID || mvs[0].Quantity != -5 || mvs[0].RefOrderID != 555 {
		t.Errorf("sale_out[0] wrong: %+v", mvs[0])
	}
	if mvs[1].BatchID != b2.ID || mvs[1].Quantity != -7 || mvs[1].RefOrderID != 555 {
		t.Errorf("sale_out[1] wrong: %+v", mvs[1])
	}

	// Stock now = 3 + 20 = 23.
	stock, err := svc.Stock(ctx, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	if stock != 23 {
		t.Errorf("Stock = %d, want 23", stock)
	}
}

func TestConsumeFIFO_DrainAllThenStockZero(t *testing.T) {
	svc, _, clk := newService(tenantLookuper())
	ctx := ctxFor(100, 10, 7)
	purchase(t, svc, clk, ctx, 1, 4, 100)
	purchase(t, svc, clk, ctx, 1, 6, 100)

	if _, err := svc.ConsumeFIFO(ctx, 10, 1, 10, 1); err != nil {
		t.Fatal(err)
	}
	stock, err := svc.Stock(ctx, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	if stock != 0 {
		t.Errorf("Stock = %d, want 0", stock)
	}
	// All lots cleared -> further FIFO list is empty.
	batches, err := svc.repo.ListActiveBatchesFIFO(ctx, 100, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 0 {
		t.Errorf("active batches after drain = %d, want 0", len(batches))
	}
}

func TestConsumeFIFO_InsufficientNoPartialDeduction(t *testing.T) {
	svc, repo, clk := newService(tenantLookuper())
	ctx := ctxFor(100, 10, 7)
	b1 := purchase(t, svc, clk, ctx, 1, 5, 100)
	b2 := purchase(t, svc, clk, ctx, 1, 4, 100) // total 9

	// Request 10 -> insufficient. Must return ErrInsufficientStock and leave ALL
	// batches untouched (no partial deduction), and append NO sale_out movement.
	_, err := svc.ConsumeFIFO(ctx, 10, 1, 10, 777)
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("err = %v, want ErrInsufficientStock", err)
	}
	if got, _ := repo.GetBatch(ctx, 100, b1.ID); got.Quantity != 5 || got.Status != BatchStatusActive {
		t.Errorf("b1 mutated after insufficient: %+v", got)
	}
	if got, _ := repo.GetBatch(ctx, 100, b2.ID); got.Quantity != 4 || got.Status != BatchStatusActive {
		t.Errorf("b2 mutated after insufficient: %+v", got)
	}
	mvs, _ := svc.ListMovements(ctx, MovementFilter{ProductID: 1, Type: MovementTypeSaleOut})
	if len(mvs) != 0 {
		t.Errorf("sale_out movements after insufficient = %d, want 0", len(mvs))
	}
	stock, _ := svc.Stock(ctx, 10, 1)
	if stock != 9 {
		t.Errorf("Stock after insufficient = %d, want 9 (unchanged)", stock)
	}
}

func TestConsumeFIFO_InvalidQtyRejected(t *testing.T) {
	svc, _, clk := newService(tenantLookuper())
	ctx := ctxFor(100, 10, 7)
	purchase(t, svc, clk, ctx, 1, 5, 100)
	for _, qty := range []int64{0, -2} {
		if _, err := svc.ConsumeFIFO(ctx, 10, 1, qty, 1); err != errcode.ErrParam {
			t.Errorf("qty=%d err = %v, want ErrParam", qty, err)
		}
	}
}

func TestConsumeFIFO_NoTenantRejected(t *testing.T) {
	svc, _, _ := newService(tenantLookuper())
	if _, err := svc.ConsumeFIFO(context.Background(), 10, 1, 1, 1); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant err = %v, want ErrNoTenant", err)
	}
}

func TestCrossTenantIsolation(t *testing.T) {
	svc, repo, clk := newService(tenantLookuper())
	ctxA := ctxFor(100, 10, 7)
	ctxB := ctxFor(200, 20, 8)

	// Tenant A stocks product 1 (lookuper says it belongs to 100).
	b := purchase(t, svc, clk, ctxA, 1, 30, 100)

	// Tenant B cannot read tenant A's batch.
	if _, err := repo.GetBatch(ctxB, 200, b.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant GetBatch err = %v, want ErrNotFound", err)
	}
	// Tenant B sees zero stock for that product in its own store scope.
	if stock, _ := svc.Stock(ctxB, 10, 1); stock != 0 {
		t.Errorf("tenant B Stock = %d, want 0", stock)
	}
	// Tenant B consuming product 1 hits empty stock -> ErrInsufficientStock,
	// and tenant A's batches are untouched.
	if _, err := svc.ConsumeFIFO(ctxB, 10, 1, 1, 1); !errors.Is(err, ErrInsufficientStock) {
		t.Errorf("tenant B ConsumeFIFO err = %v, want ErrInsufficientStock", err)
	}
	if got, _ := repo.GetBatch(ctxA, 100, b.ID); got.Quantity != 30 {
		t.Errorf("tenant A batch mutated by tenant B: qty=%d", got.Quantity)
	}
	// Tenant B's movement list does not include tenant A's purchase_in.
	mvsB, _ := svc.ListMovements(ctxB, MovementFilter{ProductID: 1})
	if len(mvsB) != 0 {
		t.Errorf("tenant B sees %d movements, want 0", len(mvsB))
	}
	// Tenant A still sees its own.
	mvsA, _ := svc.ListMovements(ctxA, MovementFilter{ProductID: 1})
	if len(mvsA) != 1 {
		t.Errorf("tenant A sees %d movements, want 1", len(mvsA))
	}
}

func TestStock_MultipleBatchesSum(t *testing.T) {
	svc, _, clk := newService(tenantLookuper())
	ctx := ctxFor(100, 10, 7)
	purchase(t, svc, clk, ctx, 1, 7, 100)
	purchase(t, svc, clk, ctx, 1, 8, 100)
	purchase(t, svc, clk, ctx, 2, 100, 100) // different product must not count

	stock, err := svc.Stock(ctx, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	if stock != 15 {
		t.Errorf("Stock(product1) = %d, want 15", stock)
	}
}

func TestListMovements_Filters(t *testing.T) {
	svc, _, clk := newService(tenantLookuper())
	ctx := ctxFor(100, 10, 7)
	// Product 1 in store 10; product 2 in store 11 (explicit store).
	purchase(t, svc, clk, ctx, 1, 5, 100)
	if _, err := svc.PurchaseIn(ctx, PurchaseInInput{StoreID: 11, ProductID: 2, Quantity: 9, InPriceCents: 100}); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	if _, err := svc.ConsumeFIFO(ctx, 10, 1, 2, 42); err != nil {
		t.Fatal(err)
	}

	// By type.
	inMvs, _ := svc.ListMovements(ctx, MovementFilter{Type: MovementTypePurchaseIn})
	if len(inMvs) != 2 {
		t.Errorf("purchase_in count = %d, want 2", len(inMvs))
	}
	outMvs, _ := svc.ListMovements(ctx, MovementFilter{Type: MovementTypeSaleOut})
	if len(outMvs) != 1 || outMvs[0].Quantity != -2 || outMvs[0].RefOrderID != 42 {
		t.Errorf("sale_out filter wrong: %+v", outMvs)
	}
	// By store.
	store11, _ := svc.ListMovements(ctx, MovementFilter{StoreID: 11})
	if len(store11) != 1 || store11[0].ProductID != 2 {
		t.Errorf("store=11 filter wrong: %+v", store11)
	}
	// By product.
	prod1, _ := svc.ListMovements(ctx, MovementFilter{ProductID: 1})
	if len(prod1) != 2 { // 1 purchase_in + 1 sale_out
		t.Errorf("product=1 filter count = %d, want 2", len(prod1))
	}
	// By natural day: all activity is on baseNow's day; a different day is empty.
	today := baseNow
	dayMvs, _ := svc.ListMovements(ctx, MovementFilter{Day: &today})
	if len(dayMvs) != 3 {
		t.Errorf("day=today count = %d, want 3", len(dayMvs))
	}
	yesterday := baseNow.AddDate(0, 0, -1)
	empty, _ := svc.ListMovements(ctx, MovementFilter{Day: &yesterday})
	if len(empty) != 0 {
		t.Errorf("day=yesterday count = %d, want 0", len(empty))
	}
	// Limit/Offset pagination over all 3 movements.
	page1, _ := svc.ListMovements(ctx, MovementFilter{Limit: 2})
	if len(page1) != 2 {
		t.Errorf("limit=2 returned %d", len(page1))
	}
	page2, _ := svc.ListMovements(ctx, MovementFilter{Limit: 2, Offset: 2})
	if len(page2) != 1 {
		t.Errorf("offset=2 limit=2 returned %d, want 1", len(page2))
	}
}

func TestMemoryRepo_DefensiveCopy(t *testing.T) {
	repo := NewMemoryBatchRepo()
	ctx := context.Background()
	exp := baseNow
	in := &Batch{
		TenantID: 100, StoreID: 10, ProductID: 1,
		Quantity: 10, InPriceCents: 50, ExpiryDate: &exp, Status: BatchStatusActive,
	}
	created, err := repo.CreateBatch(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate the input, the returned record, and the shared expiry pointer.
	in.Quantity = 999
	created.Quantity = 888
	*in.ExpiryDate = baseNow.AddDate(1, 0, 0)

	got, err := repo.GetBatch(ctx, 100, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Quantity != 10 {
		t.Errorf("stored quantity mutated: %d", got.Quantity)
	}
	if !got.ExpiryDate.Equal(exp) && got.ExpiryDate.Equal(*in.ExpiryDate) {
		t.Errorf("stored expiry aliased the caller's pointer: %v", got.ExpiryDate)
	}
}

func TestMemoryRepo_UpdateBatchCrossTenant(t *testing.T) {
	repo := NewMemoryBatchRepo()
	ctx := context.Background()
	created, _ := repo.CreateBatch(ctx, &Batch{
		TenantID: 100, StoreID: 10, ProductID: 1, Quantity: 5, Status: BatchStatusActive,
	})
	// Same batch id, but claimed under tenant 200 -> ErrNotFound.
	created.TenantID = 200
	if _, err := repo.UpdateBatch(ctx, created); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant UpdateBatch err = %v, want ErrNotFound", err)
	}
}

func TestService_ConcurrentConsumeNoOversell(t *testing.T) {
	// Seed stock directly via the repo (bypassing the service's lookuper) so the
	// service under test can be given a lookuper that always denies — this blocks
	// any interleaved PurchaseIn during the concurrent ConsumeFIFO storm, making
	// the no-oversell assertion deterministic. A start barrier maximises overlap.
	repo := NewMemoryBatchRepo()
	ctx := ctxFor(100, 10, 7)
	if _, err := repo.CreateBatch(ctx, &Batch{
		TenantID: 100, StoreID: 10, ProductID: 1,
		Quantity: 10, InPriceCents: 100, Status: BatchStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	svc := New(repo, rejectingLookuper{}, WithNowFunc(newClock().now))

	// 16 goroutines each try to consume 1 (=16 wanted, 10 available). Exactly 10
	// succeed and 6 get ErrInsufficientStock; final stock is exactly 0 (no
	// oversell). Because the in-memory repo consumes atomically under one lock,
	// this holds on every run.
	const workers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all workers together to force contention
			_, err := svc.ConsumeFIFO(ctx, 10, 1, 1, 1)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	succeeded, insufficient := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrInsufficientStock):
			insufficient++
		}
	}
	if succeeded != 10 || insufficient != 6 {
		t.Errorf("succeeded=%d insufficient=%d, want 10/6", succeeded, insufficient)
	}
	stock, _ := svc.Stock(ctx, 10, 1)
	if stock != 0 {
		t.Errorf("final stock = %d, want 0 (no oversell)", stock)
	}
}
