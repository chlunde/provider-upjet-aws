// SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package clients

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/google/go-cmp/cmp"

	"github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
)

// TestTFEndpointOverrides covers the endpoint overrides handed to the Terraform
// AWS client -- the client that performs every resource create, read, update
// and delete. Before the Dynamic case was implemented here, a ProviderConfig
// with endpoint.url.type: Dynamic produced no overrides at all and all resource
// traffic went to the public AWS endpoints.
func TestTFEndpointOverrides(t *testing.T) {
	cases := map[string]struct {
		endpoint *v1beta1.EndpointConfig
		region   string
		want     map[string]string
		wantErr  string
	}{
		"NoEndpointConfig": {
			endpoint: nil,
			region:   "eu-west-1",
			want:     map[string]string{},
		},
		"StaticAppliesTheSameURLToEveryService": {
			endpoint: &v1beta1.EndpointConfig{
				URL: v1beta1.URLConfig{
					Type:   URLConfigTypeStatic,
					Static: aws.String("http://localstack:4566"),
				},
				Services: []string{"s3", "ec2", "iam"},
			},
			region: "eu-west-1",
			want: map[string]string{
				"s3":  "http://localstack:4566",
				"ec2": "http://localstack:4566",
				"iam": "http://localstack:4566",
			},
		},
		"StaticWithoutServicesOverridesNothing": {
			endpoint: &v1beta1.EndpointConfig{
				URL: v1beta1.URLConfig{
					Type:   URLConfigTypeStatic,
					Static: aws.String("http://localstack:4566"),
				},
			},
			region: "eu-west-1",
			want:   map[string]string{},
		},
		"StaticEmptyURLWithServices": {
			endpoint: &v1beta1.EndpointConfig{
				URL: v1beta1.URLConfig{
					Type:   URLConfigTypeStatic,
					Static: aws.String(""),
				},
				Services: []string{"s3"},
			},
			region:  "eu-west-1",
			wantErr: "endpoint.url.static cannot be empty",
		},
		"StaticWithoutStaticField": {
			endpoint: &v1beta1.EndpointConfig{
				URL:      v1beta1.URLConfig{Type: URLConfigTypeStatic},
				Services: []string{"s3"},
			},
			region:  "eu-west-1",
			wantErr: `endpoint.url.static must be set when endpoint.url.type is "Static"`,
		},
		"DynamicTemplatesRegionalAndGlobalServices": {
			endpoint: &v1beta1.EndpointConfig{
				URL: v1beta1.URLConfig{
					Type: URLConfigTypeDynamic,
					Dynamic: &v1beta1.DynamicURLConfig{
						Protocol: "https",
						Host:     "vpce.example.com",
					},
				},
				// iam is global and must not carry a region.
				Services: []string{"ec2", "s3", "iam"},
			},
			region: "eu-west-1",
			want: map[string]string{
				"ec2": "https://ec2.eu-west-1.vpce.example.com",
				"s3":  "https://s3.eu-west-1.vpce.example.com",
				"iam": "https://iam.vpce.example.com",
			},
		},
		"DynamicHonoursProtocol": {
			endpoint: &v1beta1.EndpointConfig{
				URL: v1beta1.URLConfig{
					Type: URLConfigTypeDynamic,
					Dynamic: &v1beta1.DynamicURLConfig{
						Protocol: "http",
						Host:     "proxy.internal",
					},
				},
				Services: []string{"rds"},
			},
			region: "us-gov-west-1",
			want:   map[string]string{"rds": "http://rds.us-gov-west-1.proxy.internal"},
		},
		"DynamicWithoutDynamicField": {
			endpoint: &v1beta1.EndpointConfig{
				URL:      v1beta1.URLConfig{Type: URLConfigTypeDynamic},
				Services: []string{"ec2"},
			},
			region:  "eu-west-1",
			wantErr: `endpoint.url.dynamic must be set when endpoint.url.type is "Dynamic"`,
		},
		"AutoResolvesFromThePartition": {
			endpoint: &v1beta1.EndpointConfig{
				URL:         v1beta1.URLConfig{Type: URLConfigTypeAuto},
				PartitionID: aws.String("aws-us-gov"),
				Services:    []string{"ec2"},
			},
			region: "us-gov-west-1",
			want:   map[string]string{},
		},
		"UnknownTypeIsRejectedRatherThanIgnored": {
			endpoint: &v1beta1.EndpointConfig{
				URL:      v1beta1.URLConfig{Type: "Whatever"},
				Services: []string{"ec2"},
			},
			region:  "eu-west-1",
			wantErr: `unsupported endpoint.url.type "Whatever"`,
		},
		"EmptyTypeIsRejectedRatherThanIgnored": {
			endpoint: &v1beta1.EndpointConfig{
				URL:      v1beta1.URLConfig{},
				Services: []string{"ec2"},
			},
			region:  "eu-west-1",
			wantErr: `unsupported endpoint.url.type ""`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := tfEndpointOverrides(tc.endpoint, tc.region)
			switch {
			case tc.wantErr != "" && err == nil:
				t.Fatalf("tfEndpointOverrides(...): want error %q, got none (endpoints %v)", tc.wantErr, got)
			case tc.wantErr != "" && err.Error() != tc.wantErr:
				t.Fatalf("tfEndpointOverrides(...): want error %q, got %q", tc.wantErr, err.Error())
			case tc.wantErr != "":
				return
			case err != nil:
				t.Fatalf("tfEndpointOverrides(...): unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("tfEndpointOverrides(...): -want, +got:\n%s", diff)
			}
		})
	}
}

// TestDynamicEndpointAgreesWithSDKResolver pins the two consumers of a Dynamic
// endpoint configuration to the same URL: the AWS SDK resolver installed by
// SetResolver, which is handed SDK service IDs, and the Terraform AWS client
// overrides, which are keyed by Terraform service names.
func TestDynamicEndpointAgreesWithSDKResolver(t *testing.T) {
	const region = "eu-west-1"
	dynamic := &v1beta1.DynamicURLConfig{Protocol: "https", Host: "vpce.example.com"}

	// sdkServiceID is the name the AWS SDK hands to the endpoint resolver,
	// tfService is the name the Terraform AWS provider keys its endpoint
	// overrides by. They differ in case, and for IAM the case used to decide
	// whether the region was templated into the URL.
	for _, svc := range []struct{ sdkServiceID, tfService string }{
		{"EC2", "ec2"},
		{"IAM", "iam"},
		{"S3", "s3"},
		{"RDS", "rds"},
	} {
		t.Run(svc.tfService, func(t *testing.T) {
			pc := &v1beta1.ClusterProviderConfig{}
			pc.Spec.Endpoint = &v1beta1.EndpointConfig{
				URL:      v1beta1.URLConfig{Type: URLConfigTypeDynamic, Dynamic: dynamic},
				Services: []string{svc.tfService},
			}

			cfg, err := SetResolver(pc, &aws.Config{})
			if err != nil {
				t.Fatalf("SetResolver(...): unexpected error: %v", err)
			}
			resolved, err := cfg.EndpointResolverWithOptions.ResolveEndpoint(svc.sdkServiceID, region) //nolint:staticcheck // the resolver under test is the deprecated one
			if err != nil {
				t.Fatalf("ResolveEndpoint(%q, %q): unexpected error: %v", svc.sdkServiceID, region, err)
			}

			overrides, err := tfEndpointOverrides(pc.Spec.Endpoint, region)
			if err != nil {
				t.Fatalf("tfEndpointOverrides(...): unexpected error: %v", err)
			}
			got, ok := overrides[svc.tfService]
			if !ok {
				t.Fatalf("tfEndpointOverrides(...): no override for service %q, got %v", svc.tfService, overrides)
			}
			if got != resolved.URL {
				t.Errorf("endpoint for %q: SDK resolver says %q, Terraform client says %q", svc.tfService, resolved.URL, got)
			}
		})
	}
}
