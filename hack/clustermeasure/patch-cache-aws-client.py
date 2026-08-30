#!/usr/bin/env python3
"""UPJET_CACHE_AWS_CLIENT=1: reuse the configured AWS client across reconciles.

configureNoForkAWSClient builds a fresh *conns.AWSClient on every Connect, and
internal/conns/config.go:100 gives each one its own HTTPClient - so each
reconcile gets a fresh http.Transport with an empty connection pool. Measured:
56 MB per 3.3 minutes of bufio buffers allocated under Transport.dialConn, i.e.
connections dialled and thrown away rather than reused.

This is a MEASUREMENT-ONLY cache: keyed on provider config, region and access
key, with no expiry, so it must not ship as-is (credentials rotate). It exists to
size the win before the real fix - see docs/fixes/09.
"""
p = "internal/clients/aws.go"
s = open(p).read()

anchor = "func configureNoForkAWSClient(ctx context.Context, ps *terraform.Setup, config *SetupConfig, region string, creds aws.Credentials, pc *namespacedv1beta1.ClusterProviderConfig) error { //nolint:gocyclo"
assert anchor in s, "anchor"

cache = '''// awsClientCache reuses a configured AWS client across reconciles. The entry
// is a closure so this needs no import of the framework provider's type.
var (
	awsClientCacheMu sync.Mutex
	awsClientCache   = map[string]func(*terraform.Setup){}
)

'''
s = s.replace(anchor, cache + anchor, 1)

old = '''	tfAwsConnsClient, diags := tfAwsConnsCfg.GetClient(ctx, xpac)
	if diags.HasError() {
		return errors.Errorf("cannot construct TF AWS Client from TF AWS Config, %v", diags)
	}'''
new = '''	cacheKey := ""
	if os.Getenv("UPJET_CACHE_AWS_CLIENT") == "1" {
		cacheKey = pc.GetName() + "|" + region + "|" + creds.AccessKeyID
		awsClientCacheMu.Lock()
		if apply, ok := awsClientCache[cacheKey]; ok {
			awsClientCacheMu.Unlock()
			apply(ps)
			return nil
		}
		awsClientCacheMu.Unlock()
	}

	tfAwsConnsClient, diags := tfAwsConnsCfg.GetClient(ctx, xpac)
	if diags.HasError() {
		return errors.Errorf("cannot construct TF AWS Client from TF AWS Config, %v", diags)
	}'''
assert old in s, "getclient anchor"
s = s.replace(old, new, 1)

old2 = '''	// Register AWS SDK v2 call counter
	tfAwsConnsClient.AppendAPIOptions(withExternalAPICallCounter)

	return nil
}'''
new2 = '''	// Register AWS SDK v2 call counter
	tfAwsConnsClient.AppendAPIOptions(withExternalAPICallCounter)

	if cacheKey != "" {
		awsClientCacheMu.Lock()
		meta, fw := ps.Meta, ps.FrameworkProvider
		awsClientCache[cacheKey] = func(s *terraform.Setup) { s.Meta = meta; s.FrameworkProvider = fw }
		awsClientCacheMu.Unlock()
	}

	return nil
}'''
assert old2 in s, "tail anchor"
s = s.replace(old2, new2, 1)

if '\n\t"sync"\n' not in s:
    s = s.replace('\n\t"context"\n', '\n\t"context"\n\t"os"\n\t"sync"\n', 1)
open(p, "w").write(s)
print("E8 patch applied")
