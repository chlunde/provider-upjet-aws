<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 02. Clear `SchemaFunc` after materialising `Schema`

| | |
| --- | --- |
| **Category** | correctness + waste |
| **Severity** | high |
| **Size** | one line |
| **Lives in** | upjet `pkg/config/provider.go:443`; workaroundable in this repo |
| **Evidence** | measured |

## What happens

upjet materialises the lazy Terraform schema but leaves the lazy accessor in
place:

```go
terraformResource.Schema = terraformResource.SchemaFunc()   // SchemaFunc still set
```

The Terraform SDK considers that combination invalid —
`helper/schema/resource.go:1313`, `"SchemaFunc and Schema should not both be
set"` — and `SchemaMap()` breaks the tie by preferring `SchemaFunc`, rebuilding
the whole map on every call. **955 of 1,029 configured resources** are in this
state.

Two consequences:

1. **Split schemas.** This repo's edits in `config/` are written to `.Schema`,
   which only the *diff* path reads. Everything reached through `SchemaMap()` —
   `RefreshWithoutUpgrade` (the read), `Apply` (create/update),
   `CoreConfigSchema()` — sees the unedited upstream schema. 35 resources
   measurably diverge (50 attributes), including 4 that exist only at diff time
   (`auto_generate_password` on `aws_rds_cluster`, `aws_db_instance`,
   `aws_docdb_cluster`; `auto_generate_auth_token` on
   `aws_elasticache_replication_group`) and 5 deleted from the diff schema but
   still live at apply time (`aws_apigatewayv2_deployment.triggers`,
   `aws_lambda_function.filename`, `aws_organizations_account.role_name`,
   `aws_wafv2_rule_group.rule`, `aws_wafv2_web_acl.rule`).
2. **A full schema rebuild four times per reconcile**, before the AWS read.

## Evidence

Measured with `hack/memprofile/reconcile`:

| resource | `SchemaMap()` | `CoreConfigSchema()` | with `SchemaFunc` cleared |
| --- | ---: | ---: | --- |
| `aws_instance` | 25 µs / 55 KB | 52 µs / 80 KB | 0 s / 0 KB, 23 µs / 24 KB |
| `aws_s3_bucket` | 20 µs / 46 KB | 44 µs / 67 KB | 0 s / 0 KB, 35 µs / 31 KB |
| `aws_iam_role` | 4 µs / 7 KB | 8 µs / 11 KB | 0 s / 0 KB, 4 µs / 4 KB |

Four `SchemaFunc` calls per Connect+Observe pair, excluding the AWS read, which
calls `SchemaMap()` several more times internally.

## The fix

```go
terraformResource.Schema = terraformResource.SchemaFunc()
terraformResource.SchemaFunc = nil // Schema is now authoritative; see SDK InternalValidate
```

Workaround available here without waiting for upjet — after
`config.GetProvider` returns, walk `pc.Resources` and nil the field. That is
the pragmatic short-term move given the dependency bump cadence.

**Review the 35 divergent resources as part of this change.** The fix makes the
provider's edits authoritative on paths that previously ignored them — that is
the intent, but it is a behaviour change for those resources, not a no-op.

## How to test

* **Unit (upjet):** after `NewProvider`, assert
  `r.TerraformResource.SchemaFunc == nil` and that `InternalValidate` passes.
* **Regression (this repo):** `hack/memprofile/reconcile` section 2 already
  reports the divergence set — assert it is empty.
* **e2e:** exercise at least one resource from each divergence class: a
  `auto_generate_password` resource (RDS), a deleted-attribute resource
  (`aws_lambda_function`), and a flag-difference resource
  (`aws_glue_catalog_database`).

## Suggested issue

Repo: `crossplane/upjet`

**Title:** `NewProvider leaves SchemaFunc set alongside Schema, so resource config edits never reach the read and apply paths`

**Body:**

> `pkg/config/provider.go:443` materialises the lazy schema with
> `terraformResource.Schema = terraformResource.SchemaFunc()` but does not clear
> `SchemaFunc`. The plugin SDK treats both-set as invalid
> (`helper/schema/resource.go:1313`) and `SchemaMap()` prefers `SchemaFunc`,
> returning a freshly built, unedited schema on every call.
>
> Provider configuration that edits `Resource.TerraformResource.Schema` is
> therefore visible only to the diff path (`schema.InternalMap(...).Diff`) and
> invisible to `RefreshWithoutUpgrade`, `Apply` and `CoreConfigSchema()`. On
> provider-upjet-aws this affects 955/1029 resources, with 35 measurably
> divergent — including synthetic attributes present at diff time and absent at
> apply time.
>
> It also costs a full schema rebuild on every `SchemaMap()` call: four per
> Connect+Observe pair before the read, 25 µs / 55 KB each for `aws_instance`.
>
> Suggested fix: set `terraformResource.SchemaFunc = nil` after materialising.

## Branch

`fix/clear-schemafunc-after-materialise` (upjet fork), or
`fix/clear-schemafunc-workaround` here for the interim walk.
