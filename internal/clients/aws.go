// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/smithy-go/middleware"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	"github.com/crossplane/upjet/v2/pkg/metrics"
	"github.com/crossplane/upjet/v2/pkg/terraform"
	"github.com/hashicorp/aws-sdk-go-base/v2/endpoints"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-provider-aws/xpprovider"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	namespacedv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
)

const (
	keyAccountID        = "account_id"
	keyRegion           = "region"
	keyPartition        = "partition"
	localstackAccountID = "000000000000"
)

type SetupConfig struct {
	TerraformProvider *schema.Provider
	Logger            logging.Logger
}

// globalResources maps specific Kubernetes resource names to their corresponding AWS service names.
// These individual resources are global in nature (not region-specific) but still
// require a region to be set for Terraform AWS provider compatibility.
// Format: "group/kind" -> "service"
var globalResources = map[string]string{
	// Add specific global resources here as needed
	// Example: "backup.aws.upbound.io/GlobalSettings": "backup",
	"backup.aws.upbound.io/GlobalSettings":              "backup",
	"directconnect.aws.upbound.io/Gateway":              "directconnect",
	"directconnect.aws.upbound.io/GatewayAssociation":   "directconnect",
	"s3control.aws.upbound.io/AccountPublicAccessBlock": "s3control",
	// namespaced apis
	"backup.aws.m.upbound.io/GlobalSettings":              "backup",
	"directconnect.aws.m.upbound.io/Gateway":              "directconnect",
	"directconnect.aws.m.upbound.io/GatewayAssociation":   "directconnect",
	"s3control.aws.m.upbound.io/AccountPublicAccessBlock": "s3control",
}

// globalGroups maps Kubernetes API group names to their corresponding AWS service names.
// These groups contain resources that are global in nature (not region-specific) but still
// require a region to be set for Terraform AWS provider compatibility.
var globalGroups = map[string]string{
	"account.aws.upbound.io":                      "account",
	"budgets.aws.upbound.io":                      "budgets",
	"ce.aws.upbound.io":                           "ce",
	"cloudfront.aws.upbound.io":                   "cloudfront",
	"cur.aws.upbound.io":                          "cur",
	"globalaccelerator.aws.upbound.io":            "globalaccelerator",
	"iam.aws.upbound.io":                          "iam",
	"networkmanager.aws.upbound.io":               "networkmanager",
	"organizations.aws.upbound.io":                "organizations",
	"rolesanywhere.aws.upbound.io":                "rolesanywhere",
	"route53.aws.upbound.io":                      "route53",
	"route53recoverycontrolconfig.aws.upbound.io": "route53recoverycontrolconfig",
	"route53recoveryreadiness.aws.upbound.io":     "route53recoveryreadiness",
	"waf.aws.upbound.io":                          "waf",
	// namespaced apis
	"account.aws.m.upbound.io":                      "account",
	"budgets.aws.m.upbound.io":                      "budgets",
	"ce.aws.m.upbound.io":                           "ce",
	"cloudfront.aws.m.upbound.io":                   "cloudfront",
	"cur.aws.m.upbound.io":                          "cur",
	"globalaccelerator.aws.m.upbound.io":            "globalaccelerator",
	"iam.aws.m.upbound.io":                          "iam",
	"networkmanager.aws.m.upbound.io":               "networkmanager",
	"organizations.aws.m.upbound.io":                "organizations",
	"rolesanywhere.aws.m.upbound.io":                "rolesanywhere",
	"route53.aws.m.upbound.io":                      "route53",
	"route53recoverycontrolconfig.aws.m.upbound.io": "route53recoverycontrolconfig",
	"route53recoveryreadiness.aws.m.upbound.io":     "route53recoveryreadiness",
	"waf.aws.m.upbound.io":                          "waf",
}

func SelectTerraformSetup(config *SetupConfig) terraform.SetupFn { // nolint:gocyclo
	credsCache := NewAWSCredentialsProviderCache(WithCacheLogger(config.Logger))
	return func(ctx context.Context, c client.Client, mg resource.Managed) (terraform.Setup, error) {
		pc, err := resolveProviderConfig(ctx, c, mg)
		if err != nil {
			return terraform.Setup{}, err
		}

		// The resolved AWS config depends only on the ProviderConfig and the
		// region, so both make up the cache key below. For global API groups
		// the managed resource has no region of its own; an appropriate
		// partition-specific one is substituted, because the TF AWS provider
		// requires a non-empty region and so does the sts:GetCallerIdentity
		// used to resolve the account ID.
		region, err := effectiveRegion(mg, pc)
		if err != nil {
			return terraform.Setup{}, errors.Wrap(err, "cannot get region")
		}

		ps := terraform.Setup{}
		// The credential resolution below -- reading the credential Secret,
		// building the STS assume role providers and retrieving the AWS
		// account ID -- is cached per ProviderConfig and region, so on a
		// cache hit neither of the two functions passed here is invoked: no
		// Kubernetes API request and no STS call is made.
		awsCfg, credCache, err := credsCache.RetrieveConfig(ctx, pc, region,
			func(ctx context.Context) (*aws.Config, error) {
				cfg, err := GetAWSConfigWithoutTracking(ctx, c, mg, pc)
				if err != nil {
					return nil, err
				}
				if cfg.Region == "" {
					cfg.Region = region
				}
				return cfg, nil
			},
			func(ctx context.Context, cfg *aws.Config, creds aws.Credentials) (string, error) {
				if pc.Spec.SkipCredsValidation {
					// then we do not try to resolve the account ID and
					// instead, return a constant value as before.
					return localstackAccountID, nil
				}
				return getAccountId(ctx, cfg, creds)
			})
		if err != nil {
			return terraform.Setup{}, errors.Wrap(err, "cannot get aws config")
		} else if awsCfg == nil {
			return terraform.Setup{}, errors.New("obtained aws config cannot be nil")
		}

		// just in case the localstack implementation relies on this...
		if credCache.accountID == "" {
			credCache.accountID = localstackAccountID
		}
		ps.ClientMetadata = map[string]string{
			keyAccountID: credCache.accountID,
			keyPartition: "aws",
		}

		if err := setPartition(awsCfg, pc, &ps); err != nil {
			return terraform.Setup{}, errors.Wrap(err, "cannot configure AWS partition")
		}

		// several external name configs depend on the setup.Configuration for templating region
		ps.Configuration = map[string]any{
			keyRegion: awsCfg.Region,
		}
		if config.TerraformProvider == nil {
			return terraform.Setup{}, errors.New("terraform provider cannot be nil")
		}
		return ps, errors.Wrap(configureNoForkAWSClient(ctx, &ps, config, awsCfg.Region, credCache.creds, pc), "could not configure the no-fork AWS client")
	}
}

func setPartition(awsCfg *aws.Config, pc *namespacedv1beta1.ClusterProviderConfig, ps *terraform.Setup) error {
	var partitionFromConfig string
	if pc.Spec.Endpoint != nil && pc.Spec.Endpoint.PartitionID != nil {
		partitionFromConfig = *pc.Spec.Endpoint.PartitionID
		ps.ClientMetadata[keyPartition] = partitionFromConfig
	}
	// region should never be empty, but defensively code to preserve existing behavior
	if awsCfg.Region == "" {
		return nil
	}

	// TODO(erhan): localstack environments with ALLOW_NONSTANDARD_REGIONS configuration
	// might fail this check. Consider introducing a config that opt-out from partition
	// resolution
	partitionFromRegion, ok := endpoints.PartitionForRegion(endpoints.DefaultPartitions(), awsCfg.Region)
	if !ok || partitionFromRegion.ID() == "" {
		// tolerate unknown region and honor when explicit partition config exists
		if partitionFromConfig != "" {
			return nil
		}
		return errors.Errorf("managed resource region %q does not belong to a known partition", awsCfg.Region)
	}

	if partitionFromConfig != "" && partitionFromConfig != partitionFromRegion.ID() {
		return errors.Errorf("conflicting partition config: managed resource region %q does not belong to configured partition %q at provider config", awsCfg.Region, *pc.Spec.Endpoint.PartitionID)
	}

	ps.ClientMetadata[keyPartition] = partitionFromRegion.ID()
	return nil
}

// getAccountId retrieves the account ID associated with the given credentials.
// Results are cached.
func getAccountId(ctx context.Context, cfg *aws.Config, creds aws.Credentials) (string, error) {
	identity, err := GlobalCallerIdentityCache.GetCallerIdentity(ctx, *cfg, creds)
	if err != nil {
		return "", errors.Wrap(err, "cannot get the caller identity")
	}
	return *identity.Account, nil
}

// effectiveRegion returns the region to resolve credentials and the AWS config
// for. Managed resources in global API groups carry no region of their own, so
// a partition-specific one is substituted: the Terraform AWS provider requires
// a non-empty region, and so does the sts:GetCallerIdentity used to resolve the
// account ID. This must be applied before the region is used as part of a cache
// key, so that a global-group resource and a regional one do not share an entry
// resolved without a region.
func effectiveRegion(mg runtime.Object, pc *namespacedv1beta1.ClusterProviderConfig) (string, error) {
	region, err := getRegion(mg)
	if err != nil {
		return "", err
	}
	if region != "" {
		return region, nil
	}
	gvk := mg.GetObjectKind().GroupVersionKind()
	return getGlobalRegion(gvk.Group, gvk.Kind, pc), nil
}

// getGlobalRegion returns the appropriate region for global resources and API groups
// based on the partition. It first checks for resource-level configuration, then falls
// back to group-level configuration. It uses the generated partitions map to find
// the service-specific region, falling back to partition-specific defaults.
func getGlobalRegion(group, kind string, pc *namespacedv1beta1.ClusterProviderConfig) string {
	var serviceName string
	var isGlobal bool

	// First, check for resource-level configuration
	resourceKey := group + "/" + kind
	serviceName, isGlobal = globalResources[resourceKey]

	// If not found at resource level, check group-level configuration
	if !isGlobal {
		serviceName, isGlobal = globalGroups[group]
	}

	// If neither resource nor group is marked as global, return empty string
	if !isGlobal {
		return ""
	}

	// Determine the AWS partition, defaulting to "aws" if not explicitly configured
	partitionID := "aws" // default partition
	if pc != nil && pc.Spec.Endpoint != nil && pc.Spec.Endpoint.PartitionID != nil {
		partitionID = *pc.Spec.Endpoint.PartitionID
	}

	// Look up the service-specific default region for the determined partition
	if partition, exists := partitions[partitionID]; exists {
		if region, found := partition.serviceToDefaultRegions[serviceName]; found {
			return region
		}
		// Fallback to partition-specific default region
		return getPartitionDefaultRegion(partitionID)
	}

	// Ultimate fallback to us-east-1 if partition is not found
	return "us-east-1"
}

// getPartitionDefaultRegion returns the default region for a given partition
// when a service-specific region is not available in the partitions map.
func getPartitionDefaultRegion(partitionID string) string {
	switch partitionID {
	case "aws":
		return "us-east-1"
	case "aws-cn":
		return "cn-northwest-1"
	case "aws-iso":
		return "us-iso-east-1"
	case "aws-iso-b":
		return "us-isob-east-1"
	case "aws-iso-e":
		return "eu-isoe-west-1"
	case "aws-iso-f":
		return "us-isof-south-1"
	case "aws-us-gov":
		return "us-gov-west-1"
	case "aws-eusc":
		// aws-eusc doesn't have any services defined, but we need a fallback
		return "eusc-de-east-1"
	default:
		// For unknown partitions, fallback to us-east-1
		return "us-east-1"
	}
}

type metaOnlyPrimary struct {
	meta any
}

func (m *metaOnlyPrimary) Meta() any {
	return m.meta
}

// withExternalAPICallCounter configures an AWS SDK v2 stack (client)
// with an API call counter. AWS SDK v2 offers configuring
// "middlewares" to customize a request. Middlewares can be plugged
// into different steps of the stack. Middlewares can save and access
// metadata in the stack, such as ServiceID (EC2, IAM, etc.) and
// OperationName (DescribeVPCs, etc.). For documentation, see:
// https://aws.github.io/aws-sdk-go-v2/docs/middleware/
func withExternalAPICallCounter(stack *middleware.Stack) error {
	externalAPICallCounterMiddleware := middleware.DeserializeMiddlewareFunc("externalAPICallCounter",
		func(ctx context.Context, input middleware.DeserializeInput, next middleware.DeserializeHandler) (middleware.DeserializeOutput, middleware.Metadata, error) {
			serviceID := awsmiddleware.GetServiceID(ctx)
			operationName := awsmiddleware.GetOperationName(ctx)

			// next.HandleDeserialize() calls the next middleware function
			// in the stack, which in turn calls the next. Finally, the
			// request is performed. Each middleware function receives the
			// output from the middleware function it invoked, processes it,
			// and returns its result to the middleware function that
			// invoked itself.
			output, metadata, err := next.HandleDeserialize(ctx, input)
			if err == nil {
				metrics.ExternalAPICalls.WithLabelValues(serviceID, operationName).Inc()
			}
			return output, metadata, err
		},
	)

	// We register the call counter to the end of the deserialization
	// step, so that we're right next to Transport handler
	// (http.RoundTripper) in the stack (see
	// https://aws.github.io/aws-sdk-go-v2/docs/middleware/). In this
	// case, it's easy to distinguish API errors from connection
	// errors, because only connection errors cause a non-nil error
	// returned by next.HandleDeserialize() (see middleware
	// implementation above). If we were to register the call counter
	// to any other position (such as earlier stack steps (finalize,
	// build, etc.) or even the beginning of deserialization step), we
	// would have to implement a logic to distinguish between API
	// errors and connection errors.
	return stack.Deserialize.Add(externalAPICallCounterMiddleware, middleware.After)
}

// configureNoForkAWSClient populates the supplied *terraform.Setup with
// Terraform Plugin SDK style AWS client (Meta) and Terraform Plugin Framework
// style FrameworkProvider
func configureNoForkAWSClient(ctx context.Context, ps *terraform.Setup, config *SetupConfig, region string, creds aws.Credentials, pc *namespacedv1beta1.ClusterProviderConfig) error { //nolint:gocyclo
	tfAwsConnsCfg := xpprovider.AWSConfig{
		AccessKey:               creds.AccessKeyID,
		Endpoints:               map[string]string{},
		Region:                  region,
		S3UsePathStyle:          pc.Spec.S3UsePathStyle,
		SecretKey:               creds.SecretAccessKey,
		SkipCredsValidation:     true, // disabled to prevent extra AWS STS call
		SkipRegionValidation:    pc.Spec.SkipRegionValidation,
		SkipRequestingAccountId: true, // disabled to prevent extra AWS STS call
		Token:                   creds.SessionToken,
	}

	if pc.Spec.SkipMetadataApiCheck {
		tfAwsConnsCfg.EC2MetadataServiceEnableState = imds.ClientDisabled
	}

	if pc.Spec.Endpoint != nil {
		if pc.Spec.Endpoint.URL.Static != nil {
			if len(pc.Spec.Endpoint.Services) > 0 && *pc.Spec.Endpoint.URL.Static == "" {
				return errors.New("endpoint.url.static cannot be empty")
			} else {
				for _, service := range pc.Spec.Endpoint.Services {
					tfAwsConnsCfg.Endpoints[service] = aws.ToString(pc.Spec.Endpoint.URL.Static)
				}
			}
		}
	}

	// only used for retrieving the ServicePackages from the singleton provider instance
	p := config.TerraformProvider.Meta()

	xpac := &xpprovider.AWSClient{}
	xpac.SetServicePackagesField(p.(*xpprovider.AWSClient).GetServicePackages())

	tfAwsConnsClient, diags := tfAwsConnsCfg.GetClient(ctx, xpac)
	if diags.HasError() {
		return errors.Errorf("cannot construct TF AWS Client from TF AWS Config, %v", diags)
	}
	// accountID is already calculated/retrieved from Caller ID cache while
	// obtaining AWS config. The terraform config is explicitly constructed
	// to skip requesting account ID to prevent the extra STS call. Therefore,
	// the resulting TF AWS Client has empty account ID.
	// Fill with previously calculated account ID.
	// No need for nil check on ps.ClientMetadata per golang spec.
	tfAwsConnsClient.SetAccountID(ps.ClientMetadata[keyAccountID])
	ps.Meta = tfAwsConnsClient
	fwProvider := xpprovider.GetFrameworkProviderWithMeta(&metaOnlyPrimary{meta: tfAwsConnsClient})
	ps.FrameworkProvider = fwProvider

	// Register AWS SDK v2 call counter
	tfAwsConnsClient.AppendAPIOptions(withExternalAPICallCounter)

	return nil
}
