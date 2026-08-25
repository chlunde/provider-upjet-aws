// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

// Command allapis reports the resident memory a process pays for linking in
// every API group of both scopes, which is what every family provider binary
// does today via apis/cluster and apis/namespaced.
//
// See hack/memprofile/README.md.
package main

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"

	clusterapis "github.com/upbound/provider-aws/v2/apis/cluster"
	namespacedapis "github.com/upbound/provider-aws/v2/apis/namespaced"
	"github.com/upbound/provider-aws/v2/hack/memprofile/meminfo"
)

func main() {
	meminfo.ReportLinkCost("all apis, before any work")

	s := runtime.NewScheme()
	if err := clusterapis.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := namespacedapis.AddToScheme(s); err != nil {
		panic(err)
	}
	meminfo.ReportLinkCost("all apis, after AddToScheme")
	fmt.Printf("   GVKs=%d\n   smaps: %s\n", len(s.AllKnownTypes()), meminfo.Smaps())
}
