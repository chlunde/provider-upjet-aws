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
while parsing 26.8 MB of embedded JSON and YAML into 1,029 resource
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

> **The `binary` column is UNSTRIPPED and overstates the shipped artifact by
> roughly 28%.** These were built with plain `go build`; the release build
> strips, because `build/makelib/common.mk:71` sets `DEBUG ?= 0` and
> `build/makelib/golang.mk:50` adds `-s -w` when it is. Measured on a binary of
> this shape: 1,560 MB unstripped, of which **429.9 MiB is strippable**
> (`.strtab` 169.6, `.debug_info` 91.9, `.debug_line` 67.4, `.debug_loclists`
> 43.3, `.symtab` 31.0, `.debug_rnglists` 15.9) — so ~1,060 MB ships.
>
> **The `loadable image` and both `RSS` columns are unaffected by stripping**, and
> every RSS conclusion here stands: DWARF and the symbol table are non-`ALLOC`
> sections, absent from every `PT_LOAD` segment, so they are never mapped and
> never resident. Confirmed directly — total `PT_LOAD` memsz is 1,090.7 MiB
> against a 1,488 MiB file.
>
> Quote the stripped figure for image size; quote these columns for memory. The
> `go tool nm -size` analysis below is likewise unaffected — it reads `.symtab`
> from an unstripped build and measures code volume, which is what the
> per-service arithmetic needs.
>
> `Private_Clean` is demand-paging dependent and varies run to run (692 MB here,
> 686–689 MiB in later runs on a differently-built harness); that is page-touch
> variation, not a discrepancy.

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

* unmarshals the embedded `config/schema.json` (19.0 MB) and converts **every**
  resource schema to the plugin-SDK representation, even though the comment on
  `GetV2ResourceMap` says those are "not utilized during runtime, just for
  facilitating CRD generation";
* parses the embedded `config/provider-metadata.yaml` (7.8 MB) of scraped docs
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
Go schema (`pkg/controller/external_tfpluginsdk.go`). The 19.0 MB embedded JSON
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

**MEASURED, 2026-08-28 — 864 MiB of mapped image becomes 124.8 MiB.** This was an
estimate when written; it is now a measurement. See
[the measurement below](#per-family-linking-measured).

The largest absolute win, and it needs a change in the
`upbound/terraform-provider-aws` fork.

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

## ~~The largest win, and it was missed for most of this investigation~~ — RETRACTED

> # RETRACTED — everything from here to the end of this document is false
>
> The four sections below ("The largest win…", "`GOMEMLIMIT` is likely *causing*…",
> "Steady state: the background scavenger does not do this for you", and
> "Recommended fix shape") rest on the claim that Go's background scavenger does
> not return the idle heap unprompted. **It does** — within ~2.5 minutes idle and
> ~15 seconds under load, triggered at the forced-GC boundary. Confirmed in a
> minimal repro: `HeapReleased` jumps 3.2 → 1603.2 MiB the instant `NumGC` ticks
> at 2m15s.
>
> The evidence offered below for "it does not come back" was an idle sampling of
> two processes **that had already been scavenged explicitly** — the control had
> nothing left to return, so its flatness proved nothing.
>
> An explicit `debug.FreeOSMemory()` buys **7–10 MiB and closes a ~2.5-minute
> startup window**, not 180–255 MiB. `GOMEMLIMIT` does **not** prevent release (a
> limited process parks *lower* than an unlimited one); what it genuinely does is
> cap the arena and lower the startup high-water mark. A periodic ticker is
> pointless — the workload returns to the plateau within 15 s.
>
> Full retraction, with the 15-run matrix and raw logs:
> [`docs/fixes/23-release-startup-heap.md`](fixes/23-release-startup-heap.md).
> The sections are left in place rather than deleted so the error is legible.
>
> **The original question — why a real pod sits at ~300 MB — is open again.** The
> proxy workload has no growing live set, so it never modelled
> controller-runtime's informer cache, client-go, the workqueue, or the AWS SDK
> request cycle. That is where to look.


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

### Steady state: the background scavenger does not do this for you

The obvious objection to the finding above is that Go's background scavenger
would return that memory on its own given time, making an explicit call
unnecessary. **Measured, and it does not.**

Two processes, both having already scavenged once after startup, sampled from
`/proc/<pid>/smaps_rollup` every 20 s while idle:

| t | no `GOMEMLIMIT` | `GOMEMLIMIT=300MiB` |
| --- | ---: | ---: |
| 21:00:03 | 153,132 kB | 146,472 kB |
| 21:00:43 | 153,156 kB | 146,480 kB |
| 21:01:23 | 153,172 kB | 146,492 kB |
| 21:02:03 | 153,192 kB | 146,508 kB |
| 21:02:23 | 153,204 kB | 146,516 kB |

Over **2 min 20 s** of idling the no-limit process moved **+72 kB** and the
limited one **+44 kB**. That is drift, not reclamation. Neither configuration
returns anything unprompted.

Stated precisely: *does not reclaim within ~2.5 minutes of idling*. Go's
scavenger runs on roughly a 1% CPU budget and is slow by design, so this is not
proof it would never act — but ~40 MiB should be well inside that window, and
nothing moved.

No memory-pressure confound: 16 GB total, 15 GB available, 9.8 GB in
buff/cache, so the kernel was never pushed to reclaim on the processes' behalf.

**Second finding, from the same samples.** Both had scavenged at startup and sat
at **143–150 MiB** — about 40 MiB above the 104–112 MiB post-scavenge floor, but
**flat**, not climbing back toward the 283–367 MiB startup high-water mark. Work
after the scavenge re-grows a bounded amount and then stops.

### Caveat on the harness, and on concurrency

`hack/memprofile/steadystate` (branch `claude/steady-state-scavenge` @ `1ddcefa`)
deliberately **never calls `runtime.GC()` while sampling** — every other harness
here reports after a forced collection, which is right when attributing live heap
to a step and wrong when the question is what the runtime does unprompted.

`WORKLOAD=reconcile` is **a proxy and is documented as one**: it replays the
pure-CPU parts of Connect+Observe in a loop. With no cluster and no AWS account
it models none of the AWS SDK request cycle, controller-runtime's informer cache,
client-go, or the workqueue. It is a lower bound on per-reconcile churn with no
growing live set.

The two arms above ran **concurrently**, which makes the binary's file-backed
pages `Shared_Clean` between them — `Private_Clean` reads 10–13 MB instead of the
~690 MB seen single-process. **`Anonymous` is unaffected** (anonymous pages are
always private) and is the column that matters, but no `RSS` or `Private_Clean`
figure from those runs is comparable with the single-process measurements above.

### Recommended fix shape

On this evidence: **a one-shot `debug.FreeOSMemory()` after provider
construction**, in the generated `main`. It captures the large one-off — the
180–255 MiB of startup garbage — durably, because nothing re-grows toward that
mark and the background scavenger will not return it on its own.

A slow ticker would additionally recover the ~40 MiB that post-startup work
re-establishes. On the idle evidence that is a refinement, not a necessity, and
it carries a real cost: `FreeOSMemory` forces a full stop-the-world GC (58 ms
measured at startup scale), so a ticker trades latency spikes for memory. Ship
the one-shot first.

Do **not** rely on `GOMEMLIMIT` for this. It does not cause release — it makes
the scavenger target the limit rather than minimum footprint, and the idle
samples show the limited arm reclaiming no better than the unlimited one.


---

## Per-family linking: measured

Trimming the fork to one family's service closure and linking a minimal `main`
that calls `xpprovider.GetProvider`:

| | original (recorded `linkcost/tfaws`) | ec2 closure | delta |
| --- | ---: | ---: | ---: |
| mapped image (PT_LOAD memsz) | 864 MiB | **124.8 MiB** | **−739 MiB (−86%)** |
| `aws-sdk-go-v2` symbols | 317.2 MiB | **31.2 MiB** | **−286 MiB (−90%)** |
| binary file | 1.24 GB | 132.4 MB | −89% |

The −286 MiB independently corroborates the ~298 MiB predicted above by
per-service symbol arithmetic, reached by an unrelated route.

**Caveat:** the baseline is the recorded `linkcost/tfaws` binary, not the same
`cmd/sizetest` program. Both are minimal mains calling `xpprovider.GetProvider`,
so the shapes match, but this is not a same-binary comparison — an identical
baseline needs ~20 GB of transient build space.

### Correcting what roots the SDK

[`ideas.md`](ideas.md) §I2 says the SDK is rooted by `*conns.AWSClient`'s method
set rather than by `servicePackages()`. **That is backwards.** Two generated files
root it:

* `internal/provider/sdkv2/service_packages_gen.go:282` — `servicePackages(ctx)`
  is a flat slice literal of **267** `<svc>.ServicePackage(ctx)` calls, invoked
  unconditionally at `provider.go:308`, which is exactly where
  `xpprovider.GetProvider` → `provider.NewProvider(ctx)` lands.
* `internal/conns/awsclient_gen.go` — 266 typed accessors whose **import list**
  pulls every service package's `init`.

The method set is a consequence. Both are generated files, so this is a codegen
change rather than an architectural one — materially easier than §I2 implies.

### The closure is invisible to the import graph

`go list -deps` **understates** it, so the generator cannot use it.
`organizations/account.go` calls `meta.(*conns.AWSClient).AccountClient(ctx)`
while importing nothing from `account` — only `conns` imports it. The closure has
to be computed by walking `*Client(ctx)` call sites across the service tree to a
fixpoint, mapping method name → package via `awsclient_gen.go`. That is ~20 lines,
and getting it wrong fails the build rather than shipping something subtly broken.

**True EC2 closure: 5 of 267** — `account`, `ec2`, `organizations`, `s3`,
`s3control`. `s3`/`s3control` are a fixed floor forced by `conns` itself
(`awsclient.go:265` hardcodes `c.S3Client`), not by EC2.

### The linked floor is larger than the closure

The trimmed binary links **16** service clients, not 5: beyond the closure it
carries `sts`, `sso`, `ssooidc`, `signin`, `iam`, `dynamodb`, `sns`, `sqs`,
`apigatewayv2`, `resourcegroupstaggingapi`, pulled in by `conns`/`awsbase`
authentication and shared plumbing. Still 16 of 267, but a generator author should
expect it.

### Relation to the AWS SDK codegen patches

Independent and additive, but not comparable in size. Per-family linking is worth
~286 MiB of SDK symbols and lives entirely inside the Upbound fork. The
error-deserializer refactor (revalidated separately, still applies to upstream
HEAD, still not upstreamed) is worth −9.6% of `deserializers.go` in whichever
services survive the trim — on a 5-service binary, a small slice of an already
small number.

**Per-family linking is the dominant term and needs nothing from upstream AWS.**

---

## The change can be additive: accessors do not root at link time

**Correction to the section above.** It states that both generated files must be
trimmed. That was inferred from watching `service/backup` be *compiled* with only
the registry trimmed — but compilation is not linking, and the question is what
reaches the binary.

Tested directly with a synthetic program: a struct with two typed accessors,
`BackupClient` and `STSClient`, where only `STSClient` is ever called.

```
backup (accessor never called): 0.01 MiB, zero operation symbols (ListBackupJobs: 0 hits)
sts    (accessor called):       0.02 MiB, linked normally
```

An uncalled typed accessor **does not root its package**. The linker drops the
method as unreachable and the SDK client package with it.

**Therefore `internal/conns/awsclient_gen.go` does not need to change.** Its 266
accessors cost *compile* time only. The sole link-time root is
`servicePackages()`, reached from `provider.NewProvider` at `provider.go:308`.

This separates the two concerns cleanly:

* **image size** — a small, tag-gated change to one generated registry file;
* **compile time and build disk** — unaffected by the above, and addressed only
  by the AWS SDK codegen work upstream (`aws/smithy-go`, `aws/aws-sdk-go-v2`).

**Unverified:** the mechanism is proven synthetically; the *result* on the real
provider is not. The 132.4 MB figure came from trimming both files. Whether
registry-only reaches the same number needs one full link (~20 GB transient),
which did not fit in the analysis environment. **Measure this before proposing
anything.**

## Three designs, ranked by conflict surface

The fork's most valuable property is that it is **105 lines across 2 new files**
(`xpprovider/xpprovider.go` 76, `internal/conns/awsclient_xp.go` 29) with **zero
edits to upstream-owned code** — which is why it rebases effortlessly against a
repo that regenerates every release. Any proposal should try to preserve that.

1. **Pure additive, zero edits.** New `internal/provider/sdkv2/provider_scoped.go`
   duplicating `NewProvider` (281 lines, mostly a schema literal) but taking an
   injected `[]conns.ServicePackage`; new `xpprovider.GetProviderForServices(...)`;
   build-tagged new files carrying each family's list. Perfect additive property.
   Cost: 281 duplicated lines that must track upstream **silently** — drift
   produces no merge conflict. Mitigate with a test asserting both constructors
   yield identical providers for the full service set.

2. **One build-tag line — recommended.** Add `//go:build !xp_scoped` to
   `service_packages_gen.go`; ship a new `service_packages_scoped_gen.go` under
   `//go:build xp_scoped` with the trimmed list. A single comment line in one
   upstream-owned file, plus new files. No duplication of `NewProvider`. The
   failure mode is **loud**: if regeneration drops the tag, duplicate
   `servicePackages` definitions fail the compile rather than silently reverting.
   The line can be stamped by a build step after `go generate`, leaving the repo
   carrying only new files plus a Makefile addition.

3. **Full generator patch** — patch `internal/generate/servicepackages/main.go`
   (and `awsclient/main.go` if ever needed). Largest surface; only worth it if
   this becomes a first-class upstream feature.

## Framing for maintainers

**Pitch image size, not memory.** The ~286 MiB of SDK symbols is clean,
file-backed; `container_memory_working_set_bytes` subtracts it as those pages age
out. A memory framing invites a correct rebuttal. The defensible argument is
**178 published images at ~1.2 GB each**, with a measured path to ~130 MB —
registry storage, pull latency per node, image GC pressure.

**Ask for a decision, not a merge.** A drive-by PR making the first-ever edits to
IBM-maintained generators asks maintainers to accept a permanent rebase tax on a
stranger's measurement. An issue carrying the measurement, the reproduction, the
honest maintenance cost, and the question "would you take this?" is far more
likely to get a reply.

**Disclose the sharp edges yourself:** the closure is invisible to `go list -deps`
(services reach each other through `AWSClient` methods, so `organizations` needs
`account`); the linked floor is 16 service clients, not the 5 in the closure; and
a client reached by interface dispatch or reflection would fail at **runtime** in
a trimmed binary rather than at build time. That last is the only genuine
correctness risk, and naming it is what makes the rest credible.

**`go build -overlay` is not an escape hatch** — it explicitly cannot replace
files beneath `GOMODCACHE`, so the trim cannot be driven from provider-upjet-aws
without vendoring the fork or a `replace` to a local checkout.
