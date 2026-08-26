<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# The create and steady-state observe paths

What a family provider actually does to establish one external object, and to
observe an object nobody has touched, with the costs and defects found by
walking those two paths on `main`.

Measured with `hack/memprofile/reconcile` against this tree.

A companion document, [`reconcile-workflow-detail.md`](reconcile-workflow-detail.md),
completes the finding-1 inventory, covers the async/update/delete/framework
paths and the conversions, and adds a larger finding about schema objects
shared between resources.

## The workflow

Both paths funnel through the same two calls. `managed.Reconciler` calls
`Connect` and then `Observe`; only after `Observe` reports
`ResourceExists: false` does it call `Create`.

**`Connect`** (`upjet/pkg/controller/external_tfpluginsdk.go`) runs on *every*
reconcile, including a no-op observe:

1. `SelectTerraformSetup` (`internal/clients/aws.go`) — resolve the
   ProviderConfig, build an `aws.Config`, retrieve credentials, resolve the
   account ID, then **construct a fresh Terraform AWS client and a fresh
   Terraform Plugin Framework provider**.
2. `getExtendedParameters` — merge `spec.forProvider` with `spec.initProvider`,
   apply TF conversions, read referenced Secrets, compute the Terraform ID.
3. Convert params to a `cty.Value` against `CoreConfigSchema()`.
4. If the operation tracker has no cached state, rebuild an `InstanceState`
   from `status.atProvider`.

**`Observe`** then calls `RefreshWithoutUpgrade` (the AWS read), converts the
returned state to a JSON map, computes an `InstanceDiff` against the desired
params, late-initialises, writes `status.atProvider`, and reports
`ResourceUpToDate: !hasDiff`.

**`Create`** applies the diff, stores the new state, derives the external-name
from the returned state, and returns connection details.

## Findings

### 1. Correctness: diff-time and apply-time schemas disagree for 30 resources

`upjet/pkg/config/provider.go:443` materialises the lazy Terraform schema:

```go
terraformResource.Schema = terraformResource.SchemaFunc()
```

It sets `Schema` but leaves `SchemaFunc` non-nil. The Terraform SDK considers
that combination invalid — `helper/schema/resource.go:1313`:

```go
if r.SchemaFunc != nil && r.Schema != nil {
    return fmt.Errorf("SchemaFunc and Schema should not both be set")
}
```

and `SchemaMap()` resolves the conflict by preferring `SchemaFunc`:

```go
func (r *Resource) SchemaMap() map[string]*Schema {
    if r.SchemaFunc != nil {
        return r.SchemaFunc()
    }
    return r.Schema
}
```

**955 of 1,029 configured resources are in this state.** The consequence is a
split: this provider's own schema edits in `config/` are written to `.Schema`,
which only the diff path reads —

* `getResourceDataDiff` → `schema.InternalMap(cfg.TerraformResource.Schema).Diff(...)`
* `processParamsWithHCLParser(c.config.TerraformResource.Schema, params)`

— while everything reached through `SchemaMap()` sees the original upstream
schema instead:

* `RefreshWithoutUpgrade` — the read
* `Apply` — create and update
* `CoreConfigSchema()` — params→cty, `ShimInstanceStateFromValue`, `ApplyToValue`

Most edits survive anyway, because they mutate a `*schema.Schema` that the
upstream `SchemaFunc` hands out by pointer. 30 resources do not:

| divergence | count | example |
| --- | ---: | --- |
| attribute flags differ | 40 attrs | `aws_rds_cluster.iam_roles` is `{optional:false, computed:true}` at diff time and `{optional:true, computed:true}` at apply time |
| attribute deleted but still live | 5 attrs | `delete(r.TerraformResource.Schema, "triggers")` for `aws_apigatewayv2_deployment` does not remove it from the apply schema |
| attribute present only at diff time | 4 attrs | the synthetic `auto_generate_password` added by `config/*/rds/config.go` and `config/*/docdb/config.go`, and `auto_generate_auth_token` in `config/*/elasticache/config.go` |

The last row is the sharpest: a diff can be computed for an attribute the
schema used to apply that diff does not define. The flag divergences undo the
`Computed: true, Optional: false` idiom the provider uses to mark a field
status-only — honoured when deciding whether a change is needed, ignored when
the change is applied.

I have not reproduced end-to-end damage against a live account; what is
demonstrated here is the invariant violation and the exact set of attributes
whose two schemas disagree.

### 2. Waste: the schema is rebuilt four times per reconcile

Because `SchemaFunc` is still set, every `SchemaMap()` call reallocates the
entire schema map, and every `CoreConfigSchema()` call rebuilds the whole
config block on top of it. A `Connect`+`Observe` pair calls `SchemaFunc` four
times *before* the AWS read, which calls `SchemaMap()` several more times
internally.

| resource | `SchemaMap()` | `CoreConfigSchema()` | 4 calls, read excluded |
| --- | ---: | ---: | ---: |
| `aws_instance` | 25 µs / 55 KB | 52 µs / 80 KB | 442 µs |
| `aws_s3_bucket` | 19 µs / 46 KB | 42 µs / 67 KB | 275 µs |
| `aws_lb` | 10 µs / 23 KB | 21 µs / 34 KB | 204 µs |
| `aws_iam_role` | 4 µs / 7 KB | 8 µs / 11 KB | 90 µs |

Clearing `SchemaFunc` after materialisation makes `SchemaMap()` free — 0 s and
0 allocations, it just returns the map — and drops `CoreConfigSchema()` from
80 KB to 24 KB for `aws_instance`. One assignment fixes both this and finding 1,
and it can be done in this repo without waiting on upjet, by walking
`pc.Resources` after `config.GetProvider` returns.

### 3. Waste: the AWS client and a framework provider are rebuilt per reconcile

`configureNoForkAWSClient` (`internal/clients/aws.go`) runs on every `Connect`:

| call | cost per reconcile |
| --- | ---: |
| `AWSConfig.GetClient` | 2.6 ms, 1,768 KB |
| `GetFrameworkProviderWithMeta` | 398 µs, 293 KB |
| **combined** | **3.0 ms, 2,061 KB** |

`GetFrameworkProviderWithMeta` iterates all **267 service packages** on the
singleton provider's meta and builds a closure per resource, data source,
ephemeral resource and action — and for the 960 SDKv2-backed resources the
result is never read. Nothing here depends on the managed resource: the inputs
are the ProviderConfig, the region and the credentials. Caching on that key
would remove essentially all of it.

This is also a large share of the anonymous arena described in
[`memory-footprint.md`](memory-footprint.md): 2 MB of garbage per reconcile,
across every managed resource, at the poll interval.

### 4. Useless API calls: an STS call per reconcile for every non-IRSA source

`GetAWSConfigWithoutTracking` ends with `GetRoleChainConfig`, which builds a
**new** assume-role provider and a **new** `aws.NewCredentialsCache` on every
call:

```go
stsAssume := stscreds.NewAssumeRoleProvider(...)
cfgWithAssumeRole, err := config.LoadDefaultConfig(ctx, ...,
    config.WithCredentialsProvider(aws.NewCredentialsCache(stsAssume)))
```

A fresh `aws.CredentialsCache` is empty, so the `Retrieve()` in
`newCredentials` is a real `sts:AssumeRole`. The provider-level cache that
would prevent this is consulted only for one credential source
(`internal/clients/creds_cache.go:162`):

```go
if pc.Spec.Credentials.Source != authKeyIRSA || !ok {
    return newCredentials(ctx, credsProvider, nil)
}
```

So for `Secret`, `WebIdentity`, `PodIdentity` and `Upbound` sources, every
reconcile of every managed resource performs one STS call per role in the
chain. `WebIdentity` additionally rebuilds its web-identity provider each time,
costing an `sts:AssumeRoleWithWebIdentity` even with no chain configured.

At the default 10-minute poll with 1,000 managed resources that is roughly 1.7
STS calls per second sustained, before any change-triggered reconcile — against
an account-wide throttle, and defeating the AWS SDK's own expiry-based caching.
The code comments (`only IRSA auth credentials are currently cached`, `TODO:
Replace the identity cache with the credential cache`) show this is known to be
partial.

### 5. Leak risk: a partially failed create does not persist the external-name

When the Terraform create returns an error after the external object was
already created, `Create` stores the returned state — but only in the
in-memory tracker:

```go
if !n.opTracker.HasState() {
    n.opTracker.SetTfState(newState)
}
return managed.ExternalCreation{}, errors.Errorf("failed to create the resource: %v", diag)
```

It returns before reaching `setExternalName`, so the external-name never
reaches the managed resource. Within the same process the next `Observe` reads
the tracker and recovers. Across a provider restart the tracker is gone, and
since `managed.Reconciler` records `external-create-failed` the
`ExternalCreateIncomplete` guard does not fire — so the reconciler calls
`Create` again. For AWS this is the default case, because
`IdentifierAssignedByAWS()` makes every resource `IdentifierFromProvider`, so
the first object is orphaned and a second is created.

The runtime is ready to persist it: on create failure it calls
`UpdateCriticalAnnotations`, which updates the whole object and re-applies all
annotations on conflict. Setting the external-name from `newState` before
returning the error would close the restart window. The upjet comment at that
site shows the trade-off was deliberate, but it assumed the tracker survives.

### 6. Smaller items

* `xpprovider.GetFrameworkProviderWithMeta` discards the error from
  `NewProvider` (`fwProvider, _ := ...`) and uses `context.Background()` rather
  than the caller's context. A construction failure yields a provider that
  fails later and further away.
* `Observe` calls `resource.SetUpToDateCondition` only when no spec update is
  required, so a resource that late-initialises leaves the condition stale for
  a cycle.
* `Observe` stores the refreshed state with `SetTfState(newState)` before
  populating `RawPlan`/`RawConfig` (the `// TODO: missing RawConfig & RawPlan
  here...` at that line). It is safe today only because the pointer is mutated
  afterwards in the `resourceExists` branch.

## Suggested order of work

1. **Clear `SchemaFunc` after materialisation.** Fixes findings 1 and 2, is one
   assignment, and can be done here or upstream in upjet.
2. **Cache the Terraform AWS client and framework provider** on
   (ProviderConfig UID, generation, region, credentials). Removes finding 3
   almost entirely.
3. **Extend the credentials cache to every source**, not just IRSA. Removes
   finding 4.
4. **Set the external-name on the create-error path.** Closes finding 5.
