// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestEffectiveRegion covers the region substituted for managed resources in
// global API groups. This is applied before the region is used as part of the
// credential cache key, so a regression here resolves the AWS config -- and the
// sts:GetCallerIdentity behind it -- with an empty region, which fails for
// every resource in a global group.
func TestEffectiveRegion(t *testing.T) {
	mr := func(group, kind, region string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{"forProvider": map[string]any{}},
		}}
		if region != "" {
			u.Object["spec"].(map[string]any)["forProvider"].(map[string]any)["region"] = region
		}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: "v1beta1", Kind: kind})
		return u
	}

	cases := map[string]struct {
		obj  *unstructured.Unstructured
		want string
	}{
		"RegionalResourceKeepsItsOwnRegion": {
			obj:  mr("ec2.aws.upbound.io", "Instance", "eu-west-1"),
			want: "eu-west-1",
		},
		"GlobalGroupGetsPartitionDefault": {
			obj:  mr("iam.aws.upbound.io", "Role", ""),
			want: "us-east-1",
		},
		"NamespacedGlobalGroupGetsPartitionDefault": {
			obj:  mr("route53.aws.m.upbound.io", "Zone", ""),
			want: "us-east-1",
		},
		"GlobalGroupWithAnExplicitRegionKeepsIt": {
			obj:  mr("iam.aws.upbound.io", "Role", "eu-central-1"),
			want: "eu-central-1",
		},
		"NonGlobalGroupWithoutARegionStaysEmpty": {
			obj:  mr("ec2.aws.upbound.io", "Instance", ""),
			want: "",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := effectiveRegion(tc.obj, nil)
			if err != nil {
				t.Fatalf("effectiveRegion(...): unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("effectiveRegion(...): -want, +got:\n%s", diff)
			}
		})
	}
}
