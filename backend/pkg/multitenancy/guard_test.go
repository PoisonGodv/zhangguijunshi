package multitenancy

// guard_test.go is the cross-tenant test matrix for the runtime guard
// (ARCH-01). It MUST stay green: any regression here means a query without a
// resolved tenant becomes possible, which is the single most dangerous defect
// in this codebase. See CONTRACT.md §0 ("Day1 内建 tenant_id 行级隔离").

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnforceTenant_Matrix exercises every branch of EnforceTenant: the
// "no tenant" rejection paths, the happy path, and the system-context path.
// All assertions are checked in one test so the matrix is impossible to
// silently shrink.
func TestEnforceTenant_Matrix(t *testing.T) {
	t.Run("no tenant in context -> ErrNoTenant", func(t *testing.T) {
		// Plain background context, nothing injected. This also covers the
		// real-world "middleware forgot to inject" case.
		// NOTE: we deliberately do not pass a nil ctx here. context.go's
		// FromContext dereferences ctx (ctx.Value(...)) and panics on a nil
		// interface; a nil context.Context is a programmer error, not a
		// contract state, and EnforceTenant must stay a true alias of TenantID.
		id, err := EnforceTenant(context.Background())
		assert.ErrorIs(t, err, ErrNoTenant, "absent tenant must surface ErrNoTenant")
		assert.Equal(t, int64(0), id, "no id should be returned on error")
	})

	t.Run("TenantID == 0 -> ErrNoTenant", func(t *testing.T) {
		// A context that carries an explicit Context struct but with a zero
		// tenant_id is the most dangerous shape: it looks "set" but isolates
		// nothing. It MUST be rejected identically.
		ctx := WithContext(context.Background(), Context{
			TenantID: 0,
			Source:   SourceMerchant,
		})
		id, err := EnforceTenant(ctx)
		assert.ErrorIs(t, err, ErrNoTenant, "zero tenant_id must not pass the guard")
		assert.Equal(t, int64(0), id)
	})

	t.Run("empty Context struct -> rejected", func(t *testing.T) {
		// Zero-value Context{} — every field default. Same risk profile as the
		// TenantID==0 case; the guard must still refuse it.
		ctx := WithContext(context.Background(), Context{})
		id, err := EnforceTenant(ctx)
		assert.ErrorIs(t, err, ErrNoTenant)
		assert.Equal(t, int64(0), id)
	})

	t.Run("valid merchant tenant -> returns id", func(t *testing.T) {
		const want int64 = 10042
		ctx := WithContext(context.Background(), Context{
			TenantID: want,
			StoreID:  7,
			UserID:   99,
			Role:     "merchant_owner",
			Source:   SourceMerchant,
		})
		id, err := EnforceTenant(ctx)
		require.NoError(t, err, "a present non-zero tenant must pass")
		assert.Equal(t, want, id, "returned id must equal the injected tenant_id")
	})

	t.Run("valid consumer tenant -> returns id", func(t *testing.T) {
		const want int64 = 1
		ctx := WithContext(context.Background(), Context{
			TenantID: want,
			Source:   SourceConsumer,
		})
		id, err := EnforceTenant(ctx)
		require.NoError(t, err)
		assert.Equal(t, want, id)
	})

	t.Run("SystemContext with explicit tenant -> allowed", func(t *testing.T) {
		// Background / cron jobs must declare their tenant via SystemContext;
		// once declared the guard lets them through exactly like a request.
		const want int64 = 5001
		ctx := SystemContext(want)
		id, err := EnforceTenant(ctx)
		require.NoError(t, err, "SystemContext must satisfy the guard")
		assert.Equal(t, want, id)

		// And the source must be tagged system so downstream code can tell.
		c, ok := FromContext(ctx)
		require.True(t, ok)
		assert.Equal(t, SourceSystem, c.Source, "SystemContext must tag Source=system")
		assert.Equal(t, want, c.TenantID)
	})

	t.Run("SystemContext with zero tenant -> rejected", func(t *testing.T) {
		// A system job that forgets to name its tenant is exactly the
		// "naked cron" failure mode; the guard must refuse it.
		ctx := SystemContext(0)
		id, err := EnforceTenant(ctx)
		assert.ErrorIs(t, err, ErrNoTenant)
		assert.Equal(t, int64(0), id)
	})

	t.Run("EnforceTenant is semantic alias of TenantID", func(t *testing.T) {
		// CONTRACT: EnforceTenant and TenantID are interchangeable. Run both
		// against the same inputs and assert identical outcomes.
		cases := []struct {
			name string
			ctx  context.Context
		}{
			{"absent", context.Background()},
			{"zero", WithContext(context.Background(), Context{TenantID: 0})},
			{"set", WithContext(context.Background(), Context{TenantID: 42, Source: SourceAdmin})},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				gotID, gotErr := EnforceTenant(tc.ctx)
				wantID, wantErr := TenantID(tc.ctx)
				assert.Equal(t, wantID, gotID)
				if wantErr == nil {
					assert.NoError(t, gotErr)
				} else {
					assert.ErrorIs(t, gotErr, wantErr)
				}
			})
		}
	})
}

// TestTenantGuard_Wrap verifies the repository-wrapper demo: it must behave
// identically to the package-level EnforceTenant, since repositories will use
// Wrap as their entry point.
func TestTenantGuard_Wrap(t *testing.T) {
	g := NewGuard()
	require.NotNil(t, g)

	t.Run("rejects absent tenant", func(t *testing.T) {
		id, err := g.Wrap(context.Background())
		assert.ErrorIs(t, err, ErrNoTenant)
		assert.Equal(t, int64(0), id)
	})

	t.Run("rejects zero tenant", func(t *testing.T) {
		ctx := WithContext(context.Background(), Context{TenantID: 0})
		id, err := g.Wrap(ctx)
		assert.ErrorIs(t, err, ErrNoTenant)
		assert.Equal(t, int64(0), id)
	})

	t.Run("passes through valid tenant", func(t *testing.T) {
		const want int64 = 73
		ctx := WithContext(context.Background(), Context{TenantID: want, Source: SourceMerchant})
		id, err := g.Wrap(ctx)
		require.NoError(t, err)
		assert.Equal(t, want, id)
	})

	t.Run("independent instances behave the same", func(t *testing.T) {
		// TenantGuard is stateless; two instances must agree.
		g2 := NewGuard()
		ctx := WithContext(context.Background(), Context{TenantID: 9})
		id1, err1 := g.Wrap(ctx)
		id2, err2 := g2.Wrap(ctx)
		assert.NoError(t, err1)
		assert.NoError(t, err2)
		assert.Equal(t, id1, id2)
	})
}

// TestSystemContext verifies the system-context builder tagging.
func TestSystemContext(t *testing.T) {
	t.Run("injects tenant + system source", func(t *testing.T) {
		const want int64 = 9001
		ctx := SystemContext(want)
		c, ok := FromContext(ctx)
		require.True(t, ok, "SystemContext must embed a tenant Context")
		assert.Equal(t, want, c.TenantID)
		assert.Equal(t, SourceSystem, c.Source, "source must be system")
		// Other actor fields default to zero — the job has no human actor.
		assert.Equal(t, int64(0), c.UserID)
		assert.Equal(t, "", c.Role)
	})

	t.Run("produces a background-derived context", func(t *testing.T) {
		// SystemContext should not depend on any parent request context; it
		// roots at context.Background() so it survives past the request that
		// scheduled the job.
		ctx := SystemContext(1)
		assert.NotNil(t, ctx)
		// No panic when treated as a standalone context.
		assert.NotPanics(t, func() {
			_ = ctx.Err()
		})
	})
}

// TestErrNoTenant_IsSentinel guards the sentinel error contract: callers depend
// on errors.Is(err, ErrNoTenant) working, so ErrNoTenant must remain a stable
// unwrapped sentinel (not e.g. a freshly constructed error each time).
func TestErrNoTenant_IsSentinel(t *testing.T) {
	_, err := EnforceTenant(context.Background())
	if !errors.Is(err, ErrNoTenant) {
		t.Fatalf("expected errors.Is(err, ErrNoTenant); got err=%v", err)
	}
}
