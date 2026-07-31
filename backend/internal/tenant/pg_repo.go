package tenant

// pg_repo.go implements the pgx-backed cross-tenant Repo (M8-B) over the narrow
// PGPool surface, the same idiom as store.PGPool / catalog.PGPool. A nil pool
// is a safe no-op so a misconfigured main thread degrades to an empty page
// rather than panicking.
//
// THIS IS THE ONE PLACE IN THE CODEBASE THAT LEGITIMATELY READS ACROSS TENANTS.
// Every other repo statement carries `WHERE tenant_id = $N` (ARCH-01). This
// repo's statements deliberately do NOT: the whole point of the platform view
// is to aggregate all merchants. The isolation guarantee is therefore moved UP
// a layer and enforced twice instead:
//
//   - ensurePlatform(ctx) at the top of ListAllTenants refuses to run unless
//     the ctx carries a platform/system Source (the Service stamps it via
//     platformCtx). A tenant-scoped ctx yields an empty page, never a leak.
//   - the HTTP router gates the whole /v1/platform group behind
//     RequireRole("platform_ops") (wired by the main thread).
//
// NOTE for the tenantcheck static analyzer (pkg/multitenancy/lint.go): this
// function issues raw queries WITHOUT calling EnforceTenant, which is the
// INTENTIONAL, documented exception to the per-tenant rule — cross-tenant
// aggregation cannot resolve a single tenant. The analyzer is not part of
// `go build`/`go test`; if it is ever wired into CI, this function is the
// sanctioned exception and ensurePlatform is its gate. Do not "fix" it by
// adding a tenant filter — that would break the platform view.
//
// Per-tenant counts are computed with correlated subqueries so a tenant with no
// rows in a table still appears with a 0 for that table (no LEFT JOIN fan-out).
// Nullable name filter uses ILIKE for case-insensitive substring match.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGPool is the narrow database surface the PG tenant repo needs, structurally
// identical to store.PGPool / catalog.PGPool so the main thread can share one
// *pgxpool.Pool across packages.
type PGPool interface {
	QueryRow(ctx context.Context, query string, args ...any) pgx.Row
	Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the optional multi-row read surface that the real *pgxpool.Pool
// and pgxmock expose on top of PGPool. The PGPool interface signature is frozen
// (no Query), so ListAllTenants type-asserts the concrete pool to queryer; a
// pool lacking Query returns an empty page rather than failing.
type queryer interface {
	Query(ctx context.Context, query string, args ...any) (pgx.Rows, error)
}

// sqlTenantList selects one page of the platform aggregate view across ALL
// tenants (no tenant_id filter — see the file header). Correlated subqueries
// supply per-tenant counts; deleted rows are excluded from both the tenant list
// and the counts (orders are never soft-deleted, so no deleted_at there).
// $1=status filter (” = all), $2=name ILIKE pattern (” = all), $3=limit,
// $4=offset. Ordered by id for deterministic paging.
const sqlTenantList = `SELECT t.id, t.name, t.status, t.plan, t.created_at,
  (SELECT COUNT(*) FROM products p WHERE p.tenant_id = t.id AND p.deleted_at IS NULL),
  (SELECT COUNT(*) FROM members m WHERE m.tenant_id = t.id AND m.deleted_at IS NULL),
  (SELECT COUNT(*) FROM orders o WHERE o.tenant_id = t.id),
  (SELECT COUNT(*) FROM stores s WHERE s.tenant_id = t.id AND s.deleted_at IS NULL)
FROM tenants t
WHERE t.deleted_at IS NULL
  AND ($1 = '' OR t.status = $1)
  AND ($2 = '' OR t.name ILIKE $2)
ORDER BY t.id
LIMIT $3 OFFSET $4`

// sqlTenantCount is the total-matching-count companion to sqlTenantList (same
// predicates, sans ORDER BY / LIMIT / OFFSET). It powers ListResult.Total so
// the admin pager can render "共 N 条" independent of the page window.
const sqlTenantCount = `SELECT COUNT(*)
FROM tenants t
WHERE t.deleted_at IS NULL
  AND ($1 = '' OR t.status = $1)
  AND ($2 = '' OR t.name ILIKE $2)`

// PGRepo is the pgx-backed cross-tenant Repo. A nil pool makes ListAllTenants
// return an empty page (no panic) so a misconfigured main thread degrades
// cleanly.
type PGRepo struct{ pool PGPool }

// NewPGRepo constructs a PGRepo over pool.
func NewPGRepo(pool PGPool) *PGRepo {
	return &PGRepo{pool: pool}
}

// ListAllTenants implements Repo. It returns one page of tenant summaries
// across ALL tenants matching f, ordered by id, plus the total matching count.
// It is a platform-level operation: it refuses to run without a platform/system
// Source (ensurePlatform) — a tenant ctx yields an empty page (ARCH-01). A nil
// pool / a pool lacking Query also degrades to an empty page.
func (r *PGRepo) ListAllTenants(ctx context.Context, f Filter) (ListResult, error) {
	empty := ListResult{Items: []*TenantSummary{}}
	// Platform gate: refuse cross-tenant reads for non-platform callers.
	if !ensurePlatform(ctx) {
		return empty, nil
	}
	if r == nil || r.pool == nil {
		return empty, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return empty, nil
	}
	f = f.Normalize()

	status := f.Status
	namePattern := ""
	if f.Query != "" {
		namePattern = "%" + f.Query + "%"
	}

	// 1) Total matching count (independent of the page window).
	var total int64
	if err := r.pool.QueryRow(ctx, sqlTenantCount, status, namePattern).Scan(&total); err != nil {
		return empty, fmt.Errorf("tenant: pg count: %w", err)
	}
	if total == 0 {
		return empty, nil
	}

	// 2) Paged aggregate select.
	rows, err := q.Query(ctx, sqlTenantList, status, namePattern, f.Limit, f.Offset)
	if err != nil {
		return empty, fmt.Errorf("tenant: pg list: %w", err)
	}
	defer rows.Close()

	items := make([]*TenantSummary, 0, f.Limit)
	for rows.Next() {
		var (
			ts                   TenantSummary
			prod, mem, ord, stor int64
		)
		if err := rows.Scan(
			&ts.ID, &ts.Name, &ts.Status, &ts.Plan, &ts.CreatedAt,
			&prod, &mem, &ord, &stor,
		); err != nil {
			return empty, fmt.Errorf("tenant: pg list scan: %w", err)
		}
		ts.ProductCount = int(prod)
		ts.MemberCount = int(mem)
		ts.OrderCount = int(ord)
		ts.StoreCount = int(stor)
		items = append(items, &ts)
	}
	if err := rows.Err(); err != nil {
		return empty, fmt.Errorf("tenant: pg list rows: %w", err)
	}
	return ListResult{Items: items, Total: total}, nil
}

// Compile-time assertion that PGRepo satisfies Repo.
var _ Repo = (*PGRepo)(nil)
