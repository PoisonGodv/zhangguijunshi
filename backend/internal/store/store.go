// Package store implements the lightweight C-side store (门店) read domain
// (FE-MP-02 / FE-ADMIN, CONTRACT §16.11). A Store is the consumer-facing
// subset of a merchant's physical shop: name, address, contact phone and the
// community group QR that drives 私域引流. The domain is read-only from the
// C 端 surface — creation / editing stays in the boss/admin store-management
// surface (a later milestone).
//
// The package mirrors the membership / marketing convention: every repo /
// service method resolves the tenant from ctx via multitenancy.EnforceTenant
// — tenant is NEVER trusted from a request field (ARCH-01). It is
// infrastructure-light and anti-corruption-clean: it imports neither catalog
// nor membership. A concurrency-safe in-memory implementation covers dev and
// tests; a pgx-backed implementation (reading the existing stores table from
// migration 000001, plus the phone / group_qr columns added by 000016) covers
// production.
package store

import (
	"context"
	"time"
)

// Store is the C-side 门店 read model. It is the consumer-facing subset of the
// physical shop: the name, address, contact phone and community-group QR. The
// full stores schema (business_type / lng / lat / wework_corp_id / province /
// city / district) lives in migration 000001 and is NOT surfaced here — the C
// 端 never sees internal compliance / LBS fields.
type Store struct {
	ID, TenantID int64
	Name         string
	Address      string
	Phone        string
	GroupQR      string
	Active       bool
	CreatedAt    time.Time
}

// StoreRepo persists stores. The real implementation is pgx-backed (the stores
// table from migration 000001, plus the phone / group_qr columns from 000016);
// an in-memory implementation covers dev and tests. Every method treats a
// record owned by another tenant as absent (empty / nil).
type StoreRepo interface {
	// ListStores returns the active stores of tenantID. Cross-tenant stores
	// are never returned. Inactive stores are filtered out by default.
	ListStores(ctx context.Context, tenantID int64) ([]*Store, error)
}
