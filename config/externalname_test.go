// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: CC0-1.0

package config

import (
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

func TestWafv2WebACLRuleGroupAssociationGetExternalNameFn(t *testing.T) {
	e := wafv2WebACLRuleGroupAssociation()

	const webACLArn = "arn:aws:wafv2:us-east-1:123456789012:regional/webacl/my-acl/abcd"
	const ruleGroupArn = "arn:aws:wafv2:us-east-1:123456789012:regional/rulegroup/my-rg/efgh"

	cases := map[string]struct {
		tfstate map[string]any
		want    string
		wantErr bool
	}{
		// Observe path: singleton lists have already been converted to
		// embedded objects, so the nested block is a map.
		"CustomRuleGroupAsMap": {
			tfstate: map[string]any{
				"web_acl_arn": webACLArn,
				"rule_name":   "my-rule",
				"rule_group_reference": map[string]any{
					"arn": ruleGroupArn,
				},
			},
			want: webACLArn + ",my-rule,custom," + ruleGroupArn,
		},
		// Create path: the external name is computed from the raw Terraform
		// state, where the nested block is still a one-element list.
		"CustomRuleGroupAsSingletonList": {
			tfstate: map[string]any{
				"web_acl_arn": webACLArn,
				"rule_name":   "my-rule",
				"rule_group_reference": []any{
					map[string]any{"arn": ruleGroupArn},
				},
			},
			want: webACLArn + ",my-rule,custom," + ruleGroupArn,
		},
		"ManagedRuleGroupAsMapWithoutVersion": {
			tfstate: map[string]any{
				"web_acl_arn": webACLArn,
				"rule_name":   "my-rule",
				"managed_rule_group": map[string]any{
					"vendor_name": "AWS",
					"name":        "AWSManagedRulesCommonRuleSet",
				},
			},
			want: webACLArn + ",my-rule,managed,AWS:AWSManagedRulesCommonRuleSet",
		},
		"ManagedRuleGroupAsMapWithVersion": {
			tfstate: map[string]any{
				"web_acl_arn": webACLArn,
				"rule_name":   "my-rule",
				"managed_rule_group": map[string]any{
					"vendor_name": "AWS",
					"name":        "AWSManagedRulesCommonRuleSet",
					"version":     "Version_1.0",
				},
			},
			want: webACLArn + ",my-rule,managed,AWS:AWSManagedRulesCommonRuleSet:Version_1.0",
		},
		"ManagedRuleGroupAsSingletonListWithoutVersion": {
			tfstate: map[string]any{
				"web_acl_arn": webACLArn,
				"rule_name":   "my-rule",
				"managed_rule_group": []any{
					map[string]any{
						"vendor_name": "AWS",
						"name":        "AWSManagedRulesCommonRuleSet",
					},
				},
			},
			want: webACLArn + ",my-rule,managed,AWS:AWSManagedRulesCommonRuleSet",
		},
		"ManagedRuleGroupAsSingletonListWithVersion": {
			tfstate: map[string]any{
				"web_acl_arn": webACLArn,
				"rule_name":   "my-rule",
				"managed_rule_group": []any{
					map[string]any{
						"vendor_name": "AWS",
						"name":        "AWSManagedRulesCommonRuleSet",
						"version":     "Version_1.0",
					},
				},
			},
			want: webACLArn + ",my-rule,managed,AWS:AWSManagedRulesCommonRuleSet:Version_1.0",
		},
		"NoRuleGroupBlock": {
			tfstate: map[string]any{
				"web_acl_arn": webACLArn,
				"rule_name":   "my-rule",
			},
			wantErr: true,
		},
		"EmptyRuleGroupLists": {
			tfstate: map[string]any{
				"web_acl_arn":          webACLArn,
				"rule_name":            "my-rule",
				"rule_group_reference": []any{},
				"managed_rule_group":   []any{},
			},
			wantErr: true,
		},
		"RuleGroupListWithNonMapElement": {
			tfstate: map[string]any{
				"web_acl_arn":          webACLArn,
				"rule_name":            "my-rule",
				"rule_group_reference": []any{"not-a-map"},
			},
			wantErr: true,
		},
		"NilRuleGroupBlocks": {
			tfstate: map[string]any{
				"web_acl_arn":          webACLArn,
				"rule_name":            "my-rule",
				"rule_group_reference": nil,
				"managed_rule_group":   nil,
			},
			wantErr: true,
		},
		"ManagedRuleGroupMissingName": {
			tfstate: map[string]any{
				"web_acl_arn": webACLArn,
				"rule_name":   "my-rule",
				"managed_rule_group": []any{
					map[string]any{"vendor_name": "AWS"},
				},
			},
			wantErr: true,
		},
		"MissingWebACLArn": {
			tfstate: map[string]any{
				"rule_name": "my-rule",
				"rule_group_reference": map[string]any{
					"arn": ruleGroupArn,
				},
			},
			wantErr: true,
		},
		"MissingRuleName": {
			tfstate: map[string]any{
				"web_acl_arn": webACLArn,
				"rule_group_reference": map[string]any{
					"arn": ruleGroupArn,
				},
			},
			wantErr: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := e.GetExternalNameFn(tc.tfstate)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("GetExternalNameFn() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetExternalNameFn() returned unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("GetExternalNameFn() = %q, want %q", got, tc.want)
			}
		})
	}
}
