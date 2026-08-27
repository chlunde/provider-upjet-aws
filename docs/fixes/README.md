<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# Actionable fixes

One file per fix worth doing now. Each contains the mechanism, the evidence, the
change, how to test it, a ready-to-paste GitHub issue and a branch name.

## Status of the analysis behind these

Read this before treating any of it as ready to merge.

* **One fix has been implemented and verified.** `dropCodegenOnlyMetadata`
  (commit `a8fc8b5f5`), measured at &minus;17.2 MiB of live heap, confirmed by
  rebuilding with it compiled in. It is not in this directory because it is
  already done.
* **Everything else in this directory is analysis, not code.** No patch has been
  written or run for any of them.
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

| # | fix | category | severity | size | lives in | evidence |
| - | --- | --- | --- | --- | --- | --- |
| [01](01-movetostatus-shared-schema.md) | Stop `MoveToStatus` mutating shared schema singletons | corruption | **critical** | small | upjet | measured |
| [02](02-clear-schemafunc.md) | Clear `SchemaFunc` after materialising `Schema` | correctness + waste | high | **1 line** | upjet (or here) | measured |
| [03](03-async-credential-expiry.md) | Credentials expire mid-operation on async paths | data loss | high | medium | this repo | read |
| [04](04-missing-secret-key.md) | A missing secret key silently becomes `""` | corruption | high | small | upjet | read |
| [05](05-create-external-name.md) | Persist the external-name when create fails or is async | data loss | high | small | upjet | read |
| [06](06-dynamic-endpoint-ignored.md) | `endpoint.url.type: Dynamic` never reaches the CRUD client | correctness | high | small | this repo | read |
| [07](07-fieldpath-camel-snake.md) | camel→snake mangles nested and digit-bearing paths | corruption | high (latent) | medium | upjet | measured |
| [08](08-credentials-cache-all-sources.md) | One STS call per reconcile for every non-IRSA source | useless API calls | high | medium | this repo | read |
| [09](09-cache-aws-client.md) | Rebuilding the AWS client and FW provider every Connect | waste | high | medium | this repo | measured |
| [10](10-gate-namespaced-build.md) | Build the namespaced provider only when it is used | waste | high | medium | this repo | measured |
| [11](11-scope-secret-informer.md) | The Secret informer is cluster-wide and unbounded | security | medium-high | **small** | this repo | read |
| [12](12-caller-identity-cache.md) | Data race and STS-under-lock in the identity cache | correctness | medium | **small** | this repo | measured |
| [13](13-double-rate-limiter.md) | `--max-reconcile-rate` delivers double what it says | correctness | medium | **1 line** | this repo | read |

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
