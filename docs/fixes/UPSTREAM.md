<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# Handover: what to propose, where, and in what order

Nineteen branches across three forks (one, fix 23, retracted after measurement — see its file). No pull request has been opened anywhere.
Full write-up for each is the numbered file in this directory; this page is just
the shape of the thing.

**Verification status: nothing here has run against a live AWS account. That is
no longer true of clusters** — everything in the *cluster-measured* section below
was measured on a running provider pod in kind against LocalStack
([`docs/cluster-measurement.md`](../cluster-measurement.md),
[`hack/clustermeasure`](../../hack/clustermeasure/README.md)), and fixes 02, 06,
09 and 11 now have cluster evidence noted against them.** Every branch has unit tests, and every fix was
mutation-tested — the production change reverted, the tests confirmed to fail,
then restored — so the tests are known to bite. That is the ceiling. Three items
below name a specific thing worth checking against a real account.

## crossplane-runtime

| fix | branch @ sha | what |
| --- | --- | --- |
| [14](README.md) | `fix/suppress-noop-status-update` @ `35d1fdc` | Suppress the no-op status PUT on every reconcile |

## upjet

Ordered by what I would send first.

| fix | branch @ sha | what | severity |
| --- | --- | --- | --- |
| [01](01-movetostatus-shared-schema.md) | `fix-movetostatus-copy-before-mutate` @ `9124f35` | `MoveToStatus` mutates schema singletons shared across resources | **corruption, critical** |
| [02](02-clear-schemafunc.md) | `fix-clear-schemafunc-after-materialise` @ `786ec33` | Clear `SchemaFunc` after materialising `Schema` | 1 line |
| [16](16-tagger-noop-spec-update.md) | `fix-tagger-skip-noop-spec-update` @ `43f8c2d` | `Tagger` writes the spec on every reconcile | waste + a resource-version conflict logged on every create |
| [04](04-missing-secret-key.md) | `fix-error-on-missing-secret-key` @ `32e9967` | A missing secret key silently becomes `""` | corruption |
| [17](17-update-connection-details.md) | `fix-update-connection-details` @ `edfc8db` | `Update` returns no connection details | correctness, low |
| [18](18-framework-replace-messages.md) | `fix-error-message-defects` @ `95385db` | Three misleading messages on the replace path | observability |
| [22](22-changelog-attribute-details.md) | `feat-changelog-attribute-details` @ `e337a43` | Populate change-log `AdditionalDetails` with the changed attribute set | observability, feature |
| [07](07-fieldpath-camel-snake.md) | `fix-fieldpath-segmentwise-camel-snake` @ `046b8f2` | camel→snake mangles nested paths (annotations) | corruption, latent |
| [20](20-path-string-surgery.md) | `fix-path-string-surgery` @ `97467d6` | Same class, two more sites (inject keys, late-init filter) | correctness, latent |

Also on the fork: **`fix` @ `a9c2cc9`**, a 2025 commit of your own —
*"Avoid setting tags if there are no changes, prevent conflict on every resource
creation"* — which fixes the same defect as 16 by a tidier route and never landed
upstream. It carries the field report (`the object has been modified; please apply
your changes to the latest version`) that makes the case better than the
cost argument does. Consider reviving it instead of 16's implementation, with
16's tests on top. Note this branch name blocks the whole `fix/*` namespace on the
fork, which is why every upjet branch here uses dashes.

## provider-upjet-aws

| fix | branch @ sha | what | severity |
| --- | --- | --- | --- |
| [15](15-wafv2-rule-group-external-name.md) | `fix-wafv2-rule-group-association-external-name` @ `11facaf` | `WebACLRuleGroupAssociation` never records its external name and re-creates on every retry | **data loss, critical** |
| [19](19-external-name-template-defects.md) | `fix-external-name-template-defects` @ `453072d` | Two malformed external-name templates + a schema-backed guard for the class | correctness, high |
| [06](06-dynamic-endpoint-ignored.md) | `fix/dynamic-endpoint-for-tf-client` @ `29aa0a4` | `endpoint.url.type: Dynamic` silently ignored; all CRUD goes to public AWS | correctness, high |
| [08](08-credentials-cache-all-sources.md) | `fix/cache-credentials-for-all-sources` @ `d3d6142` | One STS call per reconcile for every non-IRSA source | useless API calls |
| [21](21-assume-role-session-duration.md) | `fix/refreshing-credentials-for-async-ops` @ `8f2462c` | Assume-role chains get 15-min sessions against a 1-hour async deadline | data loss (mitigation only) |
| [11](11-scope-secret-informer.md) | `fix/scope-secret-informer` @ `18fa5d0` | Cluster-wide unbounded Secret informer | security + memory |
| [12](12-caller-identity-cache.md) | `fix/identity-cache-race-and-lock-scope` @ `efb86ce` | Data race and STS-under-lock | correctness |
| [13](13-double-rate-limiter.md) | `fix/single-global-rate-limiter` @ `2de61d7` | `--max-reconcile-rate` delivers double what it says | 1 line |

## Cluster-measured items

Everything above predates the in-cluster harness. These were measured on a real
provider pod — `docs/cluster-measurement.md` has the arms and the caveats.
Ordered by (impact ÷ effort) within each target.

**Likelihood** is my read of how a maintainer receives it: *high* = a pure
optimisation or a one-line default with no API or maintenance cost; *medium* =
needs a design conversation or carries a maintenance tax; *low* = asks someone to
accept a permanent cost for someone else's measurement.

### upjet

| what | impact | complexity | likelihood |
| --- | --- | --- | --- |
| Precompile the include/skip regexes in `matches()` ([patch](../../hack/clustermeasure/upjet-measurement.patch)) | 575 MB of allocation, 32% of the total; config build 688 ms → 250 ms (2.7×), and 12.96 s → 7.48 s unfiltered. **No memory change.** | ~20 lines, one file, identical semantics | **high** |
| Filter the resource map before `GetV2ResourceMap` converts it | **−91.5 MiB of startup peak** — the peak sets the pod's memory limit | ~15 lines, reorders `NewProvider`; changes `GetSkippedResourceNames` semantics (skipped names never enter the map) | **high**, with a note about the semantics |
| Clear `SchemaFunc` after materialising `Schema` (fix 02) | correctness — `config/` schema edits are otherwise invisible to every path except the diff. **No memory or CPU effect** (measured; do not pitch it as one) | 1 line, branched | high |
| Don't parse `schema.json`/`provider-metadata.yaml` for a non-generation provider | would remove the parse entirely | large — ~368 configurators index `MetaResource` and panic without it; needs the configurator chain split into generation and runtime halves | low |

### aws-sdk-go-base

| what | impact | complexity | likelihood |
| --- | --- | --- | --- |
| Guard the request/response decomposition on the logger's level | **−24% of provider CPU** at 50 and 500 MRs alike. `HandleDeserialize` decomposes every request — reading the whole body — then hands it to `logger.Debug`, which discards it. No level check exists (`logger.go:113`) | small, one middleware | medium-high |

### aws-sdk-go-v2

| what | impact | complexity | likelihood |
| --- | --- | --- | --- |
| Share the partition region regexes ([analysis](https://github.com/chlunde/notes/tree/main/aws-sdk-go-v2/endpoint-partition-regexes)) | **−8.6 MiB on the pod**, 2,152 compiled regexes → 8, byte-identical in 60 of 60 packages sampled | codegen only, and `internal/endpoints/v2` is already imported by every service — but it is a **separately versioned module**, so this bumps a minimum version across ~270 independently released service modules | medium-low — expect the module-versioning objection first |

### terraform-provider-aws (the Upbound fork)

| what | impact | complexity | likelihood |
| --- | --- | --- | --- |
| Per-family linking: tag-gate `service_packages_gen.go` **and** `internal/conns/awsclient_gen.go` | **−26.4 MiB steady, −121 MiB resident text, binary 980 MB → 332 MB.** Largest single structural win measured | both files must be trimmed — `awsclient_gen.go` imports the SDK clients and Go initialises every imported package whether or not a symbol is reachable. The service closure must be derived, not hand-written | medium — big win against a permanent fork-maintenance tax |

### provider-upjet-aws

| what | impact | complexity | likelihood |
| --- | --- | --- | --- |
| Recommend `GODEBUG=disablethp=1` (or node `transparent_hugepage=madvise`) | **−47% of what the pod is charged** | zero code — a runtime-configuration and docs change | **high** |
| Set `SuppressDebugLog` when Terraform logging is off | **−24% CPU** | 1 line | **high** |
| Scope the Secret informer (fix 11, already branched) | **14.2 MiB per 5,000 Secrets in the cluster** (~2.9 KB each, whether read or not) — now a measured number rather than an estimate | branched | **high** |
| Cache the configured AWS client across reconciles (fix 09) | **−44.7 MiB at 500 managed resources**, −21 mCPU; scales with reconcile volume | ~30 lines; must key on credential identity **and expiry** — no auth change needed, because the client is handed materialised credential values | medium-high |
| Strip `managedFields` from the informer cache | **−59 KB per managed resource**, −168 MiB of peak at 500 | ~10 lines, but must **not** strip the last-applied annotation (writes derived from cache would delete it server-side) and must be scoped with `ByObject`, not `DefaultTransform` | medium-high |
| Per-family include list (`UPJET_FAMILY_FILTER`) | −20% steady, −20% peak, 13.7 s → 1.1 s startup | codegen; the one imperative `ShortGroup` is now mirrored and guarded by a test | medium-high |
| Family-scoped scheme registration | −4.5 MiB | codegen — **must be generated**: a hand-picked closure broke cross-group reference resolution, verified in-cluster | medium |

### Corrections to the sections above, from cluster evidence

* **Fix 02** — "a full schema rebuild four times per reconcile" is real, but it
  costs no measurable memory or CPU at `GOGC=25`. Pitch it as correctness.
* **Fix 06** — now exercised against LocalStack. Confirmed: `spec.endpoint` is
  ignored entirely unless `spec.endpoint.services` lists the service, and with it
  the traffic provably reaches the custom endpoint. Item 4 of *Worth checking
  against a real account* is discharged for the SDKv2 path.
* **Fix 11** — has a number now: see above.
* **Fix 13** — `--max-reconcile-rate` delivering double is worth restating: at
  the shipped default of 100 the provider holds 6,042 goroutines and 26.3 MiB of
  stacks for 50 managed resources, against 1,540 and 7.0 MiB at 10.

## Things that travel together

* **14 + 16.** One suppresses the no-op *status* PUT, the other the no-op *spec*
  PUT. Separately each is modest; together they take a steady-state reconcile of
  an unchanged taggable resource from two guaranteed writes to zero. Worth making
  that case once, across both PRs.
* **08 + 21.** Complementary: 08 caches the config so the credentials cache
  survives reconciles, 21 makes the sessions long enough to matter. If both land,
  08's comment justifying its 5-minute TTL as "below the 15 minute minimum STS
  session duration" needs rewording — the minimum becomes an hour.
* **07 + 20.** 20 reuses `convertFieldPathToSnake` verbatim from 07, in a
  different package, so both landing means two identical unexported copies. No
  textual conflict; either can go first. Offer to hoist it into `pkg/types/name`
  (both packages already import it and it owns `NewFromCamel`) if a reviewer asks.

## Decisions that are not mine

* **R3 — the connection-secret key mangling.** `pkg/resource/sensitive.go:473`
  trims a trailing `s`, so map/list sensitive attributes publish as
  `connection_propertie`, `airflow_configuration_option`. Plainly a bug; fixing it
  renames keys that live Deployments consume. Three routes: fix and call it
  breaking; emit both keys for a deprecation period; document and leave. Not
  branched, deliberately.
* **The lightsail regeneration (fix 19).** `IdentifierFields` is a codegen input
  with no runtime effect, so the branch is complete without regenerating — but the
  next `make generate` makes `spec.forProvider.target` required and drops
  `spec.initProvider.target`, breaking manifests that use `initProvider`. Same PR,
  follow-up, or not at all?
* **Fix 05 — external-name persistence on failed or async create.** Not attempted
  all session: it reverses a decision documented in upjet, which is the same trap
  that forced half of fix 04 to be reverted.
* **I8 — publishing the Terraform diff.** The `AdditionalDetails` half is now
  implemented as fix 22. The status-field and Event halves need an API version and
  a size bound, and remain a maintainer conversation before code. Fix 22 is the
  one branch here that is a *feature* rather than a defect fix, so it is the most
  likely to want buy-in before review.

## Worth checking against a real account

1. **Fix 21's `MaxSessionDuration` fallback.** It matches on error *text*
   (`ValidationError` mentioning `DurationSeconds`/`MaxSessionDuration`). If AWS
   words it differently, a role with `MaxSessionDuration < 1h` starts failing
   where it worked before. This is the single genuine regression risk in anything
   here. Set a role to 900 seconds and confirm the ProviderConfig still works.
2. **Fix 15/19's AppStream question.** `Delete` was passing a stack name with a
   trailing slash to `BatchDisassociateUserStack`. Whether that stuck the MR on
   its finalizer or silently left the association behind was not determined —
   anyone who deleted these MRs under an affected build should check AWS for
   orphans.
3. **Fixes 08 and 21 generally.** The credential paths have unit tests against
   fakes and have never touched STS.
4. **Fix 06 against localstack.** The correct endpoint map provably reaches the
   Terraform client; that the pinned provider honours every key for every service
   at request time is established by reading `internal/conns/config.go`, not by
   observed traffic.
