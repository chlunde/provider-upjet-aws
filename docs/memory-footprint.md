<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# Where a family provider's memory goes

A family provider pod sits at a few hundred MiB of RSS even when a single
managed resource is activated. This document records where that memory actually
goes, why safe-start and `ManagedResourceActivationPolicy` (MRAP) do not move
it, and what it would take to make the footprint scale with the number of types
a provider actually starts.

All numbers below were produced with the harness in
[`hack/memprofile`](../hack/memprofile/README.md) against this tree, on
linux/amd64, `go build` with the repo's default flags.

## Summary

Resident memory has two roughly equal halves, and they need different fixes.
With the whole startup path run — every API group registered, the Terraform AWS
provider constructed, both scoped `config.Provider`s built — `smaps_rollup`
reports:

| | | |
| --- | ---: | --- |
| `Private_Clean` | **692 MB** | executable text and rodata paged in from the binary |
| `Anonymous` | **386 MB** | the Go heap arena, of which only **51.5 MB is live data** |
| `Rss` | **1,079 MB** | |

Neither half is the live object graph. The first half is code the family binary
links but never runs; the second is arena grown to absorb the garbage thrown off
while parsing 22 MB of embedded JSON and YAML into 1,029 resource
configurations, twice.

That is why safe-start does not help. Safe-start gates *controller startup* on
CRD existence, and MRAP controls *which CRDs exist*. Both work, and both are
already wired up here (`spec.capabilities: [SafeStart]` in
`package/crossplane.yaml.tmpl`, the `customresourcesgate` plumbing in
`cmd/provider/*/zz_main.go`). But an unstarted controller was never the
expensive thing.

The same startup work costs **25 seconds of wall clock**, of which 24 seconds is
the two `config.Provider` builds. Everything else — registering 8,488 GVKs,
constructing the Terraform provider, and all the package `init` work before
`main` — totals about 330 ms.

## Measurements

### Startup, step by step

`hack/memprofile/startup` walks the same sequence as `cmd/provider/<family>/zz_main.go`,
timing each step's work separately from the collections it forces around each
reading:

```
                                            live heap    RSS     took
0. process start (binary linked, inits run)     20.2    621.2       0s
1. clusterapis.AddToScheme                      22.2    625.8      44ms
2. namespacedapis.AddToScheme                   24.1    628.0      39ms
3. apiextensionsv1.AddToScheme                  24.1    628.0       0s
4. xpprovider.GetProvider (TF SDKv2 + FW)       30.8    686.8      63ms
5. config.GetProvider (cluster)                 47.7    854.9   16.864s
6. config.GetProviderNamespaced                 51.5   1064.9    7.029s

time from exec to main()  180ms
time in the startup path  24.038s
```

Three things stand out.

* **RSS is already 621 MB before a single line of provider logic runs**, and
  reaching that point costs only 180 ms. That is the price of having the code in
  the binary, not of executing it.
* **Registering all 8,488 GVKs across both scopes costs 4 MiB and 83 ms.** The
  scheme is not where either budget goes.
* **The two `config.Provider` builds cost 24 of the 25 seconds**, and take RSS
  from 687 MB to 1,065 MB.

The asymmetry between steps 5 and 6 — 16.9 s against 7.0 s for identical work —
is not noise. Both providers share one `sdkProvider`, and upjet's `NewProvider`
materialises lazy schemas by assigning into it:
`terraformResource.Schema = terraformResource.SchemaFunc()`. The first pass pays
for ~1,000 `SchemaFunc()` calls and mutates the shared resources; the second
finds `Schema` already non-nil and skips them. So roughly 10 seconds of startup
is schema materialisation that a per-family include list would mostly avoid.

### Link cost per package set

Each of these programs links a different slice of the provider and reports RSS
before doing any work:

| program | binary | loadable image | RSS before work | RSS after work |
| --- | ---: | ---: | ---: | ---: |
| empty `main` | 2.7 MB | 1.9 MiB | 2.3 MiB | — |
| `apis/cluster` only | 213 MB | 179 MiB | 96.6 MiB | 101.7 MiB (4,988 GVKs) |
| `apis/cluster` + `apis/namespaced` | 311 MB | 245 MiB | 130.1 MiB | 142.7 MiB (8,477 GVKs) |
| one family's API group (`ec2`) | 81 MB | — | 39.2 MiB | 39.8 MiB (472 GVKs) |
| `terraform-provider-aws` (`xpprovider`) | 1.24 GB | 864 MiB | 314.5 MiB | 379.5 MiB |

Scoping the API packages to a single family is worth about **103 MiB of RSS**
(142.7 → 39.8) on its own, and that is before considering the controllers.

### What is in the binary

Symbol sizes from `go tool nm -size`:

`terraform-provider-aws` binary — 519 MiB of symbols:

| module | size |
| --- | ---: |
| `github.com/aws/aws-sdk-go-v2` | 317.2 MiB |
| `github.com/hashicorp/terraform-provider-aws` | 106.5 MiB |
| `go:func` wrappers | 40.4 MiB |
| `crypto` | 33.1 MiB |

The AWS SDK is 317 MiB spread over **269 service clients**, mean 1.2 MiB each.
`ec2`, the largest, is 18.3 MiB — so an EC2-only binary would drop about
**298 MiB of symbols** from the SDK alone. Similarly,
`terraform-provider-aws/internal/service/*` is 43.7 MiB over 267 services, of
which `ec2` is 3.8 MiB.

APIs binary — 129 MiB of symbols:

| kind | size |
| --- | ---: |
| generated `DeepCopy` methods | 42.1 MiB |
| generated `ResolveReferences` methods | 7.4 MiB |
| `apis/{cluster,namespaced}/*` total | 69.6 MiB over 176 of the 178 groups |
| `apis/*/ec2` | 5.9 MiB |

## Why every family binary contains everything

### The API packages are aggregated

`cmd/provider/<family>/zz_main.go` calls `clusterapis.AddToScheme` and
`namespacedapis.AddToScheme`. Those are `apis/cluster/zz_register.go` and
`apis/namespaced/zz_register.go`, which import **every** group of **every**
version. The linker therefore cannot drop a single group from an EC2-only
binary.

The repo already has the machinery to partition this: `scripts/tag.sh` runs
`buildtagger` to stamp `//go:build (ec2 || all) && !ignore_autogenerated` onto
each group directory under `apis/`, each `internal/controller/*/zz_*_setup.go`,
each `config/*/<family>/config.go`, and each `cmd/provider/<family>/zz_main.go`.
But those tags exist only to keep golangci-lint's memory down: `Makefile` wires
them to `lint.init`, and `lint.done: delete-build-tags` strips them again. The
release build (`go.build` in `build/makelib/golang.mk`) runs a separate
`go build` per subpackage but hands all of them the same `GO_STATIC_FLAGS`, so
every family is compiled with the same empty `GO_TAGS`, against untagged
sources.

The tagging is also not sufficient as it stands — `apis/cluster/zz_register.go`
is tagged `all && !ignore_autogenerated`, so a `-tags ec2` build would compile
an `apis/cluster` package with no `AddToScheme` at all.

### The Terraform provider is aggregated

`xpprovider.GetProvider` builds the full Terraform AWS provider.
`internal/provider/sdkv2/service_packages_gen.go` imports all 267 service
packages and `internal/conns/awsclient_gen.go` imports all 266
`aws-sdk-go-v2/service/*` clients. Both are generated files in the
`upbound/terraform-provider-aws` fork.

### The provider configuration is built twice, for all resources

`config.GetProvider` and `config.GetProviderNamespaced` each call upjet's
`config.NewProvider`, which:

* unmarshals the embedded `config/schema.json` (14.7 MB) and converts **every**
  resource schema to the plugin-SDK representation, even though the comment on
  `GetV2ResourceMap` says those are "not utilized during runtime, just for
  facilitating CRD generation";
* parses the embedded `config/provider-metadata.yaml` (7.3 MB) of scraped docs
  and examples and attaches it to every resource as `MetaResource`;
* builds a `config.Resource` for all **1,029** configured resources, calling
  `SchemaFunc()` on each — which defeats the laziness terraform-provider-aws
  added specifically to avoid materialising schemas it does not need — and runs
  the schema traversers and reference injectors over all of them.

An EC2 binary needs 104 of those 1,029. All of it happens twice, once per scope,
and it dominates both budgets: 24 of the 25 seconds of startup, and the arena
grown to absorb its garbage is the 386 MB of anonymous RSS.

The second pass is cheaper than the first — 7.0 s against 16.9 s — because both
providers share one `sdkProvider` and `NewProvider` materialises lazy schemas by
assigning into it (`terraformResource.Schema = terraformResource.SchemaFunc()`).
The first pass pays for ~1,000 `SchemaFunc()` calls and mutates the shared
resources; the second finds `Schema` already set. About 10 seconds of startup is
therefore schema materialisation for resources the family will never reconcile.

Note that `CLIReconciledExternalNameConfigs` is empty for AWS: no resource is
reconciled through the Terraform CLI. For plugin-SDK resources
`NewProvider` overwrites the JSON-derived schema with the Go one from
`p.TerraformProvider.ResourcesMap[name]`, and the runtime only ever reads that
Go schema (`pkg/controller/external_tfpluginsdk.go`). The 14.7 MB embedded JSON
schema is, at runtime, used for nothing but enumerating resource names.

## What it would take

Ranked by measured payoff.

### 1. Compile the API packages per family

Worth ~103 MiB of RSS, and self-contained in this repository.

* Generate a per-family scheme registration — either
  `apis/{cluster,namespaced}/<family>/register.go` exposing that family's
  `AddToSchemes`, or a build-tag-partitioned `zz_register.go` per family — so a
  `-tags ec2` build gets a scheme with just that family's groups.
* Extend it to the **reference closure**, not just the family. Generated
  resolvers call `apisresolver.GetManagedResource` for other groups
  (`kafkaconnect` resolves against `ec2`, `iam`, `s3`, `firehose`,
  `cloudwatchlogs`), so the per-family register must also pull in the groups
  that family's resolvers reach. That closure is computable at generation time
  from the same reference metadata upjet already has.
* Make the release build actually use the tags. `go.build` already runs one
  `go build` per entry in `GO_STATIC_PACKAGES`, but every one of them gets the
  same `GO_STATIC_FLAGS`, and so the same single `GO_TAGS` value. It needs to
  vary the tag per subpackage instead, and `scripts/tag.sh` has to run for
  builds rather than only for lint.

### 2. Scope terraform-provider-aws to the family's services

The largest absolute win — on the order of 300 MiB of symbols — but it needs a
change in the `upbound/terraform-provider-aws` fork.

* `service_packages_gen.go` and `conns/awsclient_gen.go` are both generated.
  Emit build-tag-partitioned variants alongside the current full-set files, so
  `-tags ec2` links only the service packages and SDK clients that family needs,
  and an untagged build stays exactly as it is today.
* The per-family service set is a closure, not a single service: resources reach
  for tagging, IAM, STS and S3 clients outside their own service package. The
  generator has to compute that closure.
* Upjet's include lists must be narrowed to match. `config.NewProvider` panics
  with "resource is configured to be reconciled with Terraform Plugin SDK but
  the Go schema does not exist" if the include list names a resource whose
  service package is not linked.

### 3. Stop building 1,029 resource configurations to use 104

> **Corrected after measurement — see [the correction below](#correction-filtering-the-resource-set-is-a-startup-win-not-a-memory-win).**
> The startup half of this claim holds and is large. The memory half does not:
> filtering buys ~47 MiB of anonymous RSS unconstrained, and **nothing at all**
> under `GOMEMLIMIT=300MiB`. The arena comes from whole-file parsing that no
> include list reaches, not from the 925 skipped resources.

This work is **24 of the 25 seconds of startup**, and it runs alongside an arena
of **386 MB of anonymous RSS** — the half a pod's working-set metric counts in
full.

* Filter `config.Provider.Resources` to the family. The mapping is already
  statically known in-repo — `config/groups.go` plus upjet's default
  group derivation — so a per-family include list can be generated. Worth
  **-11.4 MiB** of live heap for the resource configs themselves, but that
  understates it: skipping 925 of 1,029 resources also skips their
  `SchemaFunc()` materialisations and traversals, which is where the seconds and
  the garbage both come from.
* Skip the embedded JSON schema and registry metadata at runtime. This needs an
  upjet option — `NewProvider` unconditionally unmarshals both — but nothing
  downstream reads either one outside the code generation pipelines.
* Share one parse between the cluster-scoped and namespaced providers instead of
  doing all of the above twice.

### 4. Already done here

`config.GetProvider` and `config.GetProviderNamespaced` now release
`MetaResource` on every resource once the configurators have run, for
non-generation providers. That is the scraped Terraform documentation —
descriptions, argument docs and examples — which only `pkg/types`,
`pkg/pipeline` and `pkg/examples` read.

Verified end to end rather than simulated: with the change compiled in, live
heap after step 6 is **51.5 MiB against 68.7 MiB before**, and the harness's
"drop MetaResource" simulation now reclaims **+0.0 MiB** because there is
nothing left to reclaim. Startup time is unchanged; the release itself takes
10 ms.

## Reconciling this with the ~300 MB seen on a pod

The numbers above are RSS. A pod's reported memory is not RSS: cadvisor computes
`container_memory_working_set_bytes` as `memory.current - inactive_file` (cgroup
v2; `total_inactive_file` on v1). That formula treats the two halves of this
process very differently.

* The **692 MB of executable** is clean, file-backed. Once those pages age onto
  the inactive file LRU the metric subtracts them, even though they are still
  mapped and still counted in RSS.
* The **386 MB of anonymous arena** is not file-backed and is never subtracted.
  A pod pays for all of it, all the time.

So a family provider reporting ~300 MB of working set is most likely reporting
the Go arena almost in full, with the executable's clean pages largely aged out.
That matches the measurement closely: under `GOMEMLIMIT=300MiB` the anonymous
figure is 288 MB.

Two consequences follow, and they point at different work.

* **For the metric people watch**, the arena is the target: build 104 resource
  configurations instead of 1,029 so the garbage is never produced, and cap the
  heap in the meantime.
* **For node pressure and cold start**, the executable is the target. Those
  pages are real — they compete for the node's page cache, they are re-faulted
  from disk after eviction, and they are why a pod that has just been scheduled
  is slow. The working-set metric simply does not show them.

Note on completeness: the figures here come from probes that link the same
package sets as `cmd/provider/<family>`, not from the shipped binary. Building
`cmd/provider/ec2` exhausted this environment's disk allowance three times — the
`$WORK` tree for its ~6,000 packages does not fit — so these are a measured
lower bound on the shipped binary rather than a reading of it. A family binary
links everything the probes link *plus* every family's controllers, so it cannot
be smaller.

## What does not help

* **Safe-start and MRAP.** Both are already in place, and both address a
  different cost — API server memory per established CRD, and informer caches
  per started controller. Neither touches resident code or the startup arena.
* **Stripping the binary.** `-ldflags="-s -w"` removes ~730 MB of DWARF from
  the file, which halves image pull size, but DWARF is never resident so RSS is
  unchanged.

## Correction

An earlier revision of this document listed `GOGC` / `GOMEMLIMIT` tuning under
"what does not help", on the grounds that 51 MiB of live heap leaves nothing to
tune. That was wrong. Live heap is not the arena: the arena is sized by the
peak, and startup's peak is driven by transient parsing garbage. Measured:

| setting | `Anonymous` | startup path | live heap |
| --- | ---: | ---: | ---: |
| `GOGC=100` (default) | 386 MB | 25.0 s | 51.5 MiB |
| `GOMEMLIMIT=300MiB` | 288 MB | 27.2 s | 51.5 MiB |
| `GOGC=50` | 318 MB | 26.6 s | 51.5 MiB |
| `GOGC=25` | 269 MB | 32.2 s | 51.5 MiB |

`GOMEMLIMIT=300MiB` gives up ~100 MB of anonymous RSS for 2 seconds of startup,
which is the best of these trade-offs and needs no code change at all. It is a
mitigation rather than a fix — the garbage is still produced — but it is the one
lever available today, and it acts on the half of RSS that a pod's working-set
metric counts in full.


---

## Correction: filtering the resource set is a startup win, not a memory win

Everything above about *where* the memory is remains measured and correct. The
**inference** in §3 — that the arena is grown by building 1,029 resource
configurations, so building 104 would shrink it — is **wrong**, and was refuted
by direct measurement.

### The experiment

`UPJET_FAMILY_FILTER=<short group>` (branch `claude/family-filter-measurement`)
filters the three include lists before they reach `config.NewProvider`. That is
the correct cut point: upjet's include-list `continue`
(`pkg/config/provider.go:429`) runs **before**
`terraformResource.Schema = terraformResource.SchemaFunc()` at `:443`, so
filtered resources genuinely skip materialisation. One binary serves both arms,
so this isolates the runtime effect with no change in link shape. The 104 names
kept are byte-identical to the 104 an unfiltered run assigns `ShortGroup == ec2`.

### Results

Unconstrained (`GOGC`/`GOMEMLIMIT` unset), n=3 baseline / n=4 filtered:

| metric | baseline (1,029) | ec2 only (104) | delta |
| --- | --- | --- | --- |
| **anonymous RSS** | 376.0–381.1 MiB | 327.7–333.8 MiB | **−47 MiB (−12.5%)** |
| live heap | 51.5 MiB | 32.5–32.6 MiB | −19 MiB |
| startup path | 23.94–24.27 s | 4.06–4.09 s | **−20.1 s (−83%)** |
| peak RSS | 1041–1043 MiB | 977–986 MiB | −60 MiB |
| `HeapIdle` | 290.0–295.6 MiB | 280.0–299.7 MiB | **~0** |
| `HeapInuse` | 95.9–97.5 MiB | 45.8–47.9 MiB | −50 MiB |
| `TotalAlloc` | ~15,540 MiB | ~3,018 MiB | −80.6% |

Under `GOMEMLIMIT=300MiB` — independently re-run and reproduced:

| | baseline | ec2 only |
| --- | --- | --- |
| anonymous RSS | 290,460 kB = **283.7 MiB** | 288,556 kB = **281.8 MiB** |
| startup path | 28.45 s | **4.30 s** |
| `HeapInuse` | 90.9 MiB | 45.6 MiB |
| `HeapIdle` | 208.6 MiB | **246.0 MiB** |

**−1.9 MiB. Inside noise.**

### Why

`HeapIdle` does not fall — under `GOMEMLIMIT` it *rises*, 208.6 → 246.0 MiB,
while `HeapInuse` halves. **The arena is not smaller, it is emptier.** The GC
heap goal is set by peak reachable set, and that peak is dominated by work the
include list never reaches:

* `PHASES_ONLY` — only the include-list-independent parses (`tfjson` unmarshal of
  the 18.1 MB `schema.json` over 1,683 resources, `GetV2ResourceMap`, the
  `provider-metadata.yaml` parse over 1,676) — retains **129.9 MiB live**,
  `HeapSys` 207.6–211.6 MiB, anonymous RSS **222.1–222.4 MiB**.
* `STOP_AFTER_STEP=4` — schemes plus `xpprovider.GetProvider`, no config build at
  all — anonymous RSS **50.1–53.4 MiB**.
* In the *filtered* run itself, which only ever builds 104 configs,
  `GetV2ResourceMap` alone adds **+49.0 MiB** live and the registry metadata parse
  another **+5.6**, reaching 135.8 MiB.

`config.NewProvider` does all of that unconditionally, once per scope, before any
include list is consulted.

*Caveat, stated honestly:* `PHASES_ONLY` holds the `tfjson` tree live at the end
where `NewProvider` may drop it mid-function, so ~222 MiB is the cost of
doing-and-holding the parse, an upper bound rather than a proven floor.

### Where the −11.4 MiB figure came from, and why it misled

The `-11.4 MiB` in §3 is an in-process simulation that drops entries from an
**already-built** map and takes 1 ms. It never avoided the materialisation or the
parse. It understated the live-heap effect (real: −19 MiB) while implying the
arena would follow it down, which it does not. **Every simulated delta in this
document measures what is *reachable*, not what the run *allocated*** — treat
them accordingly.

### What this means

* **Do not propose "filter the resource set" to upjet as a memory fix.** It buys
  −12.5% unconstrained and nothing under the `GOMEMLIMIT` this document already
  recommends. The two mitigations do not compose; the limit binds first.
* **Do propose the startup win.** 24.2 s → 4.1 s, ~6× faster time-to-ready, from a
  downstream change needing no upjet modification at all.
* **The memory proposal worth making to upjet is a different one:** let
  `NewProvider` avoid materialising the whole `schema.json` and
  `provider-metadata.yaml` object graphs for a non-generation provider, and share
  one parse between the cluster-scoped and namespaced builds. Filtering the
  include list is a *precondition* — you cannot skip parsing what you still need —
  but is not itself the win. **That case is unmeasured**; it needs an upjet fork,
  and no number should be quoted for it until someone runs it.

---

## Shipping the startup win: the safety invariant, and one violation

The startup result above (24.2 s → 4.1 s, and 28.4 s → 4.3 s under
`GOMEMLIMIT=300MiB`) is the one memory-investigation finding worth acting on. It
is entirely downstream — no upjet change — but it is **not** as simple as
threading a family through `GetProvider`.

### Why it can panic

Every generated controller does an unchecked map index followed by a field
access, e.g. `internal/controller/cluster/ec2/instance/zz_controller.go:53`:

```go
for _, i := range o.Provider.Resources["aws_instance"].InitializerFns {
```

`config.Provider.Resources` is `map[string]*config.Resource`
(upjet `pkg/config/provider.go:187`), so a missing key yields a nil pointer and
this **panics at provider startup**. There are **3,136** such index sites across
2,058 files, covering 1,029 distinct Terraform names.

So the safety of family filtering reduces to one invariant:

> for every family F, the names the filter keeps must be a **superset** of the
> names the controllers registered by `Setup_F` index.

Corollary worth stating: under a filter, `Setup_<family>` becomes the **only**
legal entry point. `Setup_monolith` with a filter applied panics by construction.

### The invariant does not hold today

`config/family_filter_test.go` (branch `claude/family-filter-safety-test` @
`1357cf5`) checks it for all 177 generated families plus the monolith. **176 hold
exactly** — kept set and indexed set are equal, not merely a superset. One fails:

```
UPJET_FAMILY_FILTER=elbv2 drops 1 of the 7 Terraform resource(s) that Setup_elbv2
registers controllers for. config.Provider.Resources would hold no entry for them,
so Setup_elbv2 panics with a nil pointer dereference at provider startup. Missing:
  aws_lb_trust_store (../internal/controller/cluster/elbv2/lbtruststore)
```

Root cause: `config/cluster/elbv2/config.go:165` (and the identical namespaced
file) sets the group **imperatively, inside a resource configurator**:

```go
p.AddResourceConfigurator("aws_lb_trust_store", func(r *config.Resource) {
	r.ShortGroup = "elbv2"
	r.Kind = "LBTrustStore"
})
```

Those two lines are the **only** imperative `ShortGroup` assignments in the repo.
Any *static* derivation of a resource's family — `GroupMap` plus upjet's default
"second word" rule — computes `lb` for that name and can never see the override,
because the override runs later, during provider configuration.

### The design call this forces

1. **Special-case it in the static mirror.** One line, works today, and the next
   imperative `ShortGroup` assignment silently reintroduces the panic — with
   nothing but this test to catch it.
2. **Drive the filter off a generated group table**, emitted from the
   fully-configured provider so overrides are captured by construction. More
   work, and it makes the include lists a build artifact rather than a
   hand-maintained list, but it cannot rot.

(2) is the right answer for anything heading to a release; (1) is defensible only
with the test as a permanent gate.

### What the test does that matters

* **Kept side in-process, through the production code** — it calls the same three
  `*ResourceList()` functions `GetProvider` calls, so it tracks the real filter
  rather than a copy, and replicates upjet's actual `matches` rule (include
  entries are `name$` with no `^`; one `skipList` entry has no anchor at all).
* **Controller side via go/ast** over the per-family setup files' imports, then
  the string literals under `*.Provider.Resources[...]` index expressions. There
  is no in-process source of truth for what `Setup_F` registers, and getting one
  would mean importing `internal/controller` — i.e. `internal/clients` and the
  whole `apis` tree.
* **Three guards against a vacuous pass**, which is the real risk for a test like
  this: it fails if the filter did not actually filter
  (`len(kept) >= len(unfiltered)`), fails on a non-literal map key so a codegen
  template change cannot make the scan blind, and reports pre-existing
  unconfigured names in a separate subtest so they are not misattributed.
* Lives in `package config`, importing none of `internal/clients`,
  `internal/controller`, `apis` or `xpprovider`. Run it as
  `go test ./config -run TestFamilyFilterKeepsEveryIndexedResource` — **not**
  `./config/...`, which drags in the 634 s `config/test/roundtrip`.

Test body runs in **0.48 s**. Verified independently: the `elbv2` failure
reproduces, and deliberately dropping `aws_instance` from the filter fails `ec2`
naming that resource.

### Still needs a cluster

* A filtered family binary reaching `Ready` on a real MR — the panic is
  startup-time, so this fails fast if the invariant is violated somewhere the
  static test cannot see.
* **Cross-family references** — an `ec2` resource resolving against `iam` or
  `s3`. Reading suggests `ResolveReferences` goes through
  `apisresolver.GetManagedResource` and the *scheme* (which still registers all
  8,488 GVKs, at 4 MiB) rather than through `Provider.Resources`, so filtering
  should not touch it. That is code reading, and it is the one interaction worth
  observing before shipping.
* Webhook and conversion setup for a filtered family, since
  `SetupWebhookWithManager_<family>` runs on a different path from `Setup_<family>`.

---

## The largest win, and it was missed for most of this investigation

**~180–255 MiB of the anonymous footprint is idle heap the runtime is holding
but not using, and `debug.FreeOSMemory()` returns it in 58 ms.**

Every other figure in this document was taken after `runtime.GC()` only. `GC`
collects; it does not hand idle spans back to the OS. The counters said so all
along and nobody read them: in the `GOMEMLIMIT=300MiB` baseline,
`HeapIdle` 208.6 MiB against `HeapReleased` 28.2 MiB is **180.4 MiB of
collected-but-resident heap**, with only 51.5 MiB live.

### Measured

`SCAVENGE=1` on the startup harness, immediately after the startup path:

| | anonymous before | anonymous after | drop |
| --- | --- | --- | --- |
| no `GOMEMLIMIT` | 376,424 kB = **367.6 MiB** | 114,732 kB = **112.0 MiB** | **−255.6 MiB** |
| `GOMEMLIMIT=300MiB` | 289,284 kB = **282.5 MiB** | 106,160 kB = **103.7 MiB** | **−178.8 MiB** |

`HeapReleased` 25.5 → 282.8 MiB (unconstrained) and 30.2 → 209.3 MiB (limited).
`HeapAlloc` and `HeapInuse` are **unchanged** in both — nothing live is touched.
Total RSS 973.6 → 794.6 MiB; the remainder is the clean, file-backed executable.

**The release is real, not bookkeeping.** `Private_Dirty`/`Anonymous` falls by the
same amount `HeapReleased` gains, i.e. the kernel actually took the pages
(`MADV_DONTNEED`). This was checked specifically because a counter moving is not
the same as memory being returned.

Both configurations converge on ~104–112 MiB — which is `HeapInuse` (90–97) plus
non-heap `Sys` (~16). That is the floor for this architecture without reducing
live data or span fragmentation: `HeapAlloc` is 51.5 MiB against `HeapInuse` of
90–97, so ~40 MiB is fragmentation.

### `GOMEMLIMIT=300MiB` is likely *causing* the observed 300 MB, not mitigating it

Earlier in this document the 386 → 288 MB effect of `GOMEMLIMIT` is reported as a
win. On this evidence that reading is wrong. When a memory limit is set, Go's
scavenger targets **the limit**, not minimum footprint — it has no reason to
return anything while the process is under 300 MiB. That is a good explanation
for why a real pod parks near 300 MB in steady state and never comes down.

### What this does and does not change

* **Peak RSS is unchanged** (977.9 MiB). The parse still spikes, so a pod's memory
  *limit* must still cover it and startup OOM risk does not move. Scavenging
  returns the high-water mark afterwards; avoiding the parse means never taking
  it. The two compose.
* **The startup win (24 s → 4 s) is untouched** and independent.
* **The `elbv2` panic still gates family filtering**; this is unrelated to it.

### The open question that decides the fix

**Every number here, including these, is from the instant after startup.** A pod
reporting 3xx MB has been reconciling for hours. If steady-state reconciliation
re-establishes a similar high-water mark within minutes, a one-shot
`FreeOSMemory()` buys nothing durable and the answer becomes a periodic scavenge,
or `GOGC`/`GOMEMLIMIT` tuning, or both. **This is not yet measured** — it needs
the reconcile harness rather than the startup one, and it is the next thing to
run.

The likely shape of the fix, pending that: stop telling the runtime it may keep
300 MiB, and either call `debug.FreeOSMemory()` once after provider construction
— where the startup garbage is by far the largest one-off — or run it on a slow
ticker. A few lines in the generated `main`, nothing needed from upjet or the
terraform-provider-aws fork.

**Harness** `claude/family-filter-measurement` @ `f40d959` (`SCAVENGE=1`).
