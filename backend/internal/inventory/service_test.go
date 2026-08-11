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

// --- ListNearingBatches (M3-S2) ---

// purchaseExpiry stocks productID with a batch expiring at exp.
func purchaseExpiry(t *testing.T, svc *Service, clk *clock, ctx context.Context, productID, qty int64, exp *time.Time) *Batch {
	t.Helper()
	b, err := svc.PurchaseIn(ctx, PurchaseInInput{ProductID: productID, Quantity: qty, InPriceCents: 100, ExpiryDate: exp})
	if err != nil {
		t.Fatalf("PurchaseIn(product=%d): %v", productID, err)
	}
	clk.advance(time.Minute)
	return b
}

func TestListNearingBatches_CrossProduct(t *testing.T) {
	svc, repo, clk := newService(tenantLookuper())
	defer setNowForTest(clk.now)()
	ctx := ctxFor(100, 10, 7)

	// Pin the repo clock to the service clock so "now" is deterministic.
	now := clk.now()
	within3 := now.AddDate(0, 0, 3)
	within10 := now.AddDate(0, 0, 10)
	far := now.AddDate(0, 0, 60)

	// Product 1: one batch within 3 days, one far out.
	b1 := purchaseExpiry(t, svc, clk, ctx, 1, 5, &within3)
	purchaseExpiry(t, svc, clk, ctx, 1, 5, &far)
	// Product 2: one batch within 10 days (but beyond 3).
	b2 := purchaseExpiry(t, svc, clk, ctx, 2, 8, &within10)
	// Product 1: a batch with NO expiry -> excluded.
	purchaseExpiry(t, svc, clk, ctx, 1, 3, nil)

	// withinDays=3 -> only b1 (exp <= now+3d).
	got, err := svc.ListNearingBatches(ctx, 10, 3)
	if err != nil {
		t.Fatalf("ListNearingBatches(3d): %v", err)
	}
	if len(got) != 1 || got[0].ID != b1.ID {
		t.Errorf("within3d = %+v, want only b1(id=%d)", got, b1.ID)
	}

	// withinDays=10 -> b1 + b2 (both exp <= now+10d); the no-expiry batch stays out.
	got, err = svc.ListNearingBatches(ctx, 10, 10)
	if err != nil {
		t.Fatalf("ListNearingBatches(10d): %v", err)
	}
	ids := map[int64]bool{}
	for _, b := range got {
		ids[b.ID] = true
	}
	if len(got) != 2 || !ids[b1.ID] || !ids[b2.ID] {
		t.Errorf("within10d = %+v, want b1+b2", got)
	}

	// Sanity: the far-out batch never appears.
	for _, b := range got {
		if b.ExpiryDate.After(within10) {
			t.Errorf("batch %d expiry %v beyond deadline returned", b.ID, b.ExpiryDate)
		}
	}
	_ = repo
}

func TestListNearingBatches_ExcludesNoExpiryAndCleared(t *testing.T) {
	svc, _, clk := newService(tenantLookuper())
	defer setNowForTest(clk.now)()
	ctx := ctxFor(100, 10, 7)

	now := clk.now()
	soon := now.AddDate(0, 0, 2)

	// Batch with no expiry -> excluded from nearing list.
	purchaseExpiry(t, svc, clk, ctx, 1, 5, nil)

	// Active batch expiring soon -> included before drain.
	keep := purchaseExpiry(t, svc, clk, ctx, 1, 5, &soon)
	got, err := svc.ListNearingBatches(ctx, 10, 7)
	if err != nil {
		t.Fatalf("ListNearingBatches pre-drain: %v", err)
	}
	found := false
	for _, b := range got {
		if b.ID == keep.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("active soon-expiry batch %d missing from nearing list", keep.ID)
	}

	// Drain ALL of product 1 (FIFO drains the no-expiry lot first, then keep).
	// After this, keep is cleared (qty=0, status=cleared) -> excluded.
	if _, err := svc.ConsumeFIFO(ctx, 10, 1, 10, 1); err != nil {
		t.Fatalf("ConsumeFIFO drain: %v", err)
	}
	got, err = svc.ListNearingBatches(ctx, 10, 7)
	if err != nil {
		t.Fatalf("ListNearingBatches post-drain: %v", err)
	}
	for _, b := range got {
		if b.ID == keep.ID {
			t.Errorf("cleared batch %d should not appear in nearing list", b.ID)
		}
		if b.Quantity <= 0 {
			t.Errorf("zero/negative quantity batch %d returned", b.ID)
		}
		if b.ExpiryDate == nil {
			t.Errorf("no-expiry batch %d returned", b.ID)
		}
	}
}

func TestListNearingBatches_StoreScoped(t *testing.T) {
	svc, _, clk := newService(tenantLookuper())
	defer setNowForTest(clk.now)()
	ctx := ctxFor(100, 10, 7)

	now := clk.now()
	soon := now.AddDate(0, 0, 2)

	// Stock in store 10 (via ctx) and store 11 (explicit).
	purchaseExpiry(t, svc, clk, ctx, 1, 5, &soon)
	if _, err := svc.PurchaseIn(ctx, PurchaseInInput{StoreID: 11, ProductID: 2, Quantity: 4, InPriceCents: 100, ExpiryDate: &soon}); err != nil {
		t.Fatal(err)
	}

	// Store 10 sees only its own batch.
	got10, err := svc.ListNearingBatches(ctx, 10, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range got10 {
		if b.StoreID != 10 {
			t.Errorf("store-10 query returned batch from store %d", b.StoreID)
		}
	}
	// Store 11 sees only its own.
	got11, err := svc.ListNearingBatches(ctx, 11, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range got11 {
		if b.StoreID != 11 {
			t.Errorf("store-11 query returned batch from store %d", b.StoreID)
		}
	}
}

func TestListNearingBatches_CrossTenantInvisible(t *testing.T) {
	svc, repo, clk := newService(tenantLookuper())
	defer setNowForTest(clk.now)()
	ctxA := ctxFor(100, 10, 7)
	ctxB := ctxFor(200, 20, 8)

	now := clk.now()
	soon := now.AddDate(0, 0, 2)

	// Tenant A stocks a nearing batch.
	bA := purchaseExpiry(t, svc, clk, ctxA, 1, 5, &soon)

	// Tenant B lists its store 10 -> must see nothing (cross-tenant).
	got, err := svc.ListNearingBatches(ctxB, 10, 7)
	if err != nil {
		t.Fatalf("ListNearingBatches: %v", err)
	}
	for _, b := range got {
		if b.TenantID == 100 {
			t.Errorf("tenant 200 saw tenant 100 batch %d", b.ID)
		}
	}

	// Tenant B's repo-level call also excludes tenant A's batch.
	direct, err := repo.ListNearingBatches(ctxB, 200, 10, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range direct {
		if b.ID == bA.ID {
			t.Errorf("repo cross-tenant leak: tenant 200 saw batch %d", bA.ID)
		}
	}
}

func TestListNearingBatches_NoTenantRejected(t *testing.T) {
	svc, _, _ := newService(tenantLookuper())
	if _, err := svc.ListNearingBatches(context.Background(), 10, 7); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant err = %v, want ErrNoTenant", err)
	}
}

// --- EarliestExpiryByProduct (M7-A) ---

func TestEarliestExpiryByProduct_WholeTenantPerProduct(t *testing.T) {
	svc, _, clk := newService(tenantLookuper())
	defer setNowForTest(clk.now)()
	ctx := ctxFor(100, 10, 7)

	now := clk.now()
	// Product 1: two lots across two stores; the soonest is the store-11 one.
	soon := now.AddDate(0, 0, 2)
	later := now.AddDate(0, 0, 20)
	purchaseExpiry(t, svc, clk, ctx, 1, 5, &later) // store 10 (via ctx)
	if _, err := svc.PurchaseIn(ctx, PurchaseInInput{StoreID: 11, ProductID: 1, Quantity: 4, InPriceCents: 100, ExpiryDate: &soon}); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)
	// Product 2: a single lot with no expiry -> absent from the map.
	purchaseExpiry(t, svc, clk, ctx, 2, 8, nil)

	// storeID=0 spans ALL of the tenant's stores: product 1's earliest is `soon`
	// (the store-11 lot), product 2 is absent (no expiry-bearing lot).
	got, err := svc.EarliestExpiryByProduct(ctx, 0)
	if err != nil {
		t.Fatalf("EarliestExpiryByProduct(0): %v", err)
	}
	exp, ok := got[1]
	if !ok {
		t.Fatalf("product 1 missing from map: %+v", got)
	}
	if !exp.Equal(soon) {
		t.Errorf("product 1 earliest = %v, want soonest %v", exp, soon)
	}
	if _, ok := got[2]; ok {
		t.Errorf("product 2 (no-expiry lot) must be absent, got %+v", got)
	}
}

func TestEarliestExpiryByProduct_StoreScoped(t *testing.T) {
	svc, _, clk := newService(tenantLookuper())
	defer setNowForTest(clk.now)()
	ctx := ctxFor(100, 10, 7)

	now := clk.now()
	soon := now.AddDate(0, 0, 2)
	later := now.AddDate(0, 0, 20)
	// store 10 (via ctx): later; store 11 (explicit): soon.
	purchaseExpiry(t, svc, clk, ctx, 1, 5, &later)
	if _, err := svc.PurchaseIn(ctx, PurchaseInInput{StoreID: 11, ProductID: 1, Quantity: 4, InPriceCents: 100, ExpiryDate: &soon}); err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Minute)

	// storeID=10 -> only the store-10 lot (later), not the store-11 soon one.
	got10, err := svc.EarliestExpiryByProduct(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if exp := got10[1]; !exp.Equal(later) {
		t.Errorf("store-10 earliest = %v, want %v (store-11 lot must not leak in)", exp, later)
	}
	// storeID=11 -> only the soon lot.
	got11, err := svc.EarliestExpiryByProduct(ctx, 11)
	if err != nil {
		t.Fatal(err)
	}
	if exp := got11[1]; !exp.Equal(soon) {
		t.Errorf("store-11 earliest = %v, want %v", exp, soon)
	}
}

func TestEarliestExpiryByProduct_ExcludesClearedAndNoExpiry(t *testing.T) {
	svc, _, clk := newService(tenantLookuper())
	defer setNowForTest(clk.now)()
	ctx := ctxFor(100, 10, 7)

	now := clk.now()
	soon := now.AddDate(0, 0, 2)
	// Active lot with an expiry -> present before drain.
	purchaseExpiry(t, svc, clk, ctx, 1, 5, &soon)
	if got, _ := svc.EarliestExpiryByProduct(ctx, 0); len(got) != 1 {
		t.Fatalf("pre-drain map = %+v, want product 1 present", got)
	}

	// Drain product 1 -> its lot is cleared (qty=0) -> excluded.
	if _, err := svc.ConsumeFIFO(ctx, 10, 1, 5, 1); err != nil {
		t.Fatalf("ConsumeFIFO drain: %v", err)
	}
	got, err := svc.EarliestExpiryByProduct(ctx, 0)
	if err != nil {
		t.Fatalf("post-drain: %v", err)
	}
	if _, ok := got[1]; ok {
		t.Errorf("cleared product 1 must be absent post-drain, got %+v", got)
	}
}

func TestEarliestExpiryByProduct_CrossTenantInvisible(t *testing.T) {
	svc, repo, clk := newService(tenantLookuper())
	defer setNowForTest(clk.now)()
	ctxA := ctxFor(100, 10, 7)
	ctxB := ctxFor(200, 20, 8)

	now := clk.now()
	soon := now.AddDate(0, 0, 2)
	purchaseExpiry(t, svc, clk, ctxA, 1, 5, &soon)

	// Tenant B's whole-tenant sweep must NOT see tenant A's batch.
	got, err := svc.EarliestExpiryByProduct(ctxB, 0)
	if err != nil {
		t.Fatalf("EarliestExpiryByProduct: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("tenant 200 saw tenant 100 expiry: %+v", got)
	}
	// Repo-level call for tenant 200 is also empty.
	direct, err := repo.EarliestExpiryByProduct(ctxB, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(direct) != 0 {
		t.Errorf("repo cross-tenant leak: %+v", direct)
	}
}

func TestEarliestExpiryByProduct_NoTenantRejected(t *testing.T) {
	svc, _, _ := newService(tenantLookuper())
	if _, err := svc.EarliestExpiryByProduct(context.Background(), 0); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant err = %v, want ErrNoTenant", err)
	}
}

// narrowRepo wraps a BatchRepo but hides the optional ProductExpirer capability,
// proving the service degrades to an empty map (nil-safe) when the repo lacks it.
type narrowRepo struct{ BatchRepo }

func TestEarliestExpiryByProduct_RepoWithoutCapabilityDegrades(t *testing.T) {
	// narrowRepo embeds BatchRepo as an interface field, so it does NOT satisfy
	// ProductExpirer (the concrete method set is not promoted through an interface
	// field). The service must return an empty map, not an error.
	svc := New(narrowRepo{BatchRepo: NewMemoryBatchRepo()}, tenantLookuper(), WithNowFunc(newClock().now))
	got, err := svc.EarliestExpiryByProduct(ctxFor(100, 10, 7), 0)
	if err != nil {
		t.Fatalf("degraded path err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("degraded path map = %+v, want empty", got)
	}
}

// --- ListBatches (BE-A, §24.2) ---

// seedBatch stamps a batch directly into the repo with full field control,
// bypassing the service lookuper so status/expiry can be set freely for filter
// tests (nearing/expired are normally assigned by the BE-A-09 tag job).
func seedBatch(t *testing.T, repo *MemoryBatchRepo, b *Batch) *Batch {
	t.Helper()
	created, err := repo.CreateBatch(context.Background(), b)
	if err != nil {
		t.Fatalf("seedBatch: %v", err)
	}
	return created
}

func TestListBatches_FiltersAndPaging(t *testing.T) {
	svc, repo, _ := newService(tenantLookuper())
	ctx := ctxFor(100, 10, 7)

	soon := baseNow.AddDate(0, 0, 3)
	past := baseNow.AddDate(0, 0, -5)
	far := baseNow.AddDate(0, 1, 0)

	// Tenant 100: four lots of mixed status/store/product/expiry.
	b1 := seedBatch(t, repo, &Batch{TenantID: 100, StoreID: 10, ProductID: 1, Quantity: 5, InPriceCents: 100, ExpiryDate: &soon, Status: BatchStatusActive})
	seedBatch(t, repo, &Batch{TenantID: 100, StoreID: 10, ProductID: 1, Quantity: 0, InPriceCents: 100, Status: BatchStatusCleared}) // b2: cleared, no expiry
	b3 := seedBatch(t, repo, &Batch{TenantID: 100, StoreID: 11, ProductID: 2, Quantity: 7, InPriceCents: 100, ExpiryDate: &past, Status: "expired"})
	seedBatch(t, repo, &Batch{TenantID: 100, StoreID: 10, ProductID: 2, Quantity: 9, InPriceCents: 100, ExpiryDate: &far, Status: BatchStatusActive}) // b4

	// No filter: all 4 lots across every status (临期看板 sees cleared/expired too).
	got, err := svc.ListBatches(ctx, BatchFilter{})
	if err != nil {
		t.Fatalf("ListBatches: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("no-filter count = %d, want 4", len(got))
	}
	total, err := svc.ListBatchesCount(ctx, BatchFilter{})
	if err != nil {
		t.Fatalf("ListBatchesCount: %v", err)
	}
	if total != 4 {
		t.Errorf("no-filter total = %d, want 4", total)
	}

	// Status filter = active -> b1 + b4.
	if got, _ := svc.ListBatches(ctx, BatchFilter{Status: BatchStatusActive}); len(got) != 2 {
		t.Errorf("status=active count = %d, want 2", len(got))
	}
	// Store filter = 10 -> b1 + b2(cleared) + b4.
	if got, _ := svc.ListBatches(ctx, BatchFilter{StoreID: 10}); len(got) != 3 {
		t.Errorf("store=10 count = %d, want 3", len(got))
	}
	// Product filter = 1 -> b1 + b2.
	if got, _ := svc.ListBatches(ctx, BatchFilter{ProductID: 1}); len(got) != 2 {
		t.Errorf("product=1 count = %d, want 2", len(got))
	}

	// ExpiryBefore = baseNow+10d -> b1(soon=+3d) + b3(past=-5d); b2 (nil expiry)
	// and b4 (far=+1mo) are excluded.
	cutoff := baseNow.AddDate(0, 0, 10)
	got, _ = svc.ListBatches(ctx, BatchFilter{ExpiryBefore: &cutoff})
	ids := map[int64]bool{}
	for _, b := range got {
		ids[b.ID] = true
	}
	if len(got) != 2 || !ids[b1.ID] || !ids[b3.ID] {
		t.Errorf("expiry_before = ids %v, want b1(id=%d)+b3(id=%d)", ids, b1.ID, b3.ID)
	}
	if tot, _ := svc.ListBatchesCount(ctx, BatchFilter{ExpiryBefore: &cutoff}); tot != 2 {
		t.Errorf("expiry_before total = %d, want 2", tot)
	}

	// Paging over all 4 ordered by id: page1 tail id < page2 head id; total
	// ignores Limit/Offset.
	page1, _ := svc.ListBatches(ctx, BatchFilter{Limit: 2, Offset: 0})
	page2, _ := svc.ListBatches(ctx, BatchFilter{Limit: 2, Offset: 2})
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("paging lens = %d/%d, want 2/2", len(page1), len(page2))
	}
	if page1[1].ID >= page2[0].ID {
		t.Errorf("paging not ordered by id: page1 tail=%d page2 head=%d", page1[1].ID, page2[0].ID)
	}
	if tot, _ := svc.ListBatchesCount(ctx, BatchFilter{Limit: 2, Offset: 2}); tot != 4 {
		t.Errorf("count ignored paging: total = %d, want 4", tot)
	}
}

func TestListBatches_CrossTenantInvisible(t *testing.T) {
	svc, repo, _ := newService(tenantLookuper())
	ctxB := ctxFor(200, 20, 8)

	soon := baseNow.AddDate(0, 0, 2)
	// Tenant A seeds two batches directly into the shared repo (tenant 100 owns
	// products 1,2 per the lookuper).
	seedBatch(t, repo, &Batch{TenantID: 100, StoreID: 10, ProductID: 1, Quantity: 5, InPriceCents: 100, ExpiryDate: &soon, Status: BatchStatusActive})
	seedBatch(t, repo, &Batch{TenantID: 100, StoreID: 10, ProductID: 2, Quantity: 3, InPriceCents: 100, Status: BatchStatusCleared})

	// Tenant B sees zero batches and zero total (cross-tenant == absent).
	got, err := svc.ListBatches(ctxB, BatchFilter{})
	if err != nil {
		t.Fatalf("tenant 200 ListBatches: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("tenant 200 saw %d batches, want 0", len(got))
	}
	if total, _ := svc.ListBatchesCount(ctxB, BatchFilter{}); total != 0 {
		t.Errorf("tenant 200 total = %d, want 0", total)
	}
	// Repo-level calls for tenant 200 also exclude tenant 100's lots.
	if direct, _ := repo.ListBatches(context.Background(), 200, BatchFilter{}); len(direct) != 0 {
		t.Errorf("repo cross-tenant leak: %d batches", len(direct))
	}
	if direct, _ := repo.ListBatchesCount(context.Background(), 200, BatchFilter{}); direct != 0 {
		t.Errorf("repo cross-tenant count leak: %d", direct)
	}
}

func TestListBatches_NoTenantRejected(t *testing.T) {
	svc, _, _ := newService(tenantLookuper())
	if _, err := svc.ListBatches(context.Background(), BatchFilter{}); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant ListBatches err = %v, want ErrNoTenant", err)
	}
	if _, err := svc.ListBatchesCount(context.Background(), BatchFilter{}); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("no-tenant ListBatchesCount err = %v, want ErrNoTenant", err)
	}
}

// narrowRepo (defined above) also hides the new BatchLister capability, so the
// service must degrade to an empty slice / 0 total rather than erroring.
func TestListBatches_RepoWithoutCapabilityDegrades(t *testing.T) {
	svc := New(narrowRepo{BatchRepo: NewMemoryBatchRepo()}, tenantLookuper(), WithNowFunc(newClock().now))
	ctx := ctxFor(100, 10, 7)
	got, err := svc.ListBatches(ctx, BatchFilter{})
	if err != nil {
		t.Fatalf("degraded ListBatches err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("degraded ListBatches = %+v, want empty", got)
	}
	total, err := svc.ListBatchesCount(ctx, BatchFilter{})
	if err != nil {
		t.Fatalf("degraded ListBatchesCount err = %v, want nil", err)
	}
	if total != 0 {
		t.Errorf("degraded ListBatchesCount = %d, want 0", total)
	}
}
