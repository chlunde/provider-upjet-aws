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
	"strings"
	"testing"
)

// TestSecretCacheDisabledByDefault verifies that every generated
// zz_main.go defaults --enable-secret-cache to false and, while the flag
// is false, disables client-side caching for Secrets. With caching
// enabled the first Secret read starts a cluster-wide controller-runtime
// informer with no field or label selector, so the provider's memory
// scales with every Secret in the cluster rather than with its own
// workload. Caching must therefore be strictly opt-in.
func TestSecretCacheDisabledByDefault(t *testing.T) {
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
		def, ok := flagDefault(f, "enable-secret-cache")
		if !ok {
			t.Errorf("%s: cannot find an app.Flag(\"enable-secret-cache\", ...).Default(...) chain", path)
			continue
		}
		if def != "false" {
			t.Errorf("%s: --enable-secret-cache defaults to %q, must default to \"false\" so the cluster-wide Secret informer is opt-in", path, def)
		}
		if !disablesSecretCacheWhenFlagUnset(f) {
			t.Errorf("%s: expected an `if !*enableSecretCache` block setting client.CacheOptions.DisableFor for &corev1.Secret{}", path)
		}
	}
}

// TestTemplateSecretCacheDisabledByDefault applies the same invariant to
// the template the zz_main.go files are generated from. The template
// contains text/template directives, so it is checked textually rather
// than parsed.
func TestTemplateSecretCacheDisabledByDefault(t *testing.T) {
	const path = "../../config/templates/main.go.tmpl"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read template: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, `"enable-secret-cache"`) {
		t.Fatalf("%s: cannot find the enable-secret-cache flag", path)
	}
	if !strings.Contains(src, `.Default("false").Envar("ENABLE_SECRET_CACHE")`) {
		t.Errorf("%s: --enable-secret-cache must default to \"false\" so the cluster-wide Secret informer is opt-in", path)
	}
	if !strings.Contains(src, "if !*enableSecretCache {") {
		t.Errorf("%s: expected an `if !*enableSecretCache` block disabling the Secret cache", path)
	}
	if !strings.Contains(src, "DisableFor: []client.Object{&corev1.Secret{}}") {
		t.Errorf("%s: expected client.CacheOptions.DisableFor to route Secret reads to the API server while caching is off", path)
	}
}

// flagDefault returns the argument of the .Default(...) call chained onto
// app.Flag(name, ...), e.g. app.Flag("enable-secret-cache",
// ...).Default("false").Envar(...).Bool().
func flagDefault(f *ast.File, name string) (string, bool) {
	var def string
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Default" {
			return true
		}
		if flagName(sel.X) != name {
			return true
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			def = strings.Trim(lit.Value, `"`)
			found = true
		}
		return true
	})
	return def, found
}

// flagName walks down a kingpin method chain (e.g. app.Flag(name,
// help).Default(...).Envar(...)) and returns the first string argument of
// the innermost Flag call, or "" if the chain does not start with one.
func flagName(e ast.Expr) string {
	for {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return ""
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		if sel.Sel.Name == "Flag" {
			if len(call.Args) == 0 {
				return ""
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return ""
			}
			return strings.Trim(lit.Value, `"`)
		}
		e = sel.X
	}
}

// disablesSecretCacheWhenFlagUnset reports whether the file contains an
// `if !*enableSecretCache` statement whose body sets a DisableFor field
// mentioning corev1.Secret.
func disablesSecretCacheWhenFlagUnset(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok || !isNotDerefOf(ifStmt.Cond, "enableSecretCache") {
			return true
		}
		var hasDisableFor, hasSecret bool
		ast.Inspect(ifStmt.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "DisableFor" {
					hasDisableFor = true
				}
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "corev1" && n.Sel.Name == "Secret" {
					hasSecret = true
				}
			}
			return true
		})
		if hasDisableFor && hasSecret {
			found = true
		}
		return true
	})
	return found
}

// isNotDerefOf reports whether e is the expression !*name.
func isNotDerefOf(e ast.Expr, name string) bool {
	not, ok := e.(*ast.UnaryExpr)
	if !ok || not.Op != token.NOT {
		return false
	}
	star, ok := not.X.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == name
}
