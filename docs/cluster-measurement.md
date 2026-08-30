<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# What a family provider pod actually costs

[`docs/memory-footprint.md`](memory-footprint.md) measures processes that link
the same packages a family provider links, through `/proc/self/smaps_rollup` and
`runtime.MemStats`. This document measures **the provider itself, in a cluster,
reconciling real managed resources**, through the number Kubernetes actually
charges it: its cgroup.

Harness, raw samples and reproduction steps:
[`hack/clustermeasure`](../hack/clustermeasure/README.md).

## Setup

* kind, one node, Docker Desktop on darwin/arm64, LinuxKit kernel 6.12.76.
* `cmd/provider/s3` built from this tree (`claude/cluster-measure`), linux/arm64,
  `-ldflags="-s -w"`, **980 MB stripped**. One binary for every arm **in round 1**;
  from round 2 onward there are ten binaries and several deltas are therefore
  cross-binary - see [`hack/clustermeasure/BINARIES.md`](../hack/clustermeasure/BINARIES.md)
  for which binary produced which arm, and [round 10](#round-10-what-review-found)
  for which claims that invalidates.
* Run as a plain Deployment with the s3 family's CRDs applied, `--poll=1m
  --max-reconcile-rate=10 --skip-default-tags`, against LocalStack.
* Workload: 50 `s3.aws.upbound.io/v1beta1` `Bucket` MRs, all reaching
  `Ready=True`, then polled for eight minutes.
* **`podMEM` is the container's `memory.current`.** `inactive_file` is 4 kB
  throughout, so `container_memory_working_set_bytes` is the same number.
* Sampled every 20 s. `idle` is the median of 3 samples before any MR exists;
  `steady` the median of 15 samples over the last five minutes of the run.

All figures MiB.

## Results

Node default, `transparent_hugepage=always`:

| arm | podMEM idle | podMEM steady | podMEM peak | Ready | config build |
| --- | ---: | ---: | ---: | ---: | ---: |
| baseline | 535.5 | **382.2** | 537.6 | 17 s | 13.7 s / 1,029 res |
| baseline (repeat) | 521.0 | **388.7** | 523.9 | 14 s | 13.4 s / 1,029 res |
| `UPJET_FAMILY_FILTER=s3` | 424.2 | **305.6** | 428.4 | 4 s | 1.13 s / 25 res |
| `GOMEMLIMIT=300MiB` | 333.3 | 292.2 | 376.4 | 16 s | 15.3 s |
| filter + `GOMEMLIMIT` | 302.1 | 269.3 | 375.8 | 2 s | 1.19 s |
| one-shot `FreeOSMemory()` | 415.3 | 389.9 | 554.6 | 16 s | 13.4 s (+23 ms) |
| `GODEBUG=disablethp=1` | 396.3 | **205.5** | 397.6 | 14 s | 13.2 s |
| `FreeOSMemory` + `disablethp` | 164.4 | 201.2 | 393.5 | 14 s | 13.3 s |
| filter + `disablethp` | 324.8 | **142.4** | 349.0 | 2 s | 1.14 s |

Same binary, same workload, node set to `transparent_hugepage=madvise`:

| arm | podMEM idle | podMEM steady | podMEM peak |
| --- | ---: | ---: | ---: |
| baseline | 354.8 | **201.4** | 401.6 |
| filter | 120.7 | **143.1** | 330.2 |

Reproducibility: the two baseline runs, first and last of nine, agree to 1.7% at
steady state and 2.7% at idle. Treat differences under ~3% as noise.

## 1. The startup win is real, and it is bigger in a cluster

`UPJET_FAMILY_FILTER=s3` cuts the two `config.GetProvider*` builds from
**13.7 s to 1.13 s** and the resource set from 1,029 to 25. Pod `Ready` goes
from **17 s to 4 s**, and to **2 s** with `GOMEMLIMIT` also set.

The 50 buckets reconcile identically under the filter - same 100 s to all-Ready,
no panic, no missing controller. `memory-footprint.md` lists three things that
"still need a cluster": a filtered family binary reaching `Ready` on a real MR,
cross-family reference resolution, and webhook setup. The first two are now
observed for s3. **Webhooks are not**: this harness runs with `--certs-dir=`,
so `SetupWebhookWithManager_s3` never runs.

## 2. Transparent huge pages, not the Go heap, decide what the pod reports

The single largest lever measured is a setting, not a patch:

| | steady podMEM |
| --- | ---: |
| baseline, THP `always` | 382-389 |
| baseline, `GODEBUG=disablethp=1` | 205.5 |
| baseline, node THP `madvise` | 201.4 |

**-180 MiB, -47%, from one environment variable and no code change.** The
per-process `disablethp` and the node-level `madvise` agree to 2%, which is what
you would expect if they act on the same mechanism, and is the check that the
effect is THP and not something else.

The mechanism is visible in the counters. In the baseline the process's own
`smaps` `Anonymous` reads 379 MiB while the cgroup's `active_anon` reads the
same 379 MiB - but in the repeat run `Anonymous` is 203.8 MiB while
`active_anon` is 385.6 MiB. The node reports `thp_deferred_split_page 952`
against `thp_split_page 0`: when Go's scavenger `MADV_DONTNEED`s a range inside
a huge page, the kernel unmaps the sub-range - which is what `smaps` reports -
and queues the compound page for a split it performs only under memory pressure.
Until then the whole 2 MiB stays charged to the container.

So the provider's heap is returned, and the pod is billed for it anyway.

## 3. `FreeOSMemory()` buys a pod a fraction of what it buys `smaps`

`docs/fixes/23-release-startup-heap.md` was retracted on the grounds that the
background scavenger gets there on its own. The cluster agrees, and adds a
second reason to not ship it:

| at idle | `smaps` Anonymous | podMEM |
| --- | ---: | ---: |
| baseline | 385.2 | 535.5 |
| one-shot `FreeOSMemory()` | 166.0 | 415.3 |
| delta | **-219** | **-120** |

The call does what it says at the process level and less than half of that
reaches the pod's bill, for the reason in §2. By steady state it is worth
nothing at all: 389.9 against a 382-389 baseline. Under `disablethp` it moves
idle from 396.3 to 164.4 and steady still lands on the same 201-205.

**A one-shot scavenge shortens the post-startup window and changes no
steady-state number.** That is now measured on a pod rather than inferred.

## 4. `smaps` is not a proxy for what a pod is charged

The two baseline runs differ by **175 MiB** in `smaps` `Anonymous` (379.1 vs
203.8) and by **6.5 MiB** in podMEM (382.2 vs 388.7). The process-level view is
the unstable one; the cgroup view is the reproducible one.

Every number in `memory-footprint.md` is a `smaps` or `MemStats` number. They
are correct about the heap and they are not predictive of the metric anyone
alerts on.

## 5. Filtering the resource set does help a pod

`memory-footprint.md` corrects itself to "filtering the resource set is a
startup win, not a memory win: -12.5% unconstrained and nothing under
`GOMEMLIMIT`". On a pod doing real work it is worth more than that:

| | baseline | filter | delta |
| --- | ---: | ---: | ---: |
| steady, THP `always` | 382.2 | 305.6 | **-76.6 (-20%)** |
| steady, THP `madvise` | 201.4 | 143.1 | **-58.3 (-29%)** |
| steady, under `GOMEMLIMIT=300MiB` | 292.2 | 269.3 | -22.9 (-8%) |
| peak (sets the pod's limit) | 537.6 | 428.4 | **-109.2 (-20%)** |

The earlier conclusion was drawn from a process that builds the provider and
exits. A pod keeps the 1,029 `config.Resource` graphs live for its whole life
while it reconciles, and that is where the steady-state difference comes from.
The claim that filtering does nothing *under `GOMEMLIMIT`* survives directionally
- -8% is small - but the limit is not free either: it costs 1.6 s of startup and
does not reduce peak below 376 MiB.

## 6. Only a third of the binary is ever paged in

`Private_Clean` sits at **278-313 MiB** across every arm, against a 980 MB
binary. `memory-footprint.md` reports 692 MB for a harness that walks the whole
startup path; a provider pod actually running s3 controllers faults in less than
half of that. The filtered arms are consistently ~15 MiB lower than the
unfiltered ones - less code runs, so fewer text pages are touched.

**Caveat, and it cuts against the pod numbers above:** the binary is mounted by
`hostPath`, and its page cache was charged to the root cgroup by the `docker cp`
that put it there. `cg_file` is 4 kB in every sample, so **podMEM here is
essentially all anonymous and excludes the executable entirely**. A pod running
the same code from an image layer it faulted itself would be charged for those
pages until they age onto the inactive-file LRU, at which point
`container_memory_working_set_bytes` stops counting them. Read the podMEM column
as the anonymous half; add up to ~300 MiB transiently for the other half.

## 7. How the footprint scales with managed resources

Same arms, 500 `Bucket` MRs instead of 50, THP `always`:

| arm | 50 MRs steady | 500 MRs steady | delta | per MR |
| --- | ---: | ---: | ---: | ---: |
| baseline | 382-389 | **486.7** | +101 | ~0.20 MiB |
| filter | 305.6 | **351.1** | +46 | ~0.09 MiB |

All 500 reach `Ready` - in 234 s baseline, 193 s filtered.

Two things follow. The per-MR cost is real but small next to the fixed cost: ten
times the resources adds a quarter to the pod's memory, because informer cache,
workqueue and the AWS SDK request cycle are cheap next to the arena the provider
allocates before it reconciles anything. And the filter's advantage **widens**
with scale in absolute terms - it is not a constant offset that scale washes out.

This is the measurement `memory-footprint.md` asks for when it says the proxy
workload "never modelled controller-runtime's informer cache, client-go, the
workqueue, or the AWS SDK request cycle. That is where to look." It was worth
looking, and it is not where the memory is.

## What to do

Ranked by measured effect on the steady-state pod metric, cheapest first.

1. **Set `GODEBUG=disablethp=1`** in the provider's `DeploymentRuntimeConfig`,
   or run nodes with `transparent_hugepage=madvise`. -180 MiB, -47%, no code
   change, no upstream change. Verify your nodes' setting first: this is a
   no-op where THP is already `madvise`, which is the default on several
   distributions.
2. **Ship the per-family include list.** -20% steady, -20% peak, and 13.7 s ->
   1.1 s of startup. Needs the `elbv2` `ShortGroup` override handled -
   `docs/memory-footprint.md` has the design call - and webhook setup still
   needs checking.
3. **`GOMEMLIMIT`** caps peak at ~376 MiB and is worth having as a guard, but it
   composes poorly with the filter (-8% on top) and costs startup time.
4. **Do not ship a one-shot `FreeOSMemory()`.** Already retracted; the cluster
   confirms it changes no steady-state number.

## What this does not cover

* One node, arm64, one family, LocalStack rather than AWS, `n=1` per arm
  (`n=2` for baseline), 50 MRs (500 in §7), and 8 minutes of steady state.
* No webhooks, no leader election, no Crossplane package manager, no
  cross-family reference resolution exercised.
* `--skip-default-tags` is set, and an S3 Control tag shim stands in for a
  LocalStack gap, so the tag path is lighter than production.
* The executable's resident pages are not charged to the pod here; see §6.


---

# Round 2: how much lower can it go, and what breaks first

The first round asked what a pod costs and which of the existing proposals move
it. This round asks what is left after `disablethp`, what happens under a real
memory limit, and whether the embedded blobs can simply not be parsed.

Same harness. Every arm below carries `GODEBUG=disablethp=1` unless stated, so
the numbers are the cgroup's view of the runtime's own behaviour rather than the
huge-page inflation measured in round 1. Arms after a machine reboot are marked;
they are internally consistent (`ctrl-rerun` reproduces `nothp-ctrl` to 1.5%).

## Where the remaining memory is

A heap profile and a goroutine dump from a steady-state pod (unfiltered,
`disablethp`, 50 MRs), taken through `UPJET_PPROF_ADDR`:

| | |
| --- | ---: |
| live heap (`inuse_space`, after GC) | **72.6 MiB** |
| goroutine stacks | 28.7 MiB |
| GC metadata, mspan, buckhash, other | ~15 MiB |
| resident heap arena (`heap_sys` 359 − `heap_released` 178) | ~181 MiB |
| `go_memstats_sys_bytes` | 406.2 MiB |

**The live object graph is not the problem and never was.** Its largest single
line is upjet's `DefaultResource` at 4.5 MB; then `reflect.compiledTypelinks`
4.1 MB and `Scheme.AddKnownTypeWithName` 5.7 MB (the 8,488 GVKs - so per-family
API packages have an anonymous-memory component too, not just text), and ~12 MB
of compiled regexes. What a pod holds is the **arena high-water mark left by
startup**, which is why every lever below is really a lever on the peak.

Two side findings from the same dump:

* **6,278 goroutines, 5,001 of them parked in controller-runtime's priority
  queue - 28.7 MiB of stacks.** That is with the shipped default
  `--max-reconcile-rate=100`. At `=10` the same workload runs ~1,500 goroutines
  and ~7 MiB. The default costs **~22 MiB of stacks**.
* Goroutines also scale with the managed-resource count, at roughly 5 per MR:
  50 MRs → 1,735, 500 MRs → 4,101.

## Under a real memory limit: reclaim, until it isn't

`memory.max` is enforced against `memory.current`, so the huge-page inflation
from round 1 is charged against the limit, not just reported.

| limit | THP | `GOMEMLIMIT` | peak | outcome |
| --- | --- | --- | ---: | --- |
| 512Mi | on | - | 512.0 (= limit) | Ready, 0 kills, 1 reclaim event |
| 512Mi | off | - | 391.3 | Ready, no events |
| 400Mi | on | - | 400.0 (= limit) | Ready, 0 kills, 1 reclaim event |
| 400Mi | off | - | 399.1 | Ready, no events |
| 300Mi | on | - | - | **OOMKilled ×10, CrashLoopBackOff** |
| 300Mi | off | - | - | **OOMKilled ×10, CrashLoopBackOff** |
| 300Mi | on | 250MiB | 299.6 | Ready, 0 restarts |
| 300Mi | off | 250MiB | 250.6 | Ready, 0 restarts |

Two conclusions, and the second is the one that matters operationally.

**The deferred-split charge is reclaimable while there is slack.** At 512Mi and
400Mi the THP arm pressed exactly to its limit, tripped one `max` event, and the
kernel split and freed rather than killing. `deferred_split_shrinker` is
memcg-aware, and it works.

**Below the startup peak, nothing saves you - and `disablethp` is not what
saves you.** At 300Mi both arms die identically, because the Go runtime has no
idea what the cgroup limit is. `GOMEMLIMIT` is the thing that tells it, and with
it both arms run clean at 300Mi. Turning huge pages off lowers what gets
*charged*; only `GOMEMLIMIT` makes the runtime *back off*.

## Holding down the startup spike

All unfiltered, `disablethp`, 50 MRs:

| arm | peak | steady | config build |
| --- | ---: | ---: | ---: |
| control | 395.0 | 230.7 | 13.60 s |
| `GOMEMLIMIT=300MiB` | 292.9 | 228.1 | 14.99 s |
| `GOMEMLIMIT=200MiB` | 229.6 | 188.4 | **55.96 s** |
| `GOGC=25` | 265.8 | 168.6 | 17.08 s |
| `GOMAXPROCS=2` | 438.0 | 235.3 | 14.69 s |

* **Turning down concurrency does not help.** `GOMAXPROCS=2` makes the peak
  slightly *worse*. There is nothing concurrent to throttle in the startup path:
  the two `config.GetProvider` builds are sequential, and the controllers start
  after the peak.
* **`GOGC=25` is the cheap win**: −129 MiB of peak and −62 MiB of steady for
  +3.5 s. The shippable form is not the environment variable but
  `debug.SetGCPercent(25)` before the two builds and a restore after, so the CPU
  cost is paid once instead of forever.
* **`GOMEMLIMIT=200MiB` is a trap on its own**: it caps the peak at 230 MiB but
  makes startup take **56 seconds**, because the GC thrashes against the parse
  garbage. It stops being a trap once that garbage is gone - see below.

## Not parsing the blobs at all

`config.NewProvider` unmarshals 19.0 MB of `schema.json` and 7.8 MB of
`provider-metadata.yaml` before it consults any include list, once per scope.
Reading what actually consumes them (upjet `pkg/config/provider.go:376-458`):

| parsed every start | shipped | read at runtime |
| --- | ---: | ---: |
| `schema.json` → `resource_schemas` | 18.65 MB, 1,683 resources | **0.24 MB, 69** |
| `schema.json` → `data_source_schemas` | 1.81 MB, 670 | **0** |
| `provider-metadata.yaml` | 7.82 MB, 1,676 | see below |

At `provider.go:436` the JSON-derived schema is **discarded and replaced by the
Go one** for every Terraform Plugin SDK resource - 960 of the 1,029 this
provider configures. Only the 69 Plugin Framework resources need it. A further
654 resources in the file are unmarshalled, converted by `GetV2ResourceMap`, and
then dropped by the include list.

[`hack/clustermeasure/trim-embeds.py`](../hack/clustermeasure/trim-embeds.py)
rewrites both files to what is actually read - 19.0 → 0.5 MB and 7.8 → 2.8 MB -
and the provider still configures all 1,029 resources:

| arm (all `disablethp`, 1,029 resources) | idle | steady | peak | config build |
| --- | ---: | ---: | ---: | ---: |
| control | 373.9 | 227.2 | 379.5 | 13.61 s |
| trimmed embeds | **174.3** | 211.1 | **174.7** | 12.96 s |
| trimmed + `GOMEMLIMIT=200MiB` | 169.9 | 188.5 | **191.7** | **12.88 s** |

* **The startup peak halves, 380 → 175 MiB.** The provider now never allocates
  more during startup than it holds afterwards.
* **It is a peak-memory change, not a startup-time one**: 13.61 s → 12.96 s. The
  13 seconds are `SchemaFunc()` materialisation, which is what the family filter
  removes. The two changes are orthogonal and compose.
* **It rescues `GOMEMLIMIT=200MiB`**, whose startup goes from 56 s to 12.9 s
  once there is no parse garbage to thrash against.

### What blocked the metadata half

The first attempt panicked at startup:

```
panic: runtime error: index out of range [0] with length 0
  config/cluster/apigatewayv2.Configure.func4  config/cluster/apigatewayv2/config.go:50
```

which is `r.MetaResource.Examples[0].SetPathValue("lifecycle", nil)`. **The
running provider parses 2.9 MB of example manifests on every start so that a
configurator can edit one of them for the documentation pipeline.** 368 of the
1,676 resources have such a configurator, which is why the metadata only goes to
2.8 MB here rather than 0.3. Guarding those configurators - or splitting the
configurator chain into generation-time and runtime halves - is the precondition
for dropping the metadata parse entirely.

**This script is a measurement tool, not a shippable change**: `cmd/generator`
embeds the same two files and needs them whole. Shipping it means generating a
runtime copy at build time and selecting it by build tag.

## Best of all, and it fits in 256Mi

| | baseline | trim + filter + `disablethp` + `GOMEMLIMIT=200MiB` |
| --- | ---: | ---: |
| podMEM steady | 382.2 | **165.0** |
| podMEM idle | 535.5 | **114.2** |
| peak | 537.6 | **186.5** |
| config build | 13.67 s | **0.49 s** |
| Ready | 17 s | 4 s |
| pod memory limit | OOMKilled at 300Mi | **runs clean at 256Mi** |

−57% steady, −65% peak, −96% startup, and it schedules in half the memory.

**But the lowest steady number is a different arm.** `filter + disablethp` alone
parks at **142.4 MiB**, below the all-in-one's 165, because `GOMEMLIMIT` makes
the scavenger target the limit rather than minimum footprint. The two goals pull
apart:

* **lowest steady metric** → filter + `disablethp`: 142.4 steady, but peak 349,
  so the pod still needs a ~400Mi limit.
* **smallest schedulable pod** → add the trim and `GOMEMLIMIT`: peak 187,
  steady 165, runs in 256Mi.

## Revised recommendations

1. **`GODEBUG=disablethp=1`** (or nodes on `transparent_hugepage=madvise`) -
   −47% of the reported number, no code change. Round 1.
2. **`GOMEMLIMIT`** - not for the metric, for survival. Without it the provider
   is OOM-killed at any limit below its startup peak; with it, it runs at 300Mi.
   Pick it *after* the trim, or it costs 40 s of startup.
3. **Ship the per-family include list** - startup 13.6 s → 0.5 s, and the
   largest single cut to the steady-state number.
4. **Stop parsing what is never read** - −200 MiB of startup peak. Downstream
   only for the schema; the metadata needs ~368 configurators guarded first.
5. **Reconsider `--max-reconcile-rate=100`** as a default - ~22 MiB of goroutine
   stacks against `=10`, for a family provider that rarely needs 100.
6. Still do not ship a one-shot `FreeOSMemory()`.


---

# Round 3: implementing the profile's top three, and finding out they do not matter

Round 2 ended at 113-119 MiB steady with environment variables only. This round
implements the three largest allocation sites a profile identifies and measures
whether they move the pod metric. **They do not**, and that is the result worth
recording.

## What was implemented

1. **upjet `config.matches` precompiles its patterns.** `regexp.MatchString`
   compiles its pattern on every call, and `NewProvider` calls `matches` for
   every resource in the schema against every include-list entry. Patch:
   [`hack/clustermeasure/upjet-precompile-matches.patch`](../hack/clustermeasure/upjet-precompile-matches.patch).
2. **`UPJET_CLEAR_SCHEMAFUNC=1`** drops `SchemaFunc` on every resource whose
   `Schema` upjet has already materialised, so `helper/schema.Resource.SchemaMap`
   returns the cached map instead of rebuilding it - `docs/fixes/02`, implemented
   downstream in `config/registry_common.go`.
3. **`--poll-state-metric=30s`** instead of the 5 s default.

## What the profile said they were worth

`alloc_space`, filtered and trimmed provider, process start to steady state:

| site | allocated | share |
| --- | ---: | ---: |
| `config.matches` -> `regexp.MatchString` | **575.6 MB** | 32% |
| `schemaMap.DeepCopy` -> `copystructure`/`reflectwalk` | 211 MB | 12% |
| `Resource.SchemaMap` -> `SchemaFunc()` | 65 MB | 4% |

and per 3.3 minutes of *steady* reconciliation (50 MRs, poll 1m), 835 MB total:

| site | allocated | share |
| --- | ---: | ---: |
| `schemaMapWithIdentity.DeepCopy` | 168 MB | 20% |
| `bufio.NewReaderSize` <- `http.Transport.dialConn` | 56 MB | 7% |
| `Resource.SchemaMap` -> `resourceBucket.func2` | 49 MB | 6% |
| `prometheus.Registry.Gather` | 64 MB | 8% |

## What they were actually worth

All arms: trimmed embeds + family filter + `disablethp` + `GOGC=25`, 50 MRs.

| arm | idle | steady | peak | config build |
| --- | ---: | ---: | ---: | ---: |
| control (round 2 binary) | 98.2 | 119.3 | 142.4 | 688 ms |
| + regex precompile | 96.9 | **118.4** | 141.4 | **250 ms** |
| + clear `SchemaFunc` | 98.6 | **118.9** | 137.9 | 236 ms |
| + `--poll-state-metric=30s` | 98.0 | **119.6** | 138.8 | 253 ms |

**Every memory delta is inside noise.** The one real change is wall clock: the
config build goes from 688 ms to 250 ms, **2.7x faster**, from the regex patch
alone.

### Why

The pod's footprint is

> live heap x (1 + GOGC/100) + goroutine stacks + GC metadata + span fragmentation

Garbage the collector keeps up with never becomes resident. At `GOGC=25` the heap
goal is 1.25x the *live* set, so removing 575 MB of short-lived allocation
removes CPU and GC pressure and nothing else. **An `alloc_space` profile ranks
what to fix for speed; it does not rank what to fix for footprint.** The two
questions need different profiles - and `inuse_space` is the one that answers
this document's question.

This does not make the fixes worthless. The regex patch is a 2.7x startup win for
every upjet provider. Clearing `SchemaFunc` is a **correctness** fix as well: this
repository's `config/` schema edits are written to `.Schema`, which only the diff
path reads, so everything reached through `SchemaMap()` sees the unedited upstream
schema (`docs/fixes/02-clear-schemafunc.md`). They are simply not memory fixes,
and proposing them as such would not survive review.

## So what is left, to get under 100

The live heap of a filtered, trimmed provider is **64.7 MiB**:

| | |
| --- | ---: |
| `Scheme.AddKnownTypeWithName` + conversion funcs + RESTMapper (8,488 GVKs) | ~14 MiB |
| retained compiled regexes | ~7-11 MiB |
| `reflect.compiledTypelinks` / `addModuleItabs` / `addReflectOff` | ~6 MiB |
| Terraform provider resource map | ~7 MiB |
| diffuse remainder | ~27 MiB |

The first three are consequences of **linking every API group and every AWS SDK
service into a single-family binary**. So the two structural changes already on
the list - per-family API packages, and per-family linking of the fork - are the
only remaining levers, and they are worth roughly 20-25 MiB of live heap, i.e.
25-30 MiB of pod memory at `GOGC=25`. That is what lands under 100.

### A correction to §6 of round 1

Round 1 said per-family linking cannot move the pod metric because the code it
removes is file-backed. **That is wrong in one respect.** Every linked
`aws-sdk-go-v2/service/*/internal/endpoints` package compiles the same eight
partition-region regexes at package init - byte-identical in 60 of 60 packages
sampled - so a binary linking 269 services compiles **2,152 regexes for 8
patterns** and retains **6.51 MiB of heap**, 10-12% of this provider's live set.
Per-family linking removes that too, and it is fixable upstream on its own by
hoisting the shared partitions table into one package or compiling it lazily.

## Best measured, end to end

| | stock | best |
| --- | ---: | ---: |
| steady | 382.2 | **113.4** (`GOMEMLIMIT=120MiB`) |
| peak | 537.6 | **126.3** |
| config build | 13.67 s | **0.25 s** |
| pod limit | OOMKilled at 300Mi | runs at 256Mi |


---

# Round 4: the AWS SDK patch, and where the last 8 MiB came from

Round 3 established that the footprint is live heap x (1 + GOGC/100) plus stacks,
metadata and fragmentation - so the only thing that moves it is live heap. The
largest addressable item in the live heap of a filtered, trimmed provider was
7-11 MiB of retained compiled regexes. This round finds out where they come from
and removes them.

## Where the retained regexes come from

`inuse_space`, peeking at `regexp.MustCompile`: a long tail of
`<service>/internal/endpoints.init`. Every generated
`aws-sdk-go-v2/service/*/internal/endpoints/endpoints.go` compiles **the same
eight partition region matchers** at package init:

```go
Aws:      regexp.MustCompile("^(us|eu|ap|sa|ca|me|af|il|mx)\\-\\w+\\-\\d+$"),
AwsCn:    regexp.MustCompile("^cn\\-\\w+\\-\\d+$"),
AwsUsGov: regexp.MustCompile("^us\\-gov\\-\\w+\\-\\d+$"),   // + iso, isob, isoe, isof, eusc
```

Byte-identical in 60 of 60 service packages sampled. A provider linking 269
services **compiles 2,152 regexes to represent 8**, at init, whether or not the
service is ever used.

The fix has a home already: `go list -deps` on two service clients gives 48
shared `aws-sdk-go-v2` + `smithy-go` packages against three per-service ones, and
the shared set includes `internal/endpoints/v2` - which defines the
`endpoints.Partitions` type the per-service tables instantiate. Hoisting the
compiled patterns there adds **no new dependency edge**.

## Measured

Applied mechanically in a vendored tree: eight shared vars added to
`internal/endpoints/v2`, and every `regexp.MustCompile("<pattern>")` in the
service endpoint files replaced with a reference. **2,144 calls removed across
268 of 268 files**; the per-service region overrides and `*regexp.Regexp` struct
fields stay, so no import changed and the binary grew by 131 KB.

Same cluster, same arm (trimmed embeds + family filter + `disablethp` +
`GOGC=25`), one variable changed, n=2:

| | retained regexes | live heap | idle | steady | peak |
| --- | ---: | ---: | ---: | ---: | ---: |
| before | 6.51 | 64.7 | 98.2 | 119.3 | 142.4 |
| after | **3.03** | **56.2** | 90.7 / 90.3 | **110.5 / 108.8** | 133.0 / 129.8 |

**-8.6 MiB of what Kubernetes charges the pod, -7%, reproducibly.** The
per-service `endpoints.init` tail is gone from the profile; what still compiles
regexes is `regexache` (terraform-provider-aws's own, 2.02 MiB), `go-cmp.init`
and `hc-install.init`.

This is also the second half of the round-1 correction: per-family *linking* of
the fork removes the same cost without waiting for upstream, because an unlinked
service package runs no `init`.

## Best measured configuration

| | stock | best |
| --- | ---: | ---: |
| podMEM steady | 382.2 | **108.4** |
| podMEM idle | 535.5 | **89.9** |
| peak | 537.6 | **115.0** |
| config build | 13.67 s | 0.66 s (0.25 s with the upjet patch) |
| smallest pod | OOMKilled at 300Mi | 256Mi, 0 restarts |

That is: SDK partition regexes hoisted + trimmed embeds + `UPJET_FAMILY_FILTER` +
`GODEBUG=disablethp=1` + `GOGC=25` + `GOMEMLIMIT=120MiB`. **-72% steady, -79%
peak.**

Note the two patches have not yet been combined in one binary: the upjet regex
precompile (round 3, startup) and the SDK partition hoist (round 4, memory) were
built separately. They are independent, so a combined build should give 0.25 s
config build at 108 MiB steady.

## What remains, to get under 100

Live heap is now 56.2 MiB:

| | |
| --- | ---: |
| `Scheme.AddKnownTypeWithName` + conversion funcs + RESTMapper (8,488 GVKs) | ~14 MiB |
| `reflect.compiledTypelinks` / `addModuleItabs` / `addReflectOff` | ~6 MiB |
| Terraform provider resource map | ~7 MiB |
| remaining regexes (`regexache`, go-cmp, hc-install) | ~3 MiB |
| diffuse remainder | ~26 MiB |

The first two - ~20 MiB - exist only because a single-family binary links every
API group. **Per-family API packages are now the single largest remaining item**,
and at `GOGC=25` that ~20 MiB of live heap is ~25 MiB of pod memory, which lands
the steady state in the mid-80s. Nothing else measured gets under 100.


---

# Rounds 5-9: the structural changes, measured

Round 4 ended at 108 MiB steady and named two structural items as the only path
below 100: per-family API packages and per-family linking. Both are now
implemented behind environment variables, along with five smaller ones, and the
answer is **76.6 MiB steady at 50 managed resources and 109.7 MiB at 500**.

Every arm below carries the four settled levers from rounds 1-4 - trimmed or
lazily-converted embeds, `UPJET_FAMILY_FILTER=s3`, `GODEBUG=disablethp=1`,
`GOGC=25` - and adds one thing at a time.

## The eight changes

| knob | what it does | worth |
| --- | --- | ---: |
| trimmed fork | link 19 of 267 service packages, 17 of 266 SDK clients | **-26.4 steady** |
| `UPJET_LAZY_CONVERT` | filter the resource map before `GetV2ResourceMap` | **-91.5 peak** |
| `UPJET_CACHE_AWS_CLIENT` | reuse the configured AWS client across reconciles | **-59.6 at 500 MRs** |
| `UPJET_STRIP_CACHE_METADATA` | drop `managedFields` + last-applied from the cache | **-29.5 at 500 MRs** |
| `UPJET_SCHEME_FAMILY` | register 6 API groups instead of 178 | **-7.5 steady** |
| `UPJET_SHARE_SCHEME` | one scheme, not two | -1.9 steady |
| `UPJET_NO_LOG_SAMPLER` | drop controller-runtime's zapcore sampler | ~0 (see below) |
| `UPJET_CLEAR_SCHEMAFUNC` | `docs/fixes/02` | ~0 (correctness, not memory) |

### Per-family linking: measured on the real provider

The fork's two generated registries root everything, and **both** must be
trimmed. `service_packages_gen.go` roots `internal/service/*`;
`awsclient_gen.go` imports the `aws-sdk-go-v2` clients for its 266 accessors'
return types. Go initialises every imported package whether or not a symbol is
reachable, so trimming only the first leaves every SDK client's `init` running.
**`docs/memory-footprint.md` says `awsclient_gen.go` does not need to change -
that holds for text, and not for `init`, which is where the heap is.**

| | full fork | trimmed fork |
| --- | ---: | ---: |
| podMEM steady (50 MRs) | 123.9 | **97.5** |
| resident text | 277.8 | **156.8** |
| binary | 979,959,968 B | **332,333,216 B** |

**-26.4 MiB of steady state, -121 MiB of resident text, and a binary 66%
smaller** - the first end-to-end measurement of the per-family linking idea on a
real provider rather than a synthetic `main`. It crosses under 100 MiB on its
own.

### Filtering the parse instead of trimming the file

`GetV2ResourceMap` converts all 1,683 schemas before any include list is
consulted; the loop then drops what is not kept. Filtering first, on stock
embeds:

| | idle | steady | peak | config build |
| --- | ---: | ---: | ---: | ---: |
| control | 104.5 | 123.9 | **248.7** | 1.162 s |
| `UPJET_LAZY_CONVERT=1` | 102.7 | 120.6 | **157.2** | 0.963 s |

**-91.5 MiB of startup peak from ~15 lines**, with no build step and no second
copy of the blobs - about 85% of what the build-time trim achieved. If only one
of the two ships, it should be this one.

### The AWS client is rebuilt on every reconcile

`configureNoForkAWSClient` constructs a fresh `*conns.AWSClient` per `Connect`,
and `internal/conns/config.go:100` gives each one its own `HTTPClient` - so every
reconcile gets a new `http.Transport` with an empty connection pool. The steady
profile shows it: **56 MB per 3.3 minutes of `bufio` buffers under
`Transport.dialConn`**, roughly 70 new connections a second for 50 buckets
polling once a minute.

Caching the configured client (measurement-only: no expiry, keyed on provider
config, region and access key):

| | 50 MRs | 500 MRs |
| --- | ---: | ---: |
| without | 82.9 | 169.3 |
| with | **76.6** | **109.7** |

**-6.3 MiB at 50 MRs and -59.6 MiB at 500** - it scales with reconcile volume,
not resource count, which is why it barely registers on a small provider and
dominates on a busy one. This is `docs/fixes/09`, and it is worth far more than
the fix list suggests.

### The informer cache holds what nothing reads

Every cached object carries `metadata.managedFields` and the last-applied
annotation. `cache.Options.DefaultTransform` can drop both on the way in - the
same mechanism `TransformStripCRDSchema` already uses for CRDs:

| 500 MRs | idle | steady | peak |
| --- | ---: | ---: | ---: |
| control | 107.5 | 246.0 | 436.7 |
| stripped | 115.5 | **216.5** | **268.1** |

**-59 KB per managed resource** and -168 MiB of peak. All 500 still reconcile.
At 3,000 MRs that extrapolates to ~180 MiB.

## Two nulls, and why

`UPJET_NO_LOG_SAMPLER` and `UPJET_CLEAR_SCHEMAFUNC` moved nothing measurable.
The sampler patch was **incomplete on the first attempt** - it replaced the
provider's own logger while `main` sets controller-runtime's global logger
earlier with its own `zap.New`, and that is the one that is retained. Clearing
`SchemaFunc` remains worth shipping as a **correctness** fix (this repository's
`config/` schema edits are otherwise invisible to every path except the diff),
just not as a memory fix.

The general lesson from rounds 3-9: **`alloc_space` ranks what to fix for speed,
`inuse_space` ranks what to fix for footprint, and they disagree.** The three
largest allocation sites moved the pod metric by nothing; the three largest
*retained* structures moved it by 90 MiB.

## Where the memory is now

Live heap at 50 MRs is **27.5 MiB**, from 64.7 at the start of these rounds, and
it is genuinely diffuse - nothing above 3 MiB, `Scheme.AddKnownTypeWithName` down
from 5.75 to 1.10. At 500 MRs it is 52.1 MiB, so **56 KB per managed resource**,
and that growth has a single dominant cause:

| grew with MR count | 500-MR live heap |
| --- | ---: |
| `go-cty/cty.Object` | 10.24 MiB |
| `cty.ObjectVal` | 6.15 MiB |
| `schema.(*MapFieldWriter).setPrimitive` | 1.54 MiB |

`external_tfpluginsdk.go:826` calls
`n.config.TerraformResource.CoreConfigSchema().ImpliedType()` on every Observe,
Create and Update, and the value it produces is retained per managed resource.
The implied type is a pure function of the schema - there are 25 distinct ones
in a filtered s3 provider, not 500. Memoising it is the same "one copy, not N"
pattern as the AWS SDK's partition regexes.

## Round 9: a null, and what it corrects

`external_tfpluginsdk.go:826` rebuilds the implied cty type on every Observe,
Create and Update, and `cty.Object`/`cty.ObjectVal` were 16.4 MiB of the 500-MR
live heap - so memoising the type looked like the same "one copy, not N" win as
the SDK partition regexes. **It moved nothing**: 113.3 against 113.6 MiB.

That corrects the attribution. Those cty objects are mostly **in-flight reconcile
state**, not structures retained per managed resource - `inuse_space` includes
what has not been collected yet, and at 500 MRs there is a lot in flight. It also
explains why caching the AWS client helped so much at 500 MRs and so little at
50: what scales here is reconcile *volume*, not resource count.

## Validation

The eight changes stack, so the risk shifts from "is it smaller" to "is it still
correct". On the fully-patched binary, with every knob on:

* **create** - 500 buckets reach `Ready` in 102 s;
* **a second kind** - a `BucketVersioning` (v1beta2, the storage version) reaches
  `SYNCED=True READY=True`, and LocalStack independently reports
  `"Status": "Enabled"`, so the change reached the API rather than only the
  status;
* **delete** - the managed resource *and* the bucket disappear, with a control
  bucket left untouched.

Two harness traps worth recording for whoever runs this next. `aws_s3_bucket`
tags go through S3 Control, which this harness answers with a stub that accepts
writes and returns an empty tag set - **tags cannot be used as an update test
here**. And both CRDs store `v1beta2` with `conversion: None`, so a `v1beta1`
manifest using the singleton-list shape is stored verbatim and never reconciles;
submit the storage version.

## Running total

| | stock | best measured |
| --- | ---: | ---: |
| podMEM steady, 50 MRs | 382.2 | **76.6** |
| podMEM steady, 500 MRs | 486.7 | **109.7** |
| peak | 537.6 | **122.1** |
| config build | 13.67 s | 0.86 s |
| binary | 980 MB | 332 MB |

**-80% steady, -77% peak, -94% startup, -66% image.**


---

# Round 10: what review found

Two independent reviews were run over the raw samples, the orchestrators and the
patches. They found a measurement defect that invalidates part of rounds 2-9, and
several places where the write-up's precision outran its design. Everything below
is a correction to this document, not to the provider.

## The defect: three flags were never applied

`cmd/provider/s3/zz_main.go:103` builds the flag set with
`kingpin.New(filepath.Base(os.Args[0])).DefaultEnvars()`, so environment-variable
names are derived from **the binary's basename**. The orchestrators run
`/opt/provider/${bin}` with `bin` = `provider-trim2`, `provider-v5` … so the
variable kingpin honours is `PROVIDER_V9_MAX_RECONCILE_RATE`, and the
`PROVIDER_MAX_RECONCILE_RATE` the arms set was ignored. The same applies to
`PROVIDER_POLL_STATE_METRIC` and `PROVIDER_ENABLE_SECRET_CACHE`.

**Every arm from round 2 onward ran at `--max-reconcile-rate=100`**, the default,
while this document said 10. Confirmed directly on the same binary, with the flag
moved back into `args`:

| v9, identical knobs, only the rate differs | idle | steady | peak | goroutines | stacks |
| --- | ---: | ---: | ---: | ---: | ---: |
| rate=100 | 72.1 | 80.3 | 212.8 | 6,042 | 26.3 MiB |
| rate=10 | **50.7** | **56.2** | 212.8 | 1,540 | 7.0 MiB |

Three consequences:

* **The best measured figures improve to 56.2 MiB steady at 50 managed resources
  and 84.9 at 500** - every number in rounds 2-9 carries ~19 MiB of idle worker
  stacks that the documented configuration would not have had.
* **The two arms that existed to measure the reconcile rate compared rate=100
  with rate=100** (`s100-rate10`, `s100-gogc25-rate2`). They measured nothing.
  The "~22 MiB of stacks" figure in round 3 survives only as a cross-round
  inference between round 1 (which passed the flag properly) and round 2.
* **Round 3's `--poll-state-metric=30s` null is vacuous** - the setting never
  changed. It should not be counted among the three allocation-site nulls.

The rate does not affect peak (212.8 either way), so this defect does not touch
any peak-based conclusion.

## Corrections to earlier rounds

**"One binary for every arm" is false from round 2.** Ten binaries were used.
`BINARIES.md` now maps each. In particular the round 2 opener - "environment
variables only" - is wrong: those arms ran `provider-trim2`, a rebuilt binary
with trimmed embeds.

**Two different definitions of "peak" appear in the round 2 table.** The
`trimmed embeds` row quotes 174.7, which is the peak *at end of idle* - the
startup spike. Its whole-run `memory.peak` is 248.3. So the honest statement is
"the startup spike halves, 380 -> 175; whole-run peak falls 392 -> 248 (-37%)",
not "peak halves". The `GOMEMLIMIT=200MiB` row in the same table is a whole-run
peak, so the column mixes both.

**The best-case peak of 122.1 should not be quoted.** The v9 arms with the same
knobs peak at 209.9-213.9. Either v8 and v9 differ in a way not recorded, or
peak has far wider run-to-run spread than steady state. Until that is explained,
peak is only trustworthy where a single binary is compared with itself.

**The client-cache delta at 500 MRs is smaller than stated.** The -59.6 MiB
compares v7 with v8, and the 500-MR series was still declining inside its 150 s
window; endpoint-to-endpoint the honest range is **-48 to -50 MiB**. The
mechanism is confirmed same-binary at 50 MRs (`e8-ctrl` 82.9 -> `e8-cache` 76.6).
The same caveat applies to `-59 KB per managed resource` for the cache strip:
~50 KB is better supported, and the extrapolation to 3,000 MRs is not.

**"Stock" is not upstream stock.** The 382.2 baseline is this branch, which
applies `dropCodegenOnlyMetadata` unconditionally, with `--skip-default-tags`,
no webhooks, and the executable's pages charged outside the pod. Upstream stock
would be higher; the -80% headline compares a discounted baseline with a
discounted best.

**The noise band is wider than 3%.** Within-arm sampling noise is small, but
same-configuration drift *across binary generations* is 3-6% (`e6-all` 86.0 vs
`e7-all` 89.0; `e8-cache` 76.6 vs `e9-implied-50` 81.1). Deltas under ~10 MiB -
share-scheme (-1.9), scheme-family (-7.5), client cache at 50 MRs (-6.3) - are at
or inside that band and are single-run. Deltas above ~20 MiB (THP, the family
filter, the embed trim, the fork trim, the lazy-convert peak) are safe.

## Correctness problems in the patches

**`UPJET_SCHEME_FAMILY`'s closure is incomplete.** The generated resolvers reach
cluster `s3control.aws.upbound.io/v1beta2 AccessPoint` and the namespaced
`iam`/`kms`/`s3control`/`sns`/`sqs` kinds; the patch registers cluster s3control
v1beta1 only and, namespaced, only s3. With `UPJET_SHARE_SCHEME=1` also set the
resolver shares that incomplete scheme, so cross-group reference resolution would
fail. **No arm ever resolved a cross-group reference**, so the validation could
not have caught it. The -7.5 MiB is therefore measured on an under-registered
scheme.

**`UPJET_STRIP_CACHE_METADATA` is not shippable as written.** Stripping
`managedFields` from a cache is ordinary; stripping the last-applied annotation
is not - anything written back from a cached object would delete that annotation
on the server and break `kubectl apply` three-way merges. The transform is also
installed as `DefaultTransform`, so it applies to Secrets and ProviderConfigs,
not only managed resources. A shippable version strips `managedFields` only, and
scopes itself with `ByObject`.

**`UPJET_CACHE_AWS_CLIENT` has no expiry**, as its own comment says. With
assume-role or IRSA credentials the cached client would go stale within the hour.
The measured -48 to -59 MiB bounds a best case that a correct implementation,
keyed on the full provider configuration and expiring with the credentials, may
not reach.

**The fork trim's service list is hand-picked** (22 entries in `trim-fork.py`),
and only 2 of the family's 25 kinds were ever reconciled on the trimmed binary.
"Still configures all 25 resources" is not "still works". A shippable version
derives the closure from `config/<family>`.

**`UPJET_LAZY_CONVERT` changes `GetSkippedResourceNames` semantics** - resources
dropped by the include list never enter the map, so they are not reported as
skipped. Harmless at runtime, worth a note upstream.

## Revised headline

| | stock (this branch) | best measured, rate=10 |
| --- | ---: | ---: |
| podMEM steady, 50 MRs | 382.2 | **56.2** |
| podMEM steady, 500 MRs | 486.7 | **84.9** |
| config build | 13.67 s | 1.35 s |
| binary | 980 MB | 332 MB |

The two results worth taking upstream are unaffected by everything above, because
both were measured single-variable on one binary within one matrix: **transparent
huge pages inflating what the pod is charged**, and **not converting the 1,559
resource schemas the include list is about to discard**.


---

# Round 11: a CPU baseline, and acting on the review

Round 10 recorded what review found. This round fixes it, adds the measurement
axis that was missing, and works through the loose ends. Every arm here runs at a
**verified** `--max-reconcile-rate=10` - the pod's rendered `args` are checked
after each launch, because two more flag-plumbing defects turned up while doing it
(see the end).

## A CPU baseline, at last

Every number in rounds 1-10 was memory. The sampler now also reads the
container cgroup's `cpu.stat`, and `analyze.py` reports milli-cores per arm. That
matters because the last several findings were CPU wins asserted from allocation
counts rather than measured.

## The AWS SDK decomposes every request for a log nobody reads

`aws-sdk-go-base` installs a Deserialize middleware that builds the request,
calls `DecomposeHTTPRequest` - **reading the whole request body** - and hands the
result to `logger.Debug`, which discards it unless Terraform logging is on.
**There is no level check anywhere in that path** (`logger.go:113`); the only
guard is `SuppressDebugLog`, at middleware-*install* time
(`aws_config.go:389`), which this provider never sets.

| | steady podMEM | CPU |
| --- | ---: | ---: |
| control, 50 MRs | 76.7 | 47 mCPU |
| `SuppressDebugLog`, 50 MRs | 74.1 | **36 mCPU** |
| control, 500 MRs | 112.1 | 232 mCPU |
| `SuppressDebugLog`, 500 MRs | 109.4 | **176 mCPU** |

**-24% CPU at both scales**, memory unchanged inside noise - the round-3 lesson
holding exactly. Two fixes: one line downstream to stop installing the
middleware, and an upstream guard on the logger's level before decomposing.

## The Secret informer, finally measured with Secrets present

Every family pod starts a cluster-wide, selector-less `v1.Secret` informer.
`docs/architecture-wins.md` priced this at "80-120 MB per pod" and could never
measure it, because the harness cluster had one Secret. With **5,000 Secrets**
loaded:

| 50 MRs, 5,000 Secrets | idle | steady | CPU |
| --- | ---: | ---: | ---: |
| secret cache on (default) | 58.2 | **64.3** | 30 mCPU |
| secret cache off | 45.2 | **50.1** | 35 mCPU |

**-14.2 MiB for +5 mCPU**, i.e. **~2.9 KB per Secret in the cluster**, whether or
not the provider ever reads it. At 50,000 Secrets that is ~145 MiB per provider
pod, multiplied by every family pod installed. The flag is the workaround;
scoping the informer (the `fix/scope-secret-informer` branch) is the fix.

## The client cache, made shippable and properly controlled

Review was right that the -59.6 MiB figure was cross-binary on a declining
window. Re-run **same-binary, single-variable**:

| v11, 500 MRs, rate=10 | steady | CPU |
| --- | ---: | ---: |
| no client cache | 122.8 | 215 mCPU |
| client cache | **78.1** | 194 mCPU |

**-44.7 MiB (-36%)**, which is close to review's -48 to -50 estimate and well
short of my -59.6.

The cache is now shippable, and it needs **no change to how credentials are
resolved**. `configureNoForkAWSClient` is handed *materialised* credential values
(`aws.go:357-365`), not a live provider - so even under IRSA or Pod Identity the
constructed client holds static, expiring keys. Keying the cache on the
credentials' identity **and expiry**, and sweeping expired entries, means a
rotation naturally produces a new client: the existing
`AWSCredentialsProviderCache` re-retrieves, `AccessKeyID` and `Expires` change,
and the key changes with them.

## The hand-picked scheme closure was broken, exactly as review predicted

Review read the generated resolvers and predicted that `UPJET_SCHEME_FAMILY`'s
hand-written 10-group list would fail on a cross-group reference, and that no arm
could have caught it. Both were right. A `BucketMetric` referencing an
`s3control` `AccessPoint`:

```
UPJET_SCHEME_FAMILY=1  Synced=False: cannot resolve references: failed to get a new
   API object of GVK "s3control.aws.upbound.io/v1beta2, Kind=AccessPoint" from the
   runtime scheme: no kind "AccessPoint" is registered for that version
knob off               Synced=False: referenced field was empty (referenced resource
   may not yet be ready)      <- the expected behaviour
```

The fix is the one this repository's own documents recommend: **derive the
closure, do not write it**. `hack/clustermeasure/gen-family-scheme.py` walks the
family's `zz_generated.resolvers.go` files for literal
`GetManagedResource("<group>", "<version>", ...)` calls and emits the
registration list. For s3 it produces **16 registrations where the hand-written
list had 10** - the missing ones being cluster `s3control v1beta2` and the
namespaced `iam`, `kms`, `s3control`, `sns` and `sqs` groups. With the generated
closure the same probe behaves identically to the control.

Correctly registered, the family scheme is worth less than claimed:

| v12, 50 MRs, same binary | steady | CPU |
| --- | ---: | ---: |
| all 178 groups | 55.2 | 39 mCPU |
| generated 16-group closure | **50.7** | 34 mCPU |

**-4.5 MiB**, not the -7.5 measured on the under-registered scheme.

## Two more flag-plumbing defects

Round 10's defect had siblings, and both were invisible in the arm definitions:

* `RATE` and `SECCACHE` support existed only in a scratchpad copy of the
  orchestrator, so arms generated from the committed one silently ran at the
  default reconcile rate **again**.
* `kingpin` rejects `--enable-secret-cache=true`; boolean flags take
  `--flag`/`--no-flag`. The provider crash-looped with `unexpected true` until
  the setting moved to its explicit `ENABLE_SECRET_CACHE` envar.

The lesson is now procedure: **verify the pod's rendered `args` and its goroutine
count after every launch**, since 1,540 goroutines means rate=10 and ~6,000 means
the flag did not land.

## Running total, all verified at rate=10

| | stock (this branch) | best measured |
| --- | ---: | ---: |
| podMEM steady, 50 MRs | 382.2 | **50.1-50.7** |
| podMEM steady, 500 MRs | 486.7 | **78.1** |
| CPU, 50 MRs | not measured | 34-42 mCPU |
| binary | 980 MB | 332 MB |


---

# Round 12: the CPU profile, and a correction to my own recommendation

Round 11 added a CPU column. This round takes the first actual CPU profile, and
it overturns the `GOGC=25` advice given in round 2.

## 61% of the provider's CPU is garbage collection

`/debug/pprof/profile?seconds=60` against the soak pod (500 managed resources,
every knob on, steady state) — the first CPU profile taken in this whole
investigation:

| | share of CPU |
| --- | ---: |
| `runtime.gcBgMarkWorker` | **61.3%** |
| all reconcile work (`managed.Reconcile` and below) | 21.6% |
| &nbsp;&nbsp;of which `terraformPluginSDKExternal.Observe` | 15.9% |
| &nbsp;&nbsp;of which `schemaMap.Diff` (the per-Observe schema deep copy) | 11.1% |

`GOGC=25` was chosen in round 2 against a **230 MiB** steady state. Live heap is
now **27.5 MiB**, so the collector runs almost continuously to defend a goal that
is trivial to meet, and the provider spends most of its CPU marking.

## Pricing the trade

Same binary, 500 managed resources, `--max-reconcile-rate=10` verified in `args`:

| `GOGC` | podMEM steady | CPU | peak |
| --- | ---: | ---: | ---: |
| 25 | **82.2** | 182 mCPU | 125.6 |
| 50 | 95.3 | **124 mCPU** | 146.0 |
| 100 + `GOMEMLIMIT=120MiB` | 113.2 | **103 mCPU** | 120.1 |

25 → 100 costs **+31 MiB** and returns **−43% CPU**. The knee is at **`GOGC=50`**:
73% of the CPU saving for 42% of the memory cost. `GOMEMLIMIT` still does its
separate job — it is what holds the peak down (120.1 with it, 146.0 without).

**Revised recommendation: `GOGC=50` with `GOMEMLIMIT` as the ceiling**, not
`GOGC=25`. Round 2's advice was measured against a footprint that no longer
exists, and it was never priced in CPU because nothing measured CPU until round
11. Anyone quoting the round-2 numbers should quote this table instead.

The second-largest single item, `schemaMap.Diff` at 11.1%, is
`helper/schema.Diff` calling `m.DeepCopy()` — a reflection walk of the whole
resource schema — on every Observe because the tags interceptor installs a
`customizeDiff`. That is a genuine upstream target in terraform-plugin-sdk, and
unlike the churn fixes of round 3 it would show up in the CPU column rather than
the memory one.


## Round 12b: the state-metrics poller is a genuine null, and a noise band worth having

Round 3's `--poll-state-metric=30s` result was vacuous — the flag never reached
the process. Re-run with it verified in `args`, at 500 managed resources,
`GOGC=50`, same binary:

| | steady | CPU |
| --- | ---: | ---: |
| `--poll-state-metric=5s` (the default) | 95.5 | 126 mCPU |
| `--poll-state-metric=60s` | 95.1 | 126 mCPU |

**No effect on either axis.** The mechanism looked convincing — every generated
controller starts its own `MRStateRecorder`, and each ticks a `client.List`
against the informer cache, which deep-copies every object of that kind — but at
500 managed resources it does not show up. Refuted, not deferred.

### The same-binary noise band

Three arms ran at an identical configuration (`gogc50-500`, `psm5-500`,
`psm60-500`): **95.3 / 95.5 / 95.1 MiB** and **124 / 126 / 126 mCPU**. That is a
±0.2 MiB spread, ~0.2%.

So there are two noise bands, and they are an order of magnitude apart:

* **same binary, same session, adjacent arms: ~0.2%** — deltas of 1-2 MiB are
  real here;
* **across binary generations: 3-6%** (review's measurement, from `e6-all` 86.0
  vs `e7-all` 89.0 and `e8-cache` 76.6 vs `e9-implied-50` 81.1) — deltas under
  ~10 MiB are not resolvable.

Every conclusion in this document should be read against whichever band applies
to how its arms were run. The rounds 11-12 results are same-binary throughout;
most of rounds 5-9 are not.


## Round 12c: the whole GOGC curve, and what to actually recommend

| 50 managed resources | steady | CPU |
| --- | ---: | ---: |
| `GOGC=25` | 55.0 | 35 mCPU |
| `GOGC=50` | 63.3 | **25 mCPU** |

| 500 managed resources | steady | CPU | peak |
| --- | ---: | ---: | ---: |
| `GOGC=25` | **82.2** | 182 mCPU | 125.6 |
| `GOGC=50` | 95.3 | 124 mCPU | 146.0 |
| `GOGC=50` + `GOMEMLIMIT=110MiB` | 95.3 | 127 mCPU | **114.2** |
| `GOGC=100` + `GOMEMLIMIT=120MiB` | 113.2 | **103 mCPU** | 120.1 |

Two things fall out.

**`GOGC=50` is the knee at both scales.** Going 25 → 50 costs 8.3 MiB at 50
managed resources and 13.1 at 500, and returns 29% and 32% of CPU respectively.
Going 50 → 100 costs another 17.9 MiB for a further 17%.

**`GOMEMLIMIT` is free, and it is the peak lever.** At `GOGC=50` it changed
steady state not at all (95.3 either way) and CPU not at all (124 vs 127, inside
the ±0.2% same-binary band), while taking the peak from 146.0 to **114.2 MiB** -
the lowest peak measured anywhere in this document. Peak is what a pod's memory
limit has to cover, so this is the setting that decides whether the provider fits
in a 128Mi request.

**Recommendation: `GOGC=50` with `GOMEMLIMIT` set ~15-20% above expected steady
state.** Use `GOGC=25` only where memory is scarcer than CPU, and know that it
costs about a third of the provider's CPU to save ~13 MiB.
