package catalog

// pg_repo.go implements the pgx-backed ProductRepo (CONTRACT §17.2) over the
// narrow PGPool surface, the same idiom as payment.PGPool. A nil pool is a
// safe no-op so a misconfigured main thread degrades to ErrNotFound / empty
// rather than panicking.
//
// Tenant isolation lives in every statement's WHERE tenant_id = $1. Nullable
// columns (barcode / category / image_url / shelf_life_days) are scanned via
// database/sql Null types; the tags TEXT[] column round-trips as []string.
// An empty barcode is stored as NULL so it never participates in the partial
// UNIQUE (..., barcode) WHERE barcode IS NOT NULL index — matching the memory
// repo's "empty barcode = no barcode" uniqueness contract. PG unique-violation
// (sqlstate 23505) maps to ErrConflict, mirroring the memory repo.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGPool is the narrow database surface the PG repos need (CONTRACT §17.2).
// Structurally identical to payment.PGPool so the main thread can share one
// *pgxpool.Pool across packages.
type PGPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the optional multi-row read surface that the real *pgxpool.Pool
// and pgxmock expose on top of PGPool. The PGPool interface signature is frozen
// (no Query), so List type-asserts the concrete pool to queryer; a pool lacking
// Query returns an empty slice rather than failing.
type queryer interface {
	Query(ctx context.Context, q string, args ...any) (pgx.Rows, error)
}

// rowScanner is the common Scan surface of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// SQL for products (migration 000002). Create / Update RETURNING the full row
// so DB-allocated timestamps / defaults round-trip authoritatively. List uses
// static optional-filter predicates ($N IS empty/0 → match all) so the SQL text
// stays constant — same idiom as aiengine/attribution. Every WHERE carries
// tenant_id.
const (
	sqlProductCreate = `INSERT INTO products (tenant_id, store_id, barcode, name, category, unit, cost_cents, price_cents, image_url, shelf_life_days, tags, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING id, tenant_id, store_id, barcode, name, category, unit, cost_cents, price_cents, image_url, shelf_life_days, tags, status, created_at, updated_at`
	sqlProductUpdate = `UPDATE products
SET store_id = $1, barcode = $2, name = $3, category = $4, unit = $5, cost_cents = $6, price_cents = $7, image_url = $8, shelf_life_days = $9, tags = $10, status = $11, updated_at = now()
WHERE tenant_id = $12 AND id = $13
RETURNING id, tenant_id, store_id, barcode, name, category, unit, cost_cents, price_cents, image_url, shelf_life_days, tags, status, created_at, updated_at`
	sqlProductGetByID = `SELECT id, tenant_id, store_id, barcode, name, category, unit, cost_cents, price_cents, image_url, shelf_life_days, tags, status, created_at, updated_at
FROM products
WHERE tenant_id = $1 AND id = $2`
	sqlProductGetByBarcode = `SELECT id, tenant_id, store_id, barcode, name, category, unit, cost_cents, price_cents, image_url, shelf_life_days, tags, status, created_at, updated_at
FROM products
WHERE tenant_id = $1 AND store_id = $2 AND barcode = $3`
	sqlProductList = `SELECT id, tenant_id, store_id, barcode, name, category, unit, cost_cents, price_cents, image_url, shelf_life_days, tags, status, created_at, updated_at
FROM products
WHERE tenant_id = $1
  AND ($2 = 0 OR store_id = $2)
  AND ($3 = '' OR category = $3)
  AND ($4 = '' OR position($4 in name) > 0 OR position($4 in barcode) > 0)
ORDER BY id
LIMIT $5 OFFSET $6`
	// sqlProductUpdateTags touches ONLY the tags column (BE-A-09 write-back); it
	// never rewrites price/cost/name, so it cannot trip the uniqueness indexes.
	// tags is the TEXT[] column from migration 000002 (round-trips as []string).
	sqlProductUpdateTags = `UPDATE products
SET tags = $1, updated_at = now()
WHERE tenant_id = $2 AND id = $3
RETURNING id, tenant_id, store_id, barcode, name, category, unit, cost_cents, price_cents, image_url, shelf_life_days, tags, status, created_at, updated_at`
)

// ===== scan helpers =====

// scanProduct decodes one products row (TEXT[] tags → []string; nullable text
// columns collapse empty/NULL to "").
func scanProduct(s rowScanner) (*Product, error) {
	var p Product
	var barcode, category, imageURL sql.NullString
	var shelf sql.NullInt64
	var tags []string
	if err := s.Scan(
		&p.ID, &p.TenantID, &p.StoreID, &barcode, &p.Name, &category, &p.Unit,
		&p.CostCents, &p.PriceCents, &imageURL, &shelf, &tags, &p.Status,
		&p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		return nil, err
	}
	p.Barcode = barcode.String
	p.Category = category.String
	p.ImageURL = imageURL.String
	if shelf.Valid {
		p.ShelfLifeDays = int(shelf.Int64)
	}
	p.Tags = tags
	return &p, nil
}

// nullInt64 converts an int shelf-life to sql.NullInt64 (0 → NULL = 未设置).
func nullInt64(n int) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(n), Valid: true}
}

// nullBarcode converts an empty barcode to nil so it never participates in the
// partial UNIQUE (tenant_id, store_id, barcode) WHERE barcode IS NOT NULL index
// (memory repo's "empty barcode = no barcode" contract).
func nullBarcode(barcode string) any {
	if barcode == "" {
		return nil
	}
	return barcode
}

// isUniqueViolation reports whether err is a PG unique-violation (sqlstate
// 23505), mapped to ErrConflict.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ===== Product =====

// PGProductRepo is the pgx-backed ProductRepo. A nil pool makes reads return
// ErrNotFound / empty and writes return ErrNotFound.
type PGProductRepo struct{ pool PGPool }

// NewPGProductRepo constructs a PGProductRepo over pool.
func NewPGProductRepo(pool PGPool) *PGProductRepo { return &PGProductRepo{pool: pool} }

// Create inserts p and returns the persisted record. A uniqueness violation
// ((tenant, store, barcode) for non-empty barcode, or (tenant, store, name,
// unit)) returns ErrConflict; a nil pool returns ErrNotFound.
func (r *PGProductRepo) Create(ctx context.Context, p *Product) (*Product, error) {
	if r == nil || r.pool == nil || p == nil {
		return nil, ErrNotFound
	}
	out, err := scanProduct(r.pool.QueryRow(ctx, sqlProductCreate,
		p.TenantID, p.StoreID, nullBarcode(p.Barcode), p.Name, p.Category, p.Unit,
		p.CostCents, p.PriceCents, p.ImageURL, nullInt64(p.ShelfLifeDays), p.Tags, p.Status,
	))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("catalog: pg product create: %w", err)
	}
	return out, nil
}

// Update replaces the whole product addressed by p.ID within p.TenantID.
// ErrNotFound when absent or cross-tenant; ErrConflict on uniqueness violation.
func (r *PGProductRepo) Update(ctx context.Context, p *Product) (*Product, error) {
	if r == nil || r.pool == nil || p == nil {
		return nil, ErrNotFound
	}
	out, err := scanProduct(r.pool.QueryRow(ctx, sqlProductUpdate,
		p.StoreID, nullBarcode(p.Barcode), p.Name, p.Category, p.Unit,
		p.CostCents, p.PriceCents, p.ImageURL, nullInt64(p.ShelfLifeDays), p.Tags, p.Status,
		p.TenantID, p.ID,
	))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("catalog: pg product update: %w", err)
	}
	return out, nil
}

// GetByID loads a product by id within tenantID; ErrNotFound if absent or
// cross-tenant.
func (r *PGProductRepo) GetByID(ctx context.Context, tenantID, id int64) (*Product, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	p, err := scanProduct(r.pool.QueryRow(ctx, sqlProductGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("catalog: pg product get: %w", err)
	}
	return p, nil
}

// GetByBarcode loads a product by barcode within (tenantID, storeID); ErrNotFound
// if absent or cross-tenant.
func (r *PGProductRepo) GetByBarcode(ctx context.Context, tenantID, storeID int64, barcode string) (*Product, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	p, err := scanProduct(r.pool.QueryRow(ctx, sqlProductGetByBarcode, tenantID, storeID, barcode))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("catalog: pg product get by barcode: %w", err)
	}
	return p, nil
}

// List returns products within tenantID matching f (StoreID / Category / Query
// filters applied in SQL; Query is a case-sensitive substring over name or
// barcode, matching the memory repo). Results are id-ordered for deterministic
// pagination. A nil pool (or one without Query) returns an empty slice.
func (r *PGProductRepo) List(ctx context.Context, tenantID int64, f ProductFilter) ([]*Product, error) {
	out := make([]*Product, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	rows, err := q.Query(ctx, sqlProductList, tenantID, f.StoreID, f.Category, f.Query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("catalog: pg product list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, fmt.Errorf("catalog: pg product list scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: pg product list rows: %w", err)
	}
	return out, nil
}

// UpdateTags replaces ONLY the tags column of the product addressed by productID
// within tenantID (BE-A-09 write-back) and returns the updated record. It leaves
// price/cost/name untouched, so it cannot trip the uniqueness indexes.
// ErrNotFound when absent or cross-tenant; a nil pool returns ErrNotFound.
func (r *PGProductRepo) UpdateTags(ctx context.Context, tenantID, productID int64, tags []string) (*Product, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	out, err := scanProduct(r.pool.QueryRow(ctx, sqlProductUpdateTags, tags, tenantID, productID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("catalog: pg product update tags: %w", err)
	}
	return out, nil
}
