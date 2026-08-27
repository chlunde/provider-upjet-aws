<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# Lead triage

Thirty candidate findings (L1–L30) were generated without verification and are
adjudicated here against source. This document completes the series:
[`memory-footprint.md`](memory-footprint.md) covers resident memory,
[`reconcile-workflow.md`](reconcile-workflow.md) and
[`reconcile-workflow-detail.md`](reconcile-workflow-detail.md) the defects on
the reconcile path, [`architecture-wins.md`](architecture-wins.md) the
structural wins. Anything those four already cover is marked **duplicate**
here rather than restated.

Labels as in the other documents: **measured** (something was run),
**read** (source, with citations), **inferred** (follows mechanically from
read code but needs a cluster or an AWS account to observe). No live cluster
and no AWS account were used. Two local experiments were run, both in
[`hack/memprofile/leadcheck`](../hack/memprofile/leadcheck): a name
round-trip table (`go run ./hack/memprofile/leadcheck`) and a race
reproduction (`LEADCHECK_RACE=1 go test -race ./hack/memprofile/leadcheck`,
skipped by default so `go test ./...` stays green).

Abbreviations: `UPJET` =
`github.com/crossplane/upjet/v2@v2.4.1-0.20260728103920-4f6e6e10dff2`,
`XPRT` = `crossplane-runtime/v2@v2.4.0`, `TFAWS` =
`github.com/upbound/terraform-provider-aws@v0.0.0-20260807134725-70894c6370d2`.

## Verdicts

| lead | one line | verdict | category | severity |
| --- | --- | --- | --- | --- |
| L13 | The TF AWS client holds a static credential snapshot for the whole async operation | **real** | correctness / data loss | high |
| L4 | A missing secret *or a missing key* substitutes `""` into the live TF config every reconcile | **real** | corruption | high |
| L14 | `endpoint.url.type: Dynamic` never reaches the client that does all resource CRUD | **real** | correctness / security | high |
| L19 | camel→snake over whole field paths mangles nested paths and digit-bearing names | **real** (latent) | corruption | high |
| L9 | The reconciliationpolicy wrapper resolves the PC a second time — and a missing PC now blocks deletion | **partially right** | correctness + waste | medium-high |
| L20 | The Framework external has no observe-only guard before planning | **real** | correctness | medium |
| L27 | A ForceNew refusal retries forever, one Refresh+diff per minute per MR | **real** | useless API calls | medium |
| L17 | `moveTFStateValuesToAnnotation` errors on any configured path absent from TF state | **real** (latent) | correctness | medium |
| L29 | `OperationTrackerStore` retains a full TF state, sensitive values included, per live MR | **real** | waste / security | medium |
| L8 | Data race on `callerIdentityCacheEntry.AccessedAt` | **real** | correctness | medium-low |
| L25 | Terraform provider logs are discarded unconditionally, `--debug` included | **real** | observability | medium-low |
| L11 | `sts:GetCallerIdentity` runs under the global credentials-cache write lock | **real** | waste | low-medium |
| L15 | Namespaced EFS FileSystem lost its `id` connection detail (but not `UseAsync`) | **partially right** | correctness | low-medium |
| L28 | SDK diagnostics are flattened through `%v` into a Go struct dump | **real** | observability | low-medium |
| L18 | A nil annotations map silently drops the moved value and still flags a spec update | **real** (latent) | correctness | low |
| L2 | Async callbacks address the MR by name, not UID | **partially right** | correctness | low |
| L3 | An async failure report is dropped if the callback's status update conflicts | **partially right** | observability | low |
| L1 | The tracker is removed before the finalizer update, with no rollback | **partially right** | useless API calls | low |
| L24 | `ext_api_duration` has no resource/service dimension | **partially right** | observability | low |
| L12 | The IRSA token file is opened and SHA-256'd before every cache lookup | **real** | waste | low |
| L16 | Two divergent copies of the ignore-changes walker | **partially right** | maintainability | low |
| L10 | Legacy→modern ProviderConfig conversion is a per-reconcile JSON round trip | **partially right** | waste | low |
| L7 | Caller-identity cache keys are plaintext credential triples, retained without TTL | **partially right** | security | low |
| L30 | The auth/PC-resolution layer and config parity have no tests | **real** | maintainability | low |
| L5 | The connection-details secret is re-applied every poll | **duplicate** | — | — |
| L6 | Default secret caching is a cluster-wide Secret informer | **duplicate** | — | — |
| L22 | "Global" rate limits are doubled across scopes | **duplicate** | — | — |
| L26 | The MR state-metrics recorder lists every kind every 5 s | **duplicate** | — | — |
| L21 | The EventHandler leaks rate-limiter failure entries for dead MRs | **wrong** | — | — |
| L23 | The external API call counter misses failed calls | **wrong** | — | — |

Counts: **15 real**, **9 partially right**, **4 duplicate**, **2 wrong**,
**0 unfalsifiable**. Nothing needed a cluster to *adjudicate*; several
consequences below are marked inferred because measuring their *size* would.

---

## 1. L13 — the async operation outlives its credentials

**Real. Category: correctness, with data loss downstream. Severity: high.
Fix belongs in this repo.** This contradicts a parenthetical in
`reconcile-workflow-detail.md` §7 — see §26 below.

`configureNoForkAWSClient` copies the credentials into the Terraform provider
config as three plain strings (`internal/clients/aws.go:337-346`):

```go
tfAwsConnsCfg := xpprovider.AWSConfig{
    AccessKey: creds.AccessKeyID,
    SecretKey: creds.SecretAccessKey,
    Token:     creds.SessionToken,
    ...
```

`xpprovider.AWSConfig` is `conns.Config` (`TFAWS/xpprovider/xpprovider.go:23`),
whose only credential inputs are those three strings plus `AssumeRole` /
`AssumeRoleWithWebIdentity` — which this repo does not populate, because it
resolves credentials itself and hands over the result. There is no
`aws.CredentialsProvider` on that struct and therefore **no refresh hook**:
the client signs with that triple until it is thrown away. **Read.**

The snapshot's lifetime is short:

| ProviderConfig | session length of the snapshot |
| --- | --- |
| `assumeRoleChain` (any source) | **15 minutes** — `stscreds.NewAssumeRoleProvider` with no `Duration`, `stscreds.DefaultDuration` (`aws-sdk-go-v2/credentials@v1.19.29/stscreds/assume_role_provider.go:146,282`), built at `internal/clients/provider_config.go:297-303` |
| `IRSA` / `WebIdentity` | up to 1 hour (`WebIdentityRoleOptions.Duration` left zero, so STS's own default applies — `.../stscreds/web_identity_provider.go:128-130`), *minus* however much of the session had already elapsed when `Retrieve` was called |
| `Secret` | static keys, no expiry |

The prior document's guard — "today that is safe because the client is rebuilt
every Connect" — holds for the synchronous reconcile loop and fails for the
asynchronous one, which is the only loop this provider has (all 1,029
controllers are async, `reconcile-workflow-detail.md` §3). `Create` captures
the external client built by *this* Connect and runs the Terraform create,
wait loops included, on a goroutine with a **one-hour** deadline
(`UPJET/pkg/controller/external_async_tfpluginsdk.go:140-181`, `:28`). Later
reconciles build fresh clients; the in-flight goroutine keeps the stale one.
So an `aws_db_instance`, `aws_msk_cluster` or `aws_elasticache_replication_group`
create that legitimately runs 20–60 minutes will, for a role-chain
ProviderConfig, spend most of that time signing with a token that expired
after 15 minutes. **Read**; the resulting `ExpiredToken` failure is
**inferred** (needs an account to observe).

Why it matters beyond a retry: the failure lands in exactly the place
`reconcile-workflow-detail.md` §3.1/§3.2 identifies as lossy — a create that
failed *after* AWS created the object, with the external-name held only in
process memory. Category is therefore data loss, not just correctness.

The fix is to stop pre-resolving: populate `AWSConfig.AssumeRole` /
`AssumeRoleWithWebIdentity` and let `awsbase` build a refreshing chain, or (if
the pre-resolution must stay) refuse to start an async operation whose
deadline exceeds `creds.Expires`.

## 2. L4 — a missing secret, or a missing key, writes `""` into live config

**Real. Category: corruption. Severity: high. Fix belongs in upjet.**

`storeSensitiveData` resolves a `SecretKeySelector` and then writes the result
into the Terraform parameter map unconditionally
(`UPJET/pkg/resource/sensitive.go:259-266`):

```go
sensitive, err = client.GetSecretValue(ctx, *sel)
if resource.IgnoreNotFound(err) != nil {
    return errors.Wrapf(err, errFmtCannotGetSecretValue, sel)
}
if err := setSensitiveParametersWithPaved(pavedTF, expandedJSONPath, tfPath, mapping, string(sensitive)); err != nil {
```

There are two ways to reach `string(nil)` — that is, `""`:

* **The secret is absent.** `IgnoreNotFound` swallows it. The comment on the
  sibling list branch (`:283-286`) says why the tolerance exists — so that a
  resource can still be deleted after its secret has been — but nothing scopes
  it to deletion.
* **The secret exists and the key does not.** `APISecretClient.GetSecretValue`
  returns `d[sel.Key], err` with `err` already known to be nil
  (`UPJET/pkg/controller/api.go:66-72`): a missing key is a zero-value map
  lookup and **no error at all**. `IgnoreNotFound` never even gets a chance.

The lead's key claim — that this is not confined to the deletion path — is
correct. `GetSensitiveParameters` runs inside `getExtendedParameters`
(`UPJET/pkg/controller/external_tfpluginsdk.go:144`), which every `Connect`
calls (`:260-264`) to build `params`, and `params` becomes `rawConfig`
(`:267`) — the configuration the SDK diffs and applies. So a rotation
window in which the secret is briefly missing, or a single typo in
`passwordSecretRef.key`, substitutes an empty string for the credential in the
provider's live view of desired state, on every reconcile, silently.
**Read.**

What happens next is resource-dependent and is **inferred**: for a field the
AWS API validates (an RDS master password) the update is rejected loudly; for
a field where an empty string is legal (a secret payload, a description, a
policy body) the update succeeds and destroys the value. That range is why
this is filed as corruption rather than as a specific credential-nulling bug.

One thing the lead did not notice, and which limits the blast radius: `var
sensitive []byte` is declared outside the loop (`:202`), so a stale value from
a previous field could in principle leak into the next — but
`GetSecretValue` returns `nil` on its error path, so each iteration does
overwrite it. No leak.

The fix belongs in upjet: distinguish "resource is being deleted" from "the
reconciler needs the real value", and make a missing *key* an error like a
missing secret.

## 3. L14 — `Dynamic` endpoints never reach the client that does the work

**Real. Category: correctness, with a security-shaped failure mode. Severity:
high. Fix belongs in this repo.**

Two independent endpoint mechanisms exist and only one of them is wired to
resource CRUD.

* `SetResolver` (`internal/clients/provider_config.go:160-220`) installs an
  `EndpointResolverWithOptions` on the provider's **own** `aws.Config` and
  handles `Static` and `Dynamic`. That config is what `sts:GetCallerIdentity`
  and credential resolution use.
* `configureNoForkAWSClient` (`internal/clients/aws.go:349-359`) builds the
  **Terraform** AWS client — the one every resource create, read, update and
  delete goes through — and populates `tfAwsConnsCfg.Endpoints` from
  `pc.Spec.Endpoint.URL.Static` only. `Dynamic` is not read there at all, and
  `conns.Config.Endpoints` (a `map[service]url`) is the only endpoint knob the
  struct has. **Read.**

`type` is a CRD-validated enum of `Static;Dynamic;Auto`
(`apis/namespaced/v1beta1/types.go`, `package/crds/aws.upbound.io_providerconfigs.yaml:392`),
so `Dynamic` is a supported, documented option that silently sends all
resource traffic to real AWS while the provider's own STS calls go to the
configured host. A user pointing a ProviderConfig at LocalStack or an
egress proxy this way gets a sandbox that creates real infrastructure.
**Read**; the actual traffic destination is **inferred** (a mock endpoint and
a `Bucket` would settle it in minutes).

The lead's secondary claim about `Auto` is wrong: `Auto` deliberately leaves
resolution to the SDK's partition defaults (`provider_config.go:164-169`), and
those defaults apply to the Terraform client too. But the same block hides a
second gap the lead missed: the `Static` branch keys off `URL.Static != nil`
rather than `URL.Type == "Static"`, and only fills `Endpoints` for services
listed in `Endpoint.Services` — so a `Static` endpoint with no `services` list
is also a no-op on the Terraform side while `SetResolver` applies it to
everything.

## 4. L19 — the version-skew safety net mangles the paths it stores

**Real, latent today. Category: corruption. Severity: high. Fix belongs in
upjet.** **Measured.**

The field-conversion annotation stores CRD field *paths* in camelCase and
converts them back with `name.NewFromCamel(path).Snake` applied to the whole
expression — `UPJET/pkg/controller/annotation_conversions.go:81` on the way in
and `:207` on the way out. `NewFromCamel` is `camelcase.Split` plus
lowercase-and-join-with-underscore (`UPJET/pkg/types/name/name.go:37-45`),
which is not the inverse of `NewFromSnake` and knows nothing about `.` or
`[n]`. `go run ./hack/memprofile/leadcheck` produces:

| terraform attribute | CRD field | back to snake | lossless |
| --- | --- | --- | --- |
| `vpc_id` | `vpcID` | `vpc_id` | yes |
| `cloudwatch_log_group_arn` | `cloudwatchLogGroupArn` | `cloudwatch_log_group_arn` | yes |
| `ipv6_addresses` | `ipv6Addresses` | `ipv_6_addresses` | **no** |
| `ipv4_prefixes` | `ipv4Prefixes` | `ipv_4_prefixes` | **no** |
| `s3_bucket_name` | `s3BucketName` | `s_3_bucket_name` | **no** |
| `sha256_tree_hash` | `sha256TreeHash` | `sha_256_tree_hash` | **no** |

and, worse, on the path expressions the machinery actually stores:

```
fooBar.bazQux                   -> foo_bar_._baz_qux
fooBar[0].bazQux                -> foo_bar_[_0_]._baz_qux
networkInterface[0].deviceIndex -> network_interface_[_0_]._device_index
```

Every separator is swallowed into a name segment. `paved.GetValue` on
`foo_bar_._baz_qux` looks for a field `foo_bar_` containing `_baz_qux`; the
`SetValue` in `mergeAnnotationFieldsWithSpec` (`:80-88`) would *create* that
nonsense key in the Terraform parameter map. So the mechanism works only for
single-segment, digit-free field names — the minority — and fails silently for
the rest.

Latency is the only mitigation: `moveTFStateValuesToAnnotation` and
`mergeAnnotationFieldsWithSpec` run only when `ControllerReconcileVersion !=
Version`, which today is true for exactly two resources
(`config/cluster/s3/config.go:128` for `aws_s3_bucket_lifecycle_configuration`,
`config/cluster/redshift/config.go:15` for `aws_redshift_cluster`), and the
annotation itself is only ever written by
`conversion.NewNewlyIntroducedFieldConversion`, which **no resource in this
repo registers**. Both halves are live code paths executing on every reconcile
of those two resources; they are no-ops only because the annotation is always
empty. The first resource to use the feature — and `aws_s3_bucket_lifecycle_configuration`'s
own fields are `rule[*].filter.prefix`-shaped — gets the mangling.

## 5. L9 — the reconciliationpolicy wrapper: modest waste, real deletion deadlock

**Partially right. Severity: medium-high for the correctness half, low for the
waste half. Fix belongs in upjet (the wrapper) and/or this repo (the source).**

Every generated controller wraps the managed reconciler
(`internal/controller/cluster/ec2/vpcendpoint/zz_controller.go:113-121`), and
the wrapper's first act on every `Reconcile` is `setRateLimiter`, which
returns an error before the inner reconciler ever runs
(`UPJET/pkg/reconciler/reconciliationpolicy/reconciler.go:81-87`).
`setRateLimiter` does an MR `Get`, then calls the configured source
(`:94-104`), which is `clients.ReconciliationPolicy` →`resolveProviderConfig`
→ a ProviderConfig `Get` plus `ProviderConfigUsage` tracking
(`internal/clients/pc_resolver.go:79-95,97-146,148-154`) — the same work
`SelectTerraformSetup` does again during `Connect`
(`internal/clients/aws.go:97`).

**The waste half is overstated.** All three reads go through the manager's
cached client, and `ProviderConfigUsageTracker.Track` applies with
`AllowUpdateIf(providerConfigRef changed)`
(`XPRT/pkg/resource/providerconfig.go:199-205`), so in steady state it is a
cached `Get` and a suppressed write. There is no "doubling of PC-related API
traffic": the cost is a second MR deep-copy, a second PC deep-copy, a PCU
deep-copy and the JSON round trip of §22, per reconcile — CPU and garbage, not
API-server load. It is also paid by requests the *inner* global rate limiter
would have deferred, because `ratelimiter.NewReconciler` sits inside the
wrapper (`zz_controller.go:114-115`). **Read.**

**The correctness half is real and is a regression.** `XPRT`'s managed
reconciler has one path that deliberately never calls `Connect`: an MR that
was deleted and whose policy says not to delete the external resource
(`deletionPolicy: Orphan`, or `managementPolicies` without `Delete`) goes
straight to unpublish-and-remove-finalizer
(`XPRT/pkg/reconciler/managed/reconciler.go:1032-1081`). That is the escape
hatch for "the ProviderConfig is already gone and I just want this object
out of my cluster". The wrapper now fails *before* that branch: a missing or
unreadable ProviderConfig makes `resolveProviderConfig` error, so the
finalizer can never be removed and the MR is stuck until someone edits
`metadata.finalizers` by hand. Deleting a ProviderConfig before its managed
resources is an ordinary teardown ordering. **Read**; the stuck object is
**inferred** but follows directly from the two code paths.

There is an observability tail: the wrapper's error never reaches the object.
`Connect` failures are recorded as `Synced=False` with `errReconcileConnect`
(`reconciler.go:1145-1160`); a `setRateLimiter` failure is only a
controller-runtime "Reconciler error" log line, because the wrapper never
touches status.

## 6. L20 — the Framework path plans even for observe-only resources

**Real. Category: correctness. Severity: medium. Fix belongs in upjet.**

Three externals, three treatments of `managementPolicies: [Observe]`:

* CLI path: routed to `Import` (`UPJET/pkg/controller/external.go:215-217`).
* SDK path: diff skipped entirely —
  `if !isObserveOnlyPolicy || !n.isManagementPoliciesEnabled { … getResourceDataDiff … }`
  (`UPJET/pkg/controller/external_tfpluginsdk.go:537-543`).
* Framework path: `getDiffPlanResponse` is called unconditionally
  (`UPJET/pkg/controller/external_tfpluginfw.go:640`), and the *only*
  management-policy consultation in the whole function is for
  late-initialisation (`:680`).

The asymmetry is not accidental-looking; it is flagged in place, three lines
above the call: `// TODO(cem): Consider skipping diff calculation to avoid
potential config validation errors in the import path` (`:637-639`, citing
crossplane/upjet#461). **Read.**

The consequence is **inferred**: an observe-only MR normally omits required
`forProvider` fields, so `PlanResourceChange` is handed a config full of
nulls. Either it returns error diagnostics — `getFatalDiagnostics` turns those
into a hard `Observe` failure (`external_tfpluginfw.go:375-402`) — or it plans
a change, `hasDiff` is true, and `ResourceUpToDate: false` is reported forever
(`:720-725`) while the policy forbids acting on it. Both outcomes degrade
adoption/import for the 69 Framework-backed resources. A unit test driving
`Observe` with policy `[Observe]` and a config missing required attributes
would settle which of the two happens; a kind cluster would settle it for a
real resource.

## 7. L27 — an immutable-field edit is a permanent one-per-minute AWS read

**Real. Category: useless API calls + observability. Severity: medium. Fix
belongs in upjet.**

`assertNoForceNew` refuses any diff carrying `RequiresNew`
(`UPJET/pkg/controller/external_tfpluginsdk.go:731-748`) and `Update` returns
that as an ordinary error (`:764-766`). Nothing marks it terminal, and nothing
gates re-attempts on the spec having changed. The controller's rate limiter is
`reconciliationpolicy.NewExponentialFailureRateLimiter(time.Second, 60*time.Second)`
(`zz_controller.go:56`, installed as `co.RateLimiter` at `:106`), so the
backoff saturates at 60 seconds. Each retry is a full `Observe`: a Terraform
`RefreshWithoutUpgrade` — real AWS reads — plus the diff
(`external_tfpluginsdk.go:500-501,537-543`), then the same refusal.
**Read.**

So one user editing `aws_db_instance.spec.forProvider.engine` produces a
read of that instance every minute, forever, plus a warning event per attempt,
and the only signal that it will never succeed is the wording of the `Synced`
message. Fleet-wide this is a steady background of pointless AWS calls
proportional to the number of misconfigured MRs, and it is indistinguishable
in metrics from transient failure. A terminal condition (or simply not
retrying until `metadata.generation` moves) is the fix.

## 8. L17 — one missing `IsNotFound` guard bricks Observe for a whole kind

**Real, latent today. Category: correctness. Severity: medium. Fix belongs in
upjet.**

`moveTFStateValuesToAnnotation` reads each configured status-conversion path
out of the Terraform state and returns any error unconditionally
(`UPJET/pkg/controller/annotation_conversions.go:205-208`):

```go
tfFieldValue, err := tfObservationPaved.GetValue(snakeP)
if err != nil {
    return false, errors.Wrapf(err, "cannot get value for %s", snakeP)
}
_, err = atProviderPaved.GetValue(snakeP)
if err != nil {
    if fieldpath.IsNotFound(err) {
```

The very next `GetValue`, four lines down, handles `IsNotFound` — so the
omission is visibly an oversight rather than a decision. The caller turns it
into a failed `Observe` (`external_tfpluginsdk.go:603-605`, and the identical
calls in `Create` at `:722` and `Update` at `:794`). Any `TfStatusConversionPaths` entry naming a
field that is *optional* in the Terraform state therefore breaks reconciliation
for every instance that happens not to have it set, while working fine for the
instances the author tested. **Read.**

No resource in this repo sets `TfStatusConversionPaths` yet, but the
surrounding machinery is already switched on for two resources (see §4), so
this is one config line away from being live.

## 9. L29 — the tracker store is a per-MR copy of Terraform state, forever

**Real. Category: waste + security. Severity: medium. Fix belongs in upjet.**

`OperationTrackerStore` is `map[types.UID]*AsyncTracker`
(`UPJET/pkg/controller/nofork_store.go:246`), populated on the first `Connect`
for an MR (`:268-278`) and pruned only by `RemoveTracker`
(`:282-287`), which runs when the finalizer is removed. Each `AsyncTracker`
holds a complete `tfsdk.InstanceState` — every attribute flattened to a string
— or its Framework equivalent (`:55-57`). One store per scope
(`cmd/provider/ec2/zz_main.go:247,270`). **Read.**

Two consequences the existing documents do not cover.
`memory-footprint.md` is about startup and static cost, and
`architecture-wins.md` §4 mentions the tracker only as a place to *add* a
cache; neither treats it as steady-state growth. First: resident memory scales
with the number of live managed resources even when everything is idle, at a
few kB per MR for a large resource (an S3 bucket with an inline policy, an
instance with user data). Second, and less obvious: those attribute maps
contain the sensitive fields in cleartext — including the values
`GetSensitiveParameters` just pulled out of Kubernetes Secrets (§2) — so every
heap dump, core dump and `pprof` heap profile of the provider contains them.

Sizing is **inferred**; measuring it needs a populated cluster, or a harness
that constructs one representative `InstanceState` per resource kind and sums
`unsafe.Sizeof`-style estimates.

## 10. L8 — a data race on the caller-identity cache's `AccessedAt`

**Real. Category: correctness. Severity: medium-low. Fix belongs in this repo.**
**Measured.**

`GlobalCallerIdentityCache` is a process-wide singleton
(`internal/clients/cache.go:23`) consulted by `getAccountId` on every
`SelectTerraformSetup`, i.e. by every controller goroutine on every reconcile
(`internal/clients/aws.go:133-137,196-202`). Its read path releases the lock
and *then* touches the entry (`internal/clients/cache.go:103-117`):

```go
c.mu.RLock()
o, ok := c.cache[key]
c.mu.RUnlock()
if ok {
    if time.Since(o.AccessedAt) > 10*time.Minute {   // :111 — no lock held
        c.mu.Lock()
        o.AccessedAt = time.Now()                     // :113 — lock held
        c.mu.Unlock()
    }
```

The read at `:111` and the write at `:113` touch the same multi-word
`time.Time` on the same entry from different goroutines, and only one of them
is synchronised. All MRs sharing a ProviderConfig share the key
(`:98-102`), so this is the common case, not a corner. The sibling cache added
later uses `atomic.Value` for exactly this field
(`internal/clients/creds_cache.go:102,206-208`), which reads as the same
mistake already noticed once.

`hack/memprofile/leadcheck/race_test.go` transcribes the function with the AWS
types removed (so the test does not need to link `xpprovider`) and the race
detector flags it on every run:

```
WARNING: DATA RACE
Write at 0x... by goroutine 9:   race_test.go:85
Previous read at 0x... by goroutine 17: (*cache).get() race_test.go:42
```

Practical impact is small — a torn timestamp perturbs LRU eviction order in
`makeRoom` (`cache.go:135-149`) and at worst costs one extra
`sts:GetCallerIdentity`. The reason to fix it is that it is undefined
behaviour in a hot path and it blocks ever putting `-race` in CI. Moving the
`time.Since` inside the read lock, or making the field an `atomic.Value` like
its sibling, is a two-line change.

## 11. L25 — Terraform-side logs cannot be turned back on

**Real. Category: observability. Severity: medium-low. Fix belongs in this
repo.**

`cmd/provider/ec2/zz_main.go:117` runs `log.Default().SetOutput(io.Discard)`
immediately after flag parsing and before the `--debug` branch at `:122-127`,
which only re-points controller-runtime's logger. Nothing anywhere in the repo
restores the standard logger. Meanwhile the in-process Terraform stack logs
through it heavily: 43 `log.Printf` sites in terraform-plugin-sdk v2.40.1 and
**3,368** in `TFAWS/internal`. **Read.**

So the provider that runs Terraform in-process has permanently silenced
Terraform's own diagnostics — the `[DEBUG] Trying to resolve …`,
retry and diff-decision lines that are the usual way to debug a mangled diff
or an AWS SDK retry storm — with no flag, no environment variable and no
restart-time escape. Recovering them requires editing this line and rebuilding.
Gating the discard on `!*debug`, or honouring `TF_LOG`, restores the channel
without reintroducing the noise it was added to suppress.

## 12. L11 — the global credentials lock is held across an STS round trip

**Real. Category: waste. Severity: low-medium. Fix belongs in this repo.**

On a miss, `RetrieveCredentials` takes the write lock and calls the account-ID
function inside it (`internal/clients/creds_cache.go:212-231`):

```go
c.mu.Lock()
defer c.mu.Unlock()
...
id, err := accountIDFn(ctx)
```

`accountIDFn` is the closure at `internal/clients/aws.go:113-124`, whose
non-LocalStack branch is `sts.NewFromConfig(*awsCfg).GetCallerIdentity(...)` —
a network call with the SDK's retries. Because the cache is a single global
guarded by one `sync.RWMutex`, every other IRSA reconcile's `RLock` at `:195`
blocks for the duration. **Read.**

The failure mode is correlated rather than constant: token rotation
invalidates the key for every MR at once (the key includes the token file's
hash, `:188-193`), so the whole fleet misses together, and if STS is slow or
throttling, reconciles across all controllers of both scopes queue behind one
call. The double-checked pattern at `:216` shows the author was thinking about
the thundering herd; the remaining fix is to do the STS call outside the lock,
or per-key (`singleflight`) rather than globally.

## 13. L15 — the namespaced EFS FileSystem lost a connection detail

**Partially right. Category: correctness. Severity: low-medium. Fix belongs in
this repo.**

The cluster-scoped configurator for `aws_efs_file_system` sets two things the
namespaced copy does not (`config/cluster/efs/config.go:48-62` vs
`config/namespaced/efs/config.go:48-53`): `r.UseAsync = true` and an
`AdditionalConnectionDetailsFn` publishing the `id` key.

**The `UseAsync` half is inert.** `UseAsync` defaults to `true` for every
resource (`UPJET/pkg/config/common.go:92`), so writing it changes nothing:
`internal/controller/namespaced/efs/filesystem/zz_controller.go:61` uses
`NewTerraformPluginSDKAsyncConnector`, exactly like its cluster twin, and
across the whole tree 960 controllers per scope are async and **zero** are
synchronous (measured by grep). The lead's "namespaced FileSystems block a
reconciler slot" does not happen.

**The connection-detail half is real**: a namespaced `FileSystem`'s connection
secret has no `id` key, and the cluster-scoped one does — a silent behavioural
difference between `aws.upbound.io` and `aws.m.upbound.io` kinds of the same
resource that would break a Composition ported from one to the other.

The lead's broader worry — that nobody is checking which drifts are
intentional — survives the check. Normalising import paths and diffing the two
trees leaves 15 differing files; every one except `efs` is either cosmetic
(import order in `docdb`, a copyright year in `networkmonitor`) or an
intentional `v1beta1`→`v1beta2` conversion block that exists only in the
cluster scope (`autoscaling`, `connect`, `elasticache`, `kafka`, `rds`,
`redshift`, `s3`). Two test files also exist only for the cluster scope
(`autoscaling/config_test.go`, `elasticache/config_test.go`). A parity test
would pin this cheaply — see §24.

## 14. L28 — SDK diagnostics are printed as a Go struct dump

**Real. Category: observability. Severity: low-medium. Fix belongs in upjet.**

Every SDK-path failure formats the whole diagnostics slice with `%v` —
`errors.Errorf("failed to observe the resource: %v", diag)`
(`UPJET/pkg/controller/external_tfpluginsdk.go:505`), and the same shape in
`Create`/`Update`/`Delete` (`:686`, `:772`, `:813`). `diag.Diagnostics` is
`[]Diagnostic` and neither type implements `Stringer` or `error`
(`terraform-plugin-sdk/v2/diag/diagnostic.go:20-80`), so `%v` renders the raw
struct: severity as an **integer** (`Error Severity = iota`, `:103`, so an
error prints as `0`), summary, detail, and the `cty.Path` internals. Warning
diagnostics are included in the printed slice even though only errors gate the
branch. **Read.**

Upjet already has the formatter this wants, on the Framework side only:
`FrameworkDiagnosticsError` renders `summary: detail: path` and joins the
error-severity entries (`UPJET/pkg/terraform/errors/fw_diag.go:16-50`). The
fix is to write the SDK twin and use it. This matters because the `Synced`
condition message is the primary user-facing error surface in Crossplane.

## 15. L18 — a nil annotations map loses the value and loops

**Real, latent. Category: correctness. Severity: low. Fix belongs in upjet.**

`moveTFStateValuesToAnnotation` writes into the caller's map, except when
there isn't one (`UPJET/pkg/controller/annotation_conversions.go:232-236`):

```go
if annotations == nil {
    annotations = make(map[string]string)
}
annotations[conversion.AnnotationKey] = string(jsonBytes)
```

The reassignment is local. The caller does
`annotations := mg.GetAnnotations()` … `if annotationUpdate {
mg.SetAnnotations(annotations) }` (`external_tfpluginsdk.go:601-607`, and
`:722-726` in `Create`), so on the nil path it calls `SetAnnotations(nil)`,
reports `ResourceLateInitialized: true`, and the reconciler performs a spec
update that changes nothing — losing the value the function exists to
preserve and repeating every cycle. **Read.**

Reachability is the mitigation: it needs the §4 preconditions *and* an MR with
no annotations at all, which is rare for upjet resources (the external-name
annotation is normally present by the time `Observe` runs). Filed as real
because the fix is one line — return the map, or `mg.SetAnnotations` inside —
and because it sits in the same function as §8.

## 16. L2 — async callbacks address the MR by name, not UID

**Partially right. Category: correctness. Severity: low. Fix belongs in
upjet.**

The mechanism is exactly as claimed: `callbackFn` closes over a
`types.NamespacedName`, re-`Get`s whatever object now lives there, and writes
conditions with no UID comparison (`UPJET/pkg/controller/api.go:124-152`).
Delete an MR and recreate it with the same name while a create is in flight —
CI loops do this — and the finishing callback stamps `LastAsyncOperation` and
`ReconcileError`/`ReconcileSuccess` from the dead predecessor's operation onto
the new object, then requeues it (`:153-167`). The tracker store is UID-keyed
(`nofork_store.go:246`) so the operations themselves do not collide; only the
status writes do. **Read.**

**The "corruption" framing is too strong.** The blast radius is conditions and
one extra reconcile: nothing in the callback touches spec, annotations or the
external resource. The sharper version of the concern is the *success* case —
a stale `ReconcileSuccess` can overwrite a genuine error on the new object,
which is a worse lie than a stale error. A `tr.GetUID() != expectedUID`
early-return is the fix.

## 17. L3 — a conflicting status update drops the async failure report

**Partially right, and adjacent to `reconcile-workflow-detail.md` §3.5.
Category: observability. Severity: low. Fix belongs in upjet.**

The mechanism holds. `callbackFn` does a single unretried
`ac.kube.Status().Update` (`UPJET/pkg/controller/api.go:152`); the goroutine
that invoked it logs any failure at `Info` and moves on
(`external_async_tfpluginsdk.go:173-175`, `:219-221`, `:265-267`). The
callback's `Get` reads the informer cache, which is typically stale right
after the reconciler's own status write, so conflicts are plausible rather
than exotic.

**The consequence is narrower than stated.** The error is not only in the
conditions: `LastOperation.SetError` keeps it in the tracker (`:161`), and
`Clear(true)` on the next `Observe` explicitly preserves it
(`external_async_tfpluginsdk.go:126`, `UPJET/pkg/terraform/operation.go:58-67`),
so the next `Create`/`Update` returns it to the managed reconciler and it
surfaces as `Synced=False` one cycle late. The genuine loss requires the
resource to look present and up-to-date on that next `Observe`, at which point
`:132-136` sets `ReconcileSuccess` and `Clear(false)` wipes the error — which
is the path `reconcile-workflow-detail.md` §3.5 already describes as "why
`Synced` can read true during a failing create". A `RetryOnConflict` around
the status update closes the reported half.

## 18. L1 — the tracker is removed before the finalizer update, with no rollback

**Partially right. Category: useless API calls. Severity: low. Fix belongs in
upjet.**

`OperationTrackerFinalizer.RemoveFinalizer` deletes the tracker first and only
then asks the inner finalizer to update the object
(`UPJET/pkg/controller/finalizer_tfpluginsdk.go:46-51`); the inner finalizer is
a plain `client.Update` (`XPRT/pkg/resource/api.go:176-184`) that can conflict,
and there is no compensating re-insert. The state destroyed by that ordering is
the `isDeleted` flag whose only purpose is the early return in `Observe`
(`external_tfpluginsdk.go:494-498`).

**The premise about *why* `isDeleted` exists is wrong.** It is not a
parent-bound "logical deletion" marker: it is set from `newState == nil` after
an ordinary destroy (`external_tfpluginsdk.go:817`), i.e. the normal
successful-delete case.

What actually happens on a conflict — and the conflict is likely, since the
async destroy callback has just written status — is that the next reconcile
builds a fresh tracker, reconstructs state from `status.atProvider`, and
performs a **Terraform refresh against AWS** for a resource already destroyed,
which the early return existed to avoid. If that refresh still sees the
resource (AWS read-after-delete consistency), the reconciler calls `Delete`
again. So: one wasted AWS read per conflict, and a second destroy in the
eventual-consistency window — the repeated-destroy claim, but a rarer and
more benign path than the lead described. **Read**; the refresh outcome is
**inferred**. Swapping the two statements (finalizer first, tracker second) is
the fix.

## 19. L24 — the AWS-latency histogram has no resource dimension

**Partially right. Category: observability. Severity: low. Fix belongs in
upjet.**

`ExternalAPITime` (`upjet_resource_ext_api_duration`) carries a single
`operation` label — `read`, `create`, `update`, `delete`, `connect`
(`UPJET/pkg/metrics/metrics.go:39-46`, call sites
`external_tfpluginsdk.go:251,502,655,770,811`). `DeletionTime` and the TTR
metric do carry `group`/`version`/`kind` (`:63-71`), so the omission is
inconsistent within the same file. There is also no counter for *failed*
calls anywhere: SDK diagnostics increment nothing.

**Half the lead is wrong**: `ExternalAPICalls`
(`upjet_resource_external_api_calls_total`) *does* have a `service` label
(`:47-61`, populated from the AWS middleware at
`internal/clients/aws.go:311-312,322`), so "which AWS service is slow" is already
partly answerable — by call *volume*, not by latency. The accurate statement
is that latency cannot be attributed to a service or a kind, and failures
cannot be counted at all.

## 20. L12 — the IRSA token file is hashed before every cache lookup

**Real. Category: waste. Severity: low. Fix belongs in this repo.**

`RetrieveCredentials` computes `hashTokenFile(os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE"))`
(`internal/clients/creds_cache.go:188-193`) *before* it can look in the cache,
because the hash is part of the key. `hashTokenFile` opens the projected
token, streams it through SHA-256 and closes it (`:258-277`). That is an
`open`/`read`/`close` plus a hash of a few kB per reconcile per IRSA-backed MR,
on the `Connect` path. **Read.**

Small, but unnecessary: the token file's mtime (or a cached hash invalidated by
`fsnotify`) gives the same key with one `stat`. The related `// TODO: consider
implementing a TTL` at `:199` is real too — rotated-out entries linger until
the 100-entry LRU evicts them — but the LRU bounds it, so it is a non-issue.

## 21. L16 — two ignore-changes walkers, one of them with a real format bug

**Partially right. Category: maintainability. Severity: low. Fix belongs in
upjet.**

Both copies exist and both are self-labelled as copy-paste
(`UPJET/pkg/resource/ignored.go` and
`UPJET/pkg/controller/ignored_tfpluginsdk.go:13-14,20-21,47-48`). They differ
in three ways, and the lead's reading of which differences matter is only
partly right.

* **Path format** (`k["nested"]` vs `k.nested`): *intentional*. The first
  produces an HCL `ignore_changes` expression for the CLI path, the second
  flatmap attribute keys for the SDK diff. Not a divergence.
* **Nil handling**: real but practically unreachable. The SDK copy skips
  `v == nil` (`ignored_tfpluginsdk.go:26-28`), the original does not. Both
  operate on maps produced by JSON-marshalling the typed MR with `omitempty`,
  so a nil value requires an explicit `null` that the structural schema would
  have to admit first.
* **Nested lists of lists**: a genuine bug, in the *original*.
  `getIgnoredFieldsArray` recurses with `fieldPath+"%s"`
  (`ignored.go:54`), so the format string accumulates an unconsumed verb and
  the next `Sprintf` emits paths containing `%!s(MISSING)`. The SDK copy does
  not (`ignored_tfpluginsdk.go:59`).

The catch is that the buggy copy is the one this provider never runs:
`GetTerraformIgnoreChanges` has exactly one caller,
`UPJET/pkg/terraform/files.go:98`, on the CLI/workspace path. So this is a
maintainability finding — two implementations, opposite nil semantics, one
latent formatting bug — not a live defect here.

## 22. L10 — the ProviderConfig JSON round trip

**Partially right. Category: waste. Severity: low. Fix belongs in this repo.**

`legacyToModernProviderConfigSpec` marshals the entire legacy
`ProviderConfig` — object metadata, `managedFields` and all — to JSON and
unmarshals it into the namespaced type, on every reconcile of every
cluster-scoped MR (`internal/clients/pc_resolver.go:24-46`), and twice per
reconcile counting §5. The author's own `// TODO(erhan): this is hacky and
potentially lossy` is at `:25`.

**"Lossy" is not true today**, and that half of the lead is killed: the two
`ProviderConfigSpec` / `ProviderCredentials` declarations in
`apis/cluster/v1beta1/types.go` and `apis/namespaced/v1beta1/types.go` are
byte-identical (checked by diffing the struct bodies), and their JSON tag sets
match. The risk is prospective — the next field added to one type only
disappears silently — which is precisely what a reflective parity test would
pin.

The "partially populated on error" observation (`return &mSpec, err` at `:45`)
is inert: every caller checks the error.

## 23. L7 — plaintext credential triples as cache keys

**Partially right. Category: security hygiene. Severity: low. Fix belongs in
this repo.**

The facts hold: the key is
`fmt.Sprintf("%s:%s:%s", creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)`
(`internal/clients/cache.go:98-102`), up to 100 such strings live in a map for
the process lifetime, and eviction is size-based only — `makeRoom` picks the
oldest `AccessedAt` and has no notion of expiry (`:135-149`), so long-dead
session tokens sit there until pressure removes them.

**The consequence is marginal.** The same secrets are already resident in the
AWS SDK's own `CredentialsCache`, in the `aws.Config` objects, in the
Terraform client (§1, as three string fields), and — with the default
`--enable-secret-cache=true` — in the informer's copy of the ProviderConfig's
Secret (`architecture-wins.md` §3). Hashing the key removes one copy out of
many, which is worth doing because it is a two-line change, not because it
meaningfully shrinks what a heap dump yields. The absent TTL is the more
interesting half: entries for expired sessions are dead weight *and* dead
secrets.

## 24. L30 — the hand-written correctness-sensitive code is the untested code

**Real. Category: maintainability. Severity: low. Fix belongs in this repo.**

`find internal -name '*_test.go'` returns two files: `cache_test.go` and
`partitions_test.go`. Untested, in the same package: `creds_cache.go` (277
lines of LRU, locking and cache-key construction), `pc_resolver.go`
(ProviderConfig resolution, the legacy→modern conversion, usage tracking),
`provider_config.go` (`SetResolver`, `setPartition`, role chains, web-identity)
and `aws.go` (`SelectTerraformSetup`, the middleware, the TF client). Nothing
compares `config/cluster` with `config/namespaced`. **Measured** (by running
the find and the normalised tree diff of §13).

That set is the intersection of "hand-written" and "correctness-sensitive",
and it is where §1, §3, §10, §12, §13, §20, §22 and §23 all live. Two cheap
tests would have caught three of them: a reflective parity test over the two
`ProviderConfigSpec` types and the two `config` trees (§13, §22), and a
`-race` test over `GlobalCallerIdentityCache` (§10) — the second now exists in
`hack/memprofile/leadcheck` in transcribed form and would be a five-line
addition to the real `cache_test.go`.

---

## 25. Killed leads

* **L5 — connection-details secret re-applied every poll.** Duplicate, and
  the write half is refuted where it is covered: `architecture-wins.md` §5
  traces the publisher end to end — there is one *cached* Secret GET per poll
  per connection-secret-owning MR (the read-before-write in
  `APIPatchingApplicator.Apply`), and the write itself is suppressed by
  `AllowUpdateIf` when the data is unchanged. There is no PUT per poll.
* **L6 — cluster-wide Secret informer.** Duplicate of
  `architecture-wins.md` §3, which covers both the memory unboundedness and
  the exposure, and proposes label-scoped `cache.ByObject` with the
  "unlabelled secret becomes invisible" caveat.
* **L21 — EventHandler leaks failure entries keyed by dead MRs.** Wrong: the
  map it names is keyed by **rate-limiter name**, not by managed resource —
  `rateLimiter := e.rateLimiterMap[rateLimiterName]`
  (`UPJET/pkg/controller/handler/eventhandler.go:79-87`) — and the only names
  in use are `""` and `asyncCallback` (`api.go:39`), so it holds at most two
  entries for the life of the process. Per-item failure counts live inside the
  limiter, and the controller's workqueue `Forget`s an item on any successful
  reconcile, including the one that removes the finalizer; the
  reconciliationpolicy finalizer additionally calls `Remove` for
  policy-overridden requests
  (`UPJET/pkg/reconciler/reconciliationpolicy/finalizer.go:68-81`,
  `UPJET/pkg/internal/ratelimiter/*.go:64-98`). The `setQueue` latch
  (`eventhandler.go:113-119`) is real but has no trigger: controller-runtime
  builds a controller's queue once.
* **L22 — "global" rate limits doubled.** Duplicate: `architecture-wins.md`
  §7 already records the two `ratelimiter.NewGlobal` calls
  (`zz_main.go:234,257`) and the resulting 2× on `--max-reconcile-rate`. The
  remaining half of the lead, `MaxConcurrentReconciles: *maxReconcileRate` per
  controller (`XPRT/pkg/controller/options.go`, `ForControllerRuntime`), is the
  cross-provider Crossplane convention and is bounded in practice by the global
  requeue limiter, so it is not a separate defect.
* **L23 — the API call counter misses failed calls.** Wrong, and the code
  comment it doubts is correct. `withExternalAPICallCounter` is registered via
  `AppendAPIOptions` (`internal/clients/aws.go:397`), and API options are
  applied to the middleware stack *after* the operation's own middlewares; both
  use `middleware.After`, which appends at the tail
  (`smithy-go@v1.27.3/middleware/step_deserialize.go`, `Add`), and the step
  executes head-first with the tail innermost (`HandleMiddleware`). The counter
  is therefore the innermost deserialize handler, so its `next` is the
  transport: a 429 or any other HTTP error returns `err == nil` there and *is*
  counted; the operation deserializer that turns the response into an
  `smithy.APIError` sits outside it. Only connection-level failures go
  uncounted, exactly as the comment says. What survives is not the hypothesis
  but the observation in §19: there is no failure counter and no outcome label.
* **L26 — the 5-second MR state-metrics poller.** Duplicate of
  `architecture-wins.md` §6, which measures the DeepCopy churn (0.3 MB/s at
  1k MRs, 1.6 MB/s at 5k) and lists the fixes.

## 26. Where this contradicts earlier documents

One claim in the existing series does not survive.
`reconcile-workflow-detail.md` §7 closes with:

> The TF-side client (`configureNoForkAWSClient`, `aws.go:346`) receives a
> *static snapshot* of the credentials … Today that is safe because the client
> is rebuilt every Connect … (read — this is a constraint on the fix, not a
> current bug).

The mechanism is described correctly; the "safe today" conclusion is not. It
holds only if the client's lifetime is bounded by the reconcile, and it is
not: every operation in this provider is asynchronous, and the goroutine keeps
the client it was handed for up to the one-hour async deadline
(`external_async_tfpluginsdk.go:28,140-181`). With an `assumeRoleChain` the
snapshot is a 15-minute session
(`stscreds/assume_role_provider.go:146,282`). See §1. The rest of §7's
accounting — which credential source pays which STS call — is unaffected and
was independently re-read while checking L11 and L12; it holds.

Nothing else in `memory-footprint.md`, `reconcile-workflow.md`,
`reconcile-workflow-detail.md` or `architecture-wins.md` was contradicted by
this pass. Two of their conclusions were reused as kills (§25, L5 and L22) and
one, `architecture-wins.md` §4's note that the operation tracker "already
persists per-MR state across reconciles", turns out to have a cost side the
documents had not priced — §9.

## 27. What was checked and found clean

Recorded so the same ground is not re-walked:

* `ProviderConfigSpec` parity between the cluster and namespaced APIs — the
  struct bodies are identical, so §22's conversion is lossless today.
* `config/cluster` vs `config/namespaced` across the whole tree — 15 files
  differ, 14 of them cosmetically or by design (§13).
* Whether `UseAsync` still selects a connector — it does not; the default is
  `true` and all 1,920 SDK controllers across both scopes are async (§13).
* Whether `ProviderConfigUsage` tracking writes on every reconcile — it does
  not; `AllowUpdateIf` suppresses the no-op (§5).
* Whether the AWS call counter sits inside or outside error deserialization —
  inside (§25, L23).
