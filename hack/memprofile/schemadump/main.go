// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

// Command schemadump prints the visible flags of every attribute of every
// Terraform SDKv2 resource of the AWS provider, without running any of this
// repository's config/ edits. Comparing its output with the same dump taken
// from a fully configured provider (SCHEMA_DUMP=... hack/memprofile/reconcile)
// separates deliberate schema edits from accidental mutations of schema
// objects shared between resources.
//
// See hack/memprofile/README.md.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-provider-aws/xpprovider"
)

func dumpAttr(w *bufio.Writer, res, path string, s *schema.Schema) {
	fmt.Fprintf(w, "%s|%s|req=%v,opt=%v,comp=%v,sens=%v,forcenew=%v,type=%v\n",
		res, path, s.Required, s.Optional, s.Computed, s.Sensitive, s.ForceNew, s.Type)
	switch e := s.Elem.(type) {
	case *schema.Schema:
		dumpAttr(w, res, path+".elem", e)
	case *schema.Resource:
		keys := make([]string, 0, len(e.Schema))
		for k := range e.Schema {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			dumpAttr(w, res, path+"."+k, e.Schema[k])
		}
	}
}

// Dump writes the flag dump for the given resource map. Shared with the
// reconcile harness by convention, not by import: both emit the same format.
func Dump(w *bufio.Writer, resources map[string]*schema.Resource) {
	names := make([]string, 0, len(resources))
	for k := range resources {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		m := resources[name].SchemaMap()
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			dumpAttr(w, name, k, m[k])
		}
	}
}

func main() {
	ctx := context.Background()
	_, sdk, err := xpprovider.GetProvider(ctx)
	if err != nil {
		panic(err)
	}
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush() //nolint:errcheck // best-effort flush on exit
	Dump(w, sdk.ResourcesMap)
}
