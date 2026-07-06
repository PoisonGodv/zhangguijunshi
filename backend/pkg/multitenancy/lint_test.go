package multitenancy

// lint_test.go validates the AST-level detection core (analyzeFile) WITHOUT the
// analysistest testdata machinery. Per the task spec we feed small Go source
// snippets straight through go/parser.ParseFile, which keeps the test free of
// on-disk package compilation dependencies and fast. types.Info is passed nil —
// detection is pure AST text matching (see lint.go).

import (
	"go/parser"
	"go/token"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// analyzeSrc parses a Go source snippet and runs analyzeFile with nil type info.
// It fails the test on any parse error so a malformed fixture is caught loudly
// rather than silently returning zero violations.
func analyzeSrc(t *testing.T, src string) []Violation {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, parser.ParseComments)
	require.NoError(t, err, "fixture must parse; src=\n%s", src)
	return analyzeFile(f, nil)
}

func TestAnalyzeFile_RequiredMatrix(t *testing.T) {
	// The three scenarios mandated by the task spec.
	t.Run("VIOLATION: naked Query, no EnforceTenant -> 1 violation", func(t *testing.T) {
		src := `package p

import (
	"context"
	"github.com/jackc/pgx/v5"
)

func GetByID(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, "SELECT id FROM products WHERE id = $1", 1)
	_ = rows
	return err
}`
		v := analyzeSrc(t, src)
		assert.Len(t, v, 1, "exactly one naked query must be reported")
		if len(v) == 1 {
			assert.Contains(t, v[0].Msg, "tenantcheck")
			assert.NotEqual(t, token.NoPos, v[0].Pos, "violation must carry a real position")
		}
	})

	t.Run("COMPLIANT: EnforceTenant then Query -> 0 violations", func(t *testing.T) {
		src := `package p

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

func GetByID(ctx context.Context, tx pgx.Tx) error {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, "SELECT id FROM products WHERE tenant_id = $1 AND id = $2", tenantID, 1)
	_ = rows
	return err
}`
		v := analyzeSrc(t, src)
		assert.Empty(t, v, "a guard call before the query must clear the violation")
	})

	t.Run("COMPLIANT: function with no queries -> 0 violations", func(t *testing.T) {
		src := `package p

import "context"

func Pure(ctx context.Context) int {
	_ = ctx
	return 42
}`
		v := analyzeSrc(t, src)
		assert.Empty(t, v, "functions that do not query must never be flagged")
	})
}

func TestAnalyzeFile_GuardVariants(t *testing.T) {
	// Every accepted guard entry point must clear the violation.
	guards := map[string]string{
		"EnforceTenant":   "multitenancy.EnforceTenant(ctx)",
		"TenantID":        "multitenancy.TenantID(ctx)",
		"MustFromContext": "multitenancy.MustFromContext(ctx)",
		"Wrap":            "r.guard.Wrap(ctx)",
	}
	for name, guardCall := range guards {
		name, guardCall := name, guardCall
		t.Run(name+" clears Query", func(t *testing.T) {
			// "r.guard.Wrap" needs an `r` receiver; the others need the
			// multitenancy import. Build a function body that uses the guard
			// call verbatim and then a raw query.
			src := `package p

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

type repo struct{ guard *multitenancy.TenantGuard }

var r = &repo{}

func F(ctx context.Context, tx pgx.Tx) error {
	_, _ = ` + guardCall + `
	_, err := tx.Query(ctx, "SELECT 1")
	return err
}`
			v := analyzeSrc(t, src)
			assert.Empty(t, v, "%s should satisfy the guard", name)
		})
	}
}

func TestAnalyzeFile_QuerySurfaces(t *testing.T) {
	// Each raw-DB method on each common surface must be flagged when unguarded.
	methods := []string{"Query", "QueryRow", "Exec"}
	for _, m := range methods {
		m := m
		t.Run("pgx.Tx."+m+" flagged", func(t *testing.T) {
			src := `package p
import ("context"; "github.com/jackc/pgx/v5")
func F(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.` + m + `(ctx, "SELECT 1")
	return err
}`
			v := analyzeSrc(t, src)
			assert.Len(t, v, 1)
		})

		t.Run("database/sql."+m+" flagged", func(t *testing.T) {
			src := `package p
import ("context"; "database/sql")
func F(ctx context.Context, db *sql.DB) error {
	_, err := db.` + m + `(ctx, "SELECT 1")
	return err
}`
			v := analyzeSrc(t, src)
			assert.Len(t, v, 1)
		})
	}

	t.Run("pgxpool.Pool.Query flagged", func(t *testing.T) {
		src := `package p
import ("context"; "github.com/jackc/pgx/v5/pgxpool")
func F(ctx context.Context, p *pgxpool.Pool) error {
	_, err := p.Query(ctx, "SELECT 1")
	return err
}`
		v := analyzeSrc(t, src)
		assert.Len(t, v, 1)
	})
}

func TestAnalyzeFile_Scoping(t *testing.T) {
	// Soundness of the per-function-scope rule.
	t.Run("guard in nested closure does NOT cover outer query", func(t *testing.T) {
		// The outer function queries but its guard lives inside a closure that
		// never runs synchronously — unsound to trust it. Must be flagged.
		src := `package p
import ("context"; "github.com/jackc/pgx/v5"; "github.com/zhangguijunshi/backend/pkg/multitenancy")
func F(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Query(ctx, "SELECT 1")
	_ = func() {
		_, _ = multitenancy.EnforceTenant(ctx)
	}
	return err
}`
		v := analyzeSrc(t, src)
		assert.NotEmpty(t, v, "guard in a nested closure must not clear the outer query")
	})

	t.Run("query in nested closure judged against closure body", func(t *testing.T) {
		// Outer has a guard but never queries; the closure queries without its
		// own guard. The closure must be flagged independently.
		src := `package p
import ("context"; "github.com/jackc/pgx/v5"; "github.com/zhangguijunshi/backend/pkg/multitenancy")
func F(ctx context.Context, tx pgx.Tx) error {
	_, _ = multitenancy.EnforceTenant(ctx)
	_ = func() {
		_, _ = tx.Query(ctx, "SELECT 1")
	}
	return nil
}`
		v := analyzeSrc(t, src)
		assert.Len(t, v, 1, "the unguarded closure query must be reported")
	})

	t.Run("two naked queries in one function -> 2 violations", func(t *testing.T) {
		src := `package p
import ("context"; "github.com/jackc/pgx/v5")
func F(ctx context.Context, tx pgx.Tx) error {
	_, _ = tx.Query(ctx, "a")
	_, _ = tx.Exec(ctx, "b")
	return nil
}`
		v := analyzeSrc(t, src)
		assert.Len(t, v, 2)
	})

	t.Run("guard covers multiple queries in same function", func(t *testing.T) {
		src := `package p
import ("context"; "github.com/jackc/pgx/v5"; "github.com/zhangguijunshi/backend/pkg/multitenancy")
func F(ctx context.Context, tx pgx.Tx) error {
	_, _ = multitenancy.EnforceTenant(ctx)
	_, _ = tx.Query(ctx, "a")
	_, _ = tx.Exec(ctx, "b")
	return nil
}`
		v := analyzeSrc(t, src)
		assert.Empty(t, v)
	})
}

func TestAnalyzeFile_EdgeCases(t *testing.T) {
	t.Run("SystemContext alone does NOT clear query", func(t *testing.T) {
		// SystemContext builds a ctx but the function still MUST call EnforceTenant
		// before querying. Otherwise this is just "set then forget".
		src := `package p
import ("context"; "github.com/jackc/pgx/v5"; "github.com/zhangguijunshi/backend/pkg/multitenancy")
func F(tx pgx.Tx) error {
	ctx := multitenancy.SystemContext(5)
	_, err := tx.Query(ctx, "SELECT 1")
	return err
}`
		v := analyzeSrc(t, src)
		assert.NotEmpty(t, v, "SystemContext is not a guard call; query must be flagged")
	})

	t.Run("external (no body) FuncDecl not flagged", func(t *testing.T) {
		src := `package p
func F()`
		v := analyzeSrc(t, src)
		assert.Empty(t, v)
	})

	t.Run("nil file returns nil", func(t *testing.T) {
		assert.Nil(t, analyzeFile(nil, nil))
	})

	t.Run("unrelated methods not flagged", func(t *testing.T) {
		// Methods that merely share the name shape but are not DB calls are out
		// of scope; here we ensure a function with neither guard nor DB method
		// stays quiet. (Exec/Query as method names on non-DB types would still
		// be flagged by design — the lint is deliberately conservative.)
		src := `package p
import "context"
func F(ctx context.Context) error {
	doThing(ctx)
	return nil
}
func doThing(ctx context.Context) {}`
		v := analyzeSrc(t, src)
		assert.Empty(t, v)
	})
}

func TestAnalyzer_Metadata(t *testing.T) {
	// Guard the analyzer's stable identity — CI and golangci-lint config depend
	// on the Name string. If it changes, downstream wiring breaks silently.
	assert.Equal(t, "tenantcheck", Analyzer.Name, "Analyzer.Name is a CI contract")
	assert.NotNil(t, Analyzer.Run, "Analyzer.Run must be wired")
	assert.NotEmpty(t, Analyzer.Doc, "Analyzer.Doc must be populated for vet -V help")
}
