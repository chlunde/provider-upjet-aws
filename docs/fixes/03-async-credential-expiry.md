<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# 03. Credentials expire mid-operation on the async paths

| | |
| --- | --- |
| **Category** | data loss — a long create can fail partway with the object already made |
| **Severity** | high |
| **Size** | medium |
| **Lives in** | this repo — `internal/clients/aws.go` |
| **Evidence** | read (all four legs verified in source; not reproduced) |

## What happens

Every upjet operation on this provider is asynchronous. `Create`, `Update` and
`Delete` detach from the reconcile and run in a goroutine with a **one hour**
deadline. The Terraform AWS client that goroutine holds was built from a
**static credential snapshot**. With an `assumeRoleChain`, that snapshot is a
15-minute STS session.

A create that runs longer than the session — RDS instances, EKS clusters,
OpenSearch domains, CloudFront distributions — starts failing with expired
credentials partway through, after the external object exists.

| fact | source |
| --- | --- |
| `defaultAsyncTimeout = 1 * time.Hour` | upjet `pkg/controller/external_async_tfpluginsdk.go:28` |
| goroutine detaches: `WithDeadline(context.Background(), start+timeout)` | same file, `:145`, `:191`, `:241` |
| client gets `creds.AccessKeyID` / `SecretAccessKey` / `SessionToken` as strings | `internal/clients/aws.go`, `configureNoForkAWSClient` |
| assume-role sessions default to 15 minutes | `stscreds.DefaultDuration` |

## Correction to earlier analysis

[`reconcile-workflow-detail.md`](../reconcile-workflow-detail.md) §7 concluded
this was "safe today because the client is rebuilt every Connect … a constraint
on the fix, not a current bug." That holds only if the client's lifetime is
bounded by the reconcile. It is not: the async goroutine keeps the client it was
handed, detached, for up to an hour.

## The fix

Give the Terraform client a **refreshing** credentials provider rather than a
snapshot. `xpprovider.AWSConfig` takes static strings, so this needs one of:

1. Pass an `aws.CredentialsProvider` through to the client construction so the
   AWS SDK refreshes on expiry — the correct fix, needs a small change in the
   `upbound/terraform-provider-aws` fork's `xpprovider` surface.
2. Failing that, cap `defaultAsyncTimeout` at the credential expiry and let the
   operation resume on the next reconcile with fresh credentials. Weaker: it
   converts a hard failure into a retry, and upjet owns that constant.

Also raise the assume-role session duration where the role's
`MaxSessionDuration` permits — `SetAssumeRoleOptions`
(`internal/clients/provider_config.go:452`) already plumbs `Duration`, so a
ProviderConfig-level default of one hour narrows the window without any new
API.

Plan this together with [08](08-credentials-cache-all-sources.md): a credentials
cache that hands out a refreshing provider solves both.

## How to test

* **Unit:** assert the client is constructed with a provider whose
  `Retrieve` is called again after `Expires` passes, using a fake clock.
* **e2e — the only convincing test.** A ProviderConfig with an
  `assumeRoleChain` and a 15-minute session, creating a resource that takes
  longer than that (an RDS instance is the usual candidate at ~20+ minutes).
  Confirm the create completes. This reproduces the bug today.

## Suggested issue

Repo: `crossplane-contrib/provider-upjet-aws`

**Title:** `Long-running async operations fail with expired credentials when using assumeRoleChain`

**Body:**

> `configureNoForkAWSClient` copies the retrieved credentials into
> `xpprovider.AWSConfig` as static strings, so the Terraform AWS client holds a
> fixed snapshot with no ability to refresh.
>
> Every operation in this provider is async: upjet runs `Create`/`Update`/
> `Delete` in a goroutine detached from the reconcile, with a one-hour deadline
> (`external_async_tfpluginsdk.go:28,145`). With `assumeRoleChain`, the
> credentials in that snapshot come from an STS session that defaults to 15
> minutes (`stscreds.DefaultDuration`).
>
> Any operation running longer than the session — RDS, EKS, OpenSearch,
> CloudFront — should therefore begin failing with `ExpiredToken` partway
> through, after the external resource has been created.
>
> Established by reading the source; not yet reproduced against AWS. A
> reproduction would be a ProviderConfig with an assume-role chain plus an
> `aws_db_instance` create.
>
> Suggested fix: hand the Terraform client a refreshing
> `aws.CredentialsProvider` instead of a snapshot, and/or raise the default
> session duration where `MaxSessionDuration` allows.

## Branch

`fix/refreshing-credentials-for-async-ops`
