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

| # | fix | category | severity | size | lives in | status |
| - | --- | --- | --- | --- | --- | --- |
| [01](01-movetostatus-shared-schema.md) | Stop `MoveToStatus` mutating shared schema singletons | corruption | **critical** | small | upjet | not started |
| [02](02-clear-schemafunc.md) | Clear `SchemaFunc` after materialising `Schema` | correctness + waste | high | **1 line** | upjet (or here) | not started |
| [03](03-async-credential-expiry.md) | Credentials expire mid-operation on async paths | data loss | high | medium | this repo | not started |
| [04](04-missing-secret-key.md) | A missing secret key silently becomes `""` | corruption | high | small | upjet | not started |
| [05](05-create-external-name.md) | Persist the external-name when create fails or is async | data loss | high | small | upjet | not started |
| [06](06-dynamic-endpoint-ignored.md) | `endpoint.url.type: Dynamic` never reaches the CRUD client | correctness | high | small | this repo | not started |
| [07](07-fieldpath-camel-snake.md) | camel→snake mangles nested and digit-bearing paths | corruption | high (latent) | medium | upjet | not started |
| [08](08-credentials-cache-all-sources.md) | One STS call per reconcile for every non-IRSA source | useless API calls | high | medium | this repo | not started |
| [09](09-cache-aws-client.md) | Rebuilding the AWS client and FW provider every Connect | waste | high | medium | this repo | not started |
| [10](10-gate-namespaced-build.md) | Build the namespaced provider only when it is used | waste | high | medium | this repo | not started |
| [11](11-scope-secret-informer.md) | The Secret informer is cluster-wide and unbounded | security | medium-high | **small** | this repo | not started |
| [12](12-caller-identity-cache.md) | Data race and STS-under-lock in the identity cache | correctness | medium | **small** | this repo | **ready, one caveat** — `fix/identity-cache-race-and-lock-scope` @ `efb86ceee` |
| [13](13-double-rate-limiter.md) | `--max-reconcile-rate` delivers double what it says | correctness | medium | **1 line** | this repo | **reviewed, ready** — `fix/single-global-rate-limiter` @ `2de61d751` |

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
