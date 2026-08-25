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

	"github.com/crossplane/upjet/v2/pkg/config"
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

var lastLive float64

func step(name string) {
	// Two collections so that anything the previous step made unreachable is
	// really off the heap before we read it.
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	live := float64(m.HeapAlloc) / (1 << 20)
	fmt.Printf("%-48s live=%8.1f MiB  delta=%+9.1f  RSS=%7.1f  peakRSS=%7.1f\n",
		name, live, live-lastLive, meminfo.RSS(), meminfo.PeakRSS())
	lastLive = live
}

func main() {
	if f := os.Getenv("FAMILY"); f != "" {
		family = f
	}
	ctx := context.Background()
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

	fmt.Printf("\n   TF SDK provider ResourcesMap entries : %d\n", len(sdkProvider.ResourcesMap))
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
