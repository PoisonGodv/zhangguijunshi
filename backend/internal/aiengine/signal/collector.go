package signal

import (
	"context"
	"time"

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
//
// M7-B: when a SalesProvider is wired, the fused per-product daily sales over
// the lookback window are read (best-effort, concurrent with the Lookuper
// paths) and aggregated into Bundle.Sales; the inventory SlowMoving flag is
// then merged from the real sales history. A nil provider / provider error /
// empty window leaves Bundle.Sales nil and inventory untouched (cold-start
// safe).
func (s *Service) Collect(ctx context.Context, tenantID, storeID int64) (*Bundle, error) {
	var (
		inv   []InventorySig
		mem   MemberStats
		txn   TxnSig
		sales []SalesSig
	)

	now := s.now()

	g, gctx := errgroup.WithContext(ctx)
	if s.lk != nil {
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
			date := now.Format("2006-01-02")
			v, err := s.lk.DailyTxn(gctx, tenantID, storeID, date)
			if err != nil {
				return nil
			}
			txn = v
			return nil
		})
	}
	// M7-B sales path (independent of the Lookuper): read the lookback window
	// [from, to] ending yesterday (today's fused sales are not computed until
	// the next cron sweep) and aggregate into per-product sales signals.
	if s.salesProv != nil {
		g.Go(func() error {
			to := truncateDay(now).AddDate(0, 0, -1)
			from := to.AddDate(0, 0, -(s.salesCfg.windowDays - 1))
			rows, err := s.salesProv.DailySales(gctx, tenantID, storeID, from, to)
			if err != nil {
				return nil // swallow → no sales signal, do not abort siblings
			}
			sales = aggregateSales(rows, from, to, s.salesCfg)
			return nil
		})
	}
	_ = g.Wait() // each goroutine swallows its error; Wait is always nil here

	// Merge the real sales history onto the inventory signals: a product with
	// window sales <= threshold is slow-moving. No-op when sales is nil.
	inv = markSlowMoving(inv, sales, s.salesCfg.slowThreshold)

	bundle := &Bundle{
		TenantID:     tenantID,
		StoreID:      storeID,
		BusinessType: s.businessType,
		City:         s.city,
		Date:         now,
		Weather:      s.mockWeather(),
		Holiday:      s.mockHoliday(),
		Inventory:    inv,
		Members:      mem,
		Sales:        sales,
		DataQuality:  computeDataQuality(inv, mem, txn, sales),
	}
	return bundle, nil
}

// truncateDay returns t truncated to its natural day in its own location.
func truncateDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
