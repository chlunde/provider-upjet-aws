# Release the startup heap once the provider configuration is built

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
