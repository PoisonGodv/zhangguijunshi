package bandit

// pg_repo.go implements PGRepo — the production Arm store backed by the
// ai_rule_weight table (migration 000010, CONTRACT §14.3.3 / §14.5.4 dual
// implementation). The DB dependency is the narrow self-declared PGPool
// interface (QueryRow / Exec), exactly the subset of *pgxpool.Pool / pgx.Tx
// the repo uses, so tests run against pgxmock and the package never
// hard-imports pgxpool (same范式 as semantic.Pool / attribution.PGPool).
//
// Tenant isolation lives in every statement's WHERE tenant_id = $1: a read or
// list is always scoped, so another tenant's weights are never returned. An
// unseen rule (QueryRow → pgx.ErrNoRows) maps to the neutral-prior zero-value
// Arm{RuleID, Weight:0.5}, identical to MemoryRepo.

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGPool is the narrow database surface PGRepo needs (CONTRACT §14.3.3). It
// is structurally identical to attribution.PGPool, so the main thread can
// hand one *pgxpool.Pool to both packages.
type PGPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// SQL statements against ai_rule_weight (migration 000010). All are scoped by
// tenant_id. Put is an upsert on the UNIQUE(tenant_id, rule_id) constraint.
const (
	// sqlGet selects one arm; ErrNoRows → neutral prior (unseen rule).
	sqlGet = `SELECT rule_id, weight, trials, reward_sum FROM ai_rule_weight WHERE tenant_id = $1 AND rule_id = $2`

	// sqlPut upserts an arm; updated_at is refreshed by the
	// trg_set_updated_at trigger (migration 000010) on the UPDATE branch.
	sqlPut = `INSERT INTO ai_rule_weight (tenant_id, rule_id, weight, trials, reward_sum)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id, rule_id)
DO UPDATE SET weight = EXCLUDED.weight, trials = EXCLUDED.trials, reward_sum = EXCLUDED.reward_sum`

	// sqlAll lists every arm for a tenant, ordered by rule_id for a
	// deterministic route-bias application. Multi-row iteration needs the
	// Query method, which *pgxpool.Pool / pgxmock provide on top of the
	// frozen PGPool surface; All type-asserts to queryer for it.
	sqlAll = `SELECT rule_id, weight, trials, reward_sum FROM ai_rule_weight WHERE tenant_id = $1 ORDER BY rule_id`
)

// queryer is the optional multi-row surface that the real *pgxpool.Pool and
// pgxmock expose in addition to the frozen PGPool methods (QueryRow/Exec,
// CONTRACT §14.3.3). PGPool itself does not declare Query (signature frozen),
// so All type-asserts the concrete pool to queryer. A pool that only
// satisfies PGPool (a hand-rolled fake) makes All return an empty slice —
// cold-start safe rather than a hard failure.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PGRepo is the pgx-backed Repo (CONTRACT §14.3.3). A nil pool makes every
// method a safe no-op (Get → neutral prior, Put → nil, All → empty) so a
// misconfigured main thread degrades to cold-start behaviour instead of
// crashing the pipeline.
type PGRepo struct {
	pool PGPool
}

// NewPGRepo constructs a PGRepo over pool (a *pgxpool.Pool or pgx.Tx in
// production; pgxmock in tests).
func NewPGRepo(pool PGPool) *PGRepo {
	return &PGRepo{pool: pool}
}

// Get returns the arm for (tenantID, ruleID). pgx.ErrNoRows (unseen rule or
// cross-tenant) maps to the neutral-prior Arm{RuleID, Weight:0.5} — cold-start
// safe and leak-free (§14.3.3 / §14.5.8).
func (r *PGRepo) Get(ctx context.Context, tenantID int64, ruleID string) (*Arm, error) {
	if r == nil || r.pool == nil {
		return &Arm{RuleID: ruleID, Weight: neutralPrior}, nil
	}
	var a Arm
	err := r.pool.QueryRow(ctx, sqlGet, tenantID, ruleID).
		Scan(&a.RuleID, &a.Weight, &a.Trials, &a.RewardSum)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &Arm{RuleID: ruleID, Weight: neutralPrior}, nil
		}
		return nil, err
	}
	return &a, nil
}

// Put upserts arm under (tenantID, arm.RuleID) via INSERT ... ON CONFLICT
// (tenant_id, rule_id) DO UPDATE (§14.3.3). A nil arm is a no-op.
func (r *PGRepo) Put(ctx context.Context, tenantID int64, arm *Arm) error {
	if r == nil || r.pool == nil || arm == nil {
		return nil
	}
	_, err := r.pool.Exec(ctx, sqlPut, tenantID, arm.RuleID, arm.Weight, arm.Trials, arm.RewardSum)
	return err
}

// All lists every arm for tenantID ordered by rule_id (deterministic for
// route bias). A tenant with no rows returns an empty (non-nil) slice. It
// type-asserts the pool to the optional queryer interface for multi-row
// iteration (PGPool only declares QueryRow/Exec); a pool lacking Query
// returns an empty slice rather than failing.
func (r *PGRepo) All(ctx context.Context, tenantID int64) ([]Arm, error) {
	out := []Arm{}
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlAll, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Arm
		if err := rows.Scan(&a.RuleID, &a.Weight, &a.Trials, &a.RewardSum); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
