// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

// Command tfaws reports the resident memory a process pays for linking in the
// Terraform AWS provider, which pulls in the service package and the AWS SDK
// client of every AWS service.
//
// See hack/memprofile/README.md.
package main

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-provider-aws/xpprovider"

	"github.com/upbound/provider-aws/v2/hack/memprofile/meminfo"
)

func main() {
	meminfo.ReportLinkCost("terraform-provider-aws, before any work")

	_, sdk, err := xpprovider.GetProvider(context.Background())
	if err != nil {
		panic(err)
	}
	meminfo.ReportLinkCost("terraform-provider-aws, after GetProvider")
	fmt.Printf("   resources=%d\n   smaps: %s\n", len(sdk.ResourcesMap), meminfo.Smaps())
}
