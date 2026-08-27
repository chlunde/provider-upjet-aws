<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# Structural wins

Where the *shape* of the provider's work can change, ranked. This document
completes the series: [`memory-footprint.md`](memory-footprint.md) covered
where resident memory goes, [`reconcile-workflow.md`](reconcile-workflow.md)
and [`reconcile-workflow-detail.md`](reconcile-workflow-detail.md) the defects
on the reconcile path. Here the question is different: of everything the
provider does per poll cycle and per process, what is structurally necessary,
and what is not.

Measured with `hack/memprofile/{startup,reconcile}` (sections P1–P3, 8 and 9
are new in this revision) against this tree. Labels as before: **measured**
(harness output), **read** (source, with citations), **inferred** (follows
mechanically, needs a cluster or an AWS account to confirm).

## Ranked summary

| # | candidate | size of win | effort | where | evidence |
| - | --- | --- | --- | --- | --- |
| 1 | Build each scope's `config.Provider` only when that scope is used, and skip the runtime-dead schema/metadata parse | 7.6 s of the 27 s startup + ~190 MB of RSS growth for the unused scope; ~1 s + ~100 MB garbage per build for the parse | medium | this repo (gating), upjet (parse option) | measured |
| 2 | Change-driven / batched observation instead of one TF refresh per MR per 10 min | AWS read volume ÷ (poll ÷ scan interval); ~100 resources per tagging-API call vs ≥1 call per resource | high | this repo (optional controller); no upjet change needed | read + arithmetic |
| 3 | Scope the cluster-wide Secret informer | unbounded today: cache grows with *every* Secret in the cluster, per family pod | small | this repo | read |
| 4 | Short-circuit the post-read pipeline when the refreshed state is unchanged | ~60–90 % of steady-state reconcile garbage: 4.2 ms / 1.6 MB per `aws_instance` reconcile is diff+conversion of an unchanged object | medium | upjet | measured |
| 5 | Suppress the no-op status PUT client-side | 1 full-object PUT per MR per poll of API-server CPU/network — but **not** etcd writes (lead killed, see §5) | small | crossplane-runtime | measured + read |
| 6 | Stop deep-copying every MR every 5 s for state metrics | 0.3–1.6 MB/s at 1k–5k MRs; 2 goroutines/kind | small | crossplane-runtime | measured |

Killed leads, briefly: status-write **etcd** amplification (§5 — the API
server byte-compares and skips the write), conversion-webhook cost on the hot
path (§7 — storage version = controller version), the typed-MR JSON round
trips (§4 — µs range), and per-reconcile CPU as a fleet bottleneck (§4 — it is
an allocation problem, not a CPU problem).

## 1. The namespaced provider build is pure overhead until a namespaced MR exists

**Measured.** This run of `hack/memprofile/startup` (same tree, warmer
figures than the earlier doc, ratios identical):

```
5. config.GetProvider (cluster)      live=47.7  RSS= 838.2  took=19.273s
6. config.GetProviderNamespaced      live=51.5  RSS=1031.3  took= 7.636s

P1. tfjson unmarshal of schema.json (18.1 MB)   took=740ms   (+52 MB live)
P2. GetV2ResourceMap (1683 resources)           took= 97ms   (+49 MB live)
P3. registry metadata parse (1676 resources)    took=219ms   (+ 6 MB live)
```

The new P-section times the phases of `config.NewProvider` that are *identical*
across the two scoped builds, in isolation. Three conclusions:

* **Only ~1.06 s of each build is the shareable parsing** (P1+P2+P3). The
  remaining ~6.5 s of the second build is genuinely per-scope: 1,029
  `DefaultResource` constructions, configurator chains and schema traversals
  over resources whose `config.Resource` values really do differ per scope
  (root group `aws.upbound.io` vs `aws.m.upbound.io`, references, singleton-
  list version bumps — `config/registry_cluster.go:69` vs
  `config/registry_namespaced.go:27`, which differ in exactly those regards).
  So "share the parse between scopes" is worth ~1 s, not ~7 s. The way to
  kill the second build is not to share it but **not to run it**.
* **The parse is runtime-dead anyway.** `GetV2ResourceMap` converts all 1,683
  JSON schemas to plugin-SDK form (+49 MB), and for all 960 SDK-reconciled
  resources `NewProvider` immediately overwrites that with the Go schema
  (`upjet/pkg/config/provider.go:436`); the metadata (+6 MB, 219 ms) is
  released again by this repo's `dropCodegenOnlyMetadata`. An upjet runtime
  option to skip both saves ~1 s and ~107 MB of allocations, twice.
* **RSS grows ~190 MB during the namespaced build** (838 → 1031 MB), and a
  cluster that uses only cluster-scoped MRs — today, essentially all of them —
  pays it for nothing, along with 1,029 extra controllers' worth of setup.

**The mechanism for not running it**: `cmd/provider/<family>/zz_main.go:222-227`
builds both providers unconditionally, before the manager starts. But the
safe-start gate (`customresourcesgate`, already wired) knows which CRDs exist:
`SetupGated_<family>` callbacks fire per-GVK. Deferring each scope's
`config.GetProvider*` call into a `sync.OnceValue` that the first gated setup
of that scope forces would mean: no namespaced CRDs → the build never runs.
The controllers' `tjcontroller.Options` would carry the lazy handle instead of
the built `*config.Provider`. The webhook conversion registry
(`zz_main.go:320`, `conversion.RegisterConversions`) needs both providers'
resource configs; it would need the same laziness or a generated static
equivalent — that is the main blast-radius item to check.

Combined with the per-family include list from `memory-footprint.md` §3 (which
attacks the 19.3 s cluster build the same way), startup approaches the ~1 s
that the non-config work measures, and the startup arena — the half of RSS a
pod's working-set metric counts in full — shrinks proportionally.

**Fixable in**: this repo (lazy/gated builds); upjet (skip-parse option,
small). Confidence high — all numbers measured; the gate plumbing is read.

## 2. The polling model: what can actually replace per-MR refresh

The steady-state shape today, per MR per `--poll` (default 10 m): one
`RefreshWithoutUpgrade` → one or more AWS describe calls, plus the finding-3/4
overheads (client rebuild, STS for non-IRSA), plus §5's status PUT. Nothing
batches: `ExternalClient.Observe` receives one MR (read,
`external_tfpluginsdk.go:490`), and nothing in upjet coalesces reads across
MRs of a kind.

Two facts make a change-driven alternative practical *without touching upjet
or crossplane-runtime*:

* **The runtime already has the hooks.** `crossplane.io/poll-interval` is a
  per-MR override honoured by `effectivePollInterval`
  (crossplane-runtime `reconciler.go:867`, `meta.go:70`), and
  `crossplane.io/reconcile-requested-at` both triggers the watch (annotation
  changes pass `DesiredStateChanged`, `predicates.go:55`) and is handled
  explicitly by the reconciler (`reconciler.go:986`). So "poll rarely, poke on
  change" is expressible today by annotating MRs.
* **Every taggable resource already carries its MR identity in AWS.**
  `AddExternalTagsField` (`config/overrides.go:158`) runs `config.TagInitializer`
  on every resource with a `tags` map, stamping `crossplane-kind`,
  `crossplane-name` and `crossplane-providerconfig`
  (crossplane-runtime `resource.go:410`). The Resource Groups Tagging API
  (`GetResources`, 100 resources per page, one API endpoint per region) can
  therefore enumerate **all crossplane-managed taggable resources with their
  MR identity and current tags** at ~1 call per 100 resources — against ≥1
  call per resource today.

What that supports, honestly assessed:

| signal | detectable by a tagging-API sweep | needs per-resource describe |
| --- | --- | --- |
| resource deleted behind the provider's back | yes (ARN disappears) | — |
| tag drift | yes (tags come back in the sweep) | — |
| existence of unmanaged/orphaned tagged resources | yes | — |
| non-tag attribute drift | no | yes |
| untaggable resource types | no | yes |

So the realistic structure is a **drift sentinel**: an optional controller (in
this repo, or a standalone deployment) that sweeps the tagging API (and/or
consumes CloudTrail-via-EventBridge for mutation events, which carry ARNs) and
pokes `reconcile-requested-at` on the mapped MRs, while users raise
`crossplane.io/poll-interval` to hours on high-cardinality kinds. Full
attribute-drift coverage keeps the poll as a slow backstop; the sweep converts
the *common* cases (deletion, tag drift, "nothing happened") from N calls to
N/100. EventBridge additionally converts change latency from poll-interval to
seconds.

**Inferred** sizing at 5,000 taggable MRs, 10 m poll → 24 h poll + 10 m sweep:
steady-state AWS describes drop from ~8.3/s to ~0.14/s of sweep calls plus
change-driven reads; STS traffic (finding 4) drops with it. Confirmation needs
a live account. Blast radius: none on the provider itself — the sentinel is
additive; the failure mode is "sentinel down → back to (long) poll", which
must be documented. Effort: high (ARN→MR mapping per service, EventBridge
plumbing), but incremental per family.

**Fixable in**: this repo, as an optional component. A first-class version —
`ExternalClient` growing a batch/watch interface — is a Crossplane-level
change and not needed for the above.

## 3. The Secret informer is cluster-wide and unbounded

**Read.** With the default `--enable-secret-cache=true`
(`zz_main.go:99,169-176`), every Secret *read* goes through the manager's
cached client — ProviderConfig credentials, `spec.forProvider` secret refs
(`GetSensitiveParameters`, `external_tfpluginsdk.go:285`), and the connection-
secret publisher's read-before-write (`APIPatchingApplicator.Apply` does a Get
first, crossplane-runtime `api.go:65`). The first such read makes
controller-runtime start an informer for `v1.Secret` — for **all Secrets in
all namespaces**, with no field or label selector (`cache.Options` at
`zz_main.go:183-195` configures only CRDs). From then on the pod holds every
Secret in the cluster in memory — Helm release blobs, cert-manager material,
everything — and receives every Secret update in the cluster, per family pod.

The knob that exists (`DisableFor`, when `--enable-secret-cache=false`) trades
this for a live GET per secret-read per reconcile — strictly better for
API-server-friendly deployments with many unrelated Secrets, strictly worse
for a dedicated cluster; neither is right as a global default.

The structural fix is scoping, not disabling: controller-runtime supports
per-object selectors (`cache.ByObject[&corev1.Secret{}].Label`). A
provider-recognised label on credential and connection secrets (documented,
and stamped automatically on the connection secrets the provider itself
writes) bounds the cache to relevant Secrets. Blast radius: a selector-scoped
informer makes *unlabelled* secrets invisible to the cached client — reads of
them must fall through to live GETs (`DisableFor` + a dedicated labelled
informer, or the `client.Options.Cache.Unstructured` split), so the change
needs care that a missed label degrades to API-server load, not to
"secret not found".

**Not measured** (needs a populated cluster); the mechanism and the
unboundedness are read. Effort small; fixable in this repo.

## 4. The per-reconcile pipeline: an allocation problem, not a CPU problem

New harness section 8 measures the steady-state translation work with
`SchemaFunc` cleared — i.e. what *remains* after the schema-rebuild fix from
`reconcile-workflow.md` finding 2 — and section 9 the typed-MR side
(**measured**, AWS read excluded):

| step, per reconcile | `aws_iam_role` | `aws_instance` |
| --- | ---: | ---: |
| Connect: params→cty | 55 µs / 52 KB | 221 µs / 178 KB |
| Observe: state→cty→JSON map | 56 µs / 19 KB | 222 µs / 77 KB |
| Observe: `InstanceDiff` | 593 µs / 220 KB | 3.79 ms / 1.38 MB |
| Observe: late-init marshal | 7 µs / 1 KB | 11 µs / 4 KB |
| **total** | **711 µs / 293 KB** | **4.25 ms / 1.64 MB** |
| typed MR: SetObservation+LateInitialize+GetObservation+GetParameters | 45 µs / 10 KB | 121 µs / 27 KB |

(The residual diffs in the harness are non-empty only because the synthetic
state omits computed `.#` count keys a real refresh returns; the cost is
representative.)

Two verdicts fall out:

* **CPU: killed.** At 1,000 MRs on a 10-minute poll (1.7 reconciles/s), even
  the `aws_instance` figure is ~7 ms/s of CPU. The shim never shows up as a
  CPU bottleneck at any realistic scale. The typed-MR JSON round trips —
  a prior suspect — are microseconds; also killed.
* **Allocation: alive.** The same arithmetic gives ~2.8 MB/s
  (`aws_instance`-sized) of steady-state garbage from translation, *on top
  of* finding 3's 2.0 MB per reconcile (~3.4 MB/s) from client rebuilds. This
  ~6 MB/s of steady garbage is what keeps the startup-sized arena
  (`memory-footprint.md`) from ever shrinking, and it is the poll-rate-
  proportional half of the working-set metric.

**The fast path** (needs upjet): the diff dominates (89 % of the translation
cost for `aws_instance`), and its inputs in steady state are (a) the refreshed
`InstanceState.Attributes` — a flat `map[string]string` — and (b) the params
derived from spec. The operation tracker already persists per-MR state across
reconciles (`nofork_store.go`). Caching, per tracker: the previous attributes
map plus a fingerprint of (metadata.generation, external-name annotation,
ProviderConfig generation), and on a cycle where both are unchanged, skipping
the diff, the singleton-list conversions, `SetObservation` and
late-initialisation — returning the cached `ResourceUpToDate: true`
observation. Attributes-map equality is cheap (string compares over ~40
entries) against 1.4 MB of avoided diff allocation. Blast radius: the skip
must not swallow the paths that mutate the MR even when unchanged
(`Available()` condition, TTR metric), must include everything params can
depend on in the fingerprint (secret-ref contents are the awkward one — their
resourceVersions would need to join the fingerprint), and must be bypassed
while an async operation is running or a diff is pending. Effort medium.
`Connect`-side costs (params→cty, and findings 3/4) are untouched by this and
need their own caches, as already argued in `reconcile-workflow.md`.

## 5. Status write-back amplification: the etcd half is dead, the request half is real

The suspicion was that every poll writes status even when nothing changed,
making the provider the dominant etcd load. Traced end to end:

* **The PUT happens.** Every steady-state reconcile ends with
  `r.client.Status().Update` — the up-to-date branch returns
  `errors.Wrap(updateStatus(), ...)` unconditionally (crossplane-runtime
  `reconciler.go:1496-1517`, `updateStatus` at `:998-1005`). Nothing
  client-side compares old and new status. **Read.**
* **The content is stable when AWS is stable.** Conditions ignore
  `LastTransitionTime` on comparison and are not re-stamped when equal
  (crossplane apis `condition.go:114`, `SetConditions` `:193`);
  `status.atProvider` is rewritten from the refreshed state each cycle
  (`external_tfpluginsdk.go:593`) but is byte-identical when the state is;
  observedGeneration moves only with the spec. **Read.**
* **The API server discards the no-op.** `GuaranteedUpdate` short-circuits
  before the etcd transaction when the encoded object is unchanged —
  `if !origState.stale && bytes.Equal(data, origState.data)` returns without a
  write, so no etcd I/O, no resourceVersion bump, no watch event (upstream
  `k8s.io/apiserver` etcd3 `store.go:539`, verified in release-1.34; not a
  dependency of this repo). Since ~1.24 managedFields timestamps are also not
  bumped on no-ops, so the equality actually holds. **Read (upstream).**

So the etcd-amplification theory is **killed**: at N MRs the provider is not
writing N objects per poll interval to etcd. What remains is the request
itself: a full-object PUT (serialize, send, API-server decode + admission +
encode + compare) per MR per poll. Measured serialized sizes for the sparse
harness objects are 864 B (`iam_role`) and 1.2 KB (`instance`); real MRs with
managedFields, conditions and full `atProvider` run 2–20 KB. At 5,000 MRs on
a 10-minute poll: 8.3 full-object PUTs/s sustained against the API server,
invisible in the provider's own metrics — exactly as the lead suspected, but
one layer up from etcd. There is also one cached Secret GET per poll per
connection-secret-owning MR (the publisher's read-before-write,
`api.go:100-128`; the write itself is correctly suppressed by
`AllowUpdateIf` when data is unchanged).

The candidate: in crossplane-runtime, snapshot status after Observe and skip
`updateStatus()` when nothing moved (conditions equal, atProvider deep-equal,
no pending reconcile-request token). Small, but it belongs upstream; every
provider inherits it. Rank medium-low: real load, well-bounded fix, but the
API server was built to absorb this shape of traffic.

## 6. The 5-second state-metrics poller

New in this analysis. Every controller `Setup` registers an
`MRStateRecorder` (`internal/controller/cluster/ec2/instance/zz_controller.go:96-102`)
that lists all MRs of its kind through the cached client **every 5 seconds**
(`--poll-state-metric` default, `zz_main.go:88`; crossplane-runtime
`mr_state_metrics.go:104,135`) to set three gauges. The cached List
deep-copies every object (controller-runtime `cache_reader.go:176`).
**Measured** per-object DeepCopy is small (4 µs / 4 KB for the sparse
instance), so the churn is 0.3 MB/s at 1,000 MRs and 1.6 MB/s at 5,000 —
real, continuous, poll-interval-independent garbage, but an order below §4's
reconcile-path garbage at equal populations. Add one goroutine + ticker per
kind per scope (up to ~2×104 for an ec2 family with everything activated).

Cheap fixes, all in crossplane-runtime: pass `UnsafeDisableDeepCopy` for this
read-only count, or replace polling with informer-event-driven counters, or
lengthen the default. Worth doing; not a headline. **Measured/read.**

## 7. Codegen and API surface: mostly cleared

* **Conversion webhooks are off the hot path.** 311 of 2,065 CRDs carry two
  versions and 5 carry three (measured over `package/crds`); the package
  manager injects webhook conversion (`package/kustomize/webhook.yaml`). But
  the storage version equals the version the controller reconciles
  (`storage: true` on v1beta2 in `ec2.aws.upbound.io_instances.yaml:6150`,
  set by `configureSingletonListAPIConverters`,
  `config/registry_cluster.go:185`), so the per-poll status PUT of §5 needs
  **no conversion call**. Conversion fires only for clients reading/writing
  the old version. Lead killed for steady state.
* **The doubled surface is the namespaced scope.** 1,033 of the 2,065 CRDs
  are the `.m.` namespaced twins (measured), and `apis/namespaced` is 55 MB of
  generated source against `apis/cluster`'s 84 MB. This is a Crossplane-v2
  design decision (separate groups per scope), and the per-binary cost is
  §1 plus the doc'd link cost — the leverage is §1's laziness and the
  memory doc's per-family builds, not fighting the duplication itself.
* **`--sync` resyncs are swallowed.** The 1 h `SyncPeriod` (`zz_main.go:86,184`)
  replays informer stores as no-change Update events, and every controller
  filters events through `DesiredStateChanged()` (`zz_controller.go:111`) —
  equal generation, labels and annotations, so resync events are dropped
  before the reconciler. The advertised "double check for drift" rides
  entirely on the `RequeueAfter` chain; the flag buys periodic event-handler
  churn and nothing else. **Read; inferred** that no reconcile results (a
  cluster would confirm via metrics). Harmless today, but it means the resync
  safety net providers assume they have does not exist here.
* Two `GlobalRateLimiter`s are built — one per scope (`zz_main.go:234,257`) —
  so `--max-reconcile-rate` is effectively doubled once namespaced MRs are in
  use. One-line fix. **Read.**

## Order of work

1. **Gate each scope's `config.Provider` build on first use** (§1) — in-repo,
   measured 7.6 s + ~190 MB for the scope nobody uses yet; pairs with the
   memory doc's per-family include list.
2. **Scope or re-default the Secret cache** (§3) — in-repo, small, removes an
   unbounded term from every family pod.
3. **Upjet: skip the runtime-dead parse** (§1) and **add the unchanged-state
   fast path** (§4) — together they remove most of both the startup arena and
   the steady-state garbage that sustains it.
4. **Prototype the tagging-API drift sentinel** (§2) for one high-cardinality
   family — additive, and the only candidate that changes the AWS call volume
   itself.
5. Upstream small fixes: client-side no-op status suppression (§5), state-
   metrics deep-copy/polling (§6), single global rate limiter (§7).
