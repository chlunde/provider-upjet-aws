// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: CC0-1.0

package config

import (
	"context"
	"strings"
	"testing"
)

func TestEcsTaskDefinitionSetIdentifierArgumentFn(t *testing.T) {
	e := ecsTaskDefinition()

	cases := map[string]struct {
		base         map[string]any
		externalName string
		wantArn      string
	}{
		"ColdStartWithFullARN": {
			base:         map[string]any{},
			externalName: "arn:aws:ecs:us-east-1:123456789012:task-definition/my-service:7",
			wantArn:      "arn:aws:ecs:us-east-1:123456789012:task-definition/my-service:7",
		},
		"ColdStartWithFamilyRevision": {
			base:         map[string]any{},
			externalName: "my-service:7",
			wantArn:      "",
		},
		"ColdStartWithFamilyOnly": {
			base:         map[string]any{},
			externalName: "my-service",
			wantArn:      "",
		},
		"EmptyExternalName": {
			base:         map[string]any{},
			externalName: "",
			wantArn:      "",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e.SetIdentifierArgumentFn(tc.base, tc.externalName)
			got, _ := tc.base["arn"].(string)
			if got != tc.wantArn {
				t.Errorf("base[\"arn\"] = %q, want %q", got, tc.wantArn)
			}
		})
	}
}

// TestAppStreamUserStackAssociationID pins the composed Terraform ID against
// the format the AWS provider itself composes and parses:
// userStackAssociationCreateResourceID joins user_name, authentication_type and
// stack_name with "/" and nothing else, and userStackAssociationParseResourceID
// reads them back with strings.SplitN(id, "/", 3) — so a trailing separator is
// not ignored, it becomes part of the stack name.
func TestAppStreamUserStackAssociationID(t *testing.T) {
	e := TerraformPluginSDKExternalNameConfigs["aws_appstream_user_stack_association"]
	id, err := e.GetIDFn(context.Background(), "", map[string]any{
		"user_name":           "user@example.com",
		"authentication_type": "USERPOOL",
		"stack_name":          "my-stack",
	}, nil)
	if err != nil {
		t.Fatalf("GetIDFn returned an error: %v", err)
	}
	const want = "user@example.com/USERPOOL/my-stack"
	if id != want {
		t.Errorf("GetIDFn() = %q, want %q", id, want)
	}
	// What the provider's Read and Delete do with the ID.
	parts := strings.SplitN(id, "/", 3)
	if len(parts) != 3 || parts[0] != "user@example.com" || parts[1] != "USERPOOL" || parts[2] != "my-stack" {
		t.Errorf("the provider parses %q as %q, want [user@example.com USERPOOL my-stack]", id, parts)
	}
}
