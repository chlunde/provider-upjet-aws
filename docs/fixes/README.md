<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# Actionable fixes

One file per fix worth doing now. Each contains the mechanism, the evidence, the
change, how to test it, a ready-to-paste GitHub issue and a branch name.

## Status of the analysis behind these

Read this before treating any of it as ready to merge.

* **Most of these now have branches.** The `status` column says which. Anything
  marked *implemented* has a pushed branch with tests; anything marked *not
  started* is still analysis only. `dropCodegenOnlyMetadata` (commit
  `a8fc8b5f5`, &minus;17.2 MiB of live heap) has no file here because it landed
  before this directory existed.
* **No pull requests have been opened.** Every branch is pushed to a fork and
  waiting on a human to decide whether and where to propose it.
* **Verification is uneven.** Each file states its own evidence level:
  * **measured** — reproduced locally with the harness in `hack/memprofile`.
  * **read** — established by reading the source, not executed.
  * **latent** — the defective code path is confirmed by reading, but nothing
    demonstrates a user reaching it. Treat the severity as a ceiling.
* **Nothing was tested against a live AWS account or a cluster.** That is the
  single biggest gap. Several fixes below need an e2e run before merge, and each
  says so.

## What is here and what is not

Included if the fix is **very small and worth at least medium value**, or
**high value at any size**. Everything else stays in the analysis docs.

Deliberately excluded, with reasons:

| excluded | why |
| --- | --- |
| Change-driven / batched observation ([architecture-wins.md](../architecture-wins.md) §2) | High value, but a new optional controller and an ARN→MR mapping. A design proposal, not a fix. |
| Unchanged-state fast path (§4) | High value, ~60–90 % of steady-state garbage, but it needs a correctness argument about secret-ref contents that nobody has made yet. |
| No-op status PUT suppression (§5), state-metrics deep copy (§6) | Small and real, but they live in crossplane-runtime and the win is modest. |
| L1, L2, L3, L7, L10, L12, L16, L18, L24, L28, L30 | Low severity; see [lead-triage.md](../lead-triage.md). |
| L21, L23 | Refuted. |

## The fixes

| # | fix | category | severity | size | lives in | status |
| - | --- | --- | --- | --- | --- | --- |
| [01](01-movetostatus-shared-schema.md) | Stop `MoveToStatus` mutating shared schema singletons | corruption | **critical** | small | upjet | **implemented, verified** — `chlunde/upjet` `fix-movetostatus-copy-before-mutate` @ `9124f35` |
| [02](02-clear-schemafunc.md) | Clear `SchemaFunc` after materialising `Schema` | correctness + waste | high | **1 line** | upjet | **implemented, verified** — `fix-clear-schemafunc-after-materialise` @ `786ec33` |
| [03](03-async-credential-expiry.md) | Credentials expire mid-operation on async paths | data loss | high | medium | this repo | **partially addressed by fix 21** — the full fix needs a change to the `xpprovider` surface of the terraform-provider-aws fork |
| [04](04-missing-secret-key.md) | A missing secret key silently becomes `""` | corruption | high | small | upjet | **implemented, narrowed** — `fix-error-on-missing-secret-key` @ `32e9967` |
| [05](05-create-external-name.md) | Persist the external-name when create fails or is async | data loss | high | small | upjet | not started |
| [06](06-dynamic-endpoint-ignored.md) | `endpoint.url.type: Dynamic` never reaches the CRUD client | correctness | high | small | this repo | **implemented, verified** — `fix/dynamic-endpoint-for-tf-client` @ `29aa0a4`; no e2e |
| [07](07-fieldpath-camel-snake.md) | camel→snake mangles nested and digit-bearing paths | corruption | medium (latent) | small | upjet | **implemented, verified** — `fix-fieldpath-segmentwise-camel-snake` @ `046b8f2` |
| [08](08-credentials-cache-all-sources.md) | One STS call per reconcile for every non-IRSA source | useless API calls | high | medium | this repo | **reviewed, ready** — `fix/cache-credentials-for-all-sources` @ `d3d61421a1` |
| [09](09-cache-aws-client.md) | Rebuilding the AWS client and FW provider every Connect | waste | high | medium | this repo | not started |
| [10](10-gate-namespaced-build.md) | Build the namespaced provider only when it is used | waste | high | medium | this repo | not started |
| [11](11-scope-secret-informer.md) | The Secret informer is cluster-wide and unbounded | security | medium-high | **small** | this repo | **ready, re-priced** — `fix/scope-secret-informer` @ `18fa5d097`, see audit-cost note |
| [12](12-caller-identity-cache.md) | Data race and STS-under-lock in the identity cache | correctness | medium | **small** | this repo | **ready, one caveat** — `fix/identity-cache-race-and-lock-scope` @ `efb86ceee` |
| [13](13-double-rate-limiter.md) | `--max-reconcile-rate` delivers double what it says | correctness | medium | **1 line** | this repo | **reviewed, ready** — `fix/single-global-rate-limiter` @ `2de61d751` |
| 14 | Suppress the no-op status update on every reconcile | waste / cost | high (cost) | small | crossplane-runtime | **implemented, verified** — `fix/suppress-noop-status-update` @ `35d1fdc` |
| [15](15-wafv2-rule-group-external-name.md) | `WebACLRuleGroupAssociation` never records its external name and leaks associations | **data loss** | **critical** | small | this repo | **implemented, verified** — `fix-wafv2-rule-group-association-external-name` @ `de1ad72` |
| [16](16-tagger-noop-spec-update.md) | The `Tagger` initializer writes the spec on every reconcile | waste / cost | high (cost) | small | upjet | **implemented, verified** — `chlunde/upjet` `fix-tagger-skip-noop-spec-update` @ `43f8c2d` |
| [17](17-update-connection-details.md) | `Update` returns no connection details, so a rotated credential lands one Observe late | correctness | low | small | upjet | **implemented, verified** — `fix-update-connection-details` @ `edfc8db` |
| [18](18-framework-replace-messages.md) | Three misleading messages on the Framework replace path | observability | low | **trivial** | upjet | **implemented, verified** — `fix-error-message-defects` @ `95385db` |
| [19](19-external-name-template-defects.md) | Two malformed external-name templates, plus a guard that makes the class detectable | correctness | high | small | this repo | **implemented, verified** — `fix-external-name-template-defects` @ `453072d` |
| [20](20-path-string-surgery.md) | String surgery on structured field paths corrupts any path containing the manipulated character | correctness | medium (latent) | small | upjet | **implemented, verified** — `fix-path-string-surgery` @ `97467d6` |
| [21](21-assume-role-session-duration.md) | Assume-role chains get 15-minute sessions against a 1-hour async deadline | data loss | high | small | this repo | **implemented, verified — mitigation only**, `fix/refreshing-credentials-for-async-ops` @ `8f2462c`; one live-account check wanted before shipping |
| [22](22-changelog-attribute-details.md) | Change-log entries say only "an update happened"; the changed attribute set is discarded | observability | medium-high | small | upjet | **implemented, verified** — `feat-changelog-attribute-details` @ `e337a43` |
| [23](23-release-startup-heap.md) | ~~180–255 MiB of idle heap is never returned after startup~~ | — | — | — | this repo | **RETRACTED** — the runtime returns it unprompted within ~2.5 min idle, ~15 s under load. Do not open. |

## Confirmed in round-2 triage, no branch yet

Verified by reading the source (see [lead-triage-round2.md](../lead-triage-round2.md)
for the full verdicts and the three that were refuted). Ordered by what I would
do next. None of these has a patch.

| lead | what | category | size | lives in |
| --- | --- | --- | --- | --- |
| R3 | The `s`-suffix trim mangles connection-secret keys for map/list sensitive attributes — `connection_propertie`, `airflow_configuration_option` (`pkg/resource/sensitive.go:473`) | correctness | small but **API-breaking** | upjet |
| R11 | Two unasserted `Conversions[1:]` index assumptions, including `config/cluster/elasticache/config.go:114` | maintainability | small | this repo |
| R10 | A Version-only SNS/SQS policy change is silently suppressed — deliberate, and no clean fix exists | correctness | — | this repo |
| R20 | ProviderConfig inconsistency affecting `sts:GetCallerIdentity` only | correctness | small | this repo |

Branched since this table was written: R9 became fix 15, R1 fix 16, R6 fix 17,
R16 and R17 fix 18, R7 and R8 fix 19, and R12 and R14 fix 20. R2 is not a fix but a correction to
fix 11's cost accounting, recorded in the audit-cost note below.

Two findings surfaced while fixing the above and are recorded in the fix files
rather than here: `tftypes.AttributePath.String()` renders
`AttributeName("field")`, so improving the replace-refusal message needs a
field-path rendering decision (fix 18); and `ExternalNameNotTestedConfigs`
references `{{ .setup.configuration.account_id }}`, a setup key the provider does
not populate — latent, since no registry references that map (fix 19).

Forward-looking work that is not a defect fix lives in [ideas.md](../ideas.md).
Its top item — surfacing the Terraform diff — is now partly done as
[fix 22](22-changelog-attribute-details.md), which covers the `AdditionalDetails`
mechanism; the status-field and Event mechanisms remain proposals.

## Progress

Each fix gets its own branch off upstream `main` (`88c5c3776`), an implementation
pass and an independent review pass. No pull requests are opened. Branches are
pushed to `origin`; PR descriptions live outside the repo so they do not pollute
the diff they describe.

**Verification ceiling.** Nothing here has run against a live AWS account or a
cluster, and the Go build cache was flushed early in this work, so builds that
link `xpprovider` now cost ~30 minutes cold. Where a compile-level check was too
expensive to run, the fix says so rather than implying one passed. CI is expected
to provide it.

### 13 — single global rate limiter

Reviewed and ready. Two commits: the fix, plus a review correction making the
regression test match the shared-limiter reference by regex instead of gofmt's
current column padding, which would have failed misleadingly on an unrelated
field rename.

The review settled the question the analysis had not: **sharing one limiter is
correct**, and the previous code violated crossplane-runtime's own contract, not
just the flag's help text. `NewGlobal` returns a token-bucket limiter whose
`When` ignores the queue item and holds no per-item state; both scopes register
controllers on a single manager; and `Options.GlobalRateLimiter` is documented as
bounding reconciles across all of that manager's controllers.
`ratelimiter.LimitRESTConfig` was already applied once to that manager, so the
API-server budget was computed once while the reconcile budget was computed
twice.

`MaxConcurrentReconciles` was ruled deliberately out of scope: it is a
per-controller worker count, not a shared budget, so it was never doubled in the
way the limiter was.

Open for a human: CI must supply the compile check — the regression test parses
the generated mains rather than compiling them, and each is its own `package
main`. The throughput change needs a release note.

### 12 — identity cache race and lock scope

Ready, with one caveat a human must close. Three commits: the fix, and a review
correction reading the access time through a `lastAccess()` comma-ok accessor so
an entry built as a bare literal reports the zero time instead of panicking on
the hot path.

The lock-scope half turned out larger than the analysis described. The original
`defer c.mu.Unlock()` held the cache-wide write lock not only across the
`sts:GetCallerIdentity` call but across the trailing `newCredentials(...)`, which
performs a `Retrieve()` — so a second potential network call ran under the same
lock. Both are now outside it.

**Caveat: the race itself is not confirmed against the production types.**
`internal/clients` transitively imports terraform-provider-aws, so `-race`
rebuilds every dependency; two attempts exhausted the disk before linking. The
race was demonstrated in a dependency-free side module, but that scratch module
was deleted during disk recovery and the result can no longer be audited. What
*is* confirmed here: `go build` and `go test` (without `-race`) pass on the
package, and the regression test is well formed — 500 rounds of 32 goroutines
released together against a deliberately stale entry, so every goroutine takes
the refresh branch. CI running `-race` is what proves this change.

### 11 — cluster-wide Secret informer

Reviewed and ready, at `18fa5d097`. **The fix this repository's analysis proposed
was wrong, and was rejected on evidence.** All three selector options in
`11-scope-secret-informer.md` are unsafe: in controller-runtime v0.24.1
`client.Get` routes to the cache unless the type is in `DisableFor`
(`pkg/client/client.go:368-374`) and the cache reader returns `NewNotFound` for
an object absent from the informer store (`cache_reader.go:73-79`). There is no
live-read fallback, and the provider reads Secrets at arbitrary user-chosen
namespace/name. A selector would silently break credential resolution and wedge
connection publishing. Informer selectors are also fixed at creation, so the
"restrict to referenced namespaces" option is impossible outright.

What shipped instead: `--enable-secret-cache` defaults to `false`, keeping the
cache as an explicit opt-in.

The review priced the trade rather than assuming it. Added reads are one
`client.Get` per reconcile for `source: Secret` only — IRSA, Pod Identity and
Upbound read no Secret at all — giving 1.7 GET/s at 1,000 such MRs and 8.3 at
5,000, against etcd's ~10k reads/s. Against that: an informer over every Secret
in the cluster, ~80–120 MB of live objects per family pod for a mid-size
cluster, multiplied across ~15 pods, plus a full cluster-wide Secret LIST at
first credential read. Bounded and small versus unbounded and multiplied.

Two corrections came out of it. The claim that the new reads are bounded by
`ratelimiter.LimitRESTConfig` is **false** — that sets QPS 500 / burst 1000 per
pod at the default `--max-reconcile-rate`, roughly 60× the added load, so it
never engages; the real bound is the poll interval. And the flag has **never
shipped**: it landed in `b9e20fbfe` (2026-08-20), after `v2.7.0`
(`e5e520ffc7`, 2026-08-10), and is contained in no tag — verified independently.
So no existing configuration changes meaning; the release note is about
behaviour against v2.7.0, which cached Secrets implicitly by passing no client
options at all.

A client wrapper falling back to a live read on cache miss was considered and
rejected: it still requires a selector to get any hit at all, it introduces
stale reads on a write path (`APIPatchingApplicator.Apply` reads before
patching), it wins no RBAC narrowing, and it means hand-maintaining a
`client.Client` across 178 binaries to avoid single-digit GET/s.

### 01 — MoveToStatus shared-schema contamination (upjet)

Implemented and independently verified. Branch `fix-movetostatus-copy-before-mutate`
on `chlunde/upjet`, commit `9124f35`, based on current upstream `dbfccb4`.
(The branch name uses dashes because a pre-existing remote branch named `fix`
blocks the whole `fix/*` ref namespace on that fork.)

`copySchemaAtPath` walks the fieldpath copying every node it traverses — the
`*schema.Schema`, its `Elem` `*schema.Resource`, and a fresh `Schema` map — and
re-attaches the copies, so the path is exclusively owned before any flag is set.
The recursion then copies each nested subtree at every level it mutates. Copies
are shallow struct copies plus fresh maps; validators and diff funcs stay
shared, and untouched siblings keep their original pointers. The
`return` → `continue` bug is fixed in the same commit.

I re-ran the mutation test myself rather than take it on report. Applying the
leaf-only-copy mutant — the failure mode most likely to look correct while still
contaminating — produces:

```
rule.Elem still points at the shared nested resource; it must be copied before mutation
shared leaf schema mutated in place: got {Optional: false, Computed: true}, want {Optional: true, Computed: false}
shared nested schema field "tags" mutated in place: ...
```

That last line is the downstream symptom exactly. `go test ./pkg/config/...`,
`go build ./...`, `go vet`, `gofmt` all clean; upjet has no terraform-provider-aws
dependency, so unlike the provider fixes this one is genuinely tested rather than
parser-verified.

Left deliberately out of scope: `MarkAsRequired` and `ManipulateEveryField` in
the same file mutate shared schemas the same way.

## Kubernetes API calls are billed, not just capacity

Every request to the API server is an audit event. On EKS with control-plane
audit logging enabled, those events go to CloudWatch Logs and are billed on
ingest — so **a request being cheap for etcd does not make it free**, and a
write the API server discards as a no-op is still fully audited.

This re-prices two conclusions recorded elsewhere in this analysis.

**Fix 11's ruling was made against the wrong metric.** The review judged the
added Secret reads acceptable by comparing 1.7–8.3 GET/s against etcd's ~10k
reads/s. Under an audit-cost lens the relevant figure is events per day:

| MRs on `source: Secret` | added GETs | audit events/day |
| ---: | ---: | ---: |
| 1,000 | 1.7/s | ~145,000 |
| 5,000 | 8.3/s | ~717,000 |

At roughly 1–2 KB per read event that is order 0.2–1.5 GB/day of extra ingest
per cluster, aggregated across family pods. The flip is still the right call —
it removes an unbounded per-pod memory cost and a full cluster-wide Secret LIST
— but it is **not free**, and the follow-up the reviewer filed as optional now
matters: a bounded TTL cache in `internal/clients`, keyed on the ProviderConfig
secretRef, makes the flip cost-neutral. Treat that as part of the change rather
than a someday item.

**Correction (round-2 triage, R2): the added-read table above is incomplete, and
the TTL cache does not close the gap.** The table counts only the ProviderConfig
credential path. Two further read classes become live API GETs under the flip:

* every managed resource with a `SecretRef` parameter, read once per `Connect`
  via `resource.GetSensitiveParameters(ctx, &APISecretClient{kube: ...}, ...)`
  (`upjet/pkg/controller/external_tfpluginsdk.go:144,285`,
  `external_tfpluginfw.go:140,225`);
* the password generator's `Get`.

Both go through upjet's `APISecretClient`, constructed from `mgr.GetClient()`
inside the 178 generated controllers. A TTL cache in `internal/clients` keyed on
the ProviderConfig `secretRef` **cannot reach either** — it sits on a different
code path entirely. The flip is still the right call for the memory and the
cluster-wide LIST, but its audit-event cost is higher than the table states and
is not fully mitigable where the mitigation was proposed.

**The no-op status PUT is worth more than recorded.**
[`architecture-wins.md`](../architecture-wins.md) §5 correctly killed the *etcd*
half of that lead: the API server byte-compares and discards identical updates
before they reach etcd. But the PUT is still sent, still authenticated, still
authorised and **still audited**, once per managed resource per poll. Write
events are logged at a higher level than reads under typical policies and carry
the object, so they are the larger line item of the two. At 5,000 MRs that is
another ~717,000 audited writes per day that change nothing.

Suppressing it client-side is small and lives in crossplane-runtime. Under a
capacity lens it was marginal; under a cost lens it is one of the better
value-per-line changes in this analysis.

Neither figure has been measured against a real bill — they are request rates
measured here multiplied by public per-event assumptions, and the exact cost
depends on the cluster's audit policy level for reads versus writes.

### 14 — no-op status update (crossplane-runtime)

Added to this list *after* the original triage, because the audit-cost lens
re-prioritised it. [`architecture-wins.md`](../architecture-wins.md) §5 correctly
established that the API server discards the identical update before etcd — no
storage write, no watch event. What remains is the request: still sent,
authenticated, authorised, admitted and audited, once per managed resource per
poll. Roughly 720,000 audited no-op writes per day at 5,000 MRs on the default
interval.

Implemented on `chlunde/crossplane-runtime`, branch
`fix/suppress-noop-status-update`, commit `35d1fdc`, based on upstream `4e7ed23`.

The design errs toward writing. A deep copy is taken immediately after the
successful `Get`, and the comparison happens *inside* `updateStatus` **after**
`SetLastHandledReconcileAt` is applied — so a new reconcile-request token always
writes. The whole object is compared rather than just `.status`, meaning any
mutation not provably a no-op falls through to a write: comparison mistakes can
only produce redundant writes, never suppressed ones.

All 26 `updateStatus()` call sites were audited. The paused path
(`reconciler.go:989`) bypasses `updateStatus` by design and was left alone.

Verified here independently rather than on report: the diff is **238 insertions
and zero deletions**, so no existing assertion was weakened to make anything
pass. Forcing always-suppress fails three tests —
`ChangedConditionIssuesStatusUpdate`, `NewErrorConditionIssuesStatusUpdate` and
`NewReconcileRequestTokenIssuesStatusUpdate` — with `want status update, got
none`, which is exactly the stale-status regression this change must not cause.
Full `go test ./...` green.

One judgement for a reviewer: `equality.Semantic` treats Quantity `1` and
`1000m` as equal, so a status field changing only in formatting would be
suppressed. Semantically identical, but `reflect.DeepEqual` is the stricter
alternative if maintainers prefer it.

### 02 — clear `SchemaFunc` (upjet)

Implemented and verified. `chlunde/upjet`, branch
`fix-clear-schemafunc-after-materialise`, commit `786ec33`, on upstream
`dbfccb4`. Two lines of code plus a comment, and a test.

Verified here rather than on report: replacing the assignment with a no-op makes
the new test fail on three separate assertions, the last being the SDK's own
error —

```
Resource.SchemaFunc was not cleared after its schema was materialized into Resource.Schema
mutation of Resource.Schema is not visible through Resource.SchemaMap; ...
Resource.InternalValidate failed: SchemaFunc and Schema should not both be set
```

`pkg/schema/traverser/traverse.go` is the only other reader of `SchemaFunc`, and
it consults `Schema` first, so clearing the func cannot affect it. Full upjet
suite green.

### 04 — missing secret key (upjet) — **narrowed from the original scope**

Implemented as `fix-error-on-missing-secret-key` @ `32e9967`, but **half the fix
described in `04-missing-secret-key.md` was dropped, deliberately.**

What shipped: `GetSecretValue` returns an error when the named key is absent,
instead of `(nil, nil)`.

What was dropped: the doc also proposed leaving the parameter *unset* when the
referenced secret does not exist, rather than substituting `""`. The
implementation did that, and it was reverted here on review. Four lines below
that call site, the list-of-selectors branch documents the opposite as
intentional:

```go
// If referenced k8s secret is deleted before the MR, we pass empty string for the sensitive
// field to be able to destroy the resource.
```

Leaving the parameter unset would risk wedging deletion for a managed resource
whose secret was removed first — the precise scenario that comment exists for.
The original analysis proposed that change without reconciling it against the
adjacent documented decision.

The narrowed patch is also the more merge-likely one: erroring on a key that
isn't there is unarguable, while reversing a documented design choice invites a
debate that would sink the whole PR.

### 07 — field-path camel→snake (upjet)

Implemented and verified. `fix-fieldpath-segmentwise-camel-snake` @ `046b8f2`.
A `convertFieldPathToSnake` helper parses with `fieldpath.Parse`, converts each
`SegmentField`, and rejoins — so separators and indices survive. Four call sites
use it.

Two corrections to `07-fieldpath-camel-snake.md`, both from actually reproducing
the bug rather than trusting the write-up:

* The digit example was understated. `ipv6_addresses` becomes
  `ipv_6___addresses` — triple underscore, not the `ipv_6_addresses` the doc
  claimed. (`ipv_6_addresses` is what the camel input `ipv6Addresses` produces.)
* **The digit case is a different bug and is not fixed here.**
  `name.NewFromCamel("ipv6Addresses").Snake` returns `ipv_6_addresses`, not
  `ipv6_addresses` — a lossy acronym round-trip inside `pkg/types/name` that
  this change deliberately leaves alone. What this fixes is separator and index
  handling; the doc conflated the two.

Severity lowered from high to medium. The transform is provably wrong, but the
agent could not establish that any provider currently registers a nested or
digit-bearing path through this path, and top-level digit-free names — likely
the common case today — were never affected. The writer side does store expanded
**indexed** camelCase paths as annotation keys, so the plumbing points straight
at it, but that is a hazard rather than a demonstrated failure. The PR says so
plainly rather than inflating it.

Verified here: reverting the helper to whole-path conversion fails six subtests
with diffs like `-"foo_bar[0].baz_qux" +"foo_bar_[_0_]._baz_qux"`, and the
end-to-end merge produces `{"foo_bar_": {"_0_": {"_baz_qux": ...}}}`. Full upjet
suite green. No existing tests covered these functions, so none were changed.

### 08 — cache credentials per ProviderConfig

Ready at `d3d61421a1`. Credential resolution was per managed resource when the
result depends only on the ProviderConfig and region; caching collapses N
managed resources into one resolution per TTL. On a hit there is no Kubernetes
read and no STS call.

**Review halved it.** The first implementation was 325+/171− with 11 new
functions and types, against a brief asking for a simple, obvious patch. Cut to
6 new functions and net +85 lines of production code, with the entire
`provider_config.go` diff removed — it changed the IRSA and WebIdentity paths in
service of fix 03, which this change does not fix.

What survived scrutiny, on evidence: `singleflight` stays, because the
alternative that also prevents concurrent callers each doing the slow work is a
lock held across resolution — which is precisely the fix-12 bug, and removing
`singleflight` was mutation-tested to produce 32 Secret reads and 32 account-ID
resolutions instead of one. Invalidate-on-`Retrieve`-failure stays: it is what
keeps the rotation window honest. The IRSA no-expiry special case stays because
the brief forbade changing behaviour that already worked.

**Review also found a regression in the branch as first pushed.** It keyed on
the raw `spec.forProvider.region` and applied the global-API-group default
afterwards, so `sts:GetCallerIdentity` ran with an empty region for every global
group — `iam`, `route53`, `cloudfront`, `organizations`. Under `source: Secret`
there is no IRSA webhook supplying `AWS_REGION`, so that is a `MissingRegion`
failure on every IAM and Route53 reconcile, not a corner case.

That fix was reasoned but **uncovered**: re-introducing the bug left the whole
suite green. Resolution was extracted into `effectiveRegion` and covered
(`aws_region_test.go`) across the regional, global cluster-scoped, global
namespaced, explicitly overridden and non-global cases; the bug now fails two of
them. `go build`, `go vet`, `go test` and `gofmt` all pass; nine other mutants
were killed by the branch's own suite.

**The trade a human must accept:** a 5-minute TTL is the upper bound on how long
a rotated credential Secret goes unnoticed, because rotating a Secret does not
change the ProviderConfig's generation, so nothing but the TTL can invalidate
the entry. The TTL is deliberately below the 15-minute minimum STS session
duration so a cached provider is re-resolved before the SDK would refresh it
from a possibly-cancelled reconcile context.

## Correction: fix 14 does not take steady-state writes to zero

A second round of lead-hunting found a guaranteed **spec** write that the
earlier steady-state audit missed, so the claim recorded above — that
suppressing the no-op status update leaves zero Kubernetes writes per reconcile
— is wrong. It takes them from two to one.

`Tagger.Initialize` (upjet `pkg/config/resource.go:351`) sets the three
Crossplane external tags into `spec.forProvider` and then calls
`t.kube.Update(ctx, mg)` **unconditionally**, with no comparison against what is
already there. The values it writes are derived from the managed resource's own
kind, name and provider, so after the first reconcile the object is byte
identical and the API server discards the update — but the request is still
sent, admitted and audited, exactly like the status write.

`r.managed.Initialize(ctx, managed)` runs on every reconcile
(crossplane-runtime `pkg/reconciler/managed/reconciler.go:1115`), and
provider-upjet-aws applies `AddExternalTagsField()` as a default resource
option, so this affects every taggable resource — around 495 of them.

Verified by reading both call sites; not measured. Under the audit-cost lens
this is now the highest value-per-line item outstanding, and it is the same
shape of fix as 14: compare before writing.

## Second lead round

`scratchpad/leads-round2.md` (outside the repo) holds 20 new leads across all
three repositories, none overlapping the closed L1–L30 set or the 14 fixes.
Three were spot-checked here:

* **R1** — the `Tagger.Initialize` write above. Confirmed at both call sites.
* **R7** — `config/externalname.go:1894` templates the external name for
  `aws_lightsail_domain_entry` with `{{ .parameeters.target }}`. A typo, in a
  live template, that also demonstrates nothing exercises these templates in
  tests. Confirmed by inspection.
* **R12** — `conditionalFilter` (upjet `pkg/resource/lateinit.go:202`) applies
  `name.NewFromCamel(cName).Snake` to what `fieldpath.GetValue` then treats as a
  path. This is the same defect class as fix 07 but in a different file, so fix
  07 is not wrong — it was scoped to its four call sites and this instance was
  never in scope. Whether it bites depends on whether callers pass dotted paths;
  unverified.

The generator's own view was that the least-examined ground is the hand-written
per-service `config/cluster/*/config.go` custom diffs and external-name
templates: one sweep of the 67 templated identifiers surfaced two breakages
against upstream plus the typo above.

## Suggested order

1. **02, 12, 13** — very small, self-contained, each independently verifiable.
   Do these first to build confidence in the analysis.
2. **01, 04, 05, 06** — the correctness and corruption fixes. 01 is the most
   valuable single change in the whole analysis and the one most in need of an
   e2e test.
3. **03, 08** — the credential lifecycle. Related; 08's cache is a prerequisite
   for doing 03 well, so plan them together.
4. **09, 10, 11** — cost and exposure. Largest wins per line of code, no
   behaviour change intended.
5. **07** — latent. Worth a reproducer first to decide whether it deserves a
   release or a routine fix.

Fixes 01, 04, 05, 07 live in [upjet](https://github.com/crossplane/upjet) and
land here only on the next dependency bump. 02 can be worked around in this repo
in the meantime; the others cannot.
