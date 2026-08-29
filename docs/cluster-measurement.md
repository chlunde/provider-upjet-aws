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
  `-ldflags="-s -w"`, **980 MB stripped**. One binary for every arm; arms differ
  only by environment variable, so link shape is constant.
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
