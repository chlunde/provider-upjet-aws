// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: CC0-1.0

package config

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"text/template/parse"

	"github.com/crossplane/upjet/v2/pkg/config"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// A mistyped template action such as "{{ .parameeters.target }}" is not a
// syntax error: text/template resolves the missing map key to an invalid
// value and renders it as the literal string "<no value>" with a nil error.
// Neither code generation nor the compiler notices, and the resulting
// Terraform ID is silently wrong at runtime. The two tests below close that
// hole from both ends.

// snakeCase is the shape every Terraform attribute name has.
var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// templateRoots are the top-level keys upjet puts in the data object it
// executes an external-name template against. See
// upjet/pkg/config/externalname.go (TemplatedStringAsIdentifier) and
// upjet/pkg/terraform.Setup.Map().
var templateRoots = map[string]bool{
	"external_name": true,
	"parameters":    true,
	"setup":         true,
}

// setupKeys are the keys of terraform.Setup.Map().
var setupKeys = map[string]bool{
	"version":         true,
	"requirement":     true,
	"configuration":   true,
	"client_metadata": true,
}

// externalNameSourceFiles are the files holding the external-name tables.
var externalNameSourceFiles = []string{
	"externalname.go",
	"externalnamenottested.go",
}

// TestExternalNameTemplateActionsAreWellFormed parses every template string
// literal in the external-name tables and asserts that each field access in it
// starts from a root key upjet actually provides, and that a ".parameters."
// access names a plausible Terraform attribute. This is the check that catches
// a misspelt root such as ".parameeters".
func TestExternalNameTemplateActionsAreWellFormed(t *testing.T) {
	fset := token.NewFileSet()
	for _, file := range externalNameSourceFiles {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("cannot parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.Contains(s, "{{") {
				return true
			}
			tmpl, err := template.New("t").Funcs(template.FuncMap{
				"ToLower": strings.ToLower,
				"ToUpper": strings.ToUpper,
			}).Parse(s)
			if err != nil {
				t.Errorf("%s: cannot parse template %q: %v", fset.Position(lit.Pos()), s, err)
				return true
			}
			for _, fn := range fieldNodes(tmpl.Root) {
				if err := checkFieldNode(fn); err != nil {
					t.Errorf("%s: template %q: %v", fset.Position(lit.Pos()), s, err)
				}
			}
			return true
		})
	}
}

func checkFieldNode(fn *parse.FieldNode) error {
	root := fn.Ident[0]
	if !templateRoots[root] {
		return &templateError{"." + strings.Join(fn.Ident, ".") + ": unknown root key " + strconv.Quote(root) + "; expected one of " + strings.Join(sortedKeys(templateRoots), ", ")}
	}
	switch root {
	case "external_name":
		if len(fn.Ident) != 1 {
			return &templateError{"." + strings.Join(fn.Ident, ".") + ": .external_name has no fields"}
		}
	case "parameters":
		if len(fn.Ident) < 2 {
			return &templateError{".parameters: must be followed by an attribute name"}
		}
		if !snakeCase.MatchString(fn.Ident[1]) {
			return &templateError{"." + strings.Join(fn.Ident, ".") + ": " + strconv.Quote(fn.Ident[1]) + " is not a snake_case Terraform attribute name"}
		}
	case "setup":
		if len(fn.Ident) < 2 || !setupKeys[fn.Ident[1]] {
			return &templateError{"." + strings.Join(fn.Ident, ".") + ": .setup key must be one of " + strings.Join(sortedKeys(setupKeys), ", ")}
		}
	}
	return nil
}

type templateError struct{ msg string }

func (e *templateError) Error() string { return e.msg }

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// fieldNodes collects every field access in a parsed template, including the
// ones nested in pipelines such as "{{ (index .parameters.x 0).name }}".
func fieldNodes(n parse.Node) []*parse.FieldNode {
	var out []*parse.FieldNode
	var walk func(parse.Node)
	walk = func(n parse.Node) {
		switch n := n.(type) {
		case nil:
		case *parse.FieldNode:
			out = append(out, n)
		case *parse.ListNode:
			if n == nil {
				return
			}
			for _, c := range n.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			walk(n.Pipe)
		case *parse.PipeNode:
			if n == nil {
				return
			}
			for _, c := range n.Cmds {
				walk(c)
			}
		case *parse.CommandNode:
			for _, c := range n.Args {
				walk(c)
			}
		case *parse.ChainNode:
			walk(n.Node)
		case *parse.IfNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.RangeNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.WithNode:
			walk(n.Pipe)
			walk(n.List)
			walk(n.ElseList)
		case *parse.TemplateNode:
			walk(n.Pipe)
		}
	}
	walk(n)
	return out
}

// TestExternalNameTemplateParametersExistInSchema renders every registered
// external-name configuration's GetIDFn against a parameter map built from the
// resource's real Terraform schema and a setup map with the keys
// internal/clients actually populates. Any action that does not resolve renders
// as "<no value>", so a template referring to an attribute the resource does
// not have fails here.
func TestExternalNameTemplateParametersExistInSchema(t *testing.T) {
	p, err := getProviderSchema(providerSchema)
	if err != nil {
		t.Fatalf("cannot load the provider schema: %v", err)
	}

	// Mirrors terraform.Setup.Map() as populated by internal/clients/aws.go.
	setup := map[string]any{
		"version": "v1",
		"requirement": map[string]string{
			"source":  "hashicorp/aws",
			"version": "v1",
		},
		"configuration": map[string]any{
			"region": "us-east-1",
		},
		"client_metadata": map[string]string{
			"account_id": "123456789012",
			"partition":  "aws",
		},
	}

	tables := map[string]map[string]config.ExternalName{
		// ExternalNameNotTestedConfigs is deliberately absent: it is not
		// referenced by any registry and is therefore not shipped.
		"TerraformPluginSDKExternalNameConfigs":       TerraformPluginSDKExternalNameConfigs,
		"TerraformPluginFrameworkExternalNameConfigs": TerraformPluginFrameworkExternalNameConfigs,
		"CLIReconciledExternalNameConfigs":            CLIReconciledExternalNameConfigs,
	}

	templated := templatedTableEntries(t)

	var skipped []string
	for table, configs := range tables {
		for name, en := range configs {
			tmpl, ok := templated[table][name]
			if !ok {
				// Not a template-based configuration; a hand-written GetIDFn
				// is free to reject or misread synthetic input.
				continue
			}
			r, ok := p.ResourcesMap[name]
			if !ok || r == nil {
				skipped = append(skipped, name)
				continue
			}
			id, err := en.GetIDFn(context.Background(), "external-name", dummyObject(r.Schema, 0), setup)
			if err != nil {
				t.Errorf("%s[%q]: cannot render %q: %v", table, name, tmpl, err)
				continue
			}
			if strings.Contains(id, "<no value>") {
				t.Errorf("%s[%q]: template %q rendered %q against a parameter map holding every attribute of the "+
					"resource's Terraform schema. A %q means an action names something the schema does not have: "+
					"a misspelt attribute, or a misspelt %q root.",
					table, name, tmpl, id, "<no value>", ".parameters")
			}
		}
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		t.Logf("%d templated resources are absent from the embedded provider schema and were not rendered: %s",
			len(skipped), strings.Join(skipped, ", "))
	}
}

// templatedTableEntries returns, per external-name table, the resource names
// whose configuration is built from a template string, along with that
// template. It reads them out of the source because config.ExternalName does
// not retain the template it was built from.
func templatedTableEntries(t *testing.T) map[string]map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "externalname.go", nil, 0)
	if err != nil {
		t.Fatalf("cannot parse externalname.go: %v", err)
	}
	out := map[string]map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			cl, ok := vs.Values[0].(*ast.CompositeLit)
			if !ok {
				continue
			}
			entries := map[string]string{}
			for _, e := range cl.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				kl, ok := kv.Key.(*ast.BasicLit)
				if !ok || kl.Kind != token.STRING {
					continue
				}
				key, err := strconv.Unquote(kl.Value)
				if err != nil {
					continue
				}
				if tmpl, ok := firstTemplateLiteral(kv.Value); ok {
					entries[key] = tmpl
				}
			}
			if len(entries) > 0 {
				out[vs.Names[0].Name] = entries
			}
		}
	}
	return out
}

// firstTemplateLiteral finds the template string a table entry is built from,
// wherever it sits in the expression: config.TemplatedStringAsIdentifier takes
// it as a second argument, the local wrappers take it as their only one.
func firstTemplateLiteral(e ast.Expr) (string, bool) {
	var found string
	ast.Inspect(e, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err == nil && strings.Contains(s, "{{") {
			found = s
			return false
		}
		return true
	})
	return found, found != ""
}

// maxDummyDepth bounds the synthetic parameter tree. External-name templates
// reach at most one level into a nested block, e.g.
// "{{ (index .parameters.lex_bot 0).name }}".
const maxDummyDepth = 2

func dummyObject(s map[string]*schema.Schema, depth int) map[string]any {
	o := make(map[string]any, len(s))
	for name, attr := range s {
		o[name] = dummyValue(attr, depth)
	}
	return o
}

func dummyValue(s *schema.Schema, depth int) any {
	switch s.Type { //nolint:exhaustive // the default covers the scalar types
	case schema.TypeBool:
		return true
	case schema.TypeInt, schema.TypeFloat:
		return 1
	case schema.TypeMap:
		return map[string]any{"dummy": "dummy"}
	case schema.TypeList, schema.TypeSet:
		if r, ok := s.Elem.(*schema.Resource); ok && depth < maxDummyDepth {
			return []any{dummyObject(r.Schema, depth+1)}
		}
		return []any{"dummy"}
	default:
		return "dummy"
	}
}
