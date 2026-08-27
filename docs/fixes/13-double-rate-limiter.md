<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 13. `--max-reconcile-rate` delivers double what it advertises

| | |
| --- | --- |
| **Category** | correctness — a documented safety limit is silently doubled |
| **Severity** | medium |
| **Size** | one line |
| **Lives in** | this repo — `config/templates` → `cmd/provider/*/zz_main.go` |
| **Evidence** | read |

## What happens

The flag is documented as *"The **global** maximum rate per second at which
resources may be checked for drift"*. But `zz_main.go` constructs **two**
independent global rate limiters from it — one per scope:

```
cmd/provider/ec2/zz_main.go:234:  GlobalRateLimiter: ratelimiter.NewGlobal(*maxReconcileRate),   // cluster
cmd/provider/ec2/zz_main.go:257:  GlobalRateLimiter: ratelimiter.NewGlobal(*maxReconcileRate),   // namespaced
```

Each limiter admits `maxReconcileRate` per second, so the process can sustain
`2 × maxReconcileRate`. Anyone who set this to protect against AWS throttling
or API-server load is getting twice the rate they asked for.

Related: `--sync` (default 1 h) is effectively dead — those resync events are
swallowed by the `DesiredStateChanged` event filter, so the flag does not do
what its help text implies either. Worth fixing or documenting in the same
change.

## The fix

Construct one limiter and share it across both scopes:

```go
globalRateLimiter := ratelimiter.NewGlobal(*maxReconcileRate)
// ... clusterOptions.GlobalRateLimiter = globalRateLimiter
// ... namespacedOptions.GlobalRateLimiter = globalRateLimiter
```

Change `config/templates`, not the 178 generated files.

Decide deliberately whether to keep the effective rate and halve the default, or
change the effective rate. Sharing the limiter **halves the throughput of
existing deployments** that were tuned against the current behaviour, so it is
a behaviour change that belongs in release notes.

## How to test

* **Unit:** assert both `tjcontroller.Options` reference the same
  `GlobalRateLimiter` instance.
* **Template test:** the generated `zz_main.go` for a sample of families
  contains exactly one `ratelimiter.NewGlobal` call.
* **e2e:** with `--max-reconcile-rate=1` and MRs in both scopes, observe the
  aggregate reconcile rate does not exceed 1/s.

## Suggested issue

Repo: `crossplane-contrib/provider-upjet-aws`

**Title:** `--max-reconcile-rate creates one global rate limiter per scope, doubling the effective limit`

**Body:**

> `cmd/provider/<family>/zz_main.go` calls
> `ratelimiter.NewGlobal(*maxReconcileRate)` twice — once for the
> cluster-scoped controller options and once for the namespaced ones (lines 234
> and 257 in the ec2 family). Each limiter independently admits
> `maxReconcileRate` per second, so the process sustains twice the configured
> rate.
>
> The flag's help text describes it as the *global* maximum rate, and operators
> set it to bound AWS API and Kubernetes API load, so the doubling defeats its
> purpose.
>
> Suggested fix: construct a single limiter in the template and share it across
> both scopes. Note this halves effective throughput for deployments tuned
> against current behaviour, so it needs a release note.
>
> Separately, `--sync` resync events appear to be swallowed by the
> `DesiredStateChanged` event filter, so that flag may also not behave as
> documented.

## Branch

`fix/single-global-rate-limiter`
