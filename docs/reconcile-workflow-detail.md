<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# Reconcile-path detail

A companion to [`reconcile-workflow.md`](reconcile-workflow.md). That document
walked the create and steady-state observe paths; this one covers what it
skipped — the complete schema-divergence inventory behind its finding 1, the
async lifecycle used by every controller in this provider, the Terraform
Plugin Framework external, the `status.atProvider` round-trip, and a precise
accounting of the STS traffic behind its finding 4 — plus one new finding that
is larger than anything in the original document.

Measured with `hack/memprofile/reconcile` and `hack/memprofile/schemadump`
against this tree. Each claim is labelled **measured** (harness output),
**read** (traced in source, with citations), or **inferred** (mechanically
follows from the above but not demonstrated end to end).

## 1. New finding: config edits leak into schema objects shared by every resource

This is the sharpest thing found while completing the finding-1 inventory, and
it exists *independently* of the `SchemaFunc` split-brain: it corrupts both
sides of that split equally, so fixing the split does not fix this.

### The mechanism

The upstream provider now interns common attribute schemas as process-wide
singletons — `internal/tags/tags.go:13` in the terraform-provider-aws fork:

```go
var TagsSchema = sync.OnceValue(func() *schema.Schema {
    return &schema.Schema{Type: schema.TypeMap, Optional: true, ...}
})
```

and likewise `TagsSchemaComputed`, `TagsSchemaForceNew`,
`IAMPolicyDocumentSchemaOptionalComputed` (`internal/sdkv2/schema.go:76`),
`RegionOptionalComputed` (`internal/sdkv2/schema.go:168`), and others.
Hundreds of resources put the *same pointer* in their schema maps.

This repository's `config/` edits mutate schemas in place. When such an edit
lands on one of these singletons, it mutates it **for every resource in the
provider**, in both the diff-time and apply-time schema views.

### The demonstrated case: `tags` becomes computed-only, provider-wide

`config/cluster/s3/config.go:42` runs
`config.MoveToStatus(r.TerraformResource, ..., "lifecycle_rule", ..., "replication_configuration", ...)`
on `aws_s3_bucket`. upjet's `MoveToStatus` (`upjet/pkg/config/common.go:109`)
recursively sets `Optional=false, Computed=true` on every nested field of the
moved block. `aws_s3_bucket`'s `lifecycle_rule` and
`replication_configuration.rules.filter` blocks embed the raw singleton
(`internal/service/s3/bucket.go:318` and `:558` — `names.AttrTags:
tftags.TagsSchema()`, no copy). The recursion therefore flips the shared
`TagsSchema` singleton.

**Measured**, by dumping the flags of every attribute of every resource from a
pristine process (`hack/memprofile/schemadump`, no `config/` edits) and from
the fully configured provider (`SCHEMA_DUMP=... hack/memprofile/reconcile`),
then diffing:

| attribute | resources flipped `{opt:true}` → `{opt:false, comp:true}` |
| --- | --- |
| top-level `tags` | **491 of the 495** configured SDKv2 resources that have one |
| nested `.tags` | 14 paths, incl. `aws_instance.{ebs,root}_block_device.tags`, `aws_launch_template.tag_specifications.tags`, `aws_batch_compute_environment.compute_resources.tags`, `aws_ecs_service...tag_specifications.tags`, `aws_backup_framework.control.scope.tags` |
| `volume_tags` | `aws_instance`, `aws_spot_instance_request` |
| `final_backup_tags` | `aws_fsx_lustre_file_system`, `aws_fsx_windows_file_system` |
| `principal_tags` | `aws_cognito_identity_pool_provider_principal_tag` |
| `allocation_resource_tags` | `aws_vpc_ipam_pool` |
| `resource_tags` | `aws_ecr_repository_creation_template`, `aws_imagebuilder_infrastructure_configuration` |

Only the two `aws_s3_bucket` paths were intended.

### What it breaks

**Measured** with the diff experiment in harness section 7, which computes the
exact `InstanceDiff` the Observe path computes for `aws_iam_role`, first with
the schema as the runtime has it, then with the upstream flags restored:

* **Updating tags still diffs** (`tags.env:"a"->"b"` appears both ways) — the
  SDK still compares a config-supplied value even on a computed attribute, so
  this did not break tag updates outright.
* **Removing all tags from the spec produces no diff** with the runtime
  schema; with upstream flags restored, the same input produces
  `tags.%:"1"->"0" tags.env:"a"->""`. So on ~491 resources, deleting entries
  from `spec.forProvider.tags` (and the collateral attributes above) is
  **silently swallowed forever** — the resource stays `Synced/Ready` while
  drifted. Category: correctness / silent drift.
* **Create-time tags are unaffected** (measured; the create diff carries the
  tag values either way).
* **Inferred, codegen landmine**: upjet classifies `Computed && !Optional`
  fields as status-only (`upjet/pkg/types/builder.go:504`). The currently
  generated APIs still have `Tags` under `Parameters`, so generation last ran
  before this dependency state; the next `make generate` under the current
  dependencies would move `tags` to `Observation` for those 491 resources.
  The diff would be enormous and presumably caught — but nothing structural
  prevents it.

### The second demonstrated singleton: IAM policy documents

`config/cluster/apigateway/config.go:16` runs `MoveToStatus(..., "policy")` on
`aws_api_gateway_rest_api`, whose `policy` is the shared
`IAMPolicyDocumentSchemaOptionalComputed()` singleton
(`internal/service/apigateway/rest_api.go:169`). **Measured** collateral:
`policy` flips to `{opt:false}` on `aws_kms_key`, `aws_kms_external_key`,
`aws_kms_replica_key`, `aws_kms_replica_external_key`, `aws_sns_topic`,
`aws_vpc_endpoint`, `aws_s3_access_point`; `access_policies` on
`aws_elasticsearch_domain` and `aws_opensearch_domain`; and `policy_document`
on `aws_codeartifact_domain_permissions_policy` — the *primary argument* of
that last resource. The measured `aws_sns_topic` experiment (harness 7b) shows
in-place policy *changes* still diff; by the same mechanism as tags,
*unsetting* the policy is swallowed (inferred from the tags measurement; not
separately demonstrated for a string attribute).

### The third: `region`, mutated deliberately but breaking an upstream contract

`RegionRequired()` (`config/overrides.go:25`) sets
`Required=true, Optional=false, Computed=false` on the `region` attribute of
861 resources (measured). For most resources that attribute is the shared
`RegionOptionalComputed` singleton, injected once per resource as a
closure-captured value (`internal/provider/sdkv2/provider.go:556-568`) — which
is *why* this edit reaches `SchemaMap()` at all. The effect on all resources
is intended here. But the upstream `defaultRegion()` CustomizeDiff interceptor
(`internal/provider/sdkv2/region.go:63`) calls
`d.SetNew("region", ...)` whenever the diff's config has no region, and the
SDK rejects `SetNew` on non-computed keys. **Measured**: computing a diff for
`aws_sns_topic` without `region` in state/config fails with
`SetNew only operates on computed keys - region is not one`. In the normal
reconcile path `region` is always present in params (it is required in the
CRD), so this is a latent trap, not a live bug — but any path that reaches the
diff with an absent region (imports with hand-built state, future upstream
interceptor changes) errors out.

### Why "most edits survive" — a correction to the original document

`reconcile-workflow.md` explained that most `config/` edits survive into
`SchemaMap()` "because they mutate a `*schema.Schema` that the upstream
`SchemaFunc` hands out by pointer". That is only true attribute-by-attribute,
and the pointer stability comes from the *singletons and closure-captured
values* described above, not from any per-resource guarantee. **Measured**
(harness 2b): calling `SchemaFunc()` twice and comparing attribute pointers,
**0 of 955** resources return an identical pointer set; 58 rebuild every
attribute object per call and 897 return a mix of shared and rebuilt objects.
So an in-place edit survives *iff* it happens to land on a shared object — and
when the shared object is a cross-resource singleton, it "survives" onto 500
resources that were never meant to change. The two failure modes of finding 1
(edit lost) and this finding (edit over-applied) are the two sides of the same
undefined sharing contract.

### Fixes

Clearing `SchemaFunc` (the fix for findings 1/2 of the main document) does
**not** fix this: the singleton mutation is visible in both schema views.
What fixes it: `MoveToStatus` (and every in-place `config/` edit) must
copy-on-write the `*schema.Schema` (and `Elem` chain) before mutating —
a change in upjet's `pkg/config/common.go` — or this repo's configurators must
deep-copy the affected subtrees first. The `tags` case can be verified fixed
by re-running `hack/memprofile/schemadump` against `hack/memprofile/reconcile
SCHEMA_DUMP=...` and checking the collateral rows disappear.

## 2. The complete divergence inventory (finding 1, finished)

The harness now prints every divergent attribute (section 2 of
`hack/memprofile/reconcile`), compares Sensitive/ForceNew/Default/Type too,
and recurses into `Elem`. Result (**measured**): **35 resources** diverge (the
original document said 30 — the extras are Sensitive-flag and nested-Elem
divergences the old comparison didn't check), with 4 attrs present only in the
diff schema, 5 deleted-but-live, and 50 whose flags differ.

### Attributes that exist only in the diff schema (4)

| resource.attribute | call site | consequence |
| --- | --- | --- |
| `aws_db_instance.auto_generate_password` | `config/{cluster,namespaced}/rds/config.go` | The synthetic bool is injected into the CRD and diffed, but `CoreConfigSchema()` (built from `SchemaMap()`) does not contain it, so the param is dropped at the params→cty conversion. The password generation itself is done by an initializer, not by Terraform, so this is **latent, currently harmless** (read). |
| `aws_docdb_cluster.auto_generate_password` | `config/{cluster,namespaced}/docdb/config.go` | same |
| `aws_rds_cluster.auto_generate_password` | `config/{cluster,namespaced}/rds/config.go` | same |
| `aws_elasticache_replication_group.auto_generate_auth_token` | `config/{cluster,namespaced}/elasticache/config.go` | same |

### Attributes deleted from the diff schema but still live at apply time (5)

| resource.attribute | call site | consequence |
| --- | --- | --- |
| `aws_apigatewayv2_deployment.triggers` | `config/cluster/apigatewayv2/config.go:49` | Read path still populates it into state; the typed `SetObservation` drops it. Harmless waste (read). |
| `aws_lambda_function.filename` | `config/cluster/lambda/config.go:64` | as above |
| `aws_organizations_account.role_name` | `config/cluster/organization/config.go:16` | as above |
| `aws_wafv2_rule_group.rule`, `aws_wafv2_web_acl.rule` | `config/cluster/wafv2/config.go:16,27` | The `rule` set is managed as a raw HCL blob via a custom field; the live schema still refreshes the structured `rule` into state. Harmless-but-fragile: the diff never sees it, the state always carries it (read). |

### Flag divergences (50 attrs, 28 resources)

Full rows in the harness output; grouped by cause and consequence:

* **`config.MoveToStatus` lost on rebuilt attribute objects** — the
  status-only idiom (`opt=false, comp=true`) holds at diff time and in the
  CRD, but the apply schema still says `optional`:
  `aws_appstream_image_builder.image_name`,
  `aws_autoscaling_group.{load_balancers,target_group_arns}`,
  `aws_docdb_cluster.cluster_members`, `aws_rds_cluster.iam_roles`,
  `aws_instance.security_groups`, `aws_network_acl.{ingress,egress}`,
  `aws_network_interface.attachment`,
  `aws_route_table.{route,propagating_vgws}` (plus 20 nested attrs of
  `route`), `aws_s3_bucket` (14 top-level blocks plus ~90 nested attrs),
  `aws_secretsmanager_secret.policy`,
  `aws_vpc_endpoint.{route_table_ids,security_group_ids,subnet_ids}`,
  `aws_vpc_endpoint_service.allowed_principals`,
  `aws_kinesis_stream.arn`, `aws_kinesis_firehose_delivery_stream.arn`.
  Consequence: **harmless at runtime today** — the field is absent from
  `spec.forProvider`, so nothing user-supplied reaches the optional apply-side
  attribute; the divergence only re-opens if the CRD is ever regenerated from
  the other view (read).
* **Required-markers lost**: `aws_dx_gateway_association.associated_gateway_id`
  (`config/cluster/directconnect/config.go:30`), `aws_glue_*.catalog_id`
  (`config/cluster/glue/config.go`),
  `aws_servicecatalog_*.accept_language`
  (`config/cluster/servicecatalog/config.go`),
  `aws_lb_target_group.name` (`config/cluster/elbv2/config.go:134`),
  `aws_cloudformation_stack_set_instance.region` (via `RegionRequired()`; this
  resource declares its own `region` in its per-call schema instead of the
  shared singleton, so it is the one resource where that edit fails to reach
  the apply schema).
  Consequence: validation is stricter at CRD/diff level than at apply level —
  **harmless** (read).
* **Sensitive-markers lost at apply time**:
  `aws_acmpca_certificate.certificate_signing_request`,
  `aws_acmpca_certificate_authority_certificate.{certificate,certificate_chain}`
  (`config/cluster/acmpca/config.go:35,54-55`),
  `aws_cloudfront_function.code`, `aws_cloudfront_public_key.encoded_key`
  (`config/cluster/cloudfront/config.go:29,34`),
  `aws_kinesis_firehose_delivery_stream.splunk_configuration.hec_token`
  (`config/cluster/firehose/config.go:17`). The markers still work where
  upjet reads them (diff-time redaction in `assertNoForceNew`, secret-based
  CRD generation); what is lost is the TF SDK's own redaction on the
  read/apply side, e.g. in SDK-internal trace logging of state. **Low-severity
  leak surface** (read; not demonstrated in logs).
* **Contamination artifacts** (the other side of section 1):
  `aws_instance.{ebs,root}_block_device.tags` and the same on
  `aws_spot_instance_request` diverge because `tagsSchemaConflictsWith`
  (`internal/service/ec2/tags.go:102`) copies the `TagsSchema` singleton *by
  value* on every `SchemaFunc()` call — the diff view holds a pre-contamination
  copy, fresh apply views hold post-contamination copies.

One more latent bug found while reading this code: `MoveToStatus`
(`upjet/pkg/config/common.go:112`) does `if s == nil { return }` — a missing
field path silently aborts the processing of **all subsequent paths in the
same call** rather than skipping the one. None of today's call sites hit it
(read), but any upstream field rename turns one lost edit into many.

## 3. The async lifecycle

All **1,029** controllers in this provider are async: 960
`NewTerraformPluginSDKAsyncConnector` and 69
`NewTerraformPluginFrameworkAsyncConnector` (measured by grep over
`internal/controller/cluster`). Sync `Create`/`Update`/`Delete` never run on
the reconciler's goroutine here — everything below applies to every resource.

### 3.1 Every create's external-name persistence rides on a later Observe

This widens finding 5 of the main document from "partially failed create" to
**every create**, including successful ones. Read, with the chain:

1. Async `Create` (`external_async_tfpluginsdk.go:140`) marks the operation
   started, deep-copies the MR (`:149`), spawns the goroutine, and returns
   `ExternalCreation{}, nil` immediately.
2. The managed reconciler takes that nil error as success:
   `SetExternalCreateSucceeded` + `UpdateCriticalAnnotations`
   (`crossplane-runtime/pkg/reconciler/managed/reconciler.go:1437-1439`) —
   **before the AWS call has even started**, and with no external-name to
   persist, because the sync `Create` will set it on `mgCopy`
   (`external_tfpluginsdk.go:698`), which is discarded.
3. The only durable write of the external-name happens on the *next* Observe:
   `setExternalName` flips `specUpdateRequired`, returned as
   `ResourceLateInitialized`, and the reconciler then does a full
   `client.Update` (`reconciler.go:1479`).

Consequences:

* **Restart window (inferred)**: from the moment AWS creates the object in
  the goroutine until the post-create Observe's spec update commits, the
  external-name exists only in process memory (the tracker's tfState). A pod
  restart in that window leaves an MR annotated `external-create-succeeded`
  with no external-name; the next Observe reconstructs empty state, reads
  nothing, reports `ResourceExists: false`, and the reconciler creates
  **again** — the first object is orphaned. `IdentifierAssignedByAWS()` makes
  this the default shape for AWS. The `creationGracePeriod` check
  (`reconciler.go:1218`) only delays the duplicate; it persists nothing.
* **The create-annotation safety mechanism is neutralized (read)**: the
  pending→succeeded/failed annotation protocol exists precisely to make
  "provider died during create" detectable (`ExternalCreateIncomplete`). With
  async connectors it records the wrong fact — success at submission time —
  so the guard can never fire for the actual failure it was designed for.
* **Management-policy interaction (read)**: the spec update in step 3 only
  happens when the policy includes late-initialization
  (`observation.ResourceLateInitialized && policy.ShouldLateInitialize()`,
  `reconciler.go:1479`). With `managementPolicies` excluding `LateInitialize`,
  the external-name recovered during Observe is **never persisted** — every
  restart re-orphans. The FW async Observe's own comment
  (`external_async_tfpluginfw.go:157-163`, "This might not work if the late
  initialization management policy is disabled") shows upjet knows.

Category: data loss (orphaned cloud resources + duplicate billing). Not
demonstrated against a live account; every link is read from source and the
window is real.

### 3.2 The SDK async path lacks the FW path's partial-state name recovery

`external_async_tfpluginfw.go:116-171` handles the case where a failed async
create left a partial state in the tracker but the subsequent `ReadResource`
cannot see the resource: it extracts the external-name from the cached partial
state (`recoverExternalName`, `external_tfpluginfw.go:467`) and forces an
annotation update before touching the API again. The SDK async Observe
(`external_async_tfpluginsdk.go:118`) has no equivalent — it relies on the
refresh succeeding with the partial state. If the refresh errors (common for
half-created resources) the external-name stays memory-only across arbitrarily
many reconciles. Read; the asymmetry is plainly visible in the two files.
Category: data loss risk, SDK resources only.

### 3.3 Async delete captures the live MR — the race the deep-copy was added for

Async `Create` and `Update` deep-copy the MR with a comment citing
crossplane/upjet#472 (data race between the goroutine and the managed
reconciler). Async `Delete` — both variants
(`external_async_tfpluginsdk.go:232`, `external_async_tfpluginfw.go:298`) —
passes the live `mg` into the goroutine and reads
`mg.GetNamespace()/GetName()` in the completion callback while the reconciler
concurrently updates status on the same object. Read. Category: correctness
(Go data race; low practical impact since the reads are simple getters, but it
is exactly the pattern #472 fixed elsewhere).

### 3.4 The 1-hour async deadline undercuts Terraform's own timeouts

`defaultAsyncTimeout = 1 * time.Hour` (`external_async_tfpluginsdk.go:28`) caps
every async operation, unconditionally. Upstream per-resource timeouts exceed
it — e.g. `aws_db_instance` declares `Update: 80 * time.Minute`
(`internal/service/rds/instance.go:85-89`). A legitimately slow RDS update is
context-cancelled at 60 minutes: the TF apply aborts mid-wait while AWS
continues; the error path skips `SetTfState`, so the tracker keeps the
pre-update state until the next refresh. Wasted work and a spurious
`ReconcileError`/`LastAsyncOperation` failure on resources that would have
succeeded. Read. Category: correctness/waste.

### 3.5 What Observe reports while an operation runs

While `LastOperation.IsRunning()`, both async Observes return
`ResourceExists: true, ResourceUpToDate: true` — during a create this is
fiction, acknowledged in the `APICallbacks.Create` comment (`api.go`). It
keeps the reconciler quiet for up to the poll interval; the callback's
`RequestReconcile` restores liveness on completion. Errors from the goroutine
reach the MR only as `LastAsyncOperation`/`ReconcileError` conditions written
by the callback (`api.go:callbackFn`), never through an `ExternalClient`
return value; the operation's error is *also* returned by the **next**
`Create`/`Update`/`Delete` call's `LastOperation.Error()`, i.e. one full cycle
late. Read; design context rather than a defect, but it explains why `Synced`
can read true during a failing create.

## 4. Update and Delete on the SDK path

Read; both are thin relative to Observe:

* `Update` (`external_tfpluginsdk.go:751`) runs `UpdateLoopPrevention`, then
  `assertNoForceNew` — any `RequiresNew` attribute aborts the update, which is
  the XRM no-replacement policy. The diff it applies is the one computed by
  the *same reconcile's* Observe (`n.instanceDiff`), so there is no
  Observe/Update TOCTOU inside one reconcile. On apply error the new state is
  **not** stored (unlike Create's error path) — the tracker keeps the
  pre-update state and the next refresh resolves reality. On success it stores
  state, re-runs the conversions, `SetObservation`, and the annotation move —
  but never `setExternalName`: an update that changes the id-bearing
  attributes relies on the next Observe for the rename (read; benign for AWS
  since ForceNew guards id-changing updates).
* `Delete` (`external_tfpluginsdk.go:802`) sets `Destroy=true` on the diff and
  applies. `SetDeleted(newState == nil)` marks logical deletion so the next
  Observe short-circuits (`meta.WasDeleted && IsDeleted`). For
  lifecycle-bound children whose delete is a no-op upstream, TF clears the
  state and this logic is what makes them terminate (comment in
  `nofork_store.go`). A failed delete keeps `LastOperation.Type == "delete"`,
  and the async `Delete`'s first case silently returns success for the
  *reconciler* while the tracker error surfaces via the callback — the
  finalizer is not removed until an Observe actually reports non-existence, so
  no premature GC (read).

## 5. The Terraform Plugin Framework external (69 resources)

### 5.1 Per-Connect costs on top of finding 3

The FW `Connect` (`external_tfpluginfw.go:169`) repeats per reconcile, after
`SelectTerraformSetup` already paid the 3.0 ms / 2 MB of finding 3:

| work | cost per Connect (measured) |
| --- | ---: |
| `configureProvider` — provider `Schema()`, `providerserver.NewProtocol6`, `ConfigureProvider` RPC (`external_tfpluginfw.go:281`) | 468 µs, 328 KB |
| `getResourceSchema` — full framework schema rebuild (`external_tfpluginfw.go:265`) | 92–128 µs, 63–86 KB |

The `ConfigureProvider` RPC itself is trivial — the fork's FW provider
`Configure` just copies the meta pointer
(`internal/provider/framework/provider.go:349`) — the cost is rebuilding the
protocol server and provider schema each time. Nothing here depends on the MR;
all of it is cacheable on the same key as finding 3. Category: waste.

Note that `GetFrameworkProviderWithMeta` panics on a meta that is not a
`*conns.AWSClient` (`internal/provider/framework/provider.go:60` type-asserts)
— combined with the swallowed error noted in the main document's finding 6,
construction failures are a panic, not an error return (read, and tripped by
the harness during development).

### 5.2 Observe/Create/Update/Delete

Read, differences from the SDK path worth recording:

* Observe issues a real `PlanResourceChange` on every reconcile
  (`getDiffPlanResponse`, `external_tfpluginfw.go:375`) — in-process, no AWS
  call, but it is the FW path's equivalent of the 4x schema rebuild.
* The create-error path stores the partial state
  (`external_tfpluginfw.go:758`) and deliberately does *not* store the
  returned TF identity (comment citing provider-upjet-aws#2135: the partial
  identity can be garbage and later trips an "identity changed" error) — and
  the async Observe recovers the external-name from that partial state
  (section 3.2). The FW path is strictly ahead of the SDK path here.
* `Update` refuses plans with `RequiresReplace`
  (`planRequiresReplace`) after `filterRequiresReplace` drops false positives
  by comparing prior and planned values. Small bug: the "prior and plan values
  are equal. Skipping..." debug line (`external_tfpluginfw.go:459`) is logged
  unconditionally for every path, including ones that were *not* skipped.
* `Delete` applies a null planned state with `PlannedIdentity` intentionally
  nil; state cleared ⇒ `SetDeleted(true)` — same shape as SDK.
* `tfValueToGoValue` (`external_tfpluginfw.go:985`) errors on unknown values,
  `DynamicPseudoType`, and numbers that fit neither int64 nor float64 — an
  Observe of a resource whose state contains such a value fails permanently
  rather than degrading (read; no affected resource identified).
* The framework path is immune to section 1's singleton contamination — it
  reads `rschema.Schema` values, not the SDK singletons.

## 6. Late-initialisation and the `status.atProvider` round-trip

Read, focused on lossiness and leaks:

* **Singleton-list conversion is asymmetric at the edges**
  (`upjet/pkg/config/conversion/list_conversion.go`). `FromTerraform`, an
  empty list `[]` becomes an empty object `{}` (`Convert`, the
  `newVal = map[string]any{}` branch) — `ToTerraform` later turns that `{}`
  into `[{}]`, a one-element list of an empty object, where the original state
  had zero elements. The fabricated element only lives in the *reconstructed*
  prior state that seeds the first refresh after a restart, and the refresh
  overwrites it, so no persistent effect was identified — but the round trip
  is not the identity (read/inferred).
* **A multi-item list at a singleton path is a hard error**
  (`errFmtMultiItemList`, `list_conversion.go:34`): if AWS ever returns two
  elements where the config declared `MaxItems: 1`, Observe fails permanently
  — a drift in the provider's assumption becomes an unrecoverable error loop
  rather than degraded output (read).
* **Sensitive values are kept out of `status.atProvider` by typing, not by
  filtering**: Observe hands the *full* state map — including plaintext
  sensitive attributes — to `SetObservation` and `LateInitialize`
  (`external_tfpluginsdk.go:587-596`); the generated Observation/Parameters
  structs simply have no fields for them (they were replaced by secret refs at
  generation time), so JSON unmarshalling drops them. That holds as long as
  generation-time sensitivity matches runtime state content. The
  `moveTFStateValuesToAnnotation` path (`annotation_conversions.go`) writes
  raw TF state values into the
  `internal.upjet.crossplane.io/field-conversions` annotation, but only for
  explicitly configured `TfStatusConversionPaths` and only when
  `ControllerReconcileVersion != Version` — no path configured in this repo
  today makes that a sensitive value (read).
* **`GetConnectionDetails` ordering is consistent** — both Observe and Create
  extract connection details from the pre-conversion (native TF) state map,
  as the sensitive paths require, before `ApplyTFConversions` reshapes it.
* **`GetSensitiveParameters`** re-reads every referenced Secret on every
  Connect (`external_tfpluginsdk.go:144,285`), one API-server GET per secret
  ref per reconcile (through the controller-runtime client, so cached when
  Secret informers are enabled). Deleted secrets are tolerated
  (`IgnoreNotFound`) so deletes are not blocked. Waste, minor.
* `getExtendedParameters` aliases `params["tags_all"] = params["tags"]`
  (`external_tfpluginsdk.go:169`) — both keys share one map object; nothing
  currently mutates one copy after that point, but any future in-place edit of
  `tags` would silently edit `tags_all` too (read).
* The `SetUpToDateCondition` skip and the stale-`RawPlan` `SetTfState` noted
  as finding 6 in the main document also apply verbatim to the FW path
  (`external_tfpluginfw.go:707-717`).

## 7. Finding 4, quantified: which sources pay which calls

Read from `internal/clients/{aws,provider_config,creds_cache,cache}.go` and
the AWS SDK. Per **reconcile of every MR**, with an assume-role chain of
length N (`spec.assumeRoleChain`), steady state:

| credential source | AWS-facing calls per reconcile | mechanism |
| --- | --- | --- |
| `IRSA`, no chain | 0 | `RetrieveCredentials` caches the `aws.CredentialsCache` (`creds_cache.go:162`); account ID cached in the entry |
| `IRSA` + chain N | 0 | the *last* chain link's `CredentialsCache` is what gets cached; the fresh chain built each reconcile by `GetRoleChainConfig` is discarded on hit — allocation waste only |
| `Secret`, no chain | 0 | static creds; identity-cache key (`cache.go:98`, the creds triple) is stable |
| `Secret` + chain N | N × `sts:AssumeRole` + 1 × `sts:GetCallerIdentity` | fresh `aws.NewCredentialsCache` per link per reconcile (`provider_config.go:294`); see below for the extra call |
| `WebIdentity` (+ chain N) | 1 × `sts:AssumeRoleWithWebIdentity` + N × `sts:AssumeRole` + 1 × `sts:GetCallerIdentity` (+1 K8s Secret GET when `tokenConfig.source: Secret`) | `UseWebIdentityToken` (`provider_config.go:377`) rebuilds the provider each reconcile |
| `Upbound` (+ chain N) | as WebIdentity (token from file) | `UseUpbound` (`provider_config.go:440`) |
| `PodIdentity`, no chain | 1 local HTTP to the Pod Identity agent; `sts:GetCallerIdentity` only when the agent rotates | excluded from the creds cache by the `Source != authKeyIRSA` check although structurally identical to IRSA |

Two mechanisms the main document did not name:

* **The identity cache defeats itself for exactly the sources that need it.**
  `GlobalCallerIdentityCache` keys on the credentials triple (`cache.go:98`).
  Every source above that mints fresh STS session credentials per reconcile
  therefore *misses* every reconcile — adding the `sts:GetCallerIdentity`
  column above — and pushes a never-to-be-seen-again entry into the 100-entry
  cache, whose eviction is an O(size) scan (`makeRoom`). The cache only works
  for the sources whose credentials were already stable.
* **What SDK-level caching would give.** `config.LoadDefaultConfig` already
  wraps every resolved provider in an `aws.CredentialsCache`
  (`aws-sdk-go-v2/config/resolve_credentials.go:89`), and `stscreds` sessions
  default to 15 minutes (`stscreds/assume_role_provider.go:146`). Reusing the
  provider chain across reconciles — keyed like the IRSA cache entry — would
  collapse the table above to roughly one call per role per ~15 minutes per
  ProviderConfig, independent of MR count. The existing IRSA entry proves the
  cache key design already exists; the `Source` check is the only gate.

The TF-side client (`configureNoForkAWSClient`, `aws.go:346`) receives a
*static snapshot* of the credentials (`AccessKey/SecretKey/Token` copied into
`AWSConfig`). Today that is safe because the client is rebuilt every Connect;
the finding-3 fix (caching the client) must key on credential expiry or
rebuild on rotation, or a cached client will keep signing with expired session
tokens (read — this is a constraint on the fix, not a current bug).

## 8. Suggested order of work (additions to the main document's list)

1. **Stop mutating shared schema singletons** (section 1). Copy-on-write in
   upjet's `MoveToStatus`/`common.go` helpers, or deep-copy in this repo's
   configurators. This is the only new finding with demonstrated, live,
   provider-wide misbehaviour (tag removal swallowed on ~491 resources), and
   it must land *before* anyone regenerates the APIs under the current
   dependencies.
2. **Persist the external-name from the async create goroutine** (section
   3.1) — e.g. have the finishing callback write the annotation from the
   tracker's state, closing the restart window for successful creates too, and
   decoupling it from the late-initialization management policy.
3. Port the FW partial-state name recovery to the SDK async Observe (3.2).
4. Deep-copy `mg` in async Delete (3.3); derive the async deadline from the
   resource's own timeout configuration instead of a flat hour (3.4).
5. Extend the credentials cache to all sources and key the identity cache on
   the *source* identity (ProviderConfig UID/generation), not the minted
   session credentials (section 7).
6. Cache the FW protocol server + provider schema alongside the finding-3
   client cache (5.1).
