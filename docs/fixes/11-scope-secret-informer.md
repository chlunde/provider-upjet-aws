<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 11. The Secret informer is cluster-wide and unbounded

| | |
| --- | --- |
| **Category** | security + waste |
| **Severity** | medium-high |
| **Size** | small |
| **Lives in** | this repo — `config/templates` → `cmd/provider/*/zz_main.go` |
| **Evidence** | read |

## What happens

`--enable-secret-cache` defaults to `true`, and the code disables Secret
caching only when the flag is **false**:

```go
var clientOpts client.Options
if !*enableSecretCache {
    clientOpts = client.Options{
        Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}},
    }
}
```

So by default the first credential read starts a controller-runtime informer
over **every Secret in the cluster**, with no label or namespace selector — in
every family provider pod.

Two consequences: the provider holds every Secret in the cluster in memory,
scaling with the cluster rather than with its own workload; and its RBAC and
cache reach far beyond the Secrets it needs, which is a larger blast radius than
necessary for a component holding cloud credentials.

## The fix

Give the cache a selector rather than an all-or-nothing switch — via
`client.CacheOptions.ByObject` with a `ByObject[&corev1.Secret{}]` field or
label selector, so only Secrets the provider is meant to read are cached.

Options, roughly in order of preference:

* Restrict to the namespaces named by ProviderConfigs and by MR
  `SecretKeySelector`s. Most precise, but the set is dynamic.
* Restrict by a well-known label that Crossplane already applies to
  provider-managed connection secrets, and document that credential Secrets need
  it.
* At minimum, restrict to the provider's own namespace plus any explicitly
  configured ones.

Keep `--enable-secret-cache=false` working as the escape hatch.

## How to test

* **Unit:** the manager is constructed with a Secret selector; a Secret outside
  the selector is not served from the cache.
* **e2e:** create several thousand unrelated Secrets and assert provider RSS is
  unchanged. That is the measurement that demonstrates the win.
* **e2e:** confirm credential Secrets are still read, for each credential
  source, including cross-namespace references.

## Suggested issue

Repo: `crossplane-contrib/provider-upjet-aws`

**Title:** `Secret caching starts a cluster-wide, unselected Secret informer by default`

**Body:**

> `--enable-secret-cache` defaults to `true`, and `zz_main.go` only sets
> `DisableFor: []client.Object{&corev1.Secret{}}` when the flag is false. With
> the default, the first credential read starts a controller-runtime informer
> over every Secret in the cluster, with no field or label selector, in every
> family provider pod.
>
> The provider's memory then scales with the number of Secrets in the cluster
> rather than with its own workload, and its cache holds Secrets unrelated to
> its ProviderConfigs.
>
> Suggested fix: use `client.CacheOptions.ByObject` with a selector scoped to
> the Secrets the provider actually reads, keeping
> `--enable-secret-cache=false` as the escape hatch.

## Branch

`fix/scope-secret-informer`
