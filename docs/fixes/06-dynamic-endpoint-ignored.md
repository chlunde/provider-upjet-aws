<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 06. `endpoint.url.type: Dynamic` never reaches the client that does CRUD

| | |
| --- | --- |
| **Category** | correctness / security — traffic silently goes to the public endpoint |
| **Severity** | high |
| **Size** | small |
| **Lives in** | this repo — `internal/clients/aws.go` |
| **Evidence** | read |

## What happens

`configureNoForkAWSClient` builds the Terraform AWS client — the one that
performs every create, read, update and delete — and reads **only**
`pc.Spec.Endpoint.URL.Static`:

```go
if pc.Spec.Endpoint != nil {
    if pc.Spec.Endpoint.URL.Static != nil {
        ...
    }
}
```

There is no `Dynamic` branch. The `Dynamic` handling lives in `SetResolver`
(`internal/clients/provider_config.go:178-186`), which configures the
`aws.Config` used for the provider's own STS and account-ID calls — not the
client that does the work.

So a ProviderConfig with `endpoint.url.type: Dynamic` silently sends all
resource traffic to the public AWS endpoints. Users set this to reach a private
VPC endpoint, an isolated partition, or a proxy that enforces policy; failing
open to the public endpoint is the wrong direction to fail in.

## The fix

Populate `tfAwsConnsCfg.Endpoints` for the `Dynamic` case with the same
per-service URL construction `SetResolver` already implements — factor that
templating into one helper used by both, so the two cannot drift again:

```go
fullURL = fmt.Sprintf("%s://%s.%s", proto, strings.ToLower(service), host)          // global
fullURL = fmt.Sprintf("%s://%s.%s.%s", proto, strings.ToLower(service), region, host) // regional
```

`Endpoints` is keyed by service name, so the set of services to populate has to
come from `pc.Spec.Endpoint.Services` exactly as the `Static` branch does.

Consider making an unsupported endpoint configuration an **error** rather than
a silent no-op — that is the difference between this being a one-time bug and a
recurring one.

## How to test

* **Unit:** a ProviderConfig with `Dynamic` produces a non-empty
  `tfAwsConnsCfg.Endpoints` covering every service in `Endpoint.Services`, with
  the regional and global forms both exercised. Fails today.
* **Unit:** the `Static` and `Dynamic` paths agree on service-name keying.
* **e2e:** localstack, or any endpoint override, with `type: Dynamic` — assert
  the resource is created against the override and not against AWS. There is an
  existing localstack-style setup in `cluster/test` to build on.

## Suggested issue

Repo: `crossplane-contrib/provider-upjet-aws`

**Title:** `endpoint.url.type: Dynamic is ignored for all resource CRUD`

**Body:**

> `configureNoForkAWSClient` in `internal/clients/aws.go` populates
> `xpprovider.AWSConfig.Endpoints` only from `pc.Spec.Endpoint.URL.Static`.
> There is no branch for `URL.Dynamic`.
>
> The `Dynamic` templating exists, but only in `SetResolver`
> (`internal/clients/provider_config.go:178-186`), which configures the
> `aws.Config` used for the provider's own STS/account-ID calls. The Terraform
> AWS client that performs all resource CRUD never sees it.
>
> A ProviderConfig using `endpoint.url.type: Dynamic` therefore silently sends
> all resource traffic to the public AWS endpoints. For users who set this to
> reach a private VPC endpoint or a policy-enforcing proxy, that is a
> fail-open.
>
> Suggested fix: share one endpoint-templating helper between `SetResolver` and
> `configureNoForkAWSClient`, and reject endpoint configurations that cannot be
> applied rather than ignoring them.

## Branch

`fix/dynamic-endpoint-for-tf-client`
