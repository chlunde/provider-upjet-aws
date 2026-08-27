<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 05. Persist the external-name when create fails, and on the async path

| | |
| --- | --- |
| **Category** | data loss — orphaned external resources, duplicate creates |
| **Severity** | high |
| **Size** | small |
| **Lives in** | upjet — `pkg/controller/external_tfpluginsdk.go`, `external_async_tfpluginsdk.go` |
| **Evidence** | read |

## What happens

Two related windows in which the external-name of a created object never
reaches the managed resource, so a provider restart causes a second object to be
created and the first to be orphaned. Every AWS resource is affected, because
`IdentifierAssignedByAWS()` makes them all `IdentifierFromProvider` — the name
is only knowable *after* create.

**1. The create-error path.** When the Terraform create returns an error after
the object already exists, upjet stores the returned state in the in-memory
tracker and returns before `setExternalName`:

```go
if !n.opTracker.HasState() {
    n.opTracker.SetTfState(newState)
}
return managed.ExternalCreation{}, errors.Errorf("failed to create the resource: %v", diag)
```

Within the same process the next `Observe` recovers it from the tracker. Across
a restart the tracker is gone; `managed.Reconciler` recorded
`external-create-failed`, so `ExternalCreateIncomplete` does not fire, and
`Create` runs again.

**2. The async path, more broadly.** Async `Create` returns `nil` immediately;
crossplane-runtime records `external-create-succeeded` *before* AWS has been
called, and the inner sync create sets the external-name on a copy that is
discarded. Durable persistence happens only via the next `Observe`'s
`ResourceLateInitialized` spec update. With `managementPolicies` that exclude
`LateInitialize`, it is **never** persisted (`reconciler.go:1479`).

The Framework async path already recovers the name from cached partial state
when the post-create read fails
(`external_async_tfpluginfw.go:116-171`); the SDK path has no equivalent.

## The fix

* In the SDK `Create` error path, when `newState != nil && newState.ID != ""`,
  derive and set the external-name **before** returning the error.
  `managed.Reconciler` calls `UpdateCriticalAnnotations` on create failure,
  which updates the whole object and re-applies all annotations on conflict —
  so a name set here is persisted.
* Port the Framework path's partial-state recovery to the SDK async connector.
* Persist the external-name on async create completion independently of
  `LateInitialize`, so a restricted `managementPolicies` cannot lose it.

## How to test

* **Unit (upjet):** a create returning a state with an ID *and* an error sets
  the external-name on the managed resource before returning. Fails today.
* **Unit (upjet):** async create with `managementPolicies: ["Create"]`
  (no `LateInitialize`) still persists the external-name.
* **e2e:** create a resource, kill the provider pod during the create, restart,
  and assert exactly one external object exists. This is the test that matters
  and the one nobody has run.

## Suggested issue

Repo: `crossplane/upjet`

**Title:** `External-name is not persisted on create failure or on the async create path, orphaning resources across restarts`

**Body:**

> For providers where the external-name is assigned by the provider
> (`IdentifierFromProvider` — all of provider-upjet-aws), the name is only
> known after create returns. Two paths fail to persist it:
>
> 1. `terraformPluginSDKExternal.Create` returns on a Terraform diagnostic
>    error before reaching `setExternalName`, recording the state only in the
>    in-memory `AsyncTracker`. `managed.Reconciler` sets
>    `external-create-failed`, so `ExternalCreateIncomplete` does not block the
>    next attempt, and a restart in that window creates a duplicate.
> 2. On the async path, `Create` returns nil immediately and the inner sync
>    create sets the name on a discarded copy; durable persistence relies on the
>    next Observe's `ResourceLateInitialized`. With `managementPolicies`
>    excluding `LateInitialize` it is never persisted at all.
>
> The Framework async connector already recovers the name from cached partial
> state (`external_async_tfpluginfw.go:116-171`); the SDK connector does not.
>
> `managed.Reconciler` calls `UpdateCriticalAnnotations` on create failure,
> which would persist an external-name set before the error is returned.

## Branch

`fix/persist-external-name-on-create` (upjet fork)
