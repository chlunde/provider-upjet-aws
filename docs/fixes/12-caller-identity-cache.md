<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 12. Data race and an STS call under the write lock in the identity cache

| | |
| --- | --- |
| **Category** | correctness (race) + waste (lock scope) |
| **Severity** | medium |
| **Size** | small — two independent changes in one file |
| **Lives in** | this repo — `internal/clients/cache.go`, `creds_cache.go` |
| **Evidence** | measured (race reproduces on every run) |

Two defects in the same area, worth one change.

## A. Data race on `AccessedAt`

`CallerIdentityCache.GetCallerIdentity` reads the entry under `RLock`, then
takes the write lock to refresh the access time — but mutates a field of the
shared entry that other goroutines are reading under `RLock` at the same time:

```go
c.mu.RLock()
o, ok := c.cache[key]
c.mu.RUnlock()
if ok {
    if time.Since(o.AccessedAt) > 10*time.Minute {   // read under no lock
        c.mu.Lock()
        o.AccessedAt = time.Now()                    // write to shared entry
        c.mu.Unlock()
    }
    ...
}
```

Holding the write lock does not help: the readers hold only `RLock`, and
`RLock` does not exclude them from each other or protect the field. Every
reconcile of every managed resource goes through here concurrently.

The credentials cache next door already gets this right, using
`atomic.Value` for `accessedAt` — so the fix is to match it.

## B. `sts:GetCallerIdentity` under the global write lock

`AWSCredentialsProviderCache.RetrieveCredentials` calls `accountIDFn(ctx)` —
a network round trip to STS — while holding `c.mu.Lock()` for the whole cache.
Every other reconcile needing credentials blocks behind one STS call.

## The fix

* **A:** store `AccessedAt` as an `atomic.Value` (or `atomic.Int64` of Unix
  nanos) and drop the write lock from the hot path, mirroring
  `awsCredentialsProviderCacheEntry`.
* **B:** build the entry outside the lock, then take the lock only to insert,
  re-checking for a racing insert — the double-checked pattern already there,
  with the slow call moved out. Accept that a cold start may do two STS calls
  for the same key; that is cheaper than serialising the fleet.

## How to test

* **Race test:** a `-race` test hammering `GetCallerIdentity` from N goroutines
  on one key. **This fails today** and is the regression test for A.
* **Unit:** a fake `accountIDFn` that blocks on a channel; assert a second
  goroutine with a *different* cache key completes while the first is blocked.
  Fails today; regression test for B.
* Add `-race` to the unit-test target if it is not already on — see
  [fix 12's note in lead-triage](../lead-triage.md) on the absence of tests in
  `internal/clients`.

## Suggested issue

Repo: `crossplane-contrib/provider-upjet-aws`

**Title:** `Data race on callerIdentityCacheEntry.AccessedAt, and an STS call held under the credentials-cache write lock`

**Body:**

> Two issues in the caching layer, both on the per-reconcile hot path.
>
> **1. Data race.** `CallerIdentityCache.GetCallerIdentity`
> (`internal/clients/cache.go`) reads the cache entry under `RLock`, releases
> it, then reads `o.AccessedAt` with no lock and writes it under `Lock`.
> Concurrent readers hold only `RLock`, which does not exclude the writer's
> field mutation. Reproduces under `-race` on every run. The neighbouring
> credentials cache already uses `atomic.Value` for the same purpose.
>
> **2. STS under a global lock.** `AWSCredentialsProviderCache.RetrieveCredentials`
> (`internal/clients/creds_cache.go`) calls `accountIDFn(ctx)` — an
> `sts:GetCallerIdentity` round trip — while holding the cache-wide write lock,
> serialising every other reconcile that needs credentials behind it.
>
> Suggested fixes: make `AccessedAt` atomic; move the STS call outside the lock
> and re-check on insert.

## Branch

`fix/identity-cache-race-and-lock-scope`
