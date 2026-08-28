<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# Handover: what to propose, where, and in what order

Nineteen branches across three forks (one, fix 23, retracted after measurement — see its file). No pull request has been opened anywhere.
Full write-up for each is the numbered file in this directory; this page is just
the shape of the thing.

**Verification status applies to all of it: nothing has run against a live AWS
account or a Kubernetes cluster.** Every branch has unit tests, and every fix was
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
