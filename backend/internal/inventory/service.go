package inventory

// Service is the inventory use-case layer. Every method first resolves the
// tenant from ctx via multitenancy.EnforceTenant (ARCH-01): tenant NEVER comes
// from a request body or parameter. The store scope falls back to the store
// carried in ctx (multitenancy.Context.StoreID) when the caller passes zero.
//
// The service depends only on the BatchRepo and ProductLookuper interfaces plus
// an injectable clock (WithNowFunc), so the whole flow is deterministic in unit
// tests (fake lookuper + fixed now). ConsumeFIFO is atomic by contract: it
// dry-runs the deduction and refuses with ErrInsufficientStock before touching
// any batch, so a caller never observes a partial deduction.

import (
	"context"
	"errors"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Service exposes inventory use-cases.
type Service struct {
	repo     BatchRepo
	lookuper ProductLookuper
	now      func() time.Time
}

// Option customises a Service (primarily for deterministic tests).
type Option func(*Service)

// WithNowFunc overrides the clock (deterministic CreatedAt/UpdatedAt in tests).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// New constructs an inventory Service. A nil lookuper is tolerated (PurchaseIn
// then skips the product-existence check) so the service can be exercised in
// isolation; production wiring always supplies one. A nil clock falls back to
// time.Now.
func New(repo BatchRepo, lookuper ProductLookuper, opts ...Option) *Service {
	s := &Service{
		repo:     repo,
		lookuper: lookuper,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// PurchaseIn records a stock-in for the ctx tenant. It validates the product
// belongs to this tenant (via ProductLookuper; cross-tenant / unknown product
// -> ErrParam so no tenant boundary is leaked), enforces qty > 0 and
// in_price >= 0, creates an active batch, and appends one purchase_in movement.
// The batch status starts active; nearing/expired transitions are owned by the
// BE-A-09 tags job (this method only records the expiry dates).
func (s *Service) PurchaseIn(ctx context.Context, in PurchaseInInput) (*Batch, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if in.Quantity <= 0 || in.InPriceCents < 0 {
		return nil, errcode.ErrParam
	}

	if s.lookuper != nil {
		ok, err := s.lookuper.Exists(ctx, tenantID, in.ProductID)
		if err != nil {
			return nil, errcode.ErrInternal
		}
		if !ok {
			// Product unknown to THIS tenant (absent or cross-tenant): reject
			// with ErrParam rather than a catalogue not-found so inventory never
			// reveals whether the product exists under another tenant.
			return nil, errcode.ErrParam
		}
	}

	storeID := in.StoreID
	if storeID == 0 {
		if mc, ok := multitenancy.FromContext(ctx); ok {
			storeID = mc.StoreID
		}
	}

	now := s.now()
	batch := &Batch{
		TenantID:     tenantID,
		StoreID:      storeID,
		ProductID:    in.ProductID,
		Quantity:     in.Quantity,
		InPriceCents: in.InPriceCents,
		ExpiryDate:   in.ExpiryDate,
		Status:       BatchStatusActive,
	}
	created, err := s.repo.CreateBatch(ctx, batch)
	if err != nil {
		return nil, errcode.ErrInternal
	}

	var operatorID int64
	if mc, ok := multitenancy.FromContext(ctx); ok {
		operatorID = mc.UserID
	}
	mv := &Movement{
		TenantID:   tenantID,
		StoreID:    storeID,
		ProductID:  in.ProductID,
		BatchID:    created.ID,
		Type:       MovementTypePurchaseIn,
		Quantity:   in.Quantity,
		OperatorID: operatorID,
		Note:       in.Note,
		CreatedAt:  now,
	}
	if _, err := s.repo.AppendMovement(ctx, mv); err != nil {
		return nil, errcode.ErrInternal
	}
	return created, nil
}

// ConsumeFIFO deducts qty units of one product from the ctx tenant's active
// batches, oldest first (FIFO). Each deduction appends one sale_out movement
// (negative quantity) linked to the batch it drew from and to refOrderID.
//
// ATOMICITY: the deduction never partially applies. When the repo provides the
// optional StockConsumer capability (the in-memory repo does, under one write
// lock), the whole read-check-deduct-record cycle is atomic and race-free.
// Otherwise the service falls back to a dry-run-then-apply sequence against the
// narrow BatchRepo; the dry-run still guarantees no partial deduction is
// observable on the single-tenant path, but concurrent safety then depends on
// the repo's own serialisation.
func (s *Service) ConsumeFIFO(ctx context.Context, storeID, productID, qty int64, refOrderID int64) ([]*Batch, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if qty <= 0 {
		return nil, errcode.ErrParam
	}

	now := s.now()
	var operatorID int64
	if mc, ok := multitenancy.FromContext(ctx); ok {
		operatorID = mc.UserID
	}
	// mkMovement builds the sale_out ledger line for one deducted batch; the
	// repo overrides tenant/store/product/type defensively.
	mkMovement := func(batchID, deducted int64) *Movement {
		return &Movement{
			BatchID:    batchID,
			Quantity:   -deducted,
			RefOrderID: refOrderID,
			OperatorID: operatorID,
			CreatedAt:  now,
		}
	}

	// Fast path: repo offers an atomic consume -> single lock, race-free.
	if sc, ok := s.repo.(StockConsumer); ok {
		updated, err := sc.ConsumeFIFOAtomic(ctx, tenantID, storeID, productID, qty, mkMovement)
		if err != nil {
			if errors.Is(err, ErrInsufficientStock) {
				return nil, ErrInsufficientStock
			}
			return nil, errcode.ErrInternal
		}
		return updated, nil
	}

	// Fallback: dry-run against the narrow BatchRepo, then apply. (Used only by
	// repos that do not implement StockConsumer.)
	batches, err := s.repo.ListActiveBatchesFIFO(ctx, tenantID, storeID, productID)
	if err != nil {
		return nil, errcode.ErrInternal
	}
	type plan struct {
		batch  *Batch
		deduct int64
	}
	plans := make([]plan, 0, len(batches))
	remaining := qty
	for _, b := range batches {
		if remaining == 0 {
			break
		}
		d := b.Quantity
		if d > remaining {
			d = remaining
		}
		plans = append(plans, plan{batch: b, deduct: d})
		remaining -= d
	}
	if remaining > 0 {
		return nil, ErrInsufficientStock
	}

	updated := make([]*Batch, 0, len(plans))
	for _, p := range plans {
		nb := p.batch // already a defensive copy from ListActiveBatchesFIFO
		nb.Quantity -= p.deduct
		if nb.Quantity == 0 {
			// A fully drained lot is marked cleared (it drops out of the active
			// FIFO set and stock counts) rather than kept active with zero
			// quantity. Either is acceptable; cleared is the explicit terminal.
			nb.Status = BatchStatusCleared
		}
		ub, err := s.repo.UpdateBatch(ctx, nb)
		if err != nil {
			return nil, errcode.ErrInternal
		}
		mv := mkMovement(nb.ID, p.deduct)
		mv.TenantID = tenantID
		mv.StoreID = storeID
		mv.ProductID = productID
		mv.Type = MovementTypeSaleOut
		if _, err := s.repo.AppendMovement(ctx, mv); err != nil {
			return nil, errcode.ErrInternal
		}
		updated = append(updated, ub)
	}
	return updated, nil
}

// Stock returns the total remaining quantity across all active batches of one
// product within (ctx tenant, storeID). Cleared/expired/empty lots contribute 0.
func (s *Service) Stock(ctx context.Context, storeID, productID int64) (int64, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return 0, err
	}
	batches, err := s.repo.ListActiveBatchesFIFO(ctx, tenantID, storeID, productID)
	if err != nil {
		return 0, errcode.ErrInternal
	}
	var total int64
	for _, b := range batches {
		total += b.Quantity
	}
	return total, nil
}

// ListMovements returns the ctx tenant's ledger entries matching f.
func (s *Service) ListMovements(ctx context.Context, f MovementFilter) ([]*Movement, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	movements, err := s.repo.ListMovements(ctx, tenantID, f)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return movements, nil
}

// mapRepoErr translates repository errors into errcode sentinels. ErrNotFound
// is passed through so the HTTP layer can emit 404; anything else is internal.
func mapRepoErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return ErrNotFound
	default:
		return errcode.ErrInternal
	}
}
