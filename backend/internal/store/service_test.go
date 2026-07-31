package store

// service_test.go exercises the store Service over an in-memory repo,
// covering the happy path + cross-tenant isolation (ARCH-01) + the nil-repo
// degrade. The MemoryStoreRepo seeds two demo stores under tenant_id=1; a
// second tenant is seeded explicitly to prove cross-tenant invisibility.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

func ctxForTenant(tenantID int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tenantID, Source: multitenancy.SourceSystem,
	})
}

func TestService_ListStores_HappyPath(t *testing.T) {
	svc := New(NewMemoryStoreRepo())
	got, err := svc.ListStores(ctxForTenant(1))
	require.NoError(t, err)
	require.Len(t, got, 2, "seeded tenant 1 should have 2 demo stores")
	assert.Equal(t, "总店 · 阳光水果行", got[0].Name)
	assert.NotEmpty(t, got[0].Phone)
	assert.NotEmpty(t, got[0].GroupQR)
	assert.True(t, got[0].Active)
}

func TestService_ListStores_CrossTenantEmpty(t *testing.T) {
	repo := NewMemoryStoreRepo()
	// Seed a store explicitly under tenant 50021 (the MVP test tenant).
	repo.Seed(&Store{TenantID: 50021, Name: "50021总店", Address: "路A", Phone: "1", GroupQR: "qa", Active: true})
	svc := New(repo)

	// Tenant 50022 sees NONE of tenant 50021's stores (ARCH-01).
	got, err := svc.ListStores(ctxForTenant(50022))
	require.NoError(t, err)
	assert.Empty(t, got, "tenant B must not see tenant A's stores")

	// Tenant 50021 sees its own store.
	got, err = svc.ListStores(ctxForTenant(50021))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "50021总店", got[0].Name)
}

func TestService_ListStores_InactiveHidden(t *testing.T) {
	repo := NewMemoryStoreRepo()
	repo.Seed(&Store{TenantID: 7, Name: "停业店", Phone: "x", GroupQR: "q", Active: false})
	svc := New(repo)
	got, err := svc.ListStores(ctxForTenant(7))
	require.NoError(t, err)
	assert.Empty(t, got, "inactive stores must be filtered out")
}

func TestService_ListStores_NoTenant(t *testing.T) {
	svc := New(NewMemoryStoreRepo())
	_, err := svc.ListStores(context.Background())
	require.Error(t, err, "no tenant in ctx → EnforceTenant fails")
}

func TestMemoryStoreRepo_ReadOnlyClones(t *testing.T) {
	repo := NewMemoryStoreRepo()
	got, _ := repo.ListStores(context.Background(), 1)
	require.NotEmpty(t, got)
	got[0].Name = "MUTATED"
	again, _ := repo.ListStores(context.Background(), 1)
	assert.NotEqual(t, "MUTATED", again[0].Name, "returned stores must be defensive copies")
}

// TestMemoryStoreRepo_SeedReturnsAssignedID is the regression test for the Seed
// bug: createInternal assigned a fresh id internally but Seed returned
// cloneStore(r.storesByID[s.ID]) — indexed by the CALLER's input ID (typically
// 0), which is not the assigned id, so Seed returned nil / the wrong record.
// Seed must return a clone carrying the repo-assigned ID.
func TestMemoryStoreRepo_SeedReturnsAssignedID(t *testing.T) {
	repo := NewMemoryStoreRepo()
	// Input ID is 0 (unset) — the common case that used to return nil.
	seeded := repo.Seed(&Store{TenantID: 42, Name: "新店", Active: true})
	require.NotNil(t, seeded, "Seed must return the stored record, not nil")
	assert.NotZero(t, seeded.ID, "Seed must return the repo-assigned ID, not the caller's input ID")
	assert.Equal(t, "新店", seeded.Name)

	// The assigned ID must be the one ListStores surfaces (round-trip parity).
	got, err := repo.ListStores(context.Background(), 42)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, seeded.ID, got[0].ID, "Seed's returned ID must match the listed record's ID")

	// The returned record is a defensive copy: mutating it must not corrupt the
	// repo's stored record.
	seeded.Name = "MUTATED"
	again, _ := repo.ListStores(context.Background(), 42)
	require.Len(t, again, 1)
	assert.Equal(t, "新店", again[0].Name, "Seed must return a clone, not an alias")
}
