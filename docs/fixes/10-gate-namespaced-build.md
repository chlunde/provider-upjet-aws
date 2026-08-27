<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 10. Build the namespaced provider only when a namespaced MR exists

| | |
| --- | --- |
| **Category** | waste — startup time and resident memory |
| **Severity** | high |
| **Size** | medium |
| **Lives in** | this repo (gating) + upjet (parse option) |
| **Evidence** | measured |

## What happens

Startup builds two complete `config.Provider`s — cluster-scoped and namespaced
— each parsing the embedded 14.7 MB `schema.json` and 7.3 MB
`provider-metadata.yaml` and constructing all 1,029 resource configurations.
Most installations use one scope.

| | cost |
| --- | ---: |
| whole startup path | 25 s |
| namespaced build alone | 7.6 s, ~190 MB RSS growth |
| scope-independent parsing per build | ~1.06 s, ~107 MB allocations |

The parse breaks down as tfjson 740 ms, `GetV2ResourceMap` 97 ms, metadata
219 ms — and `GetV2ResourceMap`'s output is **discarded for all 960 SDK
resources** (upjet `pkg/config/provider.go:436` overwrites it with the Go
schema), so for this provider it is pure waste.

## The fix

Two independent changes, in order of payoff:

1. **Gate the namespaced build behind the safe-start gate that is already
   wired.** `cmd/provider/*/zz_main.go` builds both providers eagerly, then
   registers gated controllers. Defer `GetProviderNamespaced` until a
   namespaced CRD is actually established. Saves the full 7.6 s and ~190 MB
   when no namespaced MR is in use. Symmetrically for the cluster scope.
2. **Skip the runtime-dead parse.** Ask upjet for an option that skips
   `GetV2ResourceMap` when every resource is plugin-SDK or Framework backed
   (`CLIReconciledExternalNameConfigs` is empty for AWS), and skips the
   registry-metadata parse outside code generation. ~1 s and ~100 MB of garbage
   per build.

The generated `zz_main.go` comes from `config/templates`, so change the
template, not the 178 generated files.

## How to test

* **Bench:** `hack/memprofile/startup` sections P1–P3 already time the parse
  and each scope's build; assert the namespaced build does not run when no
  namespaced CRD is present.
* **e2e:** install the family provider with only cluster-scoped MRs, confirm
  startup time and steady RSS drop and that no namespaced controller starts.
* **e2e:** then apply a namespaced MR and confirm the namespaced provider is
  built on demand and the controller starts. This is the risk case — the gate
  must be able to fire *after* startup.

## Suggested issue

Repo: `crossplane-contrib/provider-upjet-aws`

**Title:** `Both scoped config.Providers are built eagerly at startup, costing 7.6 s and ~190 MB for the unused scope`

**Body:**

> `cmd/provider/<family>/zz_main.go` calls both `config.GetProvider` and
> `config.GetProviderNamespaced` unconditionally before registering
> controllers. Measured locally: the whole startup path is 25 s, of which the
> namespaced build alone is 7.6 s and ~190 MB of RSS growth.
>
> Most installations use one scope. The safe-start gate that decides whether a
> controller starts is already wired up, so the corresponding provider
> configuration could be built lazily behind it.
>
> Separately, ~1.06 s and ~107 MB per build is scope-independent parsing of the
> embedded JSON schema and registry metadata, and `GetV2ResourceMap`'s output
> is discarded for all 960 plugin-SDK resources
> (`upjet/pkg/config/provider.go:436`) — an upjet option to skip it at runtime
> would remove that too.

## Branch

`fix/lazy-namespaced-provider-build`
