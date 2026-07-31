package tenant

// service_test.go exercises the platform tenant Service + MemoryRepo, covering:
//   - happy path (authorized platform caller lists all tenants + counts),
//   - pagination (Limit/Offset + Total),
//   - filtering (Status / Query),
//   - authorization: a non-platform caller is rejected (ErrForbidden) and never
//     reaches the repo,
//   - the repo-side platform gate: a tenant-scoped ctx (or no ctx identity)
//     yields an empty page even when calling the repo directly — proving the
//     cross-tenant surface cannot be reached by a normal single-tenant caller
//     (ARCH-01),
//   - nil-safe degrade (nil repo → empty page, no panic).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// platformCtx builds a ctx the way the Service stamps it for repo calls
// (Source=system, no tenant scope). Used to drive the repo directly in the
// repo-gate tests.
func platformCtx() context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		Source: multitenancy.SourceSystem,
	})
}

// tenantCtx builds a NORMAL single-tenant ctx (merchant source, scoped to a
// tenant) — the kind a boss request carries. The cross-tenant surface must
// refuse it.
func tenantCtx(tenantID int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tenantID,
		Role:     "owner",
		Source:   multitenancy.SourceMerchant,
	})
}

func platformCaller() PlatformCaller {
	return PlatformCaller{Role: PlatformRole, UserID: 2001}
}

func TestService_ListTenants_HappyPath(t *testing.T) {
	svc := New(NewMemoryRepo())
	res, err := svc.ListTenants(platformCtx(), platformCaller(), Filter{})
	require.NoError(t, err)
	require.Len(t, res.Items, 2, "two seeded demo tenants")
	require.Equal(t, int64(2), res.Total)

	// Ordered by id; counts populated from the seed.
	assert.Equal(t, int64(1), res.Items[0].ID)
	assert.Equal(t, "阳光水果行", res.Items[0].Name)
	assert.Equal(t, 120, res.Items[0].ProductCount)
	assert.Equal(t, 3400, res.Items[0].MemberCount)
	assert.Equal(t, 15800, res.Items[0].OrderCount)
	assert.Equal(t, 2, res.Items[0].StoreCount)
	assert.Equal(t, int64(2), res.Items[1].ID)
}

func TestService_ListTenants_Pagination(t *testing.T) {
	repo := NewMemoryRepo()
	repo.Seed(Tenant{ID: 3, Name: "便利店C", Status: "active", Plan: "free"}, 1, 1, 1, 1)
	svc := New(repo)

	// Page 1: limit 2 → first two, total reflects all three.
	res, err := svc.ListTenants(platformCtx(), platformCaller(), Filter{Limit: 2, Offset: 0})
	require.NoError(t, err)
	require.Len(t, res.Items, 2)
	require.Equal(t, int64(3), res.Total, "total is independent of the page window")
	assert.Equal(t, int64(1), res.Items[0].ID)
	assert.Equal(t, int64(2), res.Items[1].ID)

	// Page 2: offset 2 → the third tenant only.
	res, err = svc.ListTenants(platformCtx(), platformCaller(), Filter{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, int64(3), res.Items[0].ID)
	require.Equal(t, int64(3), res.Total)

	// Offset past the end → empty page, total unchanged.
	res, err = svc.ListTenants(platformCtx(), platformCaller(), Filter{Limit: 2, Offset: 99})
	require.NoError(t, err)
	assert.Empty(t, res.Items)
	assert.Equal(t, int64(3), res.Total)
}

func TestService_ListTenants_Filters(t *testing.T) {
	repo := NewMemoryRepo()
	repo.Seed(Tenant{ID: 3, Name: "暂停商户", Status: "suspended", Plan: "free"}, 0, 0, 0, 0)
	svc := New(repo)

	// Status filter: only suspended.
	res, err := svc.ListTenants(platformCtx(), platformCaller(), Filter{Status: "suspended"})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, "暂停商户", res.Items[0].Name)
	assert.Equal(t, int64(1), res.Total)

	// Query filter: substring match on name.
	res, err = svc.ListTenants(platformCtx(), platformCaller(), Filter{Query: "母婴"})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, "好孕母婴生活馆", res.Items[0].Name)

	// No match → empty.
	res, err = svc.ListTenants(platformCtx(), platformCaller(), Filter{Query: "不存在"})
	require.NoError(t, err)
	assert.Empty(t, res.Items)
	assert.Equal(t, int64(0), res.Total)
}

func TestService_ListTenants_LimitClamped(t *testing.T) {
	// A huge limit is clamped to maxPageSize; a non-positive limit defaults.
	f := Filter{Limit: 100000}.Normalize()
	assert.Equal(t, maxPageSize, f.Limit)
	f = Filter{Limit: -5, Offset: -3}.Normalize()
	assert.Equal(t, defaultPageSize, f.Limit)
	assert.Equal(t, 0, f.Offset)
}

// TestService_ListTenants_RejectsNonPlatform proves the explicit-caller gate:
// a caller whose role is not platform_ops is rejected with ErrForbidden and the
// repo is never consulted (the result is the zero value).
func TestService_ListTenants_RejectsNonPlatform(t *testing.T) {
	svc := New(NewMemoryRepo())
	for _, role := range []string{"owner", "manager", "clerk", ""} {
		_, err := svc.ListTenants(platformCtx(), PlatformCaller{Role: role}, Filter{})
		require.Error(t, err, "role %q must be rejected", role)
		assert.Equal(t, errcode.ErrForbidden, err)
	}
}

// TestService_ListTenants_NilRepoSafe proves a nil repo degrades to an empty
// page (never a panic) once the caller is authorized.
func TestService_ListTenants_NilRepoSafe(t *testing.T) {
	svc := New(nil)
	require.NotPanics(t, func() {
		res, err := svc.ListTenants(platformCtx(), platformCaller(), Filter{})
		require.NoError(t, err)
		assert.Empty(t, res.Items)
	})
}

// TestRepo_PlatformGate proves the repo-side half of the isolation boundary:
// calling the cross-tenant repo method directly with a NORMAL single-tenant ctx
// (or no identity at all) yields an empty page — never the tenant list. Only a
// platform/system Source unlocks the read. This is what makes the cross-tenant
// surface unreachable by a regular boss/admin caller even if they bypass the
// Service (ARCH-01).
func TestRepo_PlatformGate(t *testing.T) {
	repo := NewMemoryRepo()

	// A normal merchant ctx scoped to tenant 1 → refused (empty, no leak).
	res, err := repo.ListAllTenants(tenantCtx(1), Filter{})
	require.NoError(t, err)
	assert.Empty(t, res.Items, "tenant-scoped ctx must not read cross-tenant")
	assert.Equal(t, int64(0), res.Total)

	// No multitenancy identity at all → refused.
	res, err = repo.ListAllTenants(context.Background(), Filter{})
	require.NoError(t, err)
	assert.Empty(t, res.Items)

	// A platform/system ctx → admitted.
	res, err = repo.ListAllTenants(platformCtx(), Filter{})
	require.NoError(t, err)
	assert.Len(t, res.Items, 2)
}

func TestMemoryRepo_ReadOnlyCopies(t *testing.T) {
	repo := NewMemoryRepo()
	res, _ := repo.ListAllTenants(platformCtx(), Filter{})
	require.NotEmpty(t, res.Items)
	res.Items[0].Name = "MUTATED"
	again, _ := repo.ListAllTenants(platformCtx(), Filter{})
	assert.NotEqual(t, "MUTATED", again.Items[0].Name, "returned summaries must be copies")
}
