<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 01. Stop `MoveToStatus` mutating shared schema singletons

| | |
| --- | --- |
| **Category** | corruption — silent, provider-wide |
| **Severity** | critical |
| **Size** | small (the fix), large (the blast radius to re-test) |
| **Lives in** | [upjet](https://github.com/crossplane/upjet) — `pkg/config/common.go` |
| **Evidence** | measured |

## What happens

Removing a tag from `spec.forProvider.tags` produces **no diff**. The provider
reports the resource up to date and never removes the tag in AWS. Adding and
changing tags still work, which is why this survives testing.

## Why

`tftags.TagsSchema` is a `sync.OnceValue`, so every resource that declares
`tags` shares **one** `*schema.Schema` pointer, process-wide
(`terraform-provider-aws/internal/tags/tags.go:13`, upstream
`{Optional: true, Computed: false}`).

`MoveToStatus` recurses into `s.Elem.(*schema.Resource)` and sets
`Optional = false; Computed = true` **in place** (upjet
`pkg/config/common.go`). `aws_s3_bucket`'s `lifecycle_rule` embeds that exact
singleton (`internal/service/s3/bucket.go:318`), and
`config/cluster/s3/config.go:42` calls:

```go
config.MoveToStatus(r.TerraformResource, "acceleration_status", "acl", "grant",
    "cors_rule", "lifecycle_rule", ...)
```

So configuring one S3 resource flips the tags schema for every resource in the
process.

## Evidence

Measured with `hack/memprofile/reconcile`. Four unrelated resources, all
contaminated:

```
aws_instance   tags: samePtr=true diff{opt=false comp=true} fresh{opt=false comp=true}
aws_vpc        tags: samePtr=true diff{opt=false comp=true} fresh{opt=false comp=true}
aws_iam_role   tags: samePtr=true diff{opt=false comp=true} fresh{opt=false comp=true}
```

Real diff computation on `aws_iam_role`:

| operation | contaminated schema | upstream flags restored |
| --- | --- | --- |
| update a tag | `tags.env:"a"->"b"` | `tags.env:"a"->"b"` |
| create with tags | works | works |
| **remove all tags** | **no tags entries at all** | `tags.%:"1"->"0" tags.env:"a"->""` |

Scope: 491 of 495 resources with a `tags` field, plus `volume_tags`,
`final_backup_tags`, `principal_tags` and 14 nested `.tags` paths. A second
singleton — IAM policy documents — is contaminated the same way via
`config/cluster/apigateway/config.go:16`, reaching `policy` on KMS/SNS/VPC
endpoints and `policy_document` on codeartifact.

**This is not the same defect as [02](02-clear-schemafunc.md).** Here both
schema views agree — they are consistently wrong. Fixing 02 does not fix this.

## The fix

Deep-copy before mutating, in `MoveToStatus`:

```go
func MoveToStatus(sch *schema.Resource, fieldpaths ...string) {
    for _, f := range fieldpaths {
        s := GetSchema(sch, f)
        if s == nil {
            continue // was: return
        }
        c := *s          // copy the struct
        c.Optional = false
        c.Computed = true
        SetSchema(sch, f, &c)
        ...
    }
}
```

Two changes in one: the copy, and `return` → `continue`. The current `return`
aborts the **whole loop** on the first missing fieldpath, so one renamed
attribute upstream silently skips every remaining path in the call.

Nested `Elem` resources need the same treatment recursively — copy the
`schema.Resource` and its map, not just the leaf.

There are **40 `MoveToStatus` call sites** across `config/`, so the fix belongs
in the helper, not at the call sites.

## How to test

* **Unit (upjet):** build two `config.Resource`s sharing one `*schema.Schema`
  via a `sync.OnceValue`; call `MoveToStatus` on one; assert the other's flags
  are unchanged. This test fails today.
* **Unit (upjet):** call `MoveToStatus` with a missing fieldpath followed by a
  present one; assert the present one was still moved. Fails today.
* **Regression (this repo):** extend `hack/memprofile/reconcile` section 7 —
  assert that removing a tag from the params produces a `tags.%` diff entry for
  a sample of resources across several groups.
* **e2e — required before merge.** Create a resource with tags, remove one, and
  confirm the tag is removed in AWS. Also confirm the fix does not *newly*
  surface diffs on the S3 fields `MoveToStatus` was meant to suppress; that is
  the regression risk of this change.

## Suggested issue

Repo: `crossplane/upjet`

**Title:** `MoveToStatus mutates shared schema singletons, silently disabling tag removal provider-wide`

**Body:**

> `MoveToStatus` sets `Optional=false; Computed=true` in place on the
> `*schema.Schema` it is given, and recurses into nested `Elem` resources doing
> the same. Terraform providers commonly hand out **shared** schema pointers —
> `terraform-provider-aws` returns `tftags.TagsSchema()` from a
> `sync.OnceValue`, so every resource with a `tags` field shares one pointer.
>
> The result is that calling `MoveToStatus` on any fieldpath whose subtree
> contains such a shared schema mutates it for **every** resource in the
> process. In provider-upjet-aws, `config/cluster/s3/config.go` moves
> `lifecycle_rule` to status; `lifecycle_rule` contains `tags`; the tags schema
> becomes `Computed` for 491 of 495 taggable resources.
>
> The user-visible effect is silent: adding and updating tags still produce a
> diff, but **removing** a tag produces none, so the provider reports the
> resource up to date and never removes it in AWS.
>
> Measured on provider-upjet-aws @ 88c5c3776 with a local harness; diff
> computation for `aws_iam_role` shows `tags.%:"1"->"0"` present with the
> upstream flags and absent with the mutated ones.
>
> Suggested fix: copy the `schema.Schema` (and nested `schema.Resource` maps)
> before mutating. Separately, the `if s == nil { return }` should be
> `continue` — today one missing fieldpath aborts the remaining ones.

## Branch

`fix/movetostatus-copy-before-mutate` (in a `crossplane/upjet` fork)
