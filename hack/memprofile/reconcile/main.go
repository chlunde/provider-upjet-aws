// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

// Command reconcile measures the per-reconcile costs that the Connect and
// Observe path pays for every managed resource, including a steady-state
// observation of an object nobody has modified.
//
// See hack/memprofile/README.md.
package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/crossplane/upjet/v2/pkg/config"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-provider-aws/xpprovider"

	awsconfig "github.com/upbound/provider-aws/v2/config"
)

// probes are resources chosen to span the schema-size range.
var probes = []string{
	"aws_instance",
	"aws_s3_bucket",
	"aws_iam_role",
	"aws_lb",
	"aws_vpc",
	"aws_sns_topic_subscription",
}

func allocs(n int, fn func()) (time.Duration, uint64) {
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	start := time.Now()
	for range n {
		fn()
	}
	d := time.Since(start) / time.Duration(n)
	runtime.ReadMemStats(&b)
	return d, (b.TotalAlloc - a.TotalAlloc) / uint64(n) //nolint:gosec // n is a small positive constant
}

func main() {
	ctx := context.Background()
	fw, sdk, err := xpprovider.GetProvider(ctx)
	must(err)
	pc, err := awsconfig.GetProvider(ctx, fw, sdk, false, false)
	must(err)

	fmt.Println("=== 1. Is SchemaFunc left set after upjet materialises Schema? ===")
	withFunc, withSchema, both := 0, 0, 0
	for _, r := range pc.Resources {
		tr := r.TerraformResource
		if tr == nil {
			continue
		}
		if tr.SchemaFunc != nil {
			withFunc++
		}
		if tr.Schema != nil {
			withSchema++
		}
		if tr.SchemaFunc != nil && tr.Schema != nil {
			both++
		}
	}
	fmt.Printf("   configured resources        : %d\n", len(pc.Resources))
	fmt.Printf("   with .Schema set            : %d\n", withSchema)
	fmt.Printf("   with .SchemaFunc set        : %d\n", withFunc)
	fmt.Printf("   with BOTH set               : %d   <- InternalValidate calls this an error\n\n", both)

	fmt.Println("=== 2. Do the provider's schema mutations survive into SchemaMap()? ===")
	missingInLive, extraInLive, flagsDiffer := 0, 0, 0
	affected := map[string]bool{}
	var examples []string
	note := func(format string, a ...any) {
		if len(examples) < 8 {
			examples = append(examples, fmt.Sprintf(format, a...))
		}
	}
	for name, r := range pc.Resources {
		tr := r.TerraformResource
		if tr == nil || tr.SchemaFunc == nil || tr.Schema == nil {
			continue
		}
		live := tr.SchemaMap() // what every TF SDK code path actually uses
		for k, ms := range tr.Schema {
			ls, ok := live[k]
			if !ok {
				extraInLive++ // present in mutated map, absent from live
				affected[name] = true
				note("%s: %q in .Schema but not in SchemaMap()", name, k)
				continue
			}
			if ms.Required != ls.Required || ms.Optional != ls.Optional || ms.Computed != ls.Computed {
				flagsDiffer++
				affected[name] = true
				note("%s.%s mutated{req=%v opt=%v comp=%v} live{req=%v opt=%v comp=%v}",
					name, k, ms.Required, ms.Optional, ms.Computed, ls.Required, ls.Optional, ls.Computed)
			}
		}
		for k := range live {
			if _, ok := tr.Schema[k]; !ok {
				missingInLive++ // deleted from mutated map, still live
				affected[name] = true
				note("%s: %q deleted from .Schema but still in SchemaMap()", name, k)
			}
		}
	}
	fmt.Printf("   resources with any divergence : %d of %d\n", len(affected), both)
	fmt.Printf("   attrs only in .Schema         : %d\n", extraInLive)
	fmt.Printf("   attrs deleted but still live  : %d\n", missingInLive)
	fmt.Printf("   attrs whose flags differ      : %d\n", flagsDiffer)
	for _, e := range examples {
		fmt.Printf("     %s\n", e)
	}
	fmt.Println()

	fmt.Println("=== 3. Cost of the schema rebuild the reconcile path pays ===")
	fmt.Printf("   %-30s %10s %12s %10s %12s\n", "resource", "SchemaMap", "alloc", "CoreConfig", "alloc")
	for _, name := range probes {
		r := pc.Resources[name]
		if r == nil || r.TerraformResource == nil {
			continue
		}
		tr := r.TerraformResource
		dm, am := allocs(50, func() { _ = tr.SchemaMap() })
		dc, ac := allocs(50, func() { _ = tr.CoreConfigSchema() })
		fmt.Printf("   %-30s %10s %10s KB %10s %10s KB\n",
			name, dm.Round(time.Microsecond), kb(am), dc.Round(time.Microsecond), kb(ac))
	}

	fmt.Println("\n   same resources with SchemaFunc cleared (the proposed fix):")
	fmt.Printf("   %-30s %10s %12s %10s %12s\n", "resource", "SchemaMap", "alloc", "CoreConfig", "alloc")
	for _, name := range probes {
		r := pc.Resources[name]
		if r == nil || r.TerraformResource == nil {
			continue
		}
		tr := r.TerraformResource
		saved := tr.SchemaFunc
		tr.SchemaFunc = nil
		dm, am := allocs(50, func() { _ = tr.SchemaMap() })
		dc, ac := allocs(50, func() { _ = tr.CoreConfigSchema() })
		tr.SchemaFunc = saved
		fmt.Printf("   %-30s %10s %10s KB %10s %10s KB\n",
			name, dm.Round(time.Microsecond), kb(am), dc.Round(time.Microsecond), kb(ac))
	}

	fmt.Println("\n=== 4. SchemaFunc invocations per reconcile ===")
	countSchemaFuncCalls(pc)

	fmt.Println("\n=== 5. Per-Connect construction in internal/clients/aws.go ===")
	if os.Getenv("SKIP_FW") == "" {
		measureFrameworkProvider(ctx, sdk)
	}
	_ = schema.TypeString
}

func kb(b uint64) string { return fmt.Sprintf("%.0f", float64(b)/1024) }

// countSchemaFuncCalls wraps SchemaFunc with a counter and drives the schema
// operations a single Connect+Observe pair performs, excluding the AWS Read
// itself, which needs live credentials.
func countSchemaFuncCalls(pc *config.Provider) {
	for _, name := range probes {
		r := pc.Resources[name]
		if r == nil || r.TerraformResource == nil || r.TerraformResource.SchemaFunc == nil {
			continue
		}
		tr := r.TerraformResource
		orig := tr.SchemaFunc
		n := 0
		tr.SchemaFunc = func() map[string]*schema.Schema { n++; return orig() }

		start := time.Now()
		// Connect: getExtendedParameters tags_all probe, then schemaBlock.
		_ = tr.CoreConfigSchema()
		block := tr.CoreConfigSchema()
		// Connect, cold cache: rebuild the instance state from the observation.
		empty, err := schema.JSONMapToStateValue(map[string]any{}, block)
		if err == nil {
			_, _ = tr.ShimInstanceStateFromValue(empty)
		}
		// Observe: getResourceDataDiff applies the diff to an empty value.
		_ = tr.CoreConfigSchema()
		d := time.Since(start)

		tr.SchemaFunc = orig
		fmt.Printf("   %-30s %2d SchemaFunc calls, %8s (AWS Read excluded)\n", name, n, d.Round(time.Microsecond))
	}
}

func measureFrameworkProvider(ctx context.Context, sdk *schema.Provider) {
	// This is what internal/clients/aws.go does on every Connect: take the
	// service packages off the singleton provider's meta and build a client.
	meta, ok := sdk.Meta().(*xpprovider.AWSClient)
	if !ok {
		fmt.Println("   singleton provider meta is not an *AWSClient; skipping")
		return
	}
	pkgs := meta.GetServicePackages()
	fmt.Printf("   service packages on the singleton meta : %d\n", len(pkgs))

	cfg := xpprovider.AWSConfig{
		AccessKey:               "AKIAIOSFODNN7EXAMPLE",
		SecretKey:               "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Region:                  "us-east-1",
		Endpoints:               map[string]string{},
		SkipCredsValidation:     true,
		SkipRegionValidation:    true,
		SkipRequestingAccountId: true,
	}
	client := &xpprovider.AWSClient{}
	client.SetServicePackagesField(pkgs)
	built, diags := cfg.GetClient(ctx, client)
	if diags.HasError() {
		fmt.Printf("   could not construct a TF AWS client here (%v)\n", diags)
		return
	}

	d, a := allocs(20, func() {
		_ = xpprovider.GetFrameworkProviderWithMeta(&metaOnly{built})
	})
	fmt.Printf("   GetFrameworkProviderWithMeta : %10s, %8s KB per call\n", d.Round(time.Microsecond), kb(a))

	d2, a2 := allocs(10, func() {
		c := &xpprovider.AWSClient{}
		c.SetServicePackagesField(pkgs)
		_, _ = cfg.GetClient(ctx, c)
	})
	fmt.Printf("   AWSConfig.GetClient          : %10s, %8s KB per call\n", d2.Round(time.Microsecond), kb(a2))
	fmt.Printf("   combined, per reconcile      : %10s, %8s KB\n",
		(d + d2).Round(time.Microsecond), kb(a+a2))
}

type metaOnly struct{ m any }

func (m *metaOnly) Meta() any { return m.m }

func must(err error) {
	if err != nil {
		panic(err)
	}
}

var _ = config.Provider{}
