<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 09. The AWS client and a Framework provider are rebuilt on every Connect

| | |
| --- | --- |
| **Category** | waste |
| **Severity** | high — the dominant per-reconcile allocation |
| **Size** | medium |
| **Lives in** | this repo — `internal/clients/aws.go` |
| **Evidence** | measured |

## What happens

`configureNoForkAWSClient` runs on every `Connect`, i.e. every reconcile of
every managed resource:

| call | cost per reconcile |
| --- | ---: |
| `AWSConfig.GetClient` | 2.6 ms, 1,768 KB |
| `GetFrameworkProviderWithMeta` | 435 µs, 292 KB |
| **combined** | **3.07 ms, 2,060 KB** |

`GetFrameworkProviderWithMeta` walks all **267 service packages** on the
singleton provider's meta and builds a closure per resource, data source,
ephemeral resource and action. For the 960 SDKv2-backed resources the result is
never read.

Nothing here depends on the managed resource. The inputs are the
ProviderConfig, the region and the credentials.

At 1,000 MRs on a 10-minute poll this is ~3.4 MB/s of garbage, and — with the
diff pipeline — what keeps the arena described in
[memory-footprint.md](../memory-footprint.md) alive at ~386 MB.

## The fix

Cache the constructed client on (ProviderConfig UID, ProviderConfig generation,
region, credential identity), with the same LRU discipline as the existing
credentials cache.

Two constraints:

* **Credential expiry.** The cached client holds a credential snapshot — see
  [03](03-async-credential-expiry.md). Caching it for longer makes that bug
  worse unless the client is given a refreshing provider first. **Do 08 and 03
  before, or with, this.**
* **Only build the Framework provider for Framework resources.** 960 of 1,029
  resources never touch it. Gate on
  `config.Resource.useTerraformPluginFrameworkClient` rather than building it
  unconditionally.

## How to test

* **Bench:** `hack/memprofile/reconcile` section 5 already measures both calls;
  assert the cached path allocates ~0 on the second Connect.
* **Unit:** a changed ProviderConfig generation, region, or credential identity
  produces a new client; an unchanged one does not.
* **Unit:** an SDKv2-only resource never constructs a Framework provider.
* **e2e:** steady-state RSS over ~30 minutes with a few hundred MRs, before and
  after. This is the measurement that decides whether the arena actually
  shrinks.

## Suggested issue

Repo: `crossplane-contrib/provider-upjet-aws`

**Title:** `The Terraform AWS client and Framework provider are rebuilt on every reconcile`

**Body:**

> `configureNoForkAWSClient` runs on every `Connect`. Measured locally against
> `main`: `AWSConfig.GetClient` costs 2.6 ms and 1,768 KB, and
> `GetFrameworkProviderWithMeta` a further 435 µs and 292 KB — 3.07 ms and
> 2,060 KB per reconcile of every managed resource.
>
> `GetFrameworkProviderWithMeta` iterates all 267 service packages and builds a
> closure per resource/data source/action. 960 of the 1,029 configured
> resources are SDKv2-backed and never read it.
>
> None of this work depends on the managed resource — the inputs are the
> ProviderConfig, region and credentials — so it can be cached on that key.
>
> Note the interaction with credential expiry: the client holds a static
> credential snapshot, so it should be given a refreshing credentials provider
> before its lifetime is extended.

## Branch

`fix/cache-tf-aws-client`
