package strategy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// ctxWithTenant builds a ctx carrying (tenantID, storeID) — mirrors what the
// auth middleware / dev MockTenant injects in production.
func ctxWithTenant(tenantID, storeID int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tenantID,
		StoreID:  storeID,
		Source:   multitenancy.SourceMerchant,
	})
}

// fixedNow is the service clock used across tests.
var fixedNow = time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)

func newSvc(t *testing.T, opts ...Option) *Service {
	t.Helper()
	return New(NewMemoryStrategyRepo(), append([]Option{WithNowFunc(func() time.Time { return fixedNow })}, opts...)...)
}

// sampleAdvice builds a valid 3-item advice for (tenant, store, date).
func sampleAdvice(tenantID, storeID int64, date string) *Strategy {
	return &Strategy{
		TenantID:            tenantID, // service will force-from-ctx; harmless here
		StoreID:             storeID,
		ForDate:             date,
		Signals:             jsonRaw(`{"business":"fruit"}`),
		DataQualityScore:    72,
		GeneratedBy:         GenLiveLLM,
		LLMTier:             "doubao_lite",
		TotalCostMicroCents: 1234,
		Status:              StatusGenerated,
		Items: []StrategyItem{
			{Rank: 1, ActionType: "group_seckill", ReasonType: "clearance_coupon", Copy: "芒果临期,群里秒杀", Reason: "临期清仓", MarginOK: true,
				Target: map[string]any{"segment": "all"}, Params: map[string]any{"discount_permille": int64(700)}, ExpectedEffect: map[string]any{"redeem": 35}},
			{Rank: 2, ActionType: "coupon_wake", ReasonType: "wake_dormant", Copy: "想你了,领券回店", Reason: "沉睡唤醒", MarginOK: true,
				Params: map[string]any{"off_cents": int64(500)}, ExpectedEffect: map[string]any{"visit": 12}},
			{Rank: 3, ActionType: "groupbuy", ReasonType: "bundle_slow", Copy: "滞销捆绑拼团", Reason: "滞销捆绑", MarginOK: true,
				Params: map[string]any{"price_cents": int64(990)}},
		},
	}
}

// jsonRaw is a tiny helper so the test file need not import encoding/json just
// to build an opaque snapshot.
func jsonRaw(s string) []byte { return []byte(s) }

// assertItems checks got has exactly wantRanks in order.
func assertItems(t *testing.T, got *Strategy, wantRanks ...int) {
	t.Helper()
	if len(got.Items) != len(wantRanks) {
		t.Fatalf("got %d items, want %d", len(got.Items), len(wantRanks))
	}
	for i, r := range wantRanks {
		if got.Items[i].Rank != r {
			t.Fatalf("item %d rank = %d, want %d", i, got.Items[i].Rank, r)
		}
	}
}

// --- SaveAdvice upsert ------------------------------------------------------

func TestSaveAdvice_FirstInsert_BackfillsIDs(t *testing.T) {
	svc := newSvc(t)
	ctx := ctxWithTenant(5001, 7001)
	in := sampleAdvice(5001, 7001, "2026-07-28")
	out, err := svc.SaveAdvice(ctx, in)
	if err != nil {
		t.Fatalf("SaveAdvice: %v", err)
	}
	if out.ID == 0 {
		t.Fatal("strategy ID not backfilled")
	}
	if in.ID != out.ID {
		t.Fatalf("input ID not backfilled: in=%d out=%d", in.ID, out.ID)
	}
	if len(out.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(out.Items))
	}
	for _, it := range out.Items {
		if it.ID == 0 || it.StrategyID != out.ID {
			t.Fatalf("item not backfilled: %+v", it)
		}
		if it.ExecutionStatus != ItemPending {
			t.Fatalf("new item status = %q, want pending", it.ExecutionStatus)
		}
	}
	if out.CreatedAt.IsZero() {
		t.Fatal("CreatedAt zero")
	}
}

func TestSaveAdvice_UpsertSameDay_OverwritesNoDuplication(t *testing.T) {
	svc := newSvc(t)
	ctx := ctxWithTenant(5001, 7001)

	first, err := svc.SaveAdvice(ctx, sampleAdvice(5001, 7001, "2026-07-28"))
	if err != nil {
		t.Fatalf("first SaveAdvice: %v", err)
	}
	firstCreatedAt := first.CreatedAt
	oldItemIDs := []int64{first.Items[0].ID, first.Items[1].ID, first.Items[2].ID}

	// Regenerate same day with a 2-item advice (fewer items, different params).
	regen := sampleAdvice(5001, 7001, "2026-07-28")
	regen.Items = regen.Items[:2]
	regen.Items[1].Copy = "updated copy"
	regen.DataQualityScore = 90
	second, err := svc.SaveAdvice(ctx, regen)
	if err != nil {
		t.Fatalf("regen SaveAdvice: %v", err)
	}

	// Strategy ID stable, CreatedAt preserved, fields overwritten.
	if second.ID != first.ID {
		t.Fatalf("strategy ID changed: %d -> %d", first.ID, second.ID)
	}
	if !second.CreatedAt.Equal(firstCreatedAt) {
		t.Fatalf("CreatedAt not preserved: was %v now %v", firstCreatedAt, second.CreatedAt)
	}
	if second.DataQualityScore != 90 {
		t.Fatalf("DataQualityScore not overwritten: %d", second.DataQualityScore)
	}
	// Items rebuilt (fresh IDs, exactly 2), old item IDs gone.
	assertItems(t, second, 1, 2)
	for _, old := range oldItemIDs {
		if _, err := svc.GetItem(ctx, old); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old item %d should be gone, got err=%v", old, err)
		}
	}
	for _, it := range second.Items {
		for _, old := range oldItemIDs {
			if it.ID == old {
				t.Fatalf("item ID %d reused from old set", it.ID)
			}
		}
	}

	// Only one strategy row for this day.
	today, err := svc.GetToday(ctx, 7001, "2026-07-28")
	if err != nil {
		t.Fatalf("GetToday: %v", err)
	}
	if today.ID != first.ID {
		t.Fatalf("GetToday returned different strategy: %d", today.ID)
	}
	assertItems(t, today, 1, 2)
}

func TestSaveAdvice_Validation(t *testing.T) {
	svc := newSvc(t)
	ctx := ctxWithTenant(5001, 7001)

	cases := []struct {
		name string
		mut  func(s *Strategy)
	}{
		{"bad date", func(s *Strategy) { s.ForDate = "2026/07/28" }},
		{"empty date", func(s *Strategy) { s.ForDate = "" }},
		{"bad generated_by", func(s *Strategy) { s.GeneratedBy = "magic" }},
		{"bad status", func(s *Strategy) { s.Status = "weird" }},
		{"rank out of range", func(s *Strategy) { s.Items[0].Rank = 4 }},
		{"duplicate rank", func(s *Strategy) { s.Items[1].Rank = 1 }},
		{"bad action_type", func(s *Strategy) { s.Items[0].ActionType = "fly" }},
		{"bad reason_type", func(s *Strategy) { s.Items[0].ReasonType = "whatever" }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			in := sampleAdvice(5001, 7001, "2026-07-28")
			c.mut(in)
			if _, err := svc.SaveAdvice(ctx, in); !errors.Is(err, ErrParam) {
				t.Fatalf("want ErrParam, got %v", err)
			}
		})
	}

	// nil strategy.
	if _, err := svc.SaveAdvice(ctx, nil); !errors.Is(err, ErrParam) {
		t.Fatalf("nil strategy: want ErrParam, got %v", err)
	}
	// missing store (no store in ctx either).
	ctxNoStore := multitenancy.WithContext(context.Background(), multitenancy.Context{TenantID: 5001, Source: multitenancy.SourceMerchant})
	in := sampleAdvice(5001, 0, "2026-07-28")
	in.StoreID = 0
	if _, err := svc.SaveAdvice(ctxNoStore, in); !errors.Is(err, ErrParam) {
		t.Fatalf("no store: want ErrParam, got %v", err)
	}
}

// --- cross-tenant isolation -------------------------------------------------

func TestCrossTenant_ErrNotFound(t *testing.T) {
	svc := newSvc(t)
	ctxA := ctxWithTenant(5001, 7001)
	ctxB := ctxWithTenant(5002, 7002) // different tenant

	out, err := svc.SaveAdvice(ctxA, sampleAdvice(5001, 7001, "2026-07-28"))
	if err != nil {
		t.Fatalf("SaveAdvice A: %v", err)
	}
	stratID := out.ID
	itemID := out.Items[0].ID

	// tenant B cannot see A's strategy by id.
	if _, err := svc.Get(ctxB, stratID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get cross-tenant: want ErrNotFound, got %v", err)
	}
	// tenant B cannot see A's today advice even for the "same" store+date
	// (store ids are tenant-scoped, so this is foreign).
	if _, err := svc.GetToday(ctxB, 7001, "2026-07-28"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetToday cross-tenant: want ErrNotFound, got %v", err)
	}
	// tenant B cannot load A's item.
	if _, err := svc.GetItem(ctxB, itemID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetItem cross-tenant: want ErrNotFound, got %v", err)
	}
	// tenant B cannot transition A's item.
	if _, err := svc.UpdateItemStatus(ctxB, itemID, ItemExecuted); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateItemStatus cross-tenant: want ErrNotFound, got %v", err)
	}
	// tenant B cannot feedback A's item.
	if _, err := svc.RecordFeedback(ctxB, itemID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RecordFeedback cross-tenant: want ErrNotFound, got %v", err)
	}
	// tenant A still sees its own item and can transition it.
	if _, err := svc.GetItem(ctxA, itemID); err != nil {
		t.Fatalf("GetItem own: %v", err)
	}
}

// --- no tenant in ctx -------------------------------------------------------

func TestNoTenant_Errors(t *testing.T) {
	svc := newSvc(t)
	ctx := context.Background() // no tenant
	if _, err := svc.SaveAdvice(ctx, sampleAdvice(1, 1, "2026-07-28")); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("SaveAdvice no tenant: want ErrNoTenant, got %v", err)
	}
	if _, err := svc.Get(ctx, 1); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("Get no tenant: want ErrNoTenant, got %v", err)
	}
}

// --- GetToday default date --------------------------------------------------

func TestGetToday_EmptyDateDefaultsToday(t *testing.T) {
	svc := newSvc(t)
	ctx := ctxWithTenant(5001, 7001)
	if _, err := svc.SaveAdvice(ctx, sampleAdvice(5001, 7001, fixedNow.Format("2006-01-02"))); err != nil {
		t.Fatalf("SaveAdvice: %v", err)
	}
	// Empty date -> service clock's today.
	got, err := svc.GetToday(ctx, 7001, "")
	if err != nil {
		t.Fatalf("GetToday empty date: %v", err)
	}
	if got.ForDate != fixedNow.Format("2006-01-02") {
		t.Fatalf("ForDate = %q, want %q", got.ForDate, fixedNow.Format("2006-01-02"))
	}
	// No advice for a different day.
	if _, err := svc.GetToday(ctx, 7001, "2026-01-01"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetToday other day: want ErrNotFound, got %v", err)
	}
}

// --- execution-status state machine ----------------------------------------

func TestUpdateItemStatus_Transitions(t *testing.T) {
	svc := newSvc(t)
	ctx := ctxWithTenant(5001, 7001)
	out, err := svc.SaveAdvice(ctx, sampleAdvice(5001, 7001, "2026-07-28"))
	if err != nil {
		t.Fatalf("SaveAdvice: %v", err)
	}
	itemID := out.Items[0].ID

	// pending -> executed (terminal): ExecutedAt stamped.
	got, err := svc.UpdateItemStatus(ctx, itemID, ItemExecuted)
	if err != nil {
		t.Fatalf("pending->executed: %v", err)
	}
	if got.ExecutionStatus != ItemExecuted {
		t.Fatalf("status = %q", got.ExecutionStatus)
	}
	if got.ExecutedAt == nil || !got.ExecutedAt.Equal(fixedNow) {
		t.Fatalf("ExecutedAt = %v, want %v", got.ExecutedAt, fixedNow)
	}
	// executed -> executed (self idempotent): ok, ExecutedAt re-stamped.
	if _, err := svc.UpdateItemStatus(ctx, itemID, ItemExecuted); err != nil {
		t.Fatalf("executed->executed: %v", err)
	}
	// executed -> blocked (terminal -> other terminal): illegal.
	if _, err := svc.UpdateItemStatus(ctx, itemID, ItemBlocked); !errors.Is(err, ErrParam) {
		t.Fatalf("executed->blocked: want ErrParam, got %v", err)
	}
	// executed -> pending (rollback): illegal.
	if _, err := svc.UpdateItemStatus(ctx, itemID, ItemPending); !errors.Is(err, ErrParam) {
		t.Fatalf("executed->pending: want ErrParam, got %v", err)
	}

	// On a fresh pending item: pending -> blocked legal, ExecutedAt stamped.
	out2, _ := svc.SaveAdvice(ctxWithTenant(5003, 7003), sampleAdvice(5003, 7003, "2026-07-29"))
	blocked, err := svc.UpdateItemStatus(ctxWithTenant(5003, 7003), out2.Items[1].ID, ItemBlocked)
	if err != nil {
		t.Fatalf("pending->blocked: %v", err)
	}
	if blocked.ExecutionStatus != ItemBlocked || blocked.ExecutedAt == nil {
		t.Fatalf("blocked wrong: %+v", blocked)
	}

	// pending -> pending self idempotent: ExecutedAt cleared (nil).
	out3, _ := svc.SaveAdvice(ctxWithTenant(5004, 7004), sampleAdvice(5004, 7004, "2026-07-30"))
	pendingAgain, err := svc.UpdateItemStatus(ctxWithTenant(5004, 7004), out3.Items[2].ID, ItemPending)
	if err != nil {
		t.Fatalf("pending->pending: %v", err)
	}
	if pendingAgain.ExecutionStatus != ItemPending || pendingAgain.ExecutedAt != nil {
		t.Fatalf("pending self: status=%q ExecutedAt=%v", pendingAgain.ExecutionStatus, pendingAgain.ExecutedAt)
	}

	// Garbage status rejected.
	if _, err := svc.UpdateItemStatus(ctx, itemID, "frobnicate"); !errors.Is(err, ErrParam) {
		t.Fatalf("garbage status: want ErrParam, got %v", err)
	}
	// Absent item.
	if _, err := svc.UpdateItemStatus(ctx, 999999, ItemExecuted); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent item: want ErrNotFound, got %v", err)
	}
}

// --- RecordFeedback UNIQUE overwrite ---------------------------------------

func TestRecordFeedback_UpsertOverwrite(t *testing.T) {
	svc := newSvc(t)
	ctx := ctxWithTenant(5001, 7001)
	out, err := svc.SaveAdvice(ctx, sampleAdvice(5001, 7001, "2026-07-28"))
	if err != nil {
		t.Fatalf("SaveAdvice: %v", err)
	}
	itemID := out.Items[0].ID

	fb1, err := svc.RecordFeedback(ctx, itemID, true)
	if err != nil {
		t.Fatalf("RecordFeedback 1: %v", err)
	}
	if !fb1.Useful || fb1.ID == 0 {
		t.Fatalf("fb1 wrong: %+v", fb1)
	}
	// Repeat on same item -> overwrite Useful, same ID (UNIQUE semantics).
	fb2, err := svc.RecordFeedback(ctx, itemID, false)
	if err != nil {
		t.Fatalf("RecordFeedback 2: %v", err)
	}
	if fb2.ID != fb1.ID {
		t.Fatalf("feedback ID changed: %d -> %d (UNIQUE should keep id)", fb1.ID, fb2.ID)
	}
	if fb2.Useful {
		t.Fatal("Useful not overwritten to false")
	}
}

// --- List -------------------------------------------------------------------

func TestList_FilterAndOrder(t *testing.T) {
	svc := newSvc(t)
	ctx := ctxWithTenant(5001, 7001)
	if _, err := svc.SaveAdvice(ctx, sampleAdvice(5001, 7001, "2026-07-27")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveAdvice(ctx, sampleAdvice(5001, 7001, "2026-07-28")); err != nil {
		t.Fatal(err)
	}
	// Different store same tenant.
	if _, err := svc.SaveAdvice(ctxWithTenant(5001, 7002), sampleAdvice(5001, 7002, "2026-07-28")); err != nil {
		t.Fatal(err)
	}
	// Different tenant — must not leak.
	if _, err := svc.SaveAdvice(ctxWithTenant(5009, 7009), sampleAdvice(5009, 7009, "2026-07-28")); err != nil {
		t.Fatal(err)
	}

	all, err := svc.List(ctx, StrategyFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List tenant 5001 got %d, want 3", len(all))
	}
	// newest-first (ID desc).
	for i := 1; i < len(all); i++ {
		if all[i-1].ID < all[i].ID {
			t.Fatal("List not ID-desc")
		}
	}

	byDate, _ := svc.List(ctx, StrategyFilter{Date: "2026-07-28"})
	if len(byDate) != 2 {
		t.Fatalf("Date filter got %d, want 2", len(byDate))
	}
	byStore, _ := svc.List(ctx, StrategyFilter{StoreID: 7002})
	if len(byStore) != 1 {
		t.Fatalf("Store filter got %d, want 1", len(byStore))
	}
	// Cross-tenant listing empty.
	foreign, _ := svc.List(ctxWithTenant(5009, 7009), StrategyFilter{})
	if len(foreign) != 1 {
		t.Fatalf("tenant 5009 got %d, want 1", len(foreign))
	}
}

// --- concurrency (-race) ---------------------------------------------------

func TestSaveAdvice_ConcurrentSameTenantDay(t *testing.T) {
	t.Parallel()
	svc := newSvc(t)
	ctx := ctxWithTenant(5001, 7001)
	const n = 30
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := svc.SaveAdvice(ctx, sampleAdvice(5001, 7001, "2026-07-28")); err != nil {
				t.Errorf("concurrent SaveAdvice: %v", err)
			}
		}()
	}
	wg.Wait()
	// Exactly one strategy row; 3 items (last writer wins, each wrote 3).
	got, err := svc.GetToday(ctx, 7001, "2026-07-28")
	if err != nil {
		t.Fatalf("GetToday after concurrent: %v", err)
	}
	if len(got.Items) != 3 {
		t.Fatalf("after concurrent saves got %d items, want 3", len(got.Items))
	}
}

func TestSaveAdvice_ConcurrentDifferentTenants(t *testing.T) {
	t.Parallel()
	svc := newSvc(t)
	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			tid := int64(6000 + i%5)
			ctx := ctxWithTenant(tid, 8000+int64(i%5))
			if _, err := svc.SaveAdvice(ctx, sampleAdvice(tid, 8000+int64(i%5), "2026-07-28")); err != nil {
				t.Errorf("concurrent tenant save: %v", err)
			}
		}(i)
	}
	wg.Wait()
	// 5 distinct tenants -> 5 rows.
	all, _ := svc.List(ctxWithTenant(6000, 8000), StrategyFilter{})
	if len(all) != 1 {
		t.Fatalf("tenant 6000 got %d rows, want 1", len(all))
	}
}

func TestUpdateItemStatus_ConcurrentSameItem(t *testing.T) {
	t.Parallel()
	svc := newSvc(t)
	ctx := ctxWithTenant(5001, 7001)
	out, _ := svc.SaveAdvice(ctx, sampleAdvice(5001, 7001, "2026-07-28"))
	itemID := out.Items[0].ID

	var wg sync.WaitGroup
	wg.Add(20)
	for i := 0; i < 20; i++ {
		go func(i int) {
			defer wg.Done()
			// All legal pending->terminal transitions; some repeat the same
			// terminal (self idempotent). No error is fatal here except
			// ErrNotFound (data race / corruption).
			status := []string{ItemExecuted, ItemSkipped, ItemFailed, ItemBlocked}[i%4]
			_, err := svc.UpdateItemStatus(ctx, itemID, status)
			if errors.Is(err, ErrNotFound) {
				t.Errorf("concurrent transition lost item: %v", err)
			}
		}(i)
	}
	wg.Wait()
	// Final state: a terminal status.
	it, err := svc.GetItem(ctx, itemID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	switch it.ExecutionStatus {
	case ItemExecuted, ItemSkipped, ItemFailed, ItemBlocked:
		// ok
	default:
		t.Fatalf("final status = %q, want a terminal", it.ExecutionStatus)
	}
}

// --- defensive copy ---------------------------------------------------------

func TestMemoryRepo_DefensiveCopy(t *testing.T) {
	repo := NewMemoryStrategyRepo()
	ctx := context.Background()
	in := sampleAdvice(5001, 7001, "2026-07-28")
	out, err := repo.SaveAdvice(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate the returned clone; stored record must be unaffected.
	out.Items[0].Copy = "MUTATED"
	out.Signals = nil
	out.Items[0].Params["injected"] = true
	got, err := repo.Get(ctx, 5001, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Items[0].Copy == "MUTATED" {
		t.Fatal("Copy mutated through returned clone")
	}
	if got.Signals == nil {
		t.Fatal("Signals mutated through returned clone")
	}
	if _, ok := got.Items[0].Params["injected"]; ok {
		t.Fatal("Params map shared with caller")
	}
}
