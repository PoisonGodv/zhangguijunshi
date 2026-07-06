package multitenancy

// lint.go is the STATIC side of ARCH-01. The runtime guard in guard.go refuses
// to run a query without a tenant; this analyzer refuses to COMPILE-PASS a
// repository function that issues a raw DB query without first resolving the
// tenant via the multitenancy package. The two layers are complementary: the
// lint catches the mistake at CI time, the guard catches it at runtime.
//
// The analyzer is intentionally AST-only (no heavy type-info dependency) so it
// is cheap to run in CI and easy to unit-test with go/parser. See lint_test.go.

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

// Analyzer is the go/analysis entry point for the tenantcheck check. Wire it
// into a multichecker / golangci-lint custom analyzer / `go vet -vettool` driver
// (CONTRACT.md §5, A5 owns .golangci.yml). Name is the stable identifier used by
// CI to enable/disable the check.
var Analyzer = &analysis.Analyzer{
	Name: "tenantcheck",
	Doc: `Enforce multi-tenant row-level isolation (ARCH-01).

A repository function that calls a raw DB query method (Query, QueryRow, Exec on
pgx.Tx / pgx.Rows / pgxpool.Pool / database/sql) MUST first resolve the tenant
by calling one of:

  - multitenancy.EnforceTenant(ctx)
  - multitenancy.TenantID(ctx)
  - multitenancy.MustFromContext(ctx)
  - (*multitenancy.TenantGuard).Wrap(ctx)

A function whose body contains such a query but none of the guard calls above is
reported as a violation, since it can leak data across tenants.`,
	Run: run,
}

// Violation is a single finding produced by analyzeFile: a source position and a
// human-readable explanation. It is the pure-data return type of the analyzer's
// core logic so the detection can be unit-tested without a full analysis.Pass.
type Violation struct {
	// Pos is the token.Pos of the offending query call (the receiver selector),
	// resolvable to a token.Position via a *token.FileSet.
	Pos token.Pos
	// Msg is the diagnostic message shown to the developer.
	Msg string
}

// guardMethodNames are the selector names that satisfy the "tenant resolved
// before query" contract within a function body. A function that calls any of
// these is treated as guarded and its raw query calls are NOT reported.
//
// The set intentionally includes "Wrap" — the (*TenantGuard).Wrap entry point
// documented in guard.go — so the recommended repository wrapper pattern is not
// falsely flagged. Adding a name here is a security-sensitive change: a name
// that is commonly defined on non-guard types would create a bypass hole.
var guardMethodNames = map[string]bool{
	"EnforceTenant":   true, // package-level guard (also TenantGuard-typed)
	"TenantID":        true, // low-level context accessor
	"MustFromContext": true, // panic-style accessor
	"Wrap":            true, // (*TenantGuard).Wrap -> EnforceTenant
}

// queryMethodNames are the raw-DB methods that, when present in a function body
// WITHOUT a guard call, indicate a cross-tenant leak risk. The names match the
// pgx (Tx/Rows/Pool) and database/sql (*DB / *Stmt / *Tx) surfaces.
var queryMethodNames = map[string]bool{
	"Query":    true,
	"QueryRow": true,
	"Exec":     true,
}

// run is the Analyzer.Run callback. It applies analyzeFile to every Go file in
// the package and forwards each Violation to the analysis driver as a Diagnostic.
func run(pass *analysis.Pass) (any, error) {
	for _, f := range pass.Files {
		for _, v := range analyzeFile(f, pass.TypesInfo) {
			pass.Report(analysis.Diagnostic{
				Pos:     v.Pos,
				Message: v.Msg,
			})
		}
	}
	return nil, nil
}

// analyzeFile is the pure detection core: given a parsed file (and optional type
// info, which is currently unused — detection is AST-text-based so info may be
// nil), it returns one Violation per offending raw query.
//
// Detection rule (per enclosing function scope):
//
//	For each function (declaration or literal) in the file, look at the calls
//	that appear DIRECTLY in its body (nested function literals are separate
//	scopes). If the body contains a raw query call (Query/QueryRow/Exec) but
//	contains no guard call (EnforceTenant/TenantID/MustFromContext/Wrap),
//	report every such raw query.
//
// "Directly in its body" is what makes the check sound: a guard hidden in a
// nested closure does NOT cover a query in the outer function, and a query in a
// nested closure is judged against that closure's own body.
func analyzeFile(f *ast.File, _ *types.Info) []Violation {
	if f == nil {
		return nil
	}
	var violations []Violation
	ast.Inspect(f, func(n ast.Node) bool {
		var body *ast.BlockStmt
		switch fn := n.(type) {
		case *ast.FuncDecl:
			body = fn.Body // may be nil for external (asm) funcs
		case *ast.FuncLit:
			body = fn.Body
		default:
			return true // keep descending to find functions
		}
		if body == nil {
			return true
		}
		hasGuard, naked := scanFuncBody(body)
		if hasGuard {
			return true
		}
		for _, pos := range naked {
			violations = append(violations, Violation{
				Pos: pos,
				Msg: "tenantcheck: raw DB query without prior tenant resolution; call multitenancy.EnforceTenant(ctx) (or TenantGuard.Wrap) before querying to enforce row-level isolation",
			})
		}
		return true
	})
	return violations
}

// scanFuncBody inspects ONE function scope (a block statement) and reports
// whether a guard call is present, plus the positions of any raw query calls.
// It deliberately does NOT descend into nested function literals — those are
// independent scopes analyzed separately by analyzeFile.
func scanFuncBody(body *ast.BlockStmt) (hasGuard bool, naked []token.Pos) {
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			return true
		}
		// Do not cross function-scope boundaries downward.
		if _, isClosure := n.(*ast.FuncLit); isClosure {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := sel.Sel.Name
		if guardMethodNames[name] {
			hasGuard = true
		}
		if queryMethodNames[name] {
			// Point at the method name token so the diagnostic highlights
			// exactly the offending call (e.g. the "Query" in db.Query).
			naked = append(naked, sel.Sel.Pos())
		}
		return true
	})
	return hasGuard, naked
}
