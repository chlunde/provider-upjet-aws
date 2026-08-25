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

Now measured as the largest win after the binary itself: this work is **24 of
the 25 seconds of startup**, and the arena it grows to absorb its own garbage is
**386 MB of anonymous RSS** — the half a pod's working-set metric counts in full.

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
