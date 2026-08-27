// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

// Package provider_test asserts invariants over the generated
// cmd/provider/*/zz_main.go sources and the template they are generated
// from, so that a future regeneration cannot silently reintroduce bugs
// fixed in both.
package provider_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSingleGlobalRateLimiter verifies that every generated zz_main.go
// constructs exactly one global reconcile rate limiter and shares it
// between the cluster-scoped and namespaced controller options. Two
// independent limiters would each admit --max-reconcile-rate reconciles
// per second, letting the process sustain double the documented global
// limit.
func TestSingleGlobalRateLimiter(t *testing.T) {
	mains, err := filepath.Glob("*/zz_main.go")
	if err != nil {
		t.Fatalf("cannot glob generated main files: %v", err)
	}
	if len(mains) < 100 {
		t.Fatalf("expected to find the generated zz_main.go files under cmd/provider, found only %d", len(mains))
	}
	for _, path := range mains {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Errorf("cannot parse %s: %v", path, err)
			continue
		}
		limiterVars, refs := inspectRateLimiterUsage(f)
		if len(limiterVars) != 1 {
			t.Errorf("%s: expected exactly one ratelimiter.NewGlobal call assigned to a variable, found %d", path, len(limiterVars))
			continue
		}
		if len(refs) != 2 {
			t.Errorf("%s: expected exactly two GlobalRateLimiter fields (cluster-scoped and namespaced), found %d", path, len(refs))
			continue
		}
		for _, ref := range refs {
			if ref != limiterVars[0] {
				t.Errorf("%s: GlobalRateLimiter field references %q instead of the shared limiter %q", path, ref, limiterVars[0])
			}
		}
	}
}

// TestTemplateSingleGlobalRateLimiter applies the same invariant to the
// template the zz_main.go files are generated from. The template contains
// text/template directives, so it is checked textually rather than parsed.
func TestTemplateSingleGlobalRateLimiter(t *testing.T) {
	const path = "../../config/templates/main.go.tmpl"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read template: %v", err)
	}
	src := string(b)
	if got := strings.Count(src, "ratelimiter.NewGlobal"); got != 1 {
		t.Errorf("%s: expected exactly one ratelimiter.NewGlobal call, found %d", path, got)
	}
	if got := strings.Count(src, "GlobalRateLimiter:"); got != 2 {
		t.Errorf("%s: expected exactly two GlobalRateLimiter fields, found %d", path, got)
	}
	sharedRef := regexp.MustCompile(`GlobalRateLimiter:\s+globalRateLimiter,`)
	if got := len(sharedRef.FindAllString(src, -1)); got != 2 {
		t.Errorf("%s: expected both GlobalRateLimiter fields to reference the shared globalRateLimiter variable, found %d such references", path, got)
	}
}

// inspectRateLimiterUsage returns the names of variables assigned from
// ratelimiter.NewGlobal calls and the identifier names assigned to
// GlobalRateLimiter struct fields.
func inspectRateLimiterUsage(f *ast.File) (limiterVars, refs []string) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) != 1 || len(n.Rhs) != 1 {
				return true
			}
			if !isNewGlobalCall(n.Rhs[0]) {
				return true
			}
			if ident, ok := n.Lhs[0].(*ast.Ident); ok {
				limiterVars = append(limiterVars, ident.Name)
			}
		case *ast.KeyValueExpr:
			key, ok := n.Key.(*ast.Ident)
			if !ok || key.Name != "GlobalRateLimiter" {
				return true
			}
			if ident, ok := n.Value.(*ast.Ident); ok {
				refs = append(refs, ident.Name)
			} else {
				// A non-identifier value (e.g. an inline
				// ratelimiter.NewGlobal call) can never be shared.
				refs = append(refs, "<non-identifier expression>")
			}
		}
		return true
	})
	return limiterVars, refs
}

// isNewGlobalCall reports whether e is a ratelimiter.NewGlobal(...) call.
func isNewGlobalCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "ratelimiter" && sel.Sel.Name == "NewGlobal"
}
