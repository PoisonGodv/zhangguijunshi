package attribution

import (
	"context"
	"sort"
	"sync"
	"time"
)

// memKey is the dedup key for the MemoryRepo store: the same
// (tenant,item,metric,measured_at) triple upserts in place, mirroring the
// strategy_attribution UNIQUE constraint.
type memKey struct {
	tenantID   int64
	itemID     int64
	metric     string
	measuredAt time.Time
}

// MemoryRepo is the in-memory dev/test Repo. It is concurrency-safe via
// sync.Mutex (-race clean) and enforces tenant isolation on every read:
// records owned by another tenant are treated as absent (never returned).
type MemoryRepo struct {
	mu      sync.Mutex
	nextID  int64
	records map[memKey]*Measurement
}

// NewMemoryRepo constructs an empty MemoryRepo.
func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{
		nextID:  1,
		records: make(map[memKey]*Measurement),
	}
}

// Record upserts one measurement under
// UNIQUE(tenantID,itemID,metric,measuredAt): an existing triple overwrites its
// value (and keeps its ID), a new triple allocates a fresh ID. tenantID is the
// first business param (multi-tenant row isolation). It returns a defensive
// copy of the stored record. ErrParam on an unknown metric.
func (r *MemoryRepo) Record(ctx context.Context, tenantID, itemID int64, metric string, value float64, measuredAt time.Time) (*Measurement, error) {
	if !validMetrics[metric] {
		return nil, ErrParam
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := memKey{tenantID: tenantID, itemID: itemID, metric: metric, measuredAt: measuredAt}
	if ex, ok := r.records[k]; ok {
		ex.Value = value
		cp := *ex
		return &cp, nil
	}
	m := &Measurement{
		ID:             r.nextID,
		TenantID:       tenantID,
		StrategyItemID: itemID,
		Metric:         metric,
		Value:          value,
		MeasuredAt:     measuredAt,
	}
	r.nextID++
	r.records[k] = m
	cp := *m
	return &cp, nil
}

// List returns the tenant's measurements matching f, ordered by
// (itemID, metric, measuredAt) for determinism. Cross-tenant records are never
// returned. Limit<=0 defaults to 50. Returns an empty (non-nil) slice when
// nothing matches.
func (r *MemoryRepo) List(ctx context.Context, tenantID int64, f MeasurementFilter) ([]Measurement, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	itemSet := make(map[int64]bool, len(f.ItemIDs))
	for _, id := range f.ItemIDs {
		itemSet[id] = true
	}
	metricSet := make(map[string]bool, len(f.Metrics))
	for _, m := range f.Metrics {
		metricSet[m] = true
	}

	r.mu.Lock()
	out := make([]Measurement, 0)
	for _, rec := range r.records {
		if rec.TenantID != tenantID {
			continue
		}
		if len(itemSet) > 0 && !itemSet[rec.StrategyItemID] {
			continue
		}
		if len(metricSet) > 0 && !metricSet[rec.Metric] {
			continue
		}
		out = append(out, *rec)
	}
	r.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].StrategyItemID != out[j].StrategyItemID {
			return out[i].StrategyItemID < out[j].StrategyItemID
		}
		if out[i].Metric != out[j].Metric {
			return out[i].Metric < out[j].Metric
		}
		return out[i].MeasuredAt.Before(out[j].MeasuredAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// LatestPerMetric returns, for one item, the most recent value per metric.
// Metrics with no record are omitted (not zero). Cross-tenant items yield an
// empty (non-nil) map — absence, never a leak.
func (r *MemoryRepo) LatestPerMetric(ctx context.Context, tenantID, itemID int64) (map[string]float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	latest := make(map[string]time.Time)
	out := make(map[string]float64)
	for _, rec := range r.records {
		if rec.TenantID != tenantID || rec.StrategyItemID != itemID {
			continue
		}
		if t, ok := latest[rec.Metric]; !ok || rec.MeasuredAt.After(t) {
			latest[rec.Metric] = rec.MeasuredAt
			out[rec.Metric] = rec.Value
		}
	}
	return out, nil
}
