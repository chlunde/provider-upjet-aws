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
  candidate optimisations and reports what each would reclaim. A P1-P3
  section times the scope-independent parse phases inside each
  `config.GetProvider*` build in isolation (run from the repository root so
  it can read `config/schema.json`).

* `hack/memprofile/reconcile` measures the per-reconcile costs of the Connect
  and Observe path — the schema rebuilds, the AWS client and framework provider
  construction — and checks whether this provider's schema edits survive into
  the schema the Terraform SDK actually uses. It also prints the complete
  schema-divergence inventory, probes `SchemaFunc` pointer stability, measures
  the Terraform Plugin Framework per-Connect work, and runs diff experiments
  for the shared-singleton contamination. Section 8 measures the steady-state
  params->cty->InstanceState->diff translation cost with `SchemaFunc` cleared,
  and section 9 the typed-MR JSON round trips and the per-object DeepCopy the
  state-metrics recorder pays. With `SCHEMA_DUMP=<path>` it writes a
  flag dump of the fully configured provider for comparison with
  `hack/memprofile/schemadump`. See `docs/reconcile-workflow.md`,
  `docs/reconcile-workflow-detail.md` and `docs/architecture-wins.md`.

* `hack/memprofile/steadystate` answers what happens *after* startup. Every
  other program here reports the instant the startup path finishes; this one
  runs the same startup path and then holds the process open for an
  observation window, sampling `/proc/self/smaps_rollup` and `runtime.MemStats`
  together on a fixed interval and **never calling `runtime.GC()` while
  sampling**, so what it shows is what the runtime does unprompted. It answers
  three questions: whether Go's background scavenger returns the idle startup
  heap on its own and how fast, what sustained reconcile-shaped work re-grows
  the anonymous footprint to after an explicit `debug.FreeOSMemory()`, and
  whether `GOMEMLIMIT` changes either.

  Its `WORKLOAD=reconcile` mode is a **proxy**, not a real reconcile: there is
  no cluster and no AWS account. Per iteration it replays the pure-CPU parts of
  Connect+Observe — the `CoreConfigSchema()`/`SchemaFunc` schema rebuilds, the
  params->cty->InstanceState->InstanceDiff->JSON-map round trip, and the
  typed-MR JSON round trips and `DeepCopy` — i.e. sections 3, 4, 8 and 9 of
  `hack/memprofile/reconcile`, in a loop. It does **not** model the AWS SDK
  request/response cycle, controller-runtime's informer cache (a real
  steady-state consumer that grows with the MR count and that this proxy has
  none of), client-go, the workqueue, or the per-Connect AWS client
  construction. It is therefore a lower bound on per-reconcile churn with no
  growing live set.

  | variable | effect |
  | --- | --- |
  | `WORKLOAD=idle` (default) or `reconcile` | what runs during the window |
  | `DURATION=15m` | length of the observation window (default `10m`) |
  | `INTERVAL=15s` | sampling period (default `15s`) |
  | `SCAVENGE_AFTER_STARTUP=1` | one `debug.FreeOSMemory()` before the window |
  | `SCAVENGE_EVERY=5m` | `debug.FreeOSMemory()` on a ticker inside the window |
  | `QPS=2` | reconcile iterations per second; `0` (default) runs flat out |

```console
go build -o /tmp/steadystate ./hack/memprofile/steadystate

# does the background scavenger return the startup heap on its own?
WORKLOAD=idle DURATION=15m INTERVAL=15s /tmp/steadystate
GOMEMLIMIT=300MiB WORKLOAD=idle DURATION=15m INTERVAL=15s /tmp/steadystate

# what does sustained reconcile-shaped work re-grow it to?
SCAVENGE_AFTER_STARTUP=1 WORKLOAD=reconcile QPS=2 DURATION=15m /tmp/steadystate
```

* `hack/memprofile/schemadump` prints the same flag dump from a pristine
  process — the Terraform AWS provider without any of this repository's
  `config/` edits. Diffing it against the `SCHEMA_DUMP` output separates
  deliberate schema edits from accidental mutations of schema objects shared
  between resources.

* `hack/memprofile/linkcost/*` measure the **link cost** of a package set: each
  program links a different slice of the provider and reports its RSS before it
  does any work. The difference between them is memory the process pays purely
  for having the code in the binary.

```console
go run ./hack/memprofile/startup
go run ./hack/memprofile/reconcile
go run ./hack/memprofile/schemadump > pristine.dump
SCHEMA_DUMP=$PWD/configured.dump go run ./hack/memprofile/reconcile
go run ./hack/memprofile/linkcost/allapis      # every API group, both scopes
go run ./hack/memprofile/linkcost/familyapis   # one family's API group only
go run ./hack/memprofile/linkcost/tfaws        # terraform-provider-aws
```

`Anonymous` in the reported `smaps_rollup` line is the heap and stacks;
`Private_Clean` is executable text/rodata paged in from the binary.

## The one-family experiment

`hack/memprofile/startup` can measure what happens if the provider configures
only one family's resources instead of all 1,029, in the *same binary*, so the
comparison isolates what is built at run time from what is linked.

`UPJET_FAMILY_FILTER=<short group>[,<short group>...]` (read by
`config/registry_common.go`) filters the three include lists this provider hands
to upjet's `config.NewProvider` down to the named API short groups. A resource
in none of the include lists is skipped by `NewProvider` outright - before
`DefaultResource`, before `SchemaFunc()` is called on it, before the schema
traversers and before the registry metadata is attached. The variable is inert
when unset, so the shipped behaviour is unchanged.

The filter derives the short group statically from the resource name, the same
way `config.DefaultResource` and this repo's `GroupKindOverrides()` do; `NAMES`
below exists to verify that it picks exactly the same set the full build ends up
assigning to that group.

Extra knobs on `hack/memprofile/startup`:

| variable | effect |
| --- | --- |
| `UPJET_FAMILY_FILTER=ec2` | configure only the `ec2` family's resources |
| `STOP_AFTER_STARTUP=1` | stop after step 6, so `VmHWM` is the startup peak and not the simulations' |
| `STOP_AFTER_STEP=4` | stop after the given step and print the process counters |
| `PHASES_ONLY=1` | run *only* the include-list-independent parse phases P1-P3 |
| `NAMES=<file>` | dump `<terraform name>\t<short group>` for every configured resource |

```console
go build -o /tmp/startup ./hack/memprofile/startup   # build once, run many times

STOP_AFTER_STARTUP=1 NAMES=/tmp/all.names /tmp/startup
UPJET_FAMILY_FILTER=ec2 STOP_AFTER_STARTUP=1 NAMES=/tmp/ec2.names /tmp/startup
# the filter is exact if these agree:
awk -F'\t' '$2=="ec2"{print $1}' /tmp/all.names | sort | diff - <(cut -f1 /tmp/ec2.names | sort)

PHASES_ONLY=1 /tmp/startup          # the arena the whole-file parses alone force
STOP_AFTER_STEP=4 /tmp/startup      # the arena with no config.Provider build at all
```

See `docs/memory-footprint.md` on the `claude/upjet-aws-memory-optimization-pnnsbl`
branch for the baseline results and what they imply, and this branch's commit
message for the one-family comparison.
