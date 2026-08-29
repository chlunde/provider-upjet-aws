// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: CC0-1.0

package config

import (
	_ "embed"
	"os"
	"strings"

	"github.com/crossplane/upjet/v2/pkg/config"
	conversiontfjson "github.com/crossplane/upjet/v2/pkg/types/conversion/tfjson"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/pkg/errors"
)

var (
	//go:embed schema.json
	providerSchema string

	//go:embed provider-metadata.yaml
	providerMetadata []byte
)

var skipList = []string{
	"aws_waf_rule_group$",              // Too big CRD schema
	"aws_wafregional_rule_group$",      // Too big CRD schema
	"aws_ecs_tag$",                     // tags are already managed by ecs resources.
	"aws_alb$",                         // identical with aws_lb
	"aws_alb_target_group_attachment$", // identical with aws_lb_target_group_attachment
	"aws_iam_policy_attachment$",       // identical with aws_iam_*_policy_attachment resources.
	"aws_iam_group_policy$",            // identical with aws_iam_*_policy_attachment resources.
	"aws_iam_user_policy$",             // identical with aws_iam_*_policy_attachment resources.
	"aws_alb$",                         // identical with aws_lb.
	"aws_alb_listener$",                // identical with aws_lb_listener.
	"aws_alb_target_group$",            // identical with aws_lb_target_group.
	"aws_alb_target_group_attachment$", // identical with aws_lb_target_group_attachment.
	"aws_location_map$",                // failure with unknown reason.
	"aws_appflow_connector_profile$",   // failure with unknown reason.
	"aws_rds_reserved_instance",        // Expense of testing
}

// workaround for the TF AWS v4.67.0-based no-fork release: We would like to
// keep the types in the generated CRDs intact
// (prevent number->int type replacements).
func getProviderSchema(s string) (*schema.Provider, error) {
	ps := tfjson.ProviderSchemas{}
	if err := ps.UnmarshalJSON([]byte(s)); err != nil {
		panic(err)
	}
	if len(ps.Schemas) != 1 {
		return nil, errors.Errorf("there should exactly be 1 provider schema but there are %d", len(ps.Schemas))
	}
	var rs map[string]*tfjson.Schema
	for _, v := range ps.Schemas {
		rs = v.ResourceSchemas
		break
	}
	return &schema.Provider{
		ResourcesMap: conversiontfjson.GetV2ResourceMap(rs),
	}, nil
}

// CLIReconciledResourceList returns the list of resources that have external
// name configured in ExternalNameConfigs table and to be reconciled under
// the TF CLI based architecture.
func CLIReconciledResourceList() []string {
	l := make([]string, len(CLIReconciledExternalNameConfigs))
	i := 0
	for name := range CLIReconciledExternalNameConfigs {
		// Expected format is regex, and we'd like to have exact matches.
		l[i] = name + "$"
		i++
	}
	return filterToFamily(l)
}

// TerraformPluginSDKResourceList returns the list of resources that have external
// name configured in ExternalNameConfigs table and to be reconciled under
// the no-fork architecture.
func TerraformPluginSDKResourceList() []string {
	l := make([]string, len(TerraformPluginSDKExternalNameConfigs))
	i := 0
	for name := range TerraformPluginSDKExternalNameConfigs {
		// Expected format is regex, and we'd like to have exact matches.
		l[i] = name + "$"
		i++
	}
	return filterToFamily(l)
}

func TerraformPluginFrameworkResourceList() []string {
	l := make([]string, len(TerraformPluginFrameworkExternalNameConfigs))
	i := 0
	for name := range TerraformPluginFrameworkExternalNameConfigs {
		// Expected format is regex, and we'd like to have exact matches.
		l[i] = name + "$"
		i++
	}
	return filterToFamily(l)
}

// dropCodegenOnlyMetadata releases the Terraform registry metadata (resource
// descriptions, argument docs and examples scraped from the Terraform provider
// documentation) that upjet attaches to every config.Resource. It is only read
// by the code generation pipelines, so keeping it around costs the provider
// runtime tens of MiB of live heap for the ~1000 configured resources without
// ever being used. Must only be called for a non-generation provider, and only
// after the reference injectors and the resource configurators have run, since
// those do consume the metadata.
func dropCodegenOnlyMetadata(pc *config.Provider) {
	for _, r := range pc.Resources {
		r.MetaResource = nil
	}
}

// familyFilterEnv names the environment variable that restricts the include
// lists this provider hands to upjet's config.NewProvider to one or more API
// short groups (comma separated), e.g. UPJET_FAMILY_FILTER=s3.
//
// It is deliberately inert unless the variable is set, so one binary can serve
// both arms of a measurement.
const familyFilterEnv = "UPJET_FAMILY_FILTER"

// shortGroupOf returns the API short group that would be assigned to the given
// Terraform resource name, computed statically from the name alone. It mirrors
// upjet's config.DefaultResource default (the second word of the resource name,
// or the first when the name has fewer than three words) as overridden by this
// repository's GroupMap.
func shortGroupOf(resource string) string {
	if f, ok := GroupMap[resource]; ok {
		g, _ := f(resource)
		return g
	}
	words := strings.Split(resource, "_")
	if len(words) < 3 {
		return words[0]
	}
	return words[1]
}

// filterToFamily drops from an include list every entry whose API short group
// is not named by familyFilterEnv.
func filterToFamily(l []string) []string {
	v := os.Getenv(familyFilterEnv)
	if v == "" {
		return l
	}
	keep := map[string]bool{}
	for _, g := range strings.Split(v, ",") {
		if g = strings.TrimSpace(g); g != "" {
			keep[g] = true
		}
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		if keep[shortGroupOf(strings.TrimSuffix(e, "$"))] {
			out = append(out, e)
		}
	}
	return out
}
