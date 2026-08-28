# Ideas

Not defects. Things this codebase could *become*. Every one is grounded in
something read in `/home/user/provider-upjet-aws`, `/home/user/upjet`,
`/home/user/crossplane-runtime` or the module cache; citations are file:line
where it matters. Where an existing document already establishes a fact I build
on it rather than re-deriving it, and I say so.

Confidence labels: **high** = the mechanism is read and the arithmetic is mine;
**medium** = mechanism read, sizing extrapolated; **speculative** = the shape is
right, the payoff is a guess.

---

## A. Shape and distribution of the shipped artifact

### I1. One binary, 178 packages — make the family split a runtime argument

**What.** `cmd/provider/*/zz_main.go` exists 178 times. `diff cmd/provider/ec2/zz_main.go
cmd/provider/s3/zz_main.go` is **four lines**: a leader-election ID string and three
`_ec2` → `_s3` function-name suffixes on `SetupWebhookWithManager_*`, `SetupGated_*`,
`Setup_*`. Every one of the 178 links the identical package graph — all of
`apis/{cluster,namespaced}`, all of `xpprovider`, all 266 AWS SDK clients — because
`clusterapis.AddToScheme` and `xpprovider.GetProvider` are unconditional.

Replace the 178 mains with one `cmd/provider/main.go` plus a generated
`map[string]func(ctrl.Manager, tjcontroller.Options) error` dispatch table, and select
the family from an environment variable baked into each family's image
(`ENV PROVIDER_FAMILY=ec2` in `cluster/images/provider-aws/Dockerfile`, which already
sets `TF_APPEND_USER_AGENT` the same way).

**Why the code motivates it.** Three consequences fall out of the binaries being
byte-identical today except for four lines:

* **Page cache.** `docs/memory-footprint.md` measures 692 MB of `Private_Clean`
  executable per pod. That is *per binary file*, not per pod: two pods mapping the
  same on-disk file share those pages. Today a cluster running ec2 + rds + iam + s3 +
  eks family pods on one node holds five separate ~692 MB text mappings of what is
  the same code. With one binary layer, one. This is invisible to
  `container_memory_working_set_bytes` (the memory doc explains why) but very visible
  as node memory pressure and as cold-start latency after eviction.
* **Registry and pull.** 178 packages × 2 platforms, each carrying its own copy of a
  ~1.3 GB binary. If the binary lives in a layer with an identical digest across all
  178 images, the registry stores it once and a node pulls and unpacks it once.
* **Build.** `Makefile:70` puts every `cmd/provider/%` into `GO_STATIC_PACKAGES`;
  `SUBPACKAGES=*` links 356 near-identical ~1.3 GB binaries per release. One link
  instead of 356.

**What it takes.** Small. A generated dispatch table (upjet already generates
`internal/controller/{cluster,namespaced}/zz_<family>_setup.go` per family, so the
table is a template change in `config/templates/main.go.tmpl`), an env-var read, and a
`Makefile` change so `GO_STATIC_PACKAGES` names one program. The per-family
`crossplane.yaml.tmpl` metadata, CRD subsets and `dependsOn` graph are unaffected.

**Cost and risk.** Real ones. (a) It is the *opposite* direction from
`docs/memory-footprint.md` §1–2 (per-family API packages and per-family TF service
packages), which make each binary smaller and therefore *different* — you cannot have
both sharing and trimming. Sharing is available now and needs no fork change; trimming
is worth more per pod but needs I2 and the fork. (b) A single binary means a
family-specific regression ships to every family at once — today a broken ec2 build
cannot break s3. (c) Whether the container runtime and the node actually share the
page cache depends on the storage driver deduplicating identical layers; overlayfs
with a shared lower layer does, but this should be verified on the target
distribution before the claim is made in a release note. (d) Leader-election IDs must
still differ per family — derive from the family name.

**Confidence** high on the mechanism and the build saving, medium on the page-cache
saving until someone measures two family pods on one node.

### I2. The AWS SDK is rooted by `*conns.AWSClient`'s method set, not by `servicePackages()`

**What.** In the fork, `internal/conns/awsclient_gen.go` defines **266 methods** on
`*AWSClient`, one per service, each returning a concrete client type
(`func (c *AWSClient) EC2Client(ctx context.Context) *ec2.Client`). That file is what
imports all 266 `aws-sdk-go-v2/service/*` packages. Meanwhile `*AWSClient` is handed
around as an empty interface: `terraform.Setup.Meta` is `any`, and
`xpprovider.GetFrameworkProviderWithMeta` takes `interface{ Meta() interface{} }`
(`internal/clients/aws.go:392-393`).

The proposal: in the fork, turn those 266 methods into free functions —
`func EC2Client(ctx context.Context, c *AWSClient) *ec2.Client`, emitted into
per-service files, or better, moved into the service packages themselves. Then the
`*ec2.Client` type is reachable only if `internal/service/ec2` is linked.

**Why the code motivates it.** `docs/memory-footprint.md` §2 proposes build-tag
partitioning `service_packages_gen.go` and `awsclient_gen.go` to trim the 317 MiB of
SDK symbols. Trimming `servicePackages()` alone will not do it: Go's linker only
prunes a method when its receiver type is *not* reachable through an interface and
`reflect.Value.Method` is not used anywhere in the program. Neither condition holds
here — `*AWSClient` is converted to `interface{}` on the hot path, and the plugin SDK
is reflection-heavy. Free functions sidestep the question entirely: an unreferenced
package-level function with an unreferenced return type is ordinary dead code.

This also makes the fork delta the memory doc worries about much smaller than it
looks. `xpprovider/xpprovider.go` is **76 lines**, plus a handful of accessors in
`internal/conns/awsclient_xp.go` (`SetAccountID`, `GetServicePackages`,
`SetServicePackagesField`). That is the entire Upbound fork. Adding a generated
per-service registration file is a comparable-sized, low-conflict addition — and if
service packages self-register via `init()` into a registry that `servicePackages()`
reads, no build tags are needed at all: the family binary simply imports
`xpprovider/svc/ec2`, and untagged builds that import `xpprovider/svc/all` stay
byte-identical to today.

**What it takes.** Medium, and it is in the fork, so it needs Upbound's buy-in and
carries rebase cost against upstream `terraform-provider-aws` forever. The generator
that emits `awsclient_gen.go` (`internal/generate/...`) has to emit free functions,
and every in-tree call site `client.EC2Client(ctx)` in 268 service packages becomes
`conns.EC2Client(ctx, client)` — mechanical, but it touches upstream files, which is
exactly what makes rebases painful. A less invasive variant: keep the methods but move
each into a file inside its own service package (methods on a type from another
package are not legal in Go, so this requires the free-function form; hence the
invasiveness is unavoidable).

**Cost and risk.** Large rebase surface in a vendored fork. And the payoff is only
realised together with the memory doc's per-family include list and per-family
`apis/` registration, i.e. it is step one of a three-step project — and it is
mutually exclusive with I1. Also: I have *not* verified with `go tool nm` that
converting to free functions actually drops the symbols; the linker reasoning is
read from Go's documented deadcode rules, not measured. Someone should build a
two-file probe before committing.

**Confidence** medium-high on the diagnosis (why trimming imports alone fails),
medium on the fix working as stated.

### I3. Build the resource configurations for one family, at runtime, from a generated list

**What.** Extends `docs/memory-footprint.md` §3 and `docs/architecture-wins.md` §1
rather than restating them: the point I want to add is that **this works in a single
binary**. `config.NewProvider` already takes include lists
(`config/registry_cluster.go:107-109`, `WithTerraformPluginSDKIncludeList(...)`), and
today they are passed the full 1,029-entry set from
`TerraformPluginSDKResourceList()`. Nothing about them is compile-time. Generate a
`familyResources map[string][]string` alongside the family setup files and pass
`familyResources[os.Getenv("PROVIDER_FAMILY")]` instead.

**Why.** The measured cost is 24 of 25 seconds of startup and the 386 MB arena. That
cost is *not* caused by the binary containing every family — it is caused by
`NewProvider` configuring 1,029 resources. So the two levers separate cleanly:
I1 fixes the text pages (shared), I3 fixes the arena and startup (per-process,
runtime-selected). Combined, you get a single ~1.3 GB shared binary whose ec2 pod
starts in about a second with a small arena — without touching the fork at all.

**What it takes.** Small-to-medium and entirely in this repo. The family→resource-name
map is derivable at generation time: upjet already knows each resource's `ShortGroup`
when it emits `internal/controller/cluster/zz_<family>_setup.go`. The awkward part is
the *reference closure* — `apis/cluster/ec2/v1beta2/zz_generated.resolvers.go` calls
`apisresolver.GetManagedResource("kms.aws.upbound.io", ...)`, so an ec2-only
`config.Provider` must still be able to resolve into kms. But `GetManagedResource`
reads `internal/apis`'s scheme, not `config.Provider.Resources`, so the closure matters
for scheme registration (cheap: 83 ms / 4 MiB measured) and not for the include list.
That is a much smaller problem than the memory doc implies.

**Cost and risk.** The webhook conversion registry (`zz_main.go:320`) walks both
providers' resource configs; a trimmed provider must not silently stop serving
conversions for a family's own CRDs. Also, a wrong include list is a *silent* loss of
a controller rather than a build error — needs a startup assertion that every
gated GVK has a matching `config.Resource`.

**Confidence** high; the numbers are already measured in `docs/memory-footprint.md`,
the novelty is only the "runtime, not compile-time" framing.

### I4. Ship a lean CRD variant: 61 % of `package/crds` is description text

**What.** Measured across all 2,065 files in `package/crds`: 101.9 MB total,
**62.2 MB (61.1 %) inside `description:` blocks**. The largest single CRD,
`firehose.aws.upbound.io_deliverystreams.yaml`, is 1.69 MB of which 1.07 MB (63 %) is
descriptions; `ec2.aws.upbound.io_instances.yaml` is 350 KB / 66 %. Offer a build
variant (or a package flavour) whose CRDs carry no field descriptions, and publish the
documentation where it belongs — the marketplace, `docs/`, or an `explain`-style
sidecar.

**Why the code motivates it.** The descriptions come from
`FilterDescription(cfg.MetaResource.Description, ...)` (`upjet/pkg/pipeline/crd.go:132`)
and per-field doc comments, and upjet has **no option to omit them** — grepping
`pkg/` for `OmitDescription`/`WithoutDescription` returns nothing. Meanwhile the
provider already goes to some trouble to drop this exact data at runtime:
`dropCodegenOnlyMetadata` (`config/registry_common.go:105`) releases `MetaResource`
after configuration precisely because nothing at runtime reads it. The CRDs are the
one place it is still shipped, and there it is paid for by the API server: every
established CRD's schema is held as a structural schema, and the aggregated OpenAPI
document is built from all of them. On a control plane with the ec2 family installed
(472 GVKs across both scopes), that is tens of MB of pure prose.

**What it takes.** Small: a post-processing pass over `package/crds` in the xpkg
build, or a codegen flag. Two package flavours (`provider-aws-ec2` and
`provider-aws-ec2-lean`) is more honest than silently dropping docs.

**Cost and risk.** `kubectl explain`, IDE completion and marketplace rendering all
read the CRD descriptions; removing them is a genuine UX regression for anyone who
uses them, which is why this should be a *variant*, not the default. I did not
measure API-server memory — the byte counts are measured, the API-server consequence
is inferred from how CRD structural schemas and OpenAPI aggregation work.

**Confidence** high on the bytes, medium on the API-server payoff.

---

## B. Generation and configuration maintenance

### I5. Collapse `config/cluster` and `config/namespaced` into one tree

**What.** The two trees are 114 and 112 Go files, 10,367 and 8,055 lines. Normalise
the import paths (`s#config/cluster#config/XX#`) and diff them: **96 of 106 service
configs become byte-identical**, and of the ten that differ, two differ only by the
presence of a `config_test.go`. The entire substantive delta is API version history:
`r.Version = "v1beta2"`, `r.PreviousVersions = append(..., "v1beta1", "v1beta2")`,
`r.SetCRDStorageVersion(...)`, `r.SetDeprecatedVersion(...)`, and — for
`config/cluster/kafka/config.go` — 194 lines of a hand-written `NewCustomConverter`
between `v1beta1.Cluster` and `v1beta2.Cluster` that the namespaced tree does not need
because it was born at v1beta1.

Make it one `config/services/<name>/config.go` parameterised by scope, and move the
version history into a declarative per-resource data file (YAML next to
`old-singleton-list-apis.txt` and `field-rename.yaml`, which already play this role).

**Why the code motivates it.** Duplication that is 96 %-identical is duplication that
*will* drift. It already has: `config/cluster/autoscaling/config_test.go` and
`config/cluster/elasticache/config_test.go` exist with no namespaced counterpart, so
whatever they assert is unasserted for half the provider. And every new AWS service
onboarded is two near-identical files a reviewer has to compare by eye.

**What it takes.** Medium and mechanical: the scope is already a parameter everywhere
that matters (`config/cluster/common` vs `config/namespaced/common` is itself a
duplicated pair; `registry_cluster.go` and `registry_namespaced.go` differ mainly in
root group and the version-bumping call). The version-history extraction is the real
work, and it is worth doing on its own: `configureSingletonListAPIConverters`
(`config/registry_cluster.go:161-235`) already reads a data file
(`old-singleton-list-apis.txt`) to decide which resources have version history, so the
pattern exists.

**Cost and risk.** A large, mostly-mechanical diff over the file set reviewers use to
onboard resources; a bad merge here changes CRD storage versions, which is the one
mistake that is not recoverable by rollback. Would want the API round-trip tests
(`upjet/docs/api-roundtrip-testing.md`) green before and after, and a
`crddiff`-verified no-op on `package/crds`.

**Confidence** high that the duplication is real and near-total; medium on effort.

### I6. The import-statement scraper has been dead for AWS the whole time — fix it, then derive external names from it

**What.** `config/provider-metadata.yaml` contains 1,676 resources. The number with a
non-empty `importStatements` list is **zero** — `grep -c "importStatements: \[\]"`
returns 1,676. The scraper's default XPath is
`//code[@class="language-shell"]/text()` (`upjet/cmd/scraper/main.go:25`), and the
Terraform AWS docs long ago moved their import examples to HCL `import { to = ...
id = ... }` blocks in `language-terraform` fences. `generate/generate.go:23` invokes
the scraper with no `--import-xpath` override, so the field has been silently empty
for the life of this file.

Meanwhile `config/externalname.go` is 4,085 lines whose comments are *hand-transcribed
paraphrases of exactly that documentation*:

```
// Bedrock Guardrail can be imported using the composite ID: guardrail_id,version
"aws_bedrock_guardrail": bedrockGuardrail(),
// AWS Batch job queue can be imported using the name
"aws_batch_job_queue": config.TemplatedStringAsIdentifier("name", ...),
```

**Why this is an idea and not just a bug.** Restoring the scrape is the bug fix; the
idea is what it unlocks. With `importStatements` populated for 1,676 resources you
get, in rough order of value:

1. **A drift check in CI.** For every one of the 1,029 onboarded resources, assert
   that the configured `ExternalName` shape is consistent with the documented import
   ID (single field → `ParameterAsIdentifier`; composite with a separator →
   templated; opaque → `IdentifierFromProvider`). Today nothing catches a doc change
   upstream; the failure mode is an MR that adopts the wrong external resource.
2. **A first-pass proposal for the 649 not-yet-onboarded resources.**
   `config/generated.lst` has 1,030 entries against 1,676 in the schema, and the
   gating work per resource is precisely the external-name decision. A generator that
   emits a *proposed* `config.ExternalName` plus a TODO comment turns a research task
   into a review task.
3. **Better generated examples.** Same metadata, same pipeline.

**What it takes.** The scrape fix is a flag. The consistency checker is a day. The
proposal generator is a week and will be wrong often enough that its output must
land as a reviewed patch, never auto-merged.

**Cost and risk.** The XPath fix is upstream-doc-format-coupled and will break again;
it needs a test that fails loudly when the extraction yields zero results across a
whole provider (which is the actual missing safeguard). The proposal generator risks
lending false confidence — a wrong external name is the highest-severity failure mode
this provider has (it adopts or orphans real infrastructure), so the generator should
emit resources into a quarantined list, not into `generated.lst`.

**Confidence** high — the empty field is measured, and the transcription in
`externalname.go` is right there.

### I7. Inject the known referencers at every schema depth, not just the top level

**What.** `KnownReferencers()` (`config/overrides.go:87-...`) is the heuristic that
gives the provider most of its cross-resource references for free: any field named
`vpc_id` gets an `aws_vpc` reference, anything ending `role_arn` gets an
`aws_iam_role` reference with an ARN extractor, `subnet_ids`, `security_group_ids`,
and so on. It iterates `for k, s := range r.TerraformResource.Schema` — **the top
level only**. Nested blocks are never visited.

Measured against `config/schema.json`, restricted to the 1,029 onboarded resources and
to exactly the field names `KnownReferencers` recognises: 324 matching fields at the
top level (all covered), and **284 matching fields nested one or more levels down, of
which 228 have no reference configured anywhere** — 90 `*role_arn`, 22 `kms_key_id`,
20 `security_group_ids`, 20 `kms_key_arn`, 18 `vpc_id`, 16 `subnet_ids`, 8 `subnet_id`.
Examples: `aws_apprunner_service.instance_configuration.instance_role_arn`,
`aws_appsync_graphql_api.log_config.cloudwatch_logs_role_arn`,
`aws_appflow_flow.metadata_catalog_config.glue_data_catalog.role_arn`.

Nesting is not the obstacle. `upjet/pkg/types/builder.go:154-156` looks up
`cfg.References[cPath]` where `cPath` is a dotted `traverser.FieldPath`, and
132 of the 410 hand-written `r.References["..."]` keys in `config/cluster/*/config.go`
are already dotted (`source.eks.subnet_ids`, `domain_name_configuration.certificate_arn`).
The machinery works; only the automatic injector is shallow.

**Why it matters.** Every one of those 228 fields is a place where a Crossplane user
must hard-code an ARN or an ID instead of writing `roleArnRef: {name: my-role}`. That
is the single feature that distinguishes a Crossplane provider from `terraform apply`,
and it is missing on roughly 40 % of the surface it should cover.

**What it takes.** Small: replace the top-level loop with a schema traversal (upjet
already has `pkg/schema/traverser`, and the provider already registers
`&config.SingletonListEmbedder{}` as a traverser at `config/registry_cluster.go:117`),
accumulating the dotted path. Reuse the existing match rules verbatim.

**Cost and risk.** This changes generated CRD schemas — each injected reference adds
`...Ref` and `...Selector` fields — so it is an additive API change across ~116
resources at once, which means a large `crddiff` diff and a version-bump conversation.
Additive optional fields do not need a version bump, but reviewers will want to check
that. Also the heuristics will misfire somewhere in 228 attempts (a `role_arn` inside
a block that names a *service-linked* role, say); the injector needs a per-resource
opt-out list, which `config.Resource` can carry. Do it in tranches, one service group
at a time.

**Confidence** high on the gap and its size; medium on the blast radius.

---

## C. What the provider tells you

### I8. Surface the Terraform diff — status, event, and change log

**What.** Every `Observe` computes a full `*terraform.InstanceDiff` describing exactly
which attributes differ and how (`InstanceDiff.Attributes` is
`map[string]*ResourceAttrDiff` with `Old`, `New`, `RequiresNew`, `Sensitive`). The
provider then reduces it to a single boolean: `hasDiff := !n.instanceDiff.Empty()` →
`ResourceUpToDate: !hasDiff` (`upjet/pkg/controller/external_tfpluginsdk.go:566,637`).
The only place the content escapes is
`n.logger.Debug("Diff detected", "instanceDiff", instanceDiff.GoString())` at
`:480` — debug level, Go struct dump, off by default.

Publish it instead, three ways:

* **A condition or a status field.** `status.drift: [{path: tags.Owner, from: a,
  to: b, requiresReplace: false}]`, capped in size, sensitive attributes redacted via
  the `ResourceAttrDiff.Sensitive` flag that is already there.
* **A Kubernetes Event.** The reconciler already has a recorder
  (`managed.WithRecorder(event.NewAPIRecorder(...))` in every generated
  `zz_controller.go:70`).
* **The change log.** `managed.ExternalUpdate`, `ExternalCreation` and
  `ExternalDeletion` all carry an `AdditionalDetails map[string]string`
  (`crossplane-runtime/pkg/reconciler/managed/reconciler.go:551-574`), which the
  change logger forwards verbatim to the gRPC change-log service
  (`changelogger.go:111`). The provider *wires the feature up* —
  `cmd/provider/*/zz_main.go:279-287`, `--enable-changelogs` — but grepping all of
  `upjet/pkg` for `AdditionalDetails` returns **nothing**. Every upjet change-log
  entry is therefore "an update happened", with no indication of what changed. The
  changed attribute set is sitting in `n.instanceDiff` two frames away.

**Why it matters.** "Why does my resource keep updating / why is it never Ready" is
the archetypal upjet support question, and the answer is in memory at exactly the
moment the provider decides not to tell anyone. `UpdateLoopPrevention`
(`upjet/pkg/config/resource.go:750`, invoked at `external_tfpluginsdk.go:761`) exists
specifically because update loops are common enough to need a guard — and when it
fires it reports only a `Reason` string, not the diff that caused it.

**What it takes.** The `AdditionalDetails` half is genuinely small — a helper that
flattens `instanceDiff.Attributes` into `map[string]string` with sensitive values
elided, wired into the three return sites in `external_tfpluginsdk.go` and their
plugin-framework twins. The status/condition half is larger, because status fields on
an MR need an API version and a size bound.

**Cost and risk.** Leaking secrets. `ResourceAttrDiff.Sensitive` marks fields the SDK
knows are sensitive, but the provider's own experience (`resource.GetConnectionDetails`
uses upjet's separate sensitive-path model) suggests the two notions do not perfectly
coincide; anything published must be allowlisted by path, not blocklisted. Second, a
status field that changes every reconcile re-introduces exactly the no-op-status-PUT
cost that fix 14 just removed — so drift detail must be written only when it *changes*.

**Confidence** high.

### I9. Plan mode: compute the diff under `managementPolicies: [Observe]` without applying it

**What.** A `terraform plan` for Crossplane: an MR whose spec you have edited, which
reports what *would* change and does nothing. Today the closest thing is
`managementPolicies: [Observe]`, and it is exactly wrong for this purpose — upjet
*skips the diff computation entirely* under observe-only:

```go
isObserveOnlyPolicy := policySet.Equal(observeOnlyPolicy)
if !isObserveOnlyPolicy || !n.isManagementPoliciesEnabled {
    n.instanceDiff, err = n.getResourceDataDiff(...)
}
```
(`upjet/pkg/controller/external_tfpluginsdk.go:559-565`.)

The pause annotation is not an alternative either: `meta.IsPaused` returns before
Observe runs (`crossplane-runtime/pkg/reconciler/managed/reconciler.go:982`).

**What.** Compute the diff under observe-only, publish it via I8, and never call
`Apply`. That is a two-line inversion plus I8.

**Why.** It converts the management-policy feature from "watch and do nothing" into
"watch and tell me what you would do", which is what reviewers actually want before
they hand a Crossplane provider write access to production. It also gives a safe
migration path: run new resources observe-only, read the drift report, then widen the
policy.

**Cost and risk.** The observe-only skip exists to save work
(`docs/architecture-wins.md` §4 measures the diff at 89 % of translation cost for
`aws_instance`), so this makes observe-only MRs as expensive as managed ones. Should
be opt-in — a `ManagementActionObserve` + annotation, or a new action — not a change
to what `[Observe]` means today. Also `filterInitExclusiveDiffs` and
`assertNoForceNew` assume a diff that will be applied; their behaviour under a
never-applied diff needs a read.

**Confidence** high on the mechanism, medium on whether it is worth the CPU without
I8 landing first.

### I10. An adoption path for existing AWS infrastructure

**What.** A supported, documented, tooled flow for putting existing AWS resources
under Crossplane management. Four pieces, three of which already exist:

1. **The external name is the Terraform import ID.** That is what
   `config/externalname.go` *is* — 1,029 entries mapping a resource to how its import
   ID is formed. Nothing else is needed to point an MR at existing infrastructure.
2. **Required fields are already waived under observe-only.** The generated CEL rule
   is `!('*' in self.managementPolicies || 'Create' in self.managementPolicies ||
   'Update' in self.managementPolicies) || has(self.forProvider.filter) || ...`
   (seen in `package/crds/accessanalyzer.aws.m.upbound.io_archiverules.yaml`, present
   in 1,352 of 2,065 CRDs). So an MR with only an external-name annotation and
   `managementPolicies: [Observe, LateInitialize]` *passes admission today*.
3. **Late-initialisation fills the spec from the live resource.**
   `mg.LateInitialize(buff)` at `external_tfpluginsdk.go:600` does exactly this.
4. **Discovery is missing.** Something has to enumerate what exists and emit stub
   MRs.

For (4), `docs/architecture-wins.md` §2 already established the mechanism: the Resource
Groups Tagging API returns ~100 resources per call with their ARNs. Reverse the
direction — instead of mapping ARNs to existing MRs, map ARNs to *proposed* MRs using
the `GroupMap`/`KindMap` in `config/groups.go` and the external-name configs.

**Why.** This is the single largest adoption blocker for a Crossplane provider and the
provider is about 80 % of the way there without knowing it. The remaining 20 % is a
CLI and a page of documentation.

**What it takes.** Medium and additive — a standalone tool, not a provider change.
Reads an account, emits YAML, the user reviews and applies with observe-only, watches
`spec.forProvider` fill in, then flips to `[*]`. The one provider-side change worth
making is a documented, tested "promotion" step: today flipping from
`[Observe, LateInitialize]` to `[*]` on a resource whose spec was late-initialised is
an untested transition, and `spec.initProvider`/`forProvider` interactions
(`filterInitExclusiveDiffs`) make it non-obvious.

**Cost and risk.** The failure mode of a wrong external name is adopting or deleting
the wrong resource, so the tool must never write to a cluster and its output must be
reviewed. Untaggable resource types are invisible to the tagging sweep, as §2 of the
architecture doc already notes. And late-init does not populate fields the API does
not return, so a promoted MR can immediately want to "fix" them — which is precisely
why I9 (plan mode) should ship first: adopt observe-only, read the plan, *then*
promote.

**Confidence** medium-high; every ingredient is read, the assembly is untested.

### I11. Promote `hack/memprofile` from throwaway instrumentation to a CI gate

**What.** `hack/memprofile/README.md` opens with "Throwaway instrumentation ... Not
built or shipped as part of the provider." It measures startup wall time, live heap,
RSS by phase, per-reconcile allocation for `Connect`/`Observe`, and link cost per
package set. Nine of the fourteen fixes in `docs/fixes/` are justified by its numbers.

Turn `startup` and `reconcile` into a benchmark that CI runs on every PR, with
recorded thresholds. Not a full run — the `xpprovider` link is ~30 minutes cold, per
the fixes README — but the numbers that matter (live heap after
`config.GetProvider*`, allocations per `Observe` for two representative resources)
are cheap once the build cache is warm.

**Why.** Every one of these regressions is invisible to `go test`: nothing fails when
someone re-adds a `SchemaFunc()` call, restores `MetaResource`, or reintroduces a
per-`Connect` client build. `docs/fixes/README.md` records that
`dropCodegenOnlyMetadata` was verified by rebuilding and re-measuring by hand; that
verification decays the moment nobody re-runs it.

**Cost and risk.** Small effort, real maintenance tax: allocation benchmarks are noisy
on shared CI runners, so thresholds must be generous (±20 %) or the gate becomes a
flake everyone learns to re-run. The `ci.yml` jobs already start with
`jlumbroso/free-disk-space`, which is a signal that adding a heavy build job to this
pipeline is not free.

**Confidence** high.

---

## D. Reconcile policy and lifecycle

### I12. Give `ReconciliationPolicy` a poll interval — both halves of the plumbing already exist

**What.** upjet gained a `ReconciliationPolicy` in 2026
(`upjet/apis/configuration/v1alpha1/reconciliation_policy.go`,
`upjet/pkg/reconciler/reconciliationpolicy/`), resolved per-MR from the
ProviderConfig (`internal/clients/pc_resolver.go:148`) and wired into every generated
controller (`zz_controller.go:114-119`). It has exactly one knob:
`ExponentialFailureRateLimiter{BaseDelay, MaxDelay}` — failure backoff only.

Separately, crossplane-runtime has `WithPollIntervalHook(func(managed, time.Duration)
time.Duration)` (`reconciler.go:705-721`), a per-MR dynamic poll interval, and upjet
uses it for nothing but jitter (`zz_controller.go:82`).

Connect them: add `pollInterval` (and, more usefully, a list of
`{kindSelector, pollInterval}` rules) to `ReconciliationPolicy`, and implement the
hook to read the resolved policy.

**Why the code motivates it.** `docs/architecture-wins.md` §2 makes the case for
polling less and reacting to change more, and observes that
`crossplane.io/poll-interval` already exists per-MR. But per-MR annotations do not
scale to 5,000 resources — an operator wants "poll `SecurityGroup` every 5 minutes and
`DBInstance` every 6 hours", set once. That is a *policy*, and a policy object with
a per-MR resolution `Source` was just built and left with one field in it. The
architecture doc's drift-sentinel proposal becomes far more practical when the long
poll interval can be set fleet-wide rather than stamped on every object.

**What it takes.** Small in upjet. `Source` already returns the policy per MR on
every reconcile; the hook signature already takes the MR. The one design question is
caching — `Source` does a ProviderConfig read per reconcile, which the poll hook would
now also need.

**Cost and risk.** Making poll interval remotely configurable makes it remotely
mis-configurable: a policy setting 24 h on everything silently disables drift
detection. Needs a floor (crossplane-runtime already has `minPollInterval`,
`reconciler.go:868`) and a metric. Small upstream change but it is an API addition to
an alpha type, so it needs upjet maintainer buy-in.

**Confidence** high — both ends are read, and nothing connects them.

### I13. Immutable fields: tell users at admission, and offer opt-in replacement

**What.** Two halves of one gap.

*Half one, admission.* Terraform marks fields `ForceNew` when changing them requires
destroying and recreating. upjet never reads that flag anywhere in code generation —
grepping all of `upjet/pkg` for `ForceNew` returns three hits, all inside
`assertNoForceNew` in the runtime. The generated CRDs carry
`x-kubernetes-validations` only for "required parameter" rules. So a user can `kubectl
apply` a changed `availability_zone` and get a green `kubectl apply` followed by a
permanently `Synced: False` resource. Emitting a CEL transition rule
(`rule: "self == oldSelf"`, `optionalOldSelf` semantics for the create case) for
`ForceNew` fields would reject the edit at admission with the field name in the
message.

*Half two, replacement.* Today the refusal is permanent:

```go
if ad.RequiresNew {
    return errors.Errorf("cannot change the value of the argument %q from %q to %q", ...)
}
```
(`external_tfpluginsdk.go:749-753`), and `crossplane-runtime` has no notion of
replacement — grepping `pkg/reconciler/managed/` for `RequiresNew|Replace|recreate`
returns nothing. The user's only recourse is to delete the MR and recreate it, which
means writing out the resource by hand. An opt-in
`crossplane.io/replace-on-immutable-change: Allowed` annotation that lets the
reconciler perform Delete-then-Create would make the recourse explicit and auditable
instead of manual. Terraform has had exactly this (`-replace`, and before that
`taint`) for a decade.

**Why together.** The CEL rule alone would be *wrong* if replacement were supported;
replacement alone leaves users discovering the constraint at reconcile time. Shipping
the admission rule with an escape hatch is the coherent design: the rule fires unless
the replacement annotation is present.

**What it takes.** Half one is medium and in upjet's type builder: the `ForceNew` flag
is on `*schema.Schema` and reaches the builder already; emitting a
`+kubebuilder:validation:XValidation` marker per field is a template change. Half two
is large and upstream in crossplane-runtime, touching the most safety-critical code in
the project.

**Cost and risk.** Serious, and I would ship only half one initially.
(a) CEL cost budget: the API server enforces a per-CRD estimated cost limit, and these
schemas are enormous — `firehose.aws.upbound.io_deliverystreams.yaml` is 1.69 MB with
thousands of fields. Adding a transition rule per ForceNew field could push a CRD over
the limit and make it fail to establish. That must be measured on the largest CRDs
before anything ships. (b) CRD size grows, working against I4. (c) Replacement is a
destructive operation driven by a spec edit; the failure mode is deleting production
infrastructure because a diff was computed against a stale refresh. It needs a
two-step confirmation, not a single annotation, and probably an owner-reference and
finalizer story nobody has designed.

**Confidence** medium on half one (mechanism certain, CEL budget unknown), speculative
on half two.

### I14. ProviderConfig parity with the Terraform provider's own configuration

**What.** `configureNoForkAWSClient` (`internal/clients/aws.go:346-399`) builds an
`xpprovider.AWSConfig`, which is `conns.Config` — a 39-field struct. The provider
populates **eleven** of them: `AccessKey`, `SecretKey`, `Token`, `Region`,
`Endpoints`, `S3UsePathStyle`, `SkipCredsValidation`, `SkipRegionValidation`,
`SkipRequestingAccountId`, `EC2MetadataServiceEnableState`, and the assume-role chain
elsewhere. `apis/namespaced/v1beta1/types.go` (326 lines) exposes nothing else.

Fields with real operational value that are simply unreachable:

| `conns.Config` field | why a user wants it |
| --- | --- |
| `DefaultTagsConfig` | Terraform's most-used provider feature. Today the only fleet-wide tagging is the three `crossplane-*` tags from `AddExternalTagsField`, all-or-nothing via `--skip-default-tags` (`config/templates/main.go.tmpl:91`). |
| `IgnoreTagsConfig` | Without it, any external system that tags resources (cost allocation, backup, security tooling) creates permanent drift and an endless update loop. |
| `MaxRetries`, `RetryMode`, `TokenBucketRateLimiterCapacity` | The provider makes at least one describe per MR per poll and has no AWS-side throttling control at all — `grep -n "Throttl\|RetryMode\|retry\." internal/clients/*.go` returns nothing. |
| `AllowedAccountIds`, `ForbiddenAccountIds` | A blast-radius guardrail: refuse to act if the resolved account is not the expected one. The provider already resolves the account ID (`getAccountId`) and caches it; the check is a comparison. |
| `HTTPProxy`/`HTTPSProxy`/`NoProxy`, `CustomCABundle`, `Insecure` | Regulated and air-gapped environments; today only settable via process-wide env vars, which cannot differ per ProviderConfig. |
| `UseFIPSEndpoint`, `UseDualStackEndpoint` | FedRAMP and IPv6-only clusters. Today reachable only by enumerating every service in `spec.endpoint.services`. |

**Why.** These are not exotic. `ignoreTags` in particular is a recurring source of
"my resource never becomes Ready", and the fix is one field the provider already has
a struct member for.

**What it takes.** Small per field and entirely in this repo: an API addition to
`ProviderConfigSpec` and a line in `configureNoForkAWSClient`. `defaultTags` and
`ignoreTags` are the two with real design content (they interact with
`AddExternalTagsField` and `TagsAllRemoval` in `config/overrides.go:56-63`, which
deliberately makes `tags_all` computed-only).

**Cost and risk.** Each field is an API addition to a v1beta1 type — additive and
optional, but permanent. `defaultTags` in particular changes what the diff sees on
every tagged resource and could cause a one-time update storm across a fleet on
upgrade; it should be introduced with a documented rollout. Note also that
`skip_credentials_validation` etc. are already exposed with `snake_case` JSON names in
a Go/Kubernetes API — new fields should not copy that.

**Confidence** high.

---

## E. Testing

### I15. An offline reconcile harness: record AWS HTTP once, replay in CI

**What.** There is no way to test a reconcile without an AWS account. `.github/workflows/ci.yml`
runs lint, unit tests, `check-examples.py`, and a local deploy; the only functional
testing is `make uptest`/`family-e2e` against a real account, triggered separately
(`uptest-trigger.yaml`). In `internal/` there are **2 test files against 2,436 Go
files**, and both are in `internal/clients` (`cache_test.go`, `partitions_test.go`).
`docs/lead-triage.md` L30 already records that the hand-written, correctness-sensitive
code is the untested code.

Build a record/replay layer at the AWS HTTP boundary and run it in PR CI.

**Why this is feasible here specifically.** Most of the plumbing exists:

* Endpoint redirection is a first-class ProviderConfig feature —
  `spec.endpoint.url.static` plus `spec.endpoint.services`
  (`apis/namespaced/v1beta1/types.go:122-124`) feeds
  `tfAwsConnsCfg.Endpoints[service]` (`internal/clients/aws.go:363-372`).
* Credential and account-ID validation can already be short-circuited:
  `SkipCredsValidation` returns the constant `localstackAccountID = "000000000000"`
  (`internal/clients/aws.go:33,116-119`). Someone already made this work against
  LocalStack.
* The SDK is already instrumented with a middleware hook —
  `tfAwsConnsClient.AppendAPIOptions(withExternalAPICallCounter)`
  (`internal/clients/aws.go:397`) — which is exactly the insertion point a recorder
  needs.

So: a nightly job that runs the existing uptest examples against real AWS with the
recorder on, storing cassettes; and a PR job that replays them, exercising the whole
`Connect → Observe → Create → Observe → Update → Delete` path in-process with no
credentials.

**Why it matters more than a normal test suite.** Nine of the fourteen fixes in
`docs/fixes/` are on this path, and the README's own "Verification ceiling" section
says none of them has run against AWS or a cluster. The `-race` verification of fix 12
was abandoned because linking `internal/clients` pulls in `terraform-provider-aws` and
exhausted the disk twice. A replay harness does not remove the link cost, but it makes
one expensive build serve hundreds of assertions.

**Cost and risk.** Medium-to-large. Cassettes go stale as AWS APIs evolve, and a stale
cassette is a test that passes for the wrong reason — the nightly re-record is not
optional, it is the whole design. Recorded traffic contains account IDs, ARNs and
possibly secrets, so scrubbing has to be part of the recorder, not a review step.
And the same disk/link problem that blocked fix 12's verification applies: the CI
runners already need `free-disk-space` before they can build this repo.

**Confidence** medium-high on feasibility, medium on whether the maintenance is
sustainable.

---

## F. Long shots

### I16. Typed CRDs over the Cloud Control API, for the coverage Terraform does not have

**What.** `aws_cloudcontrolapi_resource` is already onboarded
(`config/generated.lst:156`, `package/crds/cloudcontrol.aws.upbound.io_resources.yaml`)
but it is an escape hatch: a `desired_state` JSON blob with no schema, no references,
no field validation. AWS publishes CloudFormation resource schemas for ~1,100 resource
types, and Cloud Control gives uniform CRUDL over all of them through *one* SDK client.
A generator that emits typed Crossplane CRDs from those schemas and reconciles them
via `cloudcontrol` would produce a provider whose binary is a few tens of MB rather
than 1.24 GB, whose startup is milliseconds, and whose coverage tracks AWS service
launches rather than Terraform's onboarding queue.

**Why the code motivates it.** The measurements in `docs/memory-footprint.md` say the
Terraform layer *is* the cost: 317 MiB of AWS SDK across 269 clients, 106 MiB of
terraform-provider-aws, 24 s of startup building resource configurations. None of that
is inherent to managing AWS from Kubernetes; it is the price of the Terraform provider
as the execution engine. Note also `aws-cloudformation-resource-schema-sdk-go` is
already in the module graph.

**Cost and risk.** This is a different provider, not a change to this one. Cloud
Control's coverage overlaps but does not equal Terraform's; its semantics are weaker
(no `ForceNew` metadata, cruder drift detection, patch-based updates); none of the
1,029 external-name configurations, 410 references or 2,410 examples carry over; and
the migration story for existing users is "rewrite your manifests". I would treat this
as an argument for a *complementary* provider covering what upjet-AWS does not, not a
replacement. Listed because the memory measurements make the question unavoidable.

**Confidence** speculative.

### I17. Observe-only managed resources from Terraform data sources

**What.** upjet has no concept of a Terraform data source — grepping all of
`upjet/pkg` for `DataSource|DataSourcesMap` returns nothing, and the whole pipeline is
built around `ResourcesMap`. Yet the TF AWS provider ships hundreds of data sources
(`aws_ami`, `aws_availability_zones`, `aws_vpc`, `aws_caller_identity`) that answer the
question Crossplane users ask constantly: "reference the thing I did not create."

Generate read-only CRDs from `DataSourcesMap` whose reconciler calls only
`ReadDataApply`, publishing results to `status.atProvider` and connection details, so
they become valid targets for the existing reference-resolution machinery.

**Why the code motivates it.** The reference resolver
(`crossplane-runtime/pkg/reference/reference.go:338`) resolves against any
`resource.Managed` in the scheme. A data-source-backed observe-only MR would slot into
the existing `...Ref`/`...Selector` fields with no changes to the 410 configured
references. The alternative users have today — create an MR with
`managementPolicies: [Observe]` — only works when a matching *resource* type exists
and only when you already know the external name, which is exactly the case where you
do not need a lookup.

**Cost and risk.** Large and in upjet: a new CRD flavour, a new reconciler, a new
externalname-equivalent (data sources are queried by arguments, not by ID), and a
lifecycle question with no good answer (when is a lookup "deleted"? what is a
finalizer for?). Crossplane's `Managed` contract assumes ownership of an external
resource; a data source owns nothing, so this may want to be a different kind
entirely — closer to Crossplane's `Object`/observe-only pattern than to an MR.

**Confidence** speculative on the design; high that the gap is real and often felt.

---

## Considered and rejected

* **Share the generated `apis/cluster` and `apis/namespaced` types.** They look
  duplicated (138 MB, 42.1 MiB of `DeepCopy` symbols) but they genuinely differ:
  `diff` of `zz_instance_types.go` across scopes shows singleton lists (`[]X`) on the
  cluster side against embedded objects (`*X`) on the namespaced side, and
  `v2.Reference` against `v2.NamespacedReference`. Generics would not survive
  controller-gen. Not shareable.
* **Make `KnownReferencers` derive references from ARN *format* rather than field
  name.** Tempting (an ARN's service and resource-type segments name the target), but
  the fields are `*_id` as often as `*_arn`, and upjet needs the target's *external
  name*, which for many types is not the ARN. The name heuristic is crude but
  correct; I7 extends it rather than replacing it.
* **Cache resolved references across reconciles.** Already done — `IsNoOp()`
  (`crossplane-runtime/pkg/reference/reference.go:212-232`) short-circuits when
  `CurrentValue != ""` unless the resolution policy is `Always`. No Get per reference
  per reconcile.
* **Use `crossplane.io/paused` as a plan mode.** Does not work: `meta.IsPaused` returns
  before `Observe` (`reconciler.go:982`). Hence I9's route through management policies.
* **Derive `globalGroups`/`globalResources` (`internal/clients/aws.go:44-92`)
  automatically.** They are hand-maintained and duplicated per scope with an `.m.`
  prefix, which is ugly, but the AWS endpoint data does not cleanly say "this API group
  is global" and the list is short. A one-line scope-suffix strip would remove the
  duplication; too small to write up.
* **Replace the whole 178-package family split with the monolith.** The monolith is
  deprecated (`package/crossplane.yaml.tmpl:18-24`) and the split exists for CRD count,
  not binary size. Not the problem.
* **Push the TF import-doc scraping upstream to HashiCorp.** The Upbound fork of
  terraform-provider-aws is astonishingly small (a 76-line `xpprovider` package plus
  three accessors in `awsclient_xp.go`), which makes upstreaming look cheap — but the
  scraper reads the *docs website* directory, not the Go code, so there is nothing to
  upstream. Fixing the XPath locally (I6) is the whole job.
* **Everything under "defects".** Noted and dropped, since that ground is covered:
  `getTimeoutParameters` rebuilds a map per diff; `assertNoForceNew` reports only the
  first offending argument (its own TODO says so); `KnownReferencers` mutates
  `r.TerraformResource.Schema` entries in place (`RegionRequired`, `TagsAllRemoval` set
  `s.Required`/`s.Computed` on pointers shared with the singleton `sdkProvider`, which
  is the same family of hazard as fix 01).
