// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

// Command startup reproduces the provider's startup allocation path and
// attributes live heap and RSS to each step, then simulates a few candidate
// optimisations and reports what each would reclaim.
//
// See hack/memprofile/README.md.
package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/crossplane/upjet/v2/pkg/config"
	"github.com/crossplane/upjet/v2/pkg/registry"
	conversiontfjson "github.com/crossplane/upjet/v2/pkg/types/conversion/tfjson"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/hashicorp/terraform-provider-aws/xpprovider"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	clusterapis "github.com/upbound/provider-aws/v2/apis/cluster"
	namespacedapis "github.com/upbound/provider-aws/v2/apis/namespaced"
	awsconfig "github.com/upbound/provider-aws/v2/config"
	"github.com/upbound/provider-aws/v2/hack/memprofile/meminfo"
)

// family is the API group the "keep one family" simulation retains.
var family = "ec2"

var (
	lastLive float64
	mark     time.Time
	cumWork  time.Duration
)

// begin starts timing a step. The elapsed time step reports is the work
// itself, not the collections step forces around the reading.
func begin() { mark = time.Now() }

func step(name string) {
	work := time.Since(mark)
	cumWork += work
	// Two collections so that anything the previous step made unreachable is
	// really off the heap before we read it.
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	live := float64(m.HeapAlloc) / (1 << 20)
	fmt.Printf("%-46s live=%7.1f MiB  delta=%+8.1f  RSS=%7.1f  took=%8s\n",
		name, live, live-lastLive, meminfo.RSS(), work.Round(time.Millisecond))
	lastLive = live
	begin()
}

func main() {
	if f := os.Getenv("FAMILY"); f != "" {
		family = f
	}
	ctx := context.Background()
	preMain := meminfo.SinceExec()
	begin()
	step("0. process start (binary linked, inits run)")

	scheme := k8sruntime.NewScheme()
	must(clusterapis.AddToScheme(scheme))
	step("1. clusterapis.AddToScheme")

	must(namespacedapis.AddToScheme(scheme))
	step("2. namespacedapis.AddToScheme")

	must(apiextensionsv1.AddToScheme(scheme))
	step("3. apiextensionsv1.AddToScheme")

	fwProvider, sdkProvider, err := xpprovider.GetProvider(ctx)
	must(err)
	step("4. xpprovider.GetProvider (TF SDKv2 + FW)")

	clusterProvider, err := awsconfig.GetProvider(ctx, fwProvider, sdkProvider, false, false)
	must(err)
	step("5. config.GetProvider (cluster)")

	nsProvider, err := awsconfig.GetProviderNamespaced(ctx, fwProvider, sdkProvider, false, false)
	must(err)
	step("6. config.GetProviderNamespaced")

	fmt.Printf("\n   time from exec to main()             : %s\n", preMain.Round(time.Millisecond))
	fmt.Printf("   time in the startup path             : %s\n", cumWork.Round(time.Millisecond))
	fmt.Printf("   TF SDK provider ResourcesMap entries : %d\n", len(sdkProvider.ResourcesMap))
	fmt.Printf("   cluster config.Provider resources    : %d\n", len(clusterProvider.Resources))
	fmt.Printf("   namespaced config.Provider resources : %d\n", len(nsProvider.Resources))
	fmt.Printf("   scheme known GVKs                    : %d\n", len(scheme.AllKnownTypes()))
	fmt.Printf("   smaps: %s\n\n", meminfo.Smaps())

	fmt.Println("--- what each candidate optimisation would reclaim ---")

	dropMeta(clusterProvider)
	dropMeta(nsProvider)
	step("A. drop MetaResource (codegen-only docs)")

	total := len(clusterProvider.Resources)
	kept := keepOnlyGroup(clusterProvider, family)
	keepOnlyGroup(nsProvider, family)
	step(fmt.Sprintf("B. keep only %q (%d of %d resources)", family, kept, total))

	clusterProvider.Resources = nil
	nsProvider.Resources = nil
	step("C. drop both config.Provider resource maps")

	sdkProvider.ResourcesMap = nil
	sdkProvider.DataSourcesMap = nil
	step("D. drop TF SDK provider Resources/DataSources")

	runtime.KeepAlive(scheme)
	runtime.KeepAlive(fwProvider)

	if os.Getenv("GROUPS") != "" {
		printGroupHistogram(clusterProvider)
	}

	measureScopeIndependentPhases()
}

// measureScopeIndependentPhases times, in isolation, the phases of
// config.NewProvider that are identical for the cluster-scoped and namespaced
// builds: the tfjson unmarshal of the embedded JSON schema, its conversion to
// plugin-SDK resources, and the registry-metadata parse. Whatever the two
// builds cost beyond this is the genuinely per-scope work. Reads the same
// files config/ embeds, so it must run from the repository root.
func measureScopeIndependentPhases() {
	schemaBytes, err := os.ReadFile("config/schema.json")
	if err != nil {
		fmt.Printf("\n(skipping scope-independent phase timing: %v)\n", err)
		return
	}
	metaBytes, err := os.ReadFile("config/provider-metadata.yaml")
	if err != nil {
		fmt.Printf("\n(skipping scope-independent phase timing: %v)\n", err)
		return
	}

	fmt.Println("\n--- scope-independent phases inside each config.GetProvider* build ---")
	begin()
	ps := tfjson.ProviderSchemas{}
	must(ps.UnmarshalJSON(schemaBytes))
	step(fmt.Sprintf("P1. tfjson unmarshal of schema.json (%.1f MB)", float64(len(schemaBytes))/(1<<20)))

	var rs map[string]*tfjson.Schema
	for _, v := range ps.Schemas {
		rs = v.ResourceSchemas
		break
	}
	rm := conversiontfjson.GetV2ResourceMap(rs)
	step(fmt.Sprintf("P2. GetV2ResourceMap (%d resources)", len(rm)))

	pm, err := registry.NewProviderMetadataFromFile(metaBytes)
	must(err)
	step(fmt.Sprintf("P3. registry metadata parse (%d resources)", len(pm.Resources)))
	runtime.KeepAlive(rm)
	runtime.KeepAlive(pm)
}

// dropMeta releases the Terraform registry metadata, which only the code
// generation pipelines read.
func dropMeta(p *config.Provider) {
	for _, r := range p.Resources {
		r.MetaResource = nil
	}
}

// keepOnlyGroup deletes every resource outside the given API group and returns
// how many were kept.
func keepOnlyGroup(p *config.Provider, group string) int {
	kept := 0
	for n, r := range p.Resources {
		if r.ShortGroup != group {
			delete(p.Resources, n)
			continue
		}
		kept++
	}
	return kept
}

func printGroupHistogram(p *config.Provider) {
	h := map[string]int{}
	for _, r := range p.Resources {
		h[r.ShortGroup]++
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return h[keys[i]] > h[keys[j]] })
	for _, k := range keys {
		fmt.Printf("   %-30s %d\n", k, h[k])
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
