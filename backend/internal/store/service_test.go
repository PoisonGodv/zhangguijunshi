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
