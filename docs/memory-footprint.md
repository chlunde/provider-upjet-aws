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

**Roughly 90% of the resident memory is the executable image itself, not the
heap.** After the whole startup path has run — every API group registered, the
Terraform AWS provider constructed, both cluster-scoped and namespaced
`config.Provider`s built — live Go heap is only about **69 MiB**, while RSS is
about **1.0 GiB**, of which the overwhelming majority is text and rodata paged
in from the binary.

That is why safe-start does not help. Safe-start gates *controller startup* on
CRD existence, and MRAP controls *which CRDs exist*. Both work, and both are
already wired up here (`spec.capabilities: [SafeStart]` in
`package/crossplane.yaml.tmpl`, the `customresourcesgate` plumbing in
`cmd/provider/*/zz_main.go`). But an unstarted controller was never the
expensive thing. The expensive thing is that every family binary links, and
therefore pages in, the API types of all 178 API groups in both scopes and the
Terraform provider and AWS SDK client for all ~270 AWS services.

To make memory scale with the started types, the binary has to get smaller.

## Measurements

### Resident memory is file-backed, not heap

`hack/memprofile/startup` walks the same sequence as `cmd/provider/<family>/zz_main.go`:

```
0. process start (binary linked, inits run)      live=    20.2 MiB   RSS=  606.6
1. clusterapis.AddToScheme                       live=    22.2 MiB   RSS=  610.9
2. namespacedapis.AddToScheme                    live=    24.1 MiB   RSS=  613.0
3. apiextensionsv1.AddToScheme                   live=    24.1 MiB   RSS=  613.1
4. xpprovider.GetProvider (TF SDKv2 + FW)        live=    30.8 MiB   RSS=  662.4
5. config.GetProvider (cluster)                  live=    56.3 MiB   RSS=  837.3
6. config.GetProviderNamespaced                  live=    68.7 MiB   RSS= 1058.0   peak 1067.6
```

Two things stand out.

* **RSS is already 607 MiB before a single line of provider logic runs.** That
  is the cost of having the code in the binary: package `init` functions across
  thousands of packages, plus the runtime touching text, rodata and pclntab.
* **The entire startup path only adds ~49 MiB of live heap.** Scheme
  registration for all 8,488 GVKs costs 4 MiB. The two `config.Provider`s cost
  38 MiB between them.

`smaps_rollup` confirms the split. For a program that links only
terraform-provider-aws and does nothing else:

```
Rss: 388644 kB | Private_Clean: 348772 kB | Anonymous: 38236 kB
```

`Anonymous` — the actual heap and stacks — is 38 MiB. `Private_Clean` — pages
faulted in from the executable — is 349 MiB.

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
release build (`go.build` in `build/makelib/golang.mk`) compiles every subpackage
in one `go build` invocation with an empty `GO_TAGS`, against untagged sources.

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
which is what drives the ~230 MiB gap between steady-state RSS and peak RSS.

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
* Make the release build actually use the tags. `GO_STATIC_PACKAGES` builds
  every subpackage in a single `go build` with one `GO_TAGS` value, so the
  Makefile needs one invocation per family with `GO_TAGS=<family>`, and
  `scripts/tag.sh` has to run for builds rather than only for lint.

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

Heap rather than image, so smaller in absolute terms, but it also removes the
peak-RSS spike and a chunk of startup latency.

* Filter `config.Provider.Resources` to the family. The mapping is already
  statically known in-repo — `config/groups.go` plus upjet's default
  group derivation — so a per-family include list can be generated. Measured at
  **-11.4 MiB** of live heap for the resource configs themselves, on top of
  avoiding ~1,000 `SchemaFunc()` materialisations and traversals.
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
`pkg/pipeline` and `pkg/examples` read. Measured at **-17.2 MiB** of live heap.

## What does not help

* **Safe-start and MRAP.** Both are already in place, and both address a
  different cost — API server memory per established CRD, and informer caches
  per started controller. Neither touches the resident code.
* **Stripping the binary.** `-ldflags="-s -w"` removes ~730 MB of DWARF from
  the file, which halves image pull size, but DWARF is never resident so RSS is
  unchanged.
* **`GOGC` / `GOMEMLIMIT` tuning.** Live heap is 69 MiB against ~1 GiB RSS.
  There is very little heap to tune.
