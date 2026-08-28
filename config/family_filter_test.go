// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: CC0-1.0

package config

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every controller upjet generates indexes config.Provider.Resources by its
// Terraform resource name and immediately dereferences the result, e.g.
// internal/controller/cluster/ec2/instance/zz_controller.go:
//
//	for _, i := range o.Provider.Resources["aws_instance"].InitializerFns {
//
// config.Provider.Resources is a map[string]*config.Resource, so a name that
// config.NewProvider did not configure yields a nil *config.Resource and the
// field access panics. There are ~3,136 such index sites under
// internal/controller. Setup_<family> runs them all, at provider startup, with
// no error path.
//
// Restricting the include lists to one family (UPJET_FAMILY_FILTER, see
// registry_common.go) is therefore safe if and only if, for every family, the
// filter keeps every Terraform resource name that family's controllers index.
// Nothing else in the build enforces that; this test does.
//
// The kept side is computed by calling the production include-list functions
// with the filter engaged, so the test tracks the real filter rather than a
// copy of it. The controller side is read out of the generated sources with
// go/ast: importing internal/controller here would drag in internal/clients
// and the whole apis tree for no gain, and there is no in-process registry of
// what a Setup_<family> touches short of standing up a manager. The generated
// controllers have a very regular shape, and the parse below refuses to pass
// quietly if that shape changes (see indexedResourceNames).

const (
	// controllerDir is internal/controller, relative to this package.
	controllerDir = "../internal/controller"
	// modulePath prefixes every intra-module import path.
	modulePath = "github.com/upbound/provider-aws/v2/"
	// controllerImportPrefix prefixes the import path of every generated
	// controller package.
	controllerImportPrefix = modulePath + "internal/controller/"
	// monolithFamily names the generated setup file that registers every
	// family at once. It is not a family; it is used below to check that the
	// per-family decomposition is complete.
	monolithFamily = "monolith"
)

// controllerScopes are the two API scopes the provider generates controllers
// for. Both are driven by the same include lists.
var controllerScopes = []string{"cluster", "namespaced"}

// includeList is a compiled include list, mirroring upjet's unexported
// config.matches (regexp.MatchString over the raw entries, unanchored).
type includeList struct {
	entries []string
	res     []*regexp.Regexp
}

func compileIncludeList(t *testing.T, lists ...[]string) includeList {
	t.Helper()
	var il includeList
	for _, l := range lists {
		for _, e := range l {
			re, err := regexp.Compile(e)
			if err != nil {
				t.Fatalf("cannot compile include list entry %q: %v", e, err)
			}
			il.entries = append(il.entries, e)
			il.res = append(il.res, re)
		}
	}
	return il
}

func (il includeList) matches(name string) bool {
	for _, re := range il.res {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// names returns the Terraform resource names the entries were built from. The
// *ResourceList functions emit "<terraform name>$" for every configured
// resource, so the name is the entry without its anchor.
func (il includeList) names() []string {
	out := make([]string, 0, len(il.entries))
	for _, e := range il.entries {
		out = append(out, strings.TrimSuffix(e, "$"))
	}
	return out
}

// currentIncludeLists returns the three include lists GetProvider hands to
// upjet's config.NewProvider, as filtered by the currently set
// UPJET_FAMILY_FILTER.
func currentIncludeLists(t *testing.T) includeList {
	t.Helper()
	return compileIncludeList(t,
		CLIReconciledResourceList(),
		TerraformPluginSDKResourceList(),
		TerraformPluginFrameworkResourceList(),
	)
}

// indexedResourceNames returns the Terraform resource names that the generated
// controller package in dir passes to config.Provider.Resources[...].
var indexedResourceNamesCache = map[string][]string{}

func indexedResourceNames(t *testing.T, fset *token.FileSet, dir string) []string {
	t.Helper()
	if v, ok := indexedResourceNamesCache[dir]; ok {
		return v
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read controller package %s: %v", dir, err)
	}
	var names []string
	seen := map[string]bool{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("cannot parse %s: %v", p, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ie, ok := n.(*ast.IndexExpr)
			if !ok || !isProviderResources(ie.X) {
				return true
			}
			lit, ok := ie.Index.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				// A computed key would hide an index site from this test
				// and make it claim safety it has not checked.
				t.Errorf("%s: config.Provider.Resources is indexed by a non-literal key; this test can no longer see every index site and must be taught the new shape", fset.Position(ie.Pos()))
				return true
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Errorf("%s: cannot unquote the config.Provider.Resources key %s: %v", fset.Position(lit.Pos()), lit.Value, err)
				return true
			}
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
			return true
		})
	}
	indexedResourceNamesCache[dir] = names
	return names
}

// isProviderResources reports whether x is a "<something>.Provider.Resources"
// selector, the receiver of every generated map index.
func isProviderResources(x ast.Expr) bool {
	sel, ok := x.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Resources" {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "Provider"
}

// controllerImports returns the on-disk directories of the generated
// controller packages a zz_<family>_setup.go imports, which are exactly the
// packages whose Setup its Setup_<family> calls.
func controllerImports(t *testing.T, fset *token.FileSet, setupFile string) []string {
	t.Helper()
	f, err := parser.ParseFile(fset, setupFile, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("cannot parse %s: %v", setupFile, err)
	}
	var out []string
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatalf("cannot unquote the import path %s in %s: %v", imp.Path.Value, setupFile, err)
		}
		if !strings.HasPrefix(p, controllerImportPrefix) {
			continue
		}
		out = append(out, filepath.Join("..", strings.TrimPrefix(p, modulePath)))
	}
	if len(out) == 0 {
		t.Fatalf("%s imports no controller package; the generated setup file shape has changed and this test can no longer tell what Setup_<family> registers", setupFile)
	}
	return out
}

// indexControllers walks every zz_<family>_setup.go under internal/controller
// and returns, per family, the Terraform resource names that family's
// controllers index in config.Provider.Resources, along with one controller
// package that indexes each. The monolith setup file is returned separately.
func indexControllers(t *testing.T) (byFamily map[string]map[string]string, monolith map[string]bool) {
	t.Helper()
	byFamily = map[string]map[string]string{}
	monolith = map[string]bool{}
	fset := token.NewFileSet()
	for _, scope := range controllerScopes {
		setups, err := filepath.Glob(filepath.Join(controllerDir, scope, "zz_*_setup.go"))
		if err != nil {
			t.Fatalf("cannot glob the %s setup files: %v", scope, err)
		}
		if len(setups) == 0 {
			t.Fatalf("no zz_<family>_setup.go found under %s", filepath.Join(controllerDir, scope))
		}
		for _, s := range setups {
			family := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(s), "zz_"), "_setup.go")
			if family != monolithFamily && byFamily[family] == nil {
				// Record the family even if it turns out to register no
				// generated controller (Setup_config registers only the
				// hand-written providerconfig controller), so that every
				// Setup_<family> gets a subtest of its own.
				byFamily[family] = map[string]string{}
			}
			for _, pkg := range controllerImports(t, fset, s) {
				for _, name := range indexedResourceNames(t, fset, pkg) {
					if family == monolithFamily {
						monolith[name] = true
						continue
					}
					if byFamily[family] == nil {
						byFamily[family] = map[string]string{}
					}
					if _, ok := byFamily[family][name]; !ok {
						byFamily[family][name] = pkg
					}
				}
			}
		}
	}
	return byFamily, monolith
}

// TestFamilyFilterKeepsEveryIndexedResource asserts the one invariant that
// makes per-family filtering of the include lists safe to ship:
//
//	for every family F:
//	  { Terraform names indexed by the controllers Setup_F registers }
//	    is a subset of
//	  { Terraform names the include lists keep for F }
//
// A violation is not a degradation, it is a nil pointer dereference in
// Setup_F at provider startup, so it fails the build and names both the
// family and the resources.
func TestFamilyFilterKeepsEveryIndexedResource(t *testing.T) {
	byFamily, monolith := indexControllers(t)
	if len(byFamily) < 100 {
		t.Fatalf("found only %d families under %s; expected the full generated set", len(byFamily), controllerDir)
	}
	families := make([]string, 0, len(byFamily))
	for f := range byFamily {
		families = append(families, f)
	}
	sort.Strings(families)

	// The filter is inert when UPJET_FAMILY_FILTER is unset. Pin it empty so
	// an ambient value cannot make the baseline lie, and take the unfiltered
	// lists as the reference: a name the provider does not configure even
	// without a filter is a pre-existing defect, reported once below rather
	// than 178 times.
	t.Setenv(familyFilterEnv, "")
	unfiltered := currentIncludeLists(t)
	if len(unfiltered.res) < 900 {
		t.Fatalf("the unfiltered include lists hold only %d entries; expected roughly the ~1,029 configured resources, so the filter is not inert when unset", len(unfiltered.res))
	}
	skipped := compileIncludeList(t, skipList)

	configured := func(name string) bool {
		return unfiltered.matches(name) && !skipped.matches(name)
	}

	total := 0
	t.Run("unfiltered", func(t *testing.T) {
		var missing []string
		for _, family := range families {
			for _, name := range sortedKeys(byFamily[family]) {
				total++
				if !configured(name) {
					missing = append(missing, fmt.Sprintf("%s (family %s, %s)", name, family, byFamily[family][name]))
				}
			}
		}
		if len(missing) > 0 {
			t.Errorf("%d Terraform resource(s) are indexed by a generated controller but are not configured even with no family filter, so config.Provider.Resources has no entry for them and their Setup panics at startup:\n  %s", len(missing), strings.Join(missing, "\n  "))
		}
		t.Logf("%d families, %d distinct Terraform resource names indexed by the generated controllers, %d include list entries", len(families), total, len(unfiltered.res))
	})

	t.Run("monolith", func(t *testing.T) {
		// Setup_monolith registers every controller in the provider. If it
		// reaches a name no per-family setup reaches, the loop below never
		// checks that name and the test is quietly incomplete.
		union := map[string]bool{}
		for _, family := range families {
			for name := range byFamily[family] {
				union[name] = true
			}
		}
		var orphaned []string
		for name := range monolith {
			if !union[name] {
				orphaned = append(orphaned, name)
			}
		}
		if len(orphaned) > 0 {
			sort.Strings(orphaned)
			t.Errorf("%d resource(s) registered by Setup_monolith belong to no per-family setup, so no family subtest below covers them: %s", len(orphaned), strings.Join(orphaned, ", "))
		}
	})

	for _, family := range families {
		t.Run(family, func(t *testing.T) {
			t.Setenv(familyFilterEnv, family)
			kept := currentIncludeLists(t)
			// Guard against a vacuous pass: if the filter kept everything,
			// the subset check below proves nothing about filtering.
			if len(kept.res) >= len(unfiltered.res) {
				t.Fatalf("%s=%s kept %d of %d include list entries, i.e. it did not filter; the check below would be vacuous", familyFilterEnv, family, len(kept.res), len(unfiltered.res))
			}

			var missing []string
			for _, name := range sortedKeys(byFamily[family]) {
				if !configured(name) {
					continue // reported by the unfiltered subtest
				}
				if !kept.matches(name) {
					missing = append(missing, fmt.Sprintf("%s (%s)", name, byFamily[family][name]))
				}
			}
			if len(missing) > 0 {
				t.Errorf("%s=%s drops %d of the %d Terraform resource(s) that Setup_%s registers controllers for. config.Provider.Resources would hold no entry for them, so Setup_%s panics with a nil pointer dereference at provider startup. Missing:\n  %s",
					familyFilterEnv, family, len(missing), len(byFamily[family]), family, family, strings.Join(missing, "\n  "))
			}

			// The reverse direction is not a panic: a kept resource with no
			// controller in this family is only configuration the family
			// never uses. Report it, do not fail on it.
			var unused []string
			for _, name := range kept.names() {
				if _, ok := byFamily[family][name]; ok || skipped.matches(name) {
					continue
				}
				unused = append(unused, name)
			}
			if len(unused) > 0 {
				sort.Strings(unused)
				t.Logf("%s=%s keeps %d resource(s) that no controller of family %q indexes (configured but unused, not a startup failure): %s", familyFilterEnv, family, len(unused), family, strings.Join(unused, ", "))
			}
		})
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
