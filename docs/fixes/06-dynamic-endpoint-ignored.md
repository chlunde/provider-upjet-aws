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

---

# Implementation notes

**Branch** `fix/dynamic-endpoint-for-tf-client` @ `29aa0a4`.

The analysis above is correct on premise and mechanism — verified line by line
during implementation. `grep -rn "URLConfigTypeDynamic\|URL.Dynamic"` over
`internal/` and `apis/` confirms `SetResolver`
(`internal/clients/provider_config.go:188-197`) is the *only* place that handles
`Dynamic`, and it configures the `aws.Config` used for the provider's own STS and
account-ID calls — not the client that performs CRUD.

## The trap in the proposed solution

The analysis says to reuse "the same per-service URL construction `SetResolver`
already implements". That construction contains an **exact, case-sensitive**
`if service == "IAM"` for the global-service case.

`SetResolver` receives AWS SDK service **IDs** (`"IAM"`, `"EC2"`).
`tfAwsConnsCfg.Endpoints` is keyed by Terraform service **names**, which are
lowercase — confirmed in the pinned fork: `names/consts_gen.go:129` is
`IAM = "iam"`, and `internal/conns/config.go:98` looks up `c.Endpoints[names.IAM]`.

So a literal shared helper would have produced
`https://iam.<region>.<host>` for IAM on the Terraform path — a bogus regional
endpoint for a global service. The shipped helper lowercases first and compares
case-insensitively: a no-op for `SetResolver`, correct for the new caller.
Independently mutation-confirmed — reverting to `service == "IAM"` fails both
`DynamicTemplatesRegionalAndGlobalServices` and
`TestDynamicEndpointAgreesWithSDKResolver/iam`.

Note also that `Endpoints` being a fixed map is not incidental: unlike the SDK
resolver, the Terraform client genuinely cannot template lazily, so populating it
from `pc.Spec.Endpoint.Services` is required rather than a convenience.

## Unhandled types now fail loudly

`tfEndpointOverrides` switches on `URL.Type`: `Static` unchanged (except that
`type: Static` with no `static` field is now an error rather than a no-op —
`SetResolver` already errored on that, so the two were inconsistent); `Dynamic`
implemented; `Auto` an explicit documented no-op; **anything else, including `""`,
is an error at setup**.

The `default` branch is unreachable from a real cluster today, because the field
is a CRD enum — which is exactly what makes it cheap insurance. The next value
added to that enum fails loudly at setup instead of quietly routing production
traffic to public AWS for however long it takes someone to notice.

## One fail-open deliberately left alone

An endpoint override listing **no** services is equally fail-open: Terraform gets
an empty map and all CRUD goes to default endpoints. This was **not** made an
error, because `SetResolver` ignores `Services` entirely and someone may be
relying on an endpoint block with no services to redirect only the provider's own
STS calls. Erroring would break live deployments for a defect separable from this
one. It logs a warning naming the ProviderConfig instead. Candidate for a
follow-up.

## Composition

Dry-run merges against both sibling branches touching `internal/clients` are
clean, and were run rather than eyeballed: `fix/cache-credentials-for-all-sources`
(`d3d6142`) rewrites the credential/region plumbing above line ~210 of `aws.go`
and still calls `configureNoForkAWSClient` with the same signature, so the region
templated into Dynamic URLs is the effective region including the global-group
substitution; `fix/refreshing-credentials-for-async-ops` (`8f2462c`) touches
`provider_config.go` only outside `SetResolver`.

## Unverified

* **No e2e.** What is proven offline is that the correct `Endpoints` map now
  reaches `tfAwsConnsCfg`. What is *not* proven is that the pinned
  terraform-provider-aws honours every key for every service at request time —
  `internal/conns/config.go` was read and does consume the map, but that is code
  reading, not observed traffic. A localstack run with `type: Dynamic` would
  settle it.
* `golangci-lint` could not run (built with go1.25 against a repo targeting
  1.26.7), so the gocyclo reasoning against `min-complexity: 10` is by hand.
  `gofmt -l` clean, `go vet` passes.
