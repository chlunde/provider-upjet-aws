<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 04. A missing secret key silently becomes `""`

| | |
| --- | --- |
| **Category** | corruption |
| **Severity** | high |
| **Size** | small |
| **Lives in** | upjet — `pkg/controller/api.go`, `pkg/resource/sensitive.go` |
| **Evidence** | read |

## What happens

A typo in a `SecretKeySelector`'s `key:` does not error. It substitutes an
empty string into the live Terraform configuration, on **every reconcile**, so
the provider persistently drives the external resource towards a blank
password, token or key.

```go
// upjet pkg/controller/api.go:66-72
func (a *APISecretClient) GetSecretValue(ctx context.Context, sel xpv2.SecretKeySelector) ([]byte, error) {
    d, err := a.GetSecretData(ctx, &sel.SecretReference)
    if err != nil {
        return nil, errors.Wrap(err, "cannot get secret data")
    }
    return d[sel.Key], err   // err is nil here; a missing key returns nil, nil
}
```

A missing **key** returns `(nil, nil)` — no error at all. A missing **secret**
is also tolerated, because the caller applies `resource.IgnoreNotFound`
(`pkg/resource/sensitive.go:259-266`) and then writes
`string(sensitive)` — `""` — into the params regardless.

## Why it matters

The value goes into `params`, which is what the diff and the apply are computed
from. For a resource whose password or token is supplied by reference, the
provider's desired state silently becomes "empty". The user sees no event, no
condition and no error; the only symptom is the external resource being
reconfigured.

## The fix

Distinguish the three cases in `GetSecretValue`:

```go
v, ok := d[sel.Key]
if !ok {
    return nil, errors.Errorf("secret %s/%s has no key %q", sel.Namespace, sel.Name, sel.Key)
}
return v, nil
```

and at the call site, decide deliberately whether a *missing secret* should be
tolerated. It probably should — the secret may not exist yet — but the right
behaviour is to leave the parameter **unset** and requeue, not to set it empty.
Writing `""` is never the correct interpretation of "not yet available".

## How to test

* **Unit (upjet):** `GetSecretValue` with a present secret and an absent key
  returns an error. Fails today.
* **Unit (upjet):** `GetSensitiveParameters` with a not-found secret leaves the
  target path absent from `params` rather than setting `""`.
* **e2e:** create a resource referencing a secret key that does not exist;
  assert the MR reports an error condition and that no update is sent to AWS.

## Suggested issue

Repo: `crossplane/upjet`

**Title:** `A missing secret key silently resolves to "" and is written into the Terraform config`

**Body:**

> `APISecretClient.GetSecretValue` (`pkg/controller/api.go:66-72`) ends with
> `return d[sel.Key], err` where `err` is already `nil`. A `SecretKeySelector`
> naming a key that does not exist therefore returns `(nil, nil)` — no error —
> and the caller in `pkg/resource/sensitive.go` writes `string(sensitive)`,
> i.e. `""`, into the resource parameters.
>
> The same call site applies `resource.IgnoreNotFound`, so a missing *secret*
> also results in `""` rather than a deferral.
>
> Because this runs on every reconcile, a mistyped `key:` makes the provider
> persistently drive the external resource towards an empty password/token,
> with no error surfaced on the managed resource.
>
> Suggested fix: return an explicit error for a missing key, and for a
> not-yet-existing secret leave the parameter unset and requeue rather than
> substituting an empty string.

## Branch

`fix/error-on-missing-secret-key` (upjet fork)
