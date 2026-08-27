<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 08. One STS call per reconcile for every non-IRSA credential source

| | |
| --- | --- |
| **Category** | useless API calls |
| **Severity** | high — scales with fleet size, hits an account-wide throttle |
| **Size** | medium |
| **Lives in** | this repo — `internal/clients/creds_cache.go`, `provider_config.go` |
| **Evidence** | read |

## What happens

`GetAWSConfigWithoutTracking` ends with `GetRoleChainConfig`, which builds a
**new** assume-role provider wrapped in a **new** `aws.NewCredentialsCache` on
every call:

```go
stsAssume := stscreds.NewAssumeRoleProvider(...)
cfgWithAssumeRole, err := config.LoadDefaultConfig(ctx, ...,
    config.WithCredentialsProvider(aws.NewCredentialsCache(stsAssume)))
```

A fresh `CredentialsCache` is empty, so the `Retrieve()` in `newCredentials` is
a real `sts:AssumeRole`. The provider-level cache that would prevent this is
consulted for exactly one source (`internal/clients/creds_cache.go:162`):

```go
if pc.Spec.Credentials.Source != authKeyIRSA || !ok {
    return newCredentials(ctx, credsProvider, nil)
}
```

Per reconcile, per managed resource:

| source | STS calls per reconcile |
| --- | --- |
| `IRSA` (with or without chain) | 0 — cached |
| `Secret` + chain of N | N × `AssumeRole` + 1 × `GetCallerIdentity` |
| `WebIdentity` / `Upbound` | N + 2, plus a Kubernetes Secret GET for `tokenConfig.source: Secret` |
| `PodIdentity` | uncached token exchange |

The identity cache keys on the freshly minted session credentials, so it
**misses every reconcile** for precisely the rotating sources, and churns its
100-entry LRU while doing so.

At the default 10-minute poll with 1,000 managed resources and a chain of one,
that is roughly 1.7 STS calls per second sustained, before any change-driven
reconcile.

## The fix

Extend the provider-level cache to every source. The cache key already captures
what matters — ProviderConfig UID and generation, region, source, and for IRSA
the token-file hash; add the chain identity for other sources.

Cache the **`aws.CredentialsProvider`**, not the retrieved credentials, so the
AWS SDK's own expiry-based refresh does the work. That also produces the
refreshing provider [03](03-async-credential-expiry.md) needs, which is why the
two should be planned together.

Key the identity cache on the ProviderConfig rather than on credential values,
so rotation does not cause a miss.

## How to test

* **Unit:** a fake `CredentialsProvider` counting `Retrieve` calls; two
  reconciles with the same ProviderConfig produce one call, for each of
  `Secret`, `WebIdentity`, `PodIdentity` and `Upbound`. Fails today.
* **Unit:** changing the ProviderConfig generation invalidates the entry.
* **Unit:** the identity cache hits across a credential rotation.
* **e2e:** a ProviderConfig with an assume-role chain plus ~20 managed
  resources; count `AssumeRole` events in CloudTrail across two poll intervals.

## Suggested issue

Repo: `crossplane-contrib/provider-upjet-aws`

**Title:** `Every reconcile performs an sts:AssumeRole for all credential sources except IRSA`

**Body:**

> `GetRoleChainConfig` (`internal/clients/provider_config.go:294`) constructs a
> new `stscreds.AssumeRoleProvider` and a new `aws.NewCredentialsCache` on
> every call, and it is called from `SelectTerraformSetup` on every `Connect`
> — that is, every reconcile of every managed resource. A fresh
> `CredentialsCache` is empty, so the subsequent `Retrieve()` is a real STS
> call.
>
> `AWSCredentialsProviderCache.RetrieveCredentials` gates its cache on
> `pc.Spec.Credentials.Source == authKeyIRSA` (`creds_cache.go:162`), so
> `Secret`, `WebIdentity`, `PodIdentity` and `Upbound` all fall through. The
> caller-identity cache keys on the minted session credentials, so it also
> misses every reconcile for those sources.
>
> At a 10-minute poll with 1,000 managed resources and a one-role chain this is
> ~1.7 STS calls/second sustained against an account-wide throttle, and it
> defeats the AWS SDK's own expiry-based caching.
>
> Suggested fix: cache the `aws.CredentialsProvider` per ProviderConfig for all
> sources and let the SDK handle refresh; key the identity cache on the
> ProviderConfig rather than on credential values.

## Branch

`fix/cache-credentials-for-all-sources`
