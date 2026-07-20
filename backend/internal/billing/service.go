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
