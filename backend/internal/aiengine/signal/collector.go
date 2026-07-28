package signal

import (
	"context"

	"golang.org/x/sync/errgroup"
)

// Collect aggregates today's signals concurrently. It NEVER returns a nil
// Bundle — even when the Lookuper is nil or every path fails, a zero Bundle is
// returned so the Pipeline can detect cold-start (0 routed items downstream).
//
// Each Lookuper path is best-effort: a failure (or nil Lookuper) degrades to an
// empty/zero subset for that path without aborting the others (errors are
// swallowed inside each goroutine, so errgroup.Wait never short-circuits).
// Weather/holiday are in-memory mocks (external APIs deferred to M3-S2+).
func (s *Service) Collect(ctx context.Context, tenantID, storeID int64) (*Bundle, error) {
	var (
		inv []InventorySig
		mem MemberStats
		txn TxnSig
	)

	if s.lk != nil {
		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() error {
			v, err := s.lk.Inventory(gctx, tenantID, storeID)
			if err != nil {
				return nil // swallow → empty subset, do not abort siblings
			}
			inv = v
			return nil
		})
		g.Go(func() error {
			v, err := s.lk.Members(gctx, tenantID)
			if err != nil {
				return nil
			}
			mem = v
			return nil
		})
		g.Go(func() error {
			date := s.now().Format("2006-01-02")
			v, err := s.lk.DailyTxn(gctx, tenantID, storeID, date)
			if err != nil {
				return nil
			}
			txn = v
			return nil
		})
		_ = g.Wait() // each goroutine swallows its error; Wait is always nil here
	}

	bundle := &Bundle{
		TenantID:     tenantID,
		StoreID:      storeID,
		BusinessType: s.businessType,
		City:         s.city,
		Date:         s.now(),
		Weather:      s.mockWeather(),
		Holiday:      s.mockHoliday(),
		Inventory:    inv,
		Members:      mem,
		DataQuality:  computeDataQuality(inv, mem, txn),
	}
	return bundle, nil
}
