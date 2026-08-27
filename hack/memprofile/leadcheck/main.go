// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

// Command leadcheck runs the cheap, local experiments used to triage the
// candidate findings in docs/lead-triage.md. It talks to nothing: no cluster,
// no AWS account. Run it with `go run ./hack/memprofile/leadcheck`.
package main

import (
	"fmt"

	"github.com/crossplane/upjet/v2/pkg/types/name"
)

// snakeRoundTrip reports whether upjet's camel->snake converter is the inverse
// of its snake->camel converter for the given Terraform attribute name. The
// annotation-based field-conversion machinery
// (upjet/pkg/controller/annotation_conversions.go) relies on it being so.
func snakeRoundTrip(snake string) (camel, back string, ok bool) {
	camel = name.NewFromSnake(snake).LowerCamel
	back = name.NewFromCamel(camel).Snake
	return camel, back, back == snake
}

func main() {
	fmt.Println("== L19: snake -> lowerCamel -> snake round trip ==")
	fmt.Printf("%-34s %-34s %-34s %s\n", "terraform attribute", "CRD field (LowerCamel)", "back to snake", "lossless")
	for _, s := range []string{
		"vpc_id",
		"instance_type",
		"ipv6_addresses",
		"ipv6_address_count",
		"s3_bucket_name",
		"cloudwatch_log_group_arn",
		"ipv4_prefixes",
		"sha256_tree_hash",
		"iam_instance_profile",
		"kms_key_id",
		"http_put_response_hop_limit",
		"ebs_block_device",
		"az_mode",
		"acl",
	} {
		camel, back, ok := snakeRoundTrip(s)
		fmt.Printf("%-34s %-34s %-34s %v\n", s, camel, back, ok)
	}

	fmt.Println()
	fmt.Println("== L19: whole field-path expressions through NewFromCamel().Snake ==")
	// moveTFStateValuesToAnnotation and mergeAnnotationFieldsWithSpec apply the
	// converter to a whole fieldpath expression, not to a single name.
	for _, p := range []string{
		"fooBar",
		"fooBar.bazQux",
		"fooBar[0].bazQux",
		"ipv6Addresses[0]",
		"networkInterface[0].deviceIndex",
	} {
		fmt.Printf("%-34s -> %s\n", p, name.NewFromCamel(p).Snake)
	}
}
