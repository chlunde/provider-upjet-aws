// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

// Command familyapis reports the resident memory a process pays for linking in
// a single API group, for comparison with the allapis program. It stands in for
// what a per-family binary would cost if the API packages were compiled per
// family instead of aggregated by apis/cluster and apis/namespaced.
//
// See hack/memprofile/README.md.
package main

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"

	clusterv1beta1 "github.com/upbound/provider-aws/v2/apis/cluster/ec2/v1beta1"
	clusterv1beta2 "github.com/upbound/provider-aws/v2/apis/cluster/ec2/v1beta2"
	namespacedv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"github.com/upbound/provider-aws/v2/hack/memprofile/meminfo"
)

func main() {
	meminfo.ReportLinkCost("ec2 apis, before any work")

	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clusterv1beta1.AddToScheme,
		clusterv1beta2.AddToScheme,
		namespacedv1beta1.AddToScheme,
	} {
		if err := add(s); err != nil {
			panic(err)
		}
	}
	meminfo.ReportLinkCost("ec2 apis, after AddToScheme")
	fmt.Printf("   GVKs=%d\n   smaps: %s\n", len(s.AllKnownTypes()), meminfo.Smaps())
}
