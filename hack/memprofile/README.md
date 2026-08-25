<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# Provider memory profiling harness

Throwaway instrumentation used to find out where a family provider's resident
memory goes. Not built or shipped as part of the provider.

Two kinds of measurement:

* `hack/memprofile/startup` reproduces the provider's startup allocation path
  (`AddToScheme` -> `xpprovider.GetProvider` -> `config.GetProvider{,Namespaced}`)
  and attributes **live heap** and **RSS** to each step. It then simulates a few
  candidate optimisations and reports what each would reclaim.

* `hack/memprofile/linkcost/*` measure the **link cost** of a package set: each
  program links a different slice of the provider and reports its RSS before it
  does any work. The difference between them is memory the process pays purely
  for having the code in the binary.

```console
go run ./hack/memprofile/startup
go run ./hack/memprofile/linkcost/allapis      # every API group, both scopes
go run ./hack/memprofile/linkcost/familyapis   # one family's API group only
go run ./hack/memprofile/linkcost/tfaws        # terraform-provider-aws
```

`Anonymous` in the reported `smaps_rollup` line is the heap and stacks;
`Private_Clean` is executable text/rodata paged in from the binary.

See `docs/memory-footprint.md` for the results and what they imply.
