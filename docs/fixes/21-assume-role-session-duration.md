# Assume-role sessions expire mid-operation on async paths

**Read this first: what shipped is a window-narrowing mitigation, not the fix
[03-async-credential-expiry.md](03-async-credential-expiry.md) option 1
describes.** The gap is narrowed from ~45 minutes to ~5, not closed.

## The mechanism, verified line by line

Every operation in this provider is asynchronous. upjet detaches `Create`,
`Update` and `Delete` into a goroutine with `context.WithDeadline(context.Background(), …)`
and `defaultAsyncTimeout = 1 * time.Hour`
(`upjet/pkg/controller/external_async_tfpluginsdk.go:28`, and the detach at
`:145`, `:191`, `:241`). The Terraform client it hands that goroutine is built
from a **static credential snapshot** — `configureNoForkAWSClient` copies the
access key, secret and session token into `xpprovider.AWSConfig` as plain strings
(`internal/clients/aws.go:346-357`) — and has no way to refresh them.

With `spec.assumeRoleChain`, that snapshot is an STS session lasting **15
minutes**, because `GetRoleChainConfig` never asked for longer and that is
`stscreds.DefaultDuration` (`credentials@v1.19.29/stscreds/assume_role_provider.go:146`,
applied at `:282`). A create that outlives it — RDS instance, EKS cluster,
OpenSearch domain, CloudFront distribution — starts failing with `ExpiredToken`
partway through, **after the external object already exists**.

**Scope matters:** `WebIdentityRoleProvider` only sends `DurationSeconds` when
non-zero (`stscreds/web_identity_provider.go:128`), so IRSA, WebIdentity and
Upbound base sessions already get the STS default of one hour. Only the
`assumeRoleChain` hops are 15 minutes. This affects `assumeRoleChain` users, not
everyone.

## Two corrections to the original analysis doc

**1. "`SetAssumeRoleOptions` already plumbs `Duration`" is false.** It sets
`ExternalID`, `Tags` and `TransitiveTagKeys`, and nothing else.
`v1beta1.AssumeRoleOptions` (`apis/namespaced/v1beta1/types.go:53`) has **no
duration field at all** — independently confirmed. Making the duration
user-configurable needs a new API field plus CRD regeneration. What shipped is a
provider-wide default in code, which needs no API change.

**2. There is a tempting escape hatch, and it is a trap.**
`xpprovider.AWSConfig` is `conns.Config`, which *does* carry
`AssumeRole []awsbase.AssumeRole`, and `awsbase` builds a properly refreshing
`aws.NewCredentialsCache` chain from it
(`aws-sdk-go-base/v2/credentials.go:155-225`). So the chain *could* be delegated
to the Terraform client without touching the fork.

Do not do this. `awsbase` eagerly calls `Retrieve()` once per role at
client-construction time, and the Terraform client is constructed on **every
`Connect`**. That reintroduces exactly the per-reconcile `sts:AssumeRole` storm
that [fix 08](08-credentials-cache-all-sources.md) exists to remove — a direct
semantic conflict.

The genuinely correct fix — injecting a *shared, cached, refreshing*
`aws.CredentialsProvider` — still requires a setter that only the fork can add:
`AWSClient.awsConfig` is unexported and the only mutators exposed are
`AppendAPIOptions`, `SetAccountID` and the service-packages pair.

## What shipped

`GetRoleChainConfig` builds each hop through a new `NewAssumeRoleProvider` asking
for a **one-hour** session. One hour is both the AWS hard cap for role chaining
and upjet's `defaultAsyncTimeout`, so it covers as much of the window as the API
permits.

A role's `MaxSessionDuration` can be below an hour, and STS rejects the call
outright when the requested duration exceeds it — so asking unconditionally would
break those ProviderConfigs. The returned provider wraps two
`stscreds.AssumeRoleProvider`s: it retries once against a default-duration
provider on a `ValidationError` mentioning `DurationSeconds`/`MaxSessionDuration`,
latches that in an `atomic.Bool`, and passes every other error straight through.
Two providers are needed because `stscreds.AssumeRoleProvider` mutates its own
options on first `Retrieve`. The wrapper forwards `ProviderSources()` so
`aws.CredentialsCache` keeps credential-source attribution.

## The regression risk, stated plainly

**The fallback matches on error text.** If AWS words the message differently or
returns a different code, a role with `MaxSessionDuration < 1h` starts failing
where it previously worked. This is the main risk in the change and the one thing
most worth validating against a live account: set a role's `MaxSessionDuration`
to 900 and confirm the ProviderConfig still works.

## Composition with fix 08

**No textual conflict** — 08 touches `aws.go`, `creds_cache.go` and their tests;
this touches only `provider_config.go`. Disjoint.

**Semantically complementary.** 08 caches the resolved `*aws.Config` per
ProviderConfig so the `aws.CredentialsCache` inside it survives across reconciles;
longer sessions mean strictly fewer refreshes. Combined worst case: with 08's
5-minute TTL a `Connect` can hand the goroutine credentials up to 5 minutes old,
so today that is a **10-minute session against a 60-minute deadline**; after this,
**~55 against 60**.

**Tidy-up for whoever merges both:** fix 08's comment on `awsConfigCacheTTL`
justifies the 5-minute TTL as "deliberately below the 15 minute minimum STS
session duration". Once this lands the minimum is an hour (unless the fallback
fires). The TTL choice is still right; the justification needs rewording.

## Unverified without an AWS account

* That real STS accepts `DurationSeconds=3600` on every hop. AWS documents role
  chaining as capped at one hour and rejects values *greater* than that, so 3600
  exactly should be accepted — not exercised live.
* That the fallback triggers on the real error (see above).
* **The bug itself was never reproduced**, before or after. Doing so needs a
  ProviderConfig with an `assumeRoleChain` and a create that runs past 15 minutes.
* The gap is narrowed, not closed: the session starts when credentials are
  retrieved, not when the operation does, so an operation that genuinely runs the
  full hour can still hit `ExpiredToken` near the end.

## Tests

Fake `stscreds.AssumeRoleAPIClient` recording `AssumeRoleInput`s — no network, no
httptest. Covers the hour-long request, the `MaxSessionDuration` fallback, that
the fallback latches, that unrelated errors are not retried, that
`SetAssumeRoleOptions` still applies, and that `ProviderSources()` is forwarded.

Mutation-verified five ways: dropping the duration (3 subtests fail, `-3600/+900`),
neutering the fallback (2 fail), falling back on *any* error (the unrelated-error
case fails), dropping `SetAssumeRoleOptions` (ExternalId/Tags/TransitiveTagKeys
fail), and deleting `ProviderSources()` (interface assertion fails).

`golangci-lint` could not run: it is built with go1.25 against a repo targeting
1.26.7. Pre-existing environment problem, unrelated.

**Branch** `fix/refreshing-credentials-for-async-ops` @ `8f2462c`.
