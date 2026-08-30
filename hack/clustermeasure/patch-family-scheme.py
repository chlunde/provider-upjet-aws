#!/usr/bin/env python3
"""UPJET_SCHEME_FAMILY=1: register only the s3 family's reference closure.

The provider registers all 8,488 GVKs of every API group in both scopes. The s3
family's controllers and resolvers only ever instantiate six groups - iam, kms,
s3, s3control, sns, sqs - plus the ProviderConfig types. This does not reduce
what is linked (internal/controller still imports every family), so it isolates
the registration cost from the link cost.
"""
p = "cmd/provider/s3/zz_main.go"
s = open(p).read()

imports = '''	clusters3v1beta1 "github.com/upbound/provider-aws/v2/apis/cluster/s3/v1beta1"
	clusters3v1beta2 "github.com/upbound/provider-aws/v2/apis/cluster/s3/v1beta2"
	clusteriamv1beta1 "github.com/upbound/provider-aws/v2/apis/cluster/iam/v1beta1"
	clusterkmsv1beta1 "github.com/upbound/provider-aws/v2/apis/cluster/kms/v1beta1"
	clusters3controlv1beta1 "github.com/upbound/provider-aws/v2/apis/cluster/s3control/v1beta1"
	clustersnsv1beta1 "github.com/upbound/provider-aws/v2/apis/cluster/sns/v1beta1"
	clustersqsv1beta1 "github.com/upbound/provider-aws/v2/apis/cluster/sqs/v1beta1"
	clusterpcv1beta1 "github.com/upbound/provider-aws/v2/apis/cluster/v1beta1"
	nss3v1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/s3/v1beta1"
	nspcv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
'''
s = s.replace('\tclusterapis "github.com/upbound/provider-aws/v2/apis/cluster"\n',
              '\tclusterapis "github.com/upbound/provider-aws/v2/apis/cluster"\n' + imports, 1)

old = '''	kingpin.FatalIfError(clusterapis.AddToScheme(scheme), "Cannot add cluster scoped AWS APIs to scheme")
	kingpin.FatalIfError(namespacedapis.AddToScheme(scheme), "Cannot add namespaced AWS APIs to scheme")'''
new = '''	if os.Getenv("UPJET_SCHEME_FAMILY") == "1" {
		// Only the groups this family's controllers and resolvers instantiate.
		for _, add := range []func(*runtime.Scheme) error{
			clusters3v1beta1.AddToScheme, clusters3v1beta2.AddToScheme,
			clusteriamv1beta1.AddToScheme, clusterkmsv1beta1.AddToScheme,
			clusters3controlv1beta1.AddToScheme, clustersnsv1beta1.AddToScheme,
			clustersqsv1beta1.AddToScheme, clusterpcv1beta1.AddToScheme,
			nss3v1beta1.AddToScheme, nspcv1beta1.AddToScheme,
		} {
			kingpin.FatalIfError(add(scheme), "Cannot add family API group to scheme")
		}
	} else {
		kingpin.FatalIfError(clusterapis.AddToScheme(scheme), "Cannot add cluster scoped AWS APIs to scheme")
		kingpin.FatalIfError(namespacedapis.AddToScheme(scheme), "Cannot add namespaced AWS APIs to scheme")
	}'''
assert old in s, "anchor not found"
s = s.replace(old, new, 1)
open(p, "w").write(s)
print("E7 patch applied")
