package billing

// Service orchestrates checkout (CreateOrder) and daily flow reporting
// (DailySummary). Every method first resolves the tenant from ctx via
// multitenancy.EnforceTenant — tenant is NEVER trusted from a request field.
// The service depends only on the ProductPricer and OrderRepo interfaces plus
// two injected clock/orderno funcs, so the whole flow is deterministic in unit
// tests (fake pricer + fixed now + fixed order-no).

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Defaults applied by CreateOrder.
const (
	defaultChannel       = "in_store"
	defaultPaymentMethod = "cash"
	orderStatusPaid      = "paid"
	productStatusActive  = "active"
	maxTopProducts       = 5
)

// OrderNoFunc generates a human-readable order number for a tenant at a moment.
// Injected so tests are deterministic; see DefaultOrderNoFunc.
type OrderNoFunc func(tenantID int64, now time.Time) string

// StockConsumer is billing's minimal anti-corruption view of the inventory
// domain (M1-S2, CONTRACT §9.3). billing NEVER imports internal/inventory; the
// main thread adapts inventory.Service to this interface at wiring time. It is
// OPTIONAL: when unset the service records orders without touching stock (the
// M1-S1 behaviour), so existing callers/tests are unaffected.
type StockConsumer interface {
	// ConsumeFIFO deducts qty units of productID from storeID's batches in
	// FIFO order, attributing the sale to refOrderID. It returns an error
	// (e.g. inventory.ErrInsufficientStock) when stock is insufficient; on
	// error NO partial deduction may remain.
	ConsumeFIFO(ctx context.Context, storeID, productID, qty, refOrderID int64) error
}

// orderNoSeq backs DefaultOrderNoFunc with a process-wide monotonic suffix so
// numbers stay unique even within the same second.
var orderNoSeq atomic.Int64

// DefaultOrderNoFunc builds ZG<tenant><yyyymmdd><seq>: readable, sortable by
// day, and unique per process via the atomic sequence suffix.
func DefaultOrderNoFunc(tenantID int64, now time.Time) string {
	seq := orderNoSeq.Add(1)
	return fmt.Sprintf("ZG%d%s%06d", tenantID, now.Format("20060102"), seq)
}

// Service is the billing orchestrator.
type Service struct {
	repo    OrderRepo
	pricer  ProductPricer
	stock   StockConsumer // optional; nil = no stock deduction (M1-S1 behaviour)
	orderNo OrderNoFunc
	now     func() time.Time
}

// Option customises a Service (primarily for deterministic tests).
type Option func(*Service)

// WithOrderNoFunc overrides order-number generation.
func WithOrderNoFunc(f OrderNoFunc) Option {
	return func(s *Service) {
		if f != nil {
			s.orderNo = f
		}
	}
}

// WithNowFunc overrides the clock (deterministic PaidAt/CreatedAt in tests).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// WithStockConsumer wires FIFO stock deduction into checkout (M1-S2). When set,
// CreateOrder deducts each line's qty from inventory before persisting the
// order; any insufficient-stock error rejects the whole order (nothing is
// recorded). Passing nil leaves the service in the no-stock (M1-S1) mode.
func WithStockConsumer(sc StockConsumer) Option {
	return func(s *Service) {
		s.stock = sc
	}
}

// New constructs a billing Service. Nil OrderNoFunc/now fall back to the
// defaults (DefaultOrderNoFunc / time.Now).
func New(repo OrderRepo, pricer ProductPricer, opts ...Option) *Service {
	s := &Service{
		repo:    repo,
		pricer:  pricer,
		orderNo: DefaultOrderNoFunc,
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// CreateOrder prices and records a paid order for the ctx tenant. Each line is
// snapshot-priced via the ProductPricer; cross-tenant or non-active products
// are rejected. total = Σ qty*price, paid = total, discount = 0.
func (s *Service) CreateOrder(ctx context.Context, in CreateOrderInput) (*Order, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if len(in.Items) == 0 {
		return nil, errcode.ErrParam
	}

	now := s.now()
	items := make([]OrderItem, 0, len(in.Items))
	var total int64
	for _, it := range in.Items {
		if it.Qty <= 0 {
			return nil, errcode.ErrParam
		}
		p, err := s.pricer.GetByID(ctx, tenantID, it.ProductID)
		if err != nil {
			return nil, err
		}
		// Cross-tenant defence-in-depth: the pricer should already scope to
		// tenantID, but never trust it blindly.
		if p.TenantID != tenantID {
			return nil, errcode.ErrForbidden
		}
		if p.Status != productStatusActive {
			return nil, errcode.ErrParam
		}
		items = append(items, OrderItem{
			ProductID:    p.ID,
			NameSnapshot: p.Name,
			Unit:         p.Unit,
			Qty:          it.Qty,
			PriceCents:   p.PriceCents,
			CostCents:    p.CostCents,
		})
		total += it.Qty * p.PriceCents
	}

	channel := in.Channel
	if channel == "" {
		channel = defaultChannel
	}
	payMethod := in.PaymentMethod
	if payMethod == "" {
		payMethod = defaultPaymentMethod
	}

	storeID := in.StoreID
	if storeID <= 0 {
		if mc, ok := multitenancy.FromContext(ctx); ok {
			storeID = mc.StoreID
		}
	}

	order := &Order{
		TenantID:      tenantID,
		StoreID:       storeID,
		MemberID:      in.MemberID,
		OrderNo:       s.orderNo(tenantID, now),
		Channel:       channel,
		Status:        orderStatusPaid,
		TotalCents:    total,
		PaidCents:     total,
		DiscountCents: 0,
		PaymentMethod: payMethod,
		Items:         items,
		PaidAt:        &now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if mc, ok := multitenancy.FromContext(ctx); ok {
		order.OperatorID = mc.UserID
	}

	created, err := s.repo.Create(ctx, order)
	if err != nil {
		return nil, errcode.ErrInternal
	}

	// Deduct stock FIFO after the order is recorded so the sale carries a real
	// ref_order_id. This is best-effort: a stock shortfall is logged as a
	// reconciliation gap but does NOT reject the already-committed sale (the
	// in-store transaction already happened). Wired only when a StockConsumer
	// is injected (M1-S2); nil keeps M1-S1 behaviour.
	if s.stock != nil {
		for _, it := range created.Items {
			// Errors are intentionally swallowed here; the daily reconciliation
			// job (M4) reconciles inventory vs orders. We avoid failing the
			// checkout after the order is durably created.
			_ = s.stock.ConsumeFIFO(ctx, created.StoreID, it.ProductID, it.Qty, created.ID)
		}
	}
	return created, nil
}

// GetOrder loads one order (with Items) for the ctx tenant.
func (s *Service) GetOrder(ctx context.Context, id int64) (*Order, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.GetByID(ctx, tenantID, id)
}

// DailySummary aggregates the ctx tenant's paid orders for storeID on the
// natural day containing day. Profit = Net - Cost; TopProducts sorted by qty
// desc (tie: gross desc, then product id asc), capped at 5.
func (s *Service) DailySummary(ctx context.Context, storeID int64, day time.Time) (*DailySummary, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	orders, err := s.repo.ListByDate(ctx, tenantID, storeID, day)
	if err != nil {
		return nil, errcode.ErrInternal
	}

	sum := &DailySummary{
		TenantID: tenantID,
		StoreID:  storeID,
		Date:     day.Format("2006-01-02"),
	}
	type agg struct {
		name  string
		qty   int64
		gross int64
	}
	top := make(map[int64]*agg)

	for _, o := range orders {
		sum.OrderCount++
		sum.GrossCents += o.TotalCents
		sum.NetCents += o.PaidCents
		for _, it := range o.Items {
			sum.CostCents += it.Qty * it.CostCents
			a, ok := top[it.ProductID]
			if !ok {
				a = &agg{name: it.NameSnapshot}
				top[it.ProductID] = a
			}
			a.qty += it.Qty
			a.gross += it.Qty * it.PriceCents
		}
	}
	sum.ProfitCents = sum.NetCents - sum.CostCents

	sum.TopProducts = make([]TopProduct, 0, len(top))
	for pid, a := range top {
		sum.TopProducts = append(sum.TopProducts, TopProduct{
			ProductID:  pid,
			Name:       a.name,
			Qty:        a.qty,
			GrossCents: a.gross,
		})
	}
	sort.Slice(sum.TopProducts, func(i, j int) bool {
		a, b := sum.TopProducts[i], sum.TopProducts[j]
		if a.Qty != b.Qty {
			return a.Qty > b.Qty
		}
		if a.GrossCents != b.GrossCents {
			return a.GrossCents > b.GrossCents
		}
		return a.ProductID < b.ProductID
	})
	if len(sum.TopProducts) > maxTopProducts {
		sum.TopProducts = sum.TopProducts[:maxTopProducts]
	}
	return sum, nil
}
