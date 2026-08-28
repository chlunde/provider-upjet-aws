# Release the startup heap once the provider configuration is built

> # RETRACTED — do not open this PR
>
> **The premise below is false.** It rests on the claim that Go's background
> scavenger does not return the idle heap on its own. It does, unprompted,
> within ~2.5 minutes idle and ~15 seconds under load. See
> [the retraction](#retraction-the-runtime-already-does-this) at the end of this
> file. The branch `fix/release-startup-memory` @ `0dfbe58` is left in place for
> history only.
>
> An explicit `debug.FreeOSMemory()` buys **7–10 MiB and closes a ~2.5-minute
> window at startup** — not 180–255 MiB.

**The largest measured win in this analysis, and the smallest change.** One
`debug.FreeOSMemory()` after the provider configurations are built.

## The gap

Building the cluster-scoped and namespaced configurations unmarshals the
embedded Terraform schema and the registry metadata — ~15 GiB allocated in
total — leaving the Go heap sized for that peak. After startup the arena holds
**180–255 MiB of collected-but-idle spans** against ~51 MiB of live data.

`runtime.GC()` collects; it does not hand spans back. And the background
scavenger does not reclaim them on its own.

## Measured

Immediately after the startup path:

| | anonymous before | after | `HeapReleased` |
| --- | ---: | ---: | --- |
| no `GOMEMLIMIT` | **367.6 MiB** | **112.0 MiB** | 25.5 → 282.8 |
| `GOMEMLIMIT=300MiB` | **282.5 MiB** | **103.7 MiB** | 30.2 → 209.3 |

58 ms. `HeapAlloc` and `HeapInuse` unchanged — nothing live is touched — and
`Private_Dirty` falls by the same amount `HeapReleased` gains, so the kernel
really takes the pages back rather than a counter merely moving.

## It does not come back

**Idle**, two already-scavenged processes sampled every 20 s: **+72 kB**
(no limit) and **+44 kB** (`GOMEMLIMIT`) over 2 min 20 s. The background
scavenger returns nothing unprompted, so "wait a few minutes" is not an
alternative.

**Under sustained churn**, 68,158 reconcile-shaped iterations over 4 minutes:

```
t=0  (post-scavenge)   Anon 113.8 MiB
t=30s                       149.2       one step up
t=1m                        148.8
t=2m                        148.8
t=3m                        148.6
t=4m                        153.0       flat, 68k reconciles later
final FreeOSMemory (38ms)   115.7 MiB
```

Churn re-grows **~35 MiB and plateaus**. It never climbs back toward 367.6.
`HeapSys` stays pinned at 367.6 while `HeapReleased` holds ~235, so the
*resident* portion is bounded even though the arena is not.

So the one-shot captures ~250 MiB **durably**.

## Why this is the number that matters

`container_memory_working_set_bytes` is `memory.current − inactive_file`. The
~690 MB executable is clean and file-backed, so it is largely subtracted as its
pages age out. **Anonymous memory is never subtracted** — a pod pays for all of
it, always. This is the anonymous half.

## Scope and honest limits

* **Peak RSS is unaffected** (VmHWM 1043.7 MiB). This returns the high-water
  mark; it does not avoid taking it. A pod's memory *limit* must still cover the
  startup spike, and startup OOM risk does not move.
* **`GOMEMLIMIT` is not an alternative.** It does not cause release — it makes
  the scavenger target the limit rather than minimum footprint, and the limited
  arm reclaimed no better than the unlimited one. An earlier note in
  [memory-footprint.md](../memory-footprint.md) read its 386 → 288 MB effect as
  a win; that reading was wrong.
* **A periodic ticker is a refinement, not a necessity.** It would recover the
  ~35 MiB churn re-grows, at 38 ms of stop-the-world per call. Deliberately not
  in this change: it trades latency for memory and deserves its own decision.
* **The reconcile workload is a documented proxy.** It replays the pure-CPU
  parts of Connect+Observe; with no cluster and no AWS account it models none of
  the SDK request cycle, the informer cache, client-go or the workqueue. It is a
  lower bound on per-reconcile churn with no growing live set — enough for the
  question asked (does it climb back?), not a substitute for a real pod.
* **Never run against a cluster.** Worth one observation of a real family pod's
  working set before and after.

## The change

`config/templates/main.go.tmpl` plus the 178 generated `cmd/provider/*/zz_main.go`
— one call and one import each, regenerated from the template. No flag: the
behaviour is unconditional, because there is no configuration under which
holding 250 MiB of idle spans is preferable.

## Verification status — read before merging

**Measured:** all figures above, reproduced independently under two runtime
configurations, with kernel-side confirmation (`Private_Dirty` falling by the
same amount `HeapReleased` gains).

**Structurally verified:** all **178/178** generated mains carry the
`runtime/debug` import, **exactly one** `debug.FreeOSMemory()` call, positioned
**after** `GetProviderNamespaced`. All parse cleanly under `gofmt -e`. That rules
out the three realistic failure modes for a mechanical patch — missing import,
unused import, misplaced call.

**NOT compile-verified.** `go build ./cmd/provider/<family>/` could not be
completed in the analysis environment: a family `main` links its whole family
plus `xpprovider`, and with a cold cache that needs **>22 GB of transient
`$WORK`** on top of a ~20 GB build cache, which the session's fixed disk
allowance cannot hold. Six attempts, every failure `no space left on device`,
**not one compile error in any of them**.

What *did* compile cleanly: `./internal/clients/` (stage 1 — so the whole
`xpprovider` and terraform-provider-aws dependency graph builds fine alongside
this change) and `./apis/{cluster,namespaced}/ec2/...` (stage 2), plus the first
several batches of controller packages.

**One command settles it on a machine with a warm cache:**

```
go build ./cmd/provider/ec2/
```

Do that before opening the PR. The change is two lines per file and a parser has
already validated both, but "structurally verified" and "compiles" are different
claims and only one of them has been established here.

**Branch** `fix/release-startup-memory` @ `0dfbe58`.


---

## RETRACTION: the runtime already does this

### How the error was made

The evidence for "it does not come back" was an idle sampling of two processes
that showed anonymous RSS flat over 2 min 20 s. **Both had been started with
`SCAVENGE_AFTER_STARTUP=1`.** They had already released the idle heap
explicitly, so they were sitting at the post-scavenge floor with nothing left to
return. The control had nothing to give back, and its flatness was read as proof
that the runtime would not act. The correct control — a process that never
scavenges — was never run.

### What the runtime actually does

A 15-run matrix (`claude/steady-state-scavenge` @ `a9022f7`, raw logs in
`hack/memprofile/steadystate/results/`), `WORKLOAD=idle`, no explicit call:

| t | no limit rep1 | no limit rep2 | `=300MiB` rep1 | `=300MiB` rep2 |
| --- | ---: | ---: | ---: | ---: |
| 0s | 366.3 | 369.7 | 283.1 | 282.6 |
| 15s–2m0s | 158.7 flat | 277.3 flat | 163.3 flat | 123.1 flat |
| **2m30s** | **121.3** | **122.0** | **113.6** | **111.8** |
| 2m45s–15m0s | flat | flat | flat | flat |

A first burst inside 15 s, the scavenger **parks**, a second burst completes at
2m15s–2m30s, then flat for 12.5 minutes.

**The mechanism, confirmed independently** with a minimal program that allocates
1.6 GiB, drops it, calls `runtime.GC()` and then only waits:

```
t=0s  → t=2m0s   HeapReleased =    3.2 MiB   NumGC = 10  (pinned)
t=2m15s          HeapReleased = 1603.2 MiB   NumGC = 11  <- forced GC fires
```

`HeapReleased` jumps the instant `NumGC` ticks at the forced-GC boundary. The
scavenger parks when it runs out of work and is re-woken at the end of a GC
cycle; on a fully idle process the only GC is Go's forced one
(`forcegcperiod`, 2 minutes).

**Under load it is far faster.** Reconcile traffic with no explicit call takes it
380.8 → 148.1 MiB **in 15 seconds**, because an active process GCs constantly and
every GC wakes the scavenger.

### Two further claims falsified

* **`GOMEMLIMIT` does not prevent release.** Under a 300 MiB limit the process
  parks at 111.8–113.6 idle — *lower* than unconstrained (121.3–122.0). The
  "scavenger targets the limit rather than minimum footprint" explanation is
  wrong. What the limit genuinely does is cap the arena (`HeapSys` 299.6 vs
  375–399) and so lower the *startup* high-water mark. That part stands.
* **A periodic ticker is pointless.** `SCAVENGE_EVERY=2m` produces a sawtooth:
  each forced scavenge reaches 117.2 and the workload is back to 144.3 within
  15 s. A trough nobody observes, not a lower plateau.

### What survives

Only this: a one-shot `FreeOSMemory()` after provider construction would close
a **~2.5-minute window** during which a just-started pod reports 280–380 MiB
instead of ~120 MiB, and hold roughly **8 MiB** below where the runtime settles.
That may matter to a VPA or a scrape that samples a fresh pod, and nothing else.
If it is ever proposed it must be described that way.

### And the original question is open again

The proxy workload has **no growing live set**, so the ~150 MiB plateau shows
that *startup garbage does not persist* — it does **not** explain a real pod
sitting at ~300 MB. The unmodelled components are where to look next:
controller-runtime's informer cache (which grows with MR count), client-go, the
workqueue, the AWS SDK request/response cycle, and per-`Connect` AWS client
construction.
