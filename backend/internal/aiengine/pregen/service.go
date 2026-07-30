package pregen

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/semantic"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
	"go.uber.org/zap"
)

// runAndCache is the single-(tenant,store) pre-gen primitive shared by
// BatchGenerate and IncrementalRetry (§7.7.2): inject the tenant+store identity
// into ctx (Source=system — this is a background job, not a user request), run
// the pipeline, then write the resulting Advice into the L1 exact cache and the
// L2 semantic cache (TTL 24h). Cache write failures are swallowed (a failed
// write just means the next run is cold), matching the pipeline's own cache.Put
// semantics (CONTRACT §12.3.4 "Put must not block the pipeline on failure").
//
// tenant AND store are NEVER trusted from the Advice payload — both are injected
// into ctx and the pipeline re-derives them via EnforceTenant /
// FromContext.StoreID. StoreID MUST be carried in ctx because pipeline.Run reads
// it from multitenancy.Context.StoreID (defaulting to 0, which the strategy
// domain rejects) — this is precisely the single-store resolution gap this
// helper closes. The Strategy.TenantID / StoreID carried in the Advice are only
// used as defensive cache-write keys.
func (s *Scheduler) runAndCache(ctx context.Context, tenantID, storeID int64) error {
	if s.pipeline == nil {
		return fmt.Errorf("pregen: pipeline runner not configured")
	}
	pctx := multitenancy.WithContext(ctx, multitenancy.Context{
		TenantID: tenantID,
		StoreID:  storeID,
		Source:   multitenancy.SourceSystem,
	})
	advice, err := s.pipeline.Run(pctx)
	if err != nil {
		return fmt.Errorf("pipeline run: %w", err)
	}
	if advice == nil || advice.Strategy == nil {
		return fmt.Errorf("pipeline returned nil advice for tenant %d store %d", tenantID, storeID)
	}
	strat := advice.Strategy

	// L1 exact cache: keyed by (tenant, store, date). Idempotent overwrite —
	// the pipeline also puts L1 on a cold run, but pre-gen re-puts so this
	// package is self-sufficient regardless of the pipeline's cache wiring.
	// The cache key uses the pipeline output's StoreID (strat.StoreID), which
	// for a correctly-resolved run equals the injected storeID.
	if s.ch != nil {
		date := strat.ForDate
		if date == "" {
			date = s.now().Format("2006-01-02")
		}
		s.ch.Put(pctx, strat.TenantID, strat.StoreID, date, strat)
	}

	// L2 semantic cache: keyed by the embedding of the signal text. We rebuild
	// the signal text from the Strategy.Signals snapshot so pre-gen's L2 write
	// key matches the pipeline's L2 read key (semantic.BuildSignalText). A
	// missing/unparseable snapshot yields ("unknown","") which the semantic
	// cache treats as a guaranteed miss — safe but pessimistic.
	if s.sem != nil {
		business, signalText := s.deriveSemanticKey(strat.Signals)
		if perr := s.sem.Put(pctx, strat.TenantID, business, signalText, strat, semanticCacheTTL); perr != nil {
			s.log.Debug("pregen semantic Put failed (non-fatal)",
				zap.Int64("tenant_id", tenantID), zap.Error(perr))
		}
	}
	return nil
}

// storeRunner runs the pipeline for a single (tenant, store) pair. Batch/refresh
// callers pass runWithRetry (retry on the exponential backoff); the incremental
// drain passes runAndCache directly to preserve its single-attempt contract.
type storeRunner func(ctx context.Context, tenantID, storeID int64) error

// runTenantStores runs runner once per active store of tenantID and aggregates
// (processed, failed) counts. When a StoreEnumerator is wired and returns one or
// more stores, runner is invoked per store with that StoreID (so pipeline.Run
// resolves a real store instead of the rejected storeID=0). When the enumerator
// is nil, OR returns zero stores, OR errors, it falls back to ONE run with
// storeID=0 — preserving dev / no-store behaviour and keeping M3-S2/S3 callers
// backward-compatible. The runner controls retry semantics; enumeration errors
// are logged and degrade to the storeID=0 fallback (never abort the caller).
func (s *Scheduler) runTenantStores(ctx context.Context, tenantID int64, runner storeRunner) (processed, failed int) {
	var storeIDs []int64
	if s.storeEnum != nil {
		ids, err := s.storeEnum.ActiveStoreIDs(ctx, tenantID)
		if err != nil {
			s.log.Warn("pregen store enumeration failed; falling back to storeID=0",
				zap.Int64("tenant_id", tenantID), zap.Error(err))
		} else {
			storeIDs = ids
		}
	}
	if len(storeIDs) == 0 {
		// Legacy / no-store fallback: a single run with storeID=0.
		if err := runner(ctx, tenantID, 0); err != nil {
			return 0, 1
		}
		return 1, 0
	}
	for _, sid := range storeIDs {
		if err := runner(ctx, tenantID, sid); err != nil {
			failed++
			continue
		}
		processed++
	}
	return processed, failed
}

// runWithRetry wraps runAndCache with up to len(backoffs) retries on failure
// (§7.7.4). The backoff schedule defaults to {1s,2s,4s} (≤3 retries); tests
// inject near-zero durations. The ctx is consulted between attempts so a
// cancelled batch aborts promptly rather than sleeping through shutdown.
func (s *Scheduler) runWithRetry(ctx context.Context, tenantID, storeID int64) error {
	var lastErr error
	for attempt := 0; attempt <= len(s.backoffs); attempt++ {
		if err := s.runAndCache(ctx, tenantID, storeID); err != nil {
			lastErr = err
			s.log.Warn("pregen attempt failed",
				zap.Int64("tenant_id", tenantID),
				zap.Int64("store_id", storeID),
				zap.Int("attempt", attempt+1),
				zap.Error(err))
			if attempt == len(s.backoffs) {
				break // no more retries
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.backoffs[attempt]):
			}
			continue
		}
		return nil
	}
	return lastErr
}

// deriveSemanticKey rebuilds (business, signalText) from the strategy's Signals
// snapshot (the marshalled signal.Bundle) so pre-gen's L2 write keys match the
// pipeline's L2 read keys (semantic.BuildSignalText over the same Bundle). A
// missing/unparseable snapshot yields ("unknown","") which the semantic cache
// treats as a guaranteed miss on subsequent TryHit — safe but pessimistic.
func (s *Scheduler) deriveSemanticKey(raw json.RawMessage) (business, signalText string) {
	business = "unknown"
	if len(raw) == 0 {
		return business, ""
	}
	var b signal.Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return business, ""
	}
	signalText = semantic.BuildSignalText(&b)
	if trimmed := strings.TrimSpace(b.BusinessType); trimmed != "" {
		business = strings.ToLower(trimmed)
	}
	return business, signalText
}
