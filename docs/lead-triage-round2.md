<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# Lead triage, round 2

Verdicts on R1–R20 from the second lead round (`scratchpad/leads-round2.md`,
outside the repo). The first round's thirty leads are closed in
[`lead-triage.md`](lead-triage.md); the selected fixes are in
[`fixes/README.md`](fixes/README.md). Nothing here restates either.

Every claim below is labelled:

* **measured** — reproduced by running code in this session.
* **read** — established by reading source, not executed.
* **latent** — the defective path is confirmed by reading, but nothing
  demonstrates a user reaching it. Treat the severity as a ceiling.

Line numbers are from `provider-upjet-aws` at `263f715f22`, `upjet` and
`crossplane-runtime` as checked out, and
`terraform-provider-aws@v0.0.0-20260807134725-70894c6370d2` in the module
cache. No AWS account, no cluster, no build of this repository was run. Two
reproductions were run in throwaway modules that link only the standard
library plus `fatih/camelcase` and `iancoleman/strcase`.

## Verdicts

| # | lead | verdict | category | severity |
| - | --- | --- | --- | --- |
| R1 | `Tagger.Initialize` sends an unconditional spec `Update` every reconcile | **real** | waste / cost | high (cost) |
| R9 | wafv2 `GetExternalNameFn` reads the converted shape; `Create` calls it pre-conversion | **real** | data loss | high |
| R8 | `aws_appstream_user_stack_association` composes an ID with a trailing `/` | **real** | correctness | high (one resource) |
| R7 | `aws_lightsail_domain_entry`: `.parameeters` typo, plus a separator claim | **partially right** | correctness | high (one resource) |
| R3 | The `s`-trim mangles connection-secret keys for list/map sensitive attributes | **real** | correctness (user-visible) | medium |
| R2 | Fix 11's cost table misses MR-level sensitive-parameter Secret reads | **real** | waste / cost | medium |
| R10 | SNS/SQS custom diff makes a policy `Version`-only change un-applyable | **partially right** | correctness | medium-low |
| R12 | `conditionalFilter` does whole-path camel→snake | **real** (latent) | correctness | medium-low |
| R16 | `filterRequiresReplace`: literal `%v` in an error, and a lying Debug line | **real** | observability | low-medium |
| R6 | Neither SDK nor FW `Update` returns connection details | **real** | correctness | low |
| R17 | FW `Update`'s replacement refusal doubles its own message prefix | **real** | observability | low |
| R14 | Inject-key matching mangles paths via `ReplaceAll(e, "0", "*")` | **real** (latent) | correctness | low |
| R4 | Numeric map keys in sensitive attributes re-parse as array indices | **real** (latent) | corruption | low |
| R11 | `registry_cluster.go` drops `r.Conversions[0]` on an unasserted assumption | **real** | maintainability | low |
| R5 | `storeSensitiveData`'s `default` branch wraps a nil error | **partially right** | correctness | low (latent) |
| R20 | `SetResolver`'s Dynamic endpoint hard-codes `service == "IAM"` | **partially right** | correctness | low |
| R18 | `proposedState` is an unmaintained fork of Terraform 1.5.5 `objchange` | **unfalsifiable as stated** | — | — |
| R13 | `provider-meta` is never persisted and no state upgrader ever runs | **wrong** (consequence) | — | — |
| R15 | `ToEmbeddedObject` + inject key panics on an explicit-null list | **wrong** (unreachable) | — | — |
| R19 | `tfStateVlueIsEmpty` counts empty scalars as existence | **wrong** (consequence inverted) | — | — |

Counts: **12 real, 4 partially right, 3 wrong, 1 unfalsifiable, 0 duplicates.**
Zero duplicates is itself a result: the generator's own dedup pass against
L1–L30 and the fourteen fixes held on re-check.

---

## 1. R1 — the unconditional tag write: blast radius and the fix's one trap

**Real. Category: waste / cost. Severity: high (cost). Fix belongs in upjet.**

Already confirmed and recorded in
[`fixes/README.md`](fixes/README.md#correction-fix-14-does-not-take-steady-state-writes-to-zero);
not re-verified here. What follows is the blast radius and the fix design,
which that note did not cover.

**Blast radius.** `AddExternalTagsField()` attaches `config.TagInitializer` to
every resource whose Terraform schema has a `tags` attribute of type
`schema.TypeMap` (`config/overrides.go:158-165`), and it is applied as a
default resource option, so no per-service configurator can opt out.
Crossplane-runtime runs the initializer chain on every reconcile
(`crossplane-runtime/pkg/reconciler/managed/reconciler.go:1115`). The only
skip inside `Tagger.Initialize` is the pure-`Observe` management policy
(`upjet/pkg/config/resource.go:352-356`) — a paused MR, or one with a
deletion timestamp, still pays the write. **Read.**

**The fix is comparison-before-write, and there is exactly one trap.**
`setExternalTagsWithPaved` calls
`paved.SetValue("spec.forProvider.tags", tags)` with a map holding *only* the
three Crossplane keys (`resource.go:374-389`) — it **replaces** the tags map in
the paved JSON. User tags survive solely because the subsequent
`json.Unmarshal(pavedByte, mg)` (`:366`) merges JSON objects into the existing
Go map rather than replacing it. So the comparison must be taken between a
`DeepCopy` of `mg` captured **before** `fieldpath.PaveObject` and `mg` **after**
the unmarshal — comparing the tags map against `GetExternalTags(mg)` would
compare the wrong thing and would not see the merge. **Read.**

Three things that could have been traps and are not, checked so the
implementer does not have to:

* **Suppressing this write cannot strand another initializer's change.** Every
  initializer in the chain persists its own mutation:
  `NameAsExternalName.Initialize` has precisely the guard the tagger lacks and
  does its own `Update`
  (`crossplane-runtime/pkg/reconciler/managed/api.go:69-77`), and the only
  other initializers this provider registers — `common.PasswordGenerator` for
  RDS, DocDB and ElastiCache (`config/cluster/common/common.go:81-128`) —
  write a Secret and never touch `mg`'s spec. **Read.**
* **The paved round trip is otherwise identity.** `PaveObject` →
  `MarshalJSON` → `Unmarshal` back into the same object changes nothing but
  the tags map, so whole-object equality is a sound test for "the tagger did
  nothing". **Read.**
* **This one is strictly safer than fix 14 to get wrong.** Fix 14 suppressed a
  status write, where a mistake means a stale user-visible status. Here the
  in-memory `mg` still carries the tags and still drives the Terraform config,
  so a wrongly-suppressed write means the *persisted* spec lacks the three
  tags while the external resource still gets them.

One benefit the analysis has not claimed: `t.kube.Update` is a full-object
update, not a patch, so today every reconcile of every taggable MR re-submits
the whole spec and can clobber a concurrent writer. A comparison guard removes
that lost-update window as a side effect.

## 2. R9 — the Framework external-name function sees two different shapes

**Real. Category: data loss. Severity: high. Fix belongs in this repo
(`config/externalname.go`), with an upjet-side ordering question behind it.**

`customRuleGroupIdentifier` and `managedRuleGroupIdentifier` assert
`tfstate["rule_group_reference"].(map[string]interface{})` and
`tfstate["managed_rule_group"].(map[string]interface{})`
(`config/externalname.go:3779-3806`). Both attributes are declared
`schema.ListNestedBlock` in the vendored provider
(`internal/service/wafv2/web_acl_rule_group_association.go:794,819`), and
`tfValueToGoValue` renders every `tftypes.List` as `[]any`
(`upjet/pkg/controller/external_tfpluginfw.go:1046-1059`). They are maps only
*after* upjet's singleton-list conversion, which this repo registers for both
paths (`config/cluster/wafv2/config.go:41,81`) and wires as a Terraform
conversion for every resource with list-conversion paths
(`config/registry_cluster.go:154-156`,
`upjet/pkg/config/tf_conversion.go:69-79`). **Read.**

The Framework external calls `setExternalName` at three points with two
different shapes:

| call site | state shape | outcome for this resource |
| --- | --- | --- |
| `Observe`, `external_tfpluginfw.go:750` | converted (`:706` runs first) | map — works |
| `Create`, `external_tfpluginfw.go:829` | **raw** (`:840` converts after) | `[]any` — both helpers return `""` |
| `recoverExternalName`, `:520` | **raw** (`tfValueToGoValue` at `:514`) | `[]any` — same |

In `Create` both helpers therefore return `""`, `GetExternalNameFn` returns
`errors.New("either rule_group_reference or managed_rule_group must be present
in state file")` (`config/externalname.go:3832`), `setExternalName` wraps it
(`external_tfpluginfw.go:1001-1004`), and `Create` returns an error
*immediately after the association was successfully created in AWS*. The
reconciler then stamps `external-create-failed` and persists it without an
external-name (`crossplane-runtime/pkg/reconciler/managed/reconciler.go:1429-1440`).
**Read.**

This is exactly the window [fix 05](fixes/05-create-external-name.md) exists
for, but it is not the same defect: fix 05 makes the window survivable, this
makes it fire on **every** create of `WebACLRuleGroupAssociation`. Fix in this
repo: have both helpers accept either shape (`[]any` of length 1, or a map).
The framework-level smell — that `Create` and `Observe` disagree about whether
`GetExternalNameFn` sees converted state — is worth a separate upjet issue; it
will bite any external-name function that reads a nested field.

## 3. R8 — a trailing slash in the AppStream user-stack-association ID

**Real. Category: correctness. Severity: high for that one resource. Fix
belongs in this repo, one character.**

`config/externalname.go:488` composes
`"{{ .parameters.user_name }}/{{ .parameters.authentication_type }}/{{ .parameters.stack_name }}/"`.
Rendered with plausible inputs the result is `"u/USERPOOL/s/"` — **measured**,
by executing the template through the same `text/template` construction upjet
uses (`upjet/pkg/config/externalname.go:107-152`).

Upstream joins the same three parts with `/` and no trailing separator
(`internal/service/appstream/user_stack_association.go:165-172`), and parses
with `strings.SplitN(id, "/", 3)`, taking part three verbatim
(`:174-181`). So the parsed stack name is `"s/"`. Both `Read` and `Delete`
parse `d.Id()` this way (`:104`, `:132`). **Read.**

Upjet injects this ID into the reconstructed state unconditionally —
`tfState["id"] = params["id"]` where `params["id"]` came from `GetIDFn`
(`upjet/pkg/controller/external_tfpluginsdk.go:158-162,289`) — and the
reconstruction runs whenever the operation tracker has no state (`:270`),
i.e. on the first reconcile after every pod restart or tracker eviction. So
the ID from the annotation is not what gets read; the mis-composed one is.
`Read` finds nothing, calls `d.SetId("")`, `resourceExists` goes false
(`:517`) and the reconciler re-creates an association that already exists.
**Read.**

Two neighbours in the same file get it right —
`aws_appstream_fleet_stack_association` and `aws_appstream_user` both match
upstream's separators exactly (`fleet_stack_association.go:137-150`,
`user.go:225-229`) — so this is a typo, not a convention.

## 4. R7 — the `.parameeters` typo is real; the separator claim is not

**Partially right. Category: correctness. Severity: high for that one
resource. Fix belongs in this repo.**

**True half — the typo, and it is worse than "the ID is garbage".**
`config/externalname.go:1894` writes `{{ .parameeters.target }}`. Executing the
template renders `"www_example.com_A_<no value>"` with **no error**
— **measured**. Go's `text/template` default is `missingkey=invalid`, so the
missing map key yields an invalid value, the following field access returns the
zero value without erroring, and `printValue` prints `<no value>`. Nothing in
`GetIDFn` can detect this (`upjet/pkg/config/externalname.go:143-150`).

The second consequence is visible in checked-in generated code.
`TemplatedStringAsIdentifier` derives `IdentifierFields` by matching
`{{\s*\.parameters\.([^\s}]+)\s*}}` against each action node
(`externalname.go:52,111-120`); the typo means `target` is not matched —
**measured**: `identifierFields=[domain_name type]`. A field that is not an
identifier is demoted from required to optional, gains a CEL rule and is moved
into `initProvider` (`upjet/pkg/types/builder.go:400-421`,
`upjet/pkg/types/field.go:121-127,464`). And that is exactly what the
generated type shows: `Target` sits in `DomainEntryInitParameters` and carries
an XValidation rule, while `Type` — matched correctly — is
`+kubebuilder:validation:Required`
(`apis/cluster/lightsail/v1beta1/zz_domainentry_types.go:22,73,77,115`).
**Read.**

**False half — the `_` separator.** The lead says the template joins with `_`
while the vendored provider parses with `,` and rejects anything else. It does
not. `expandDomainEntry` and `FindDomainEntryById` both branch on
`flex.ResourceIdPartCount(id)` and, when the ID contains no comma, fall back to
the legacy underscore format explicitly, including a five-part case for names
that begin with `_` such as `_dmarc`
(`internal/service/lightsail/domain_entry.go:220-266,322-353`). The
underscore ID is a supported format, not a breakage.

**Half-true bonus.** The lead's aside about `_` in DNS names is right about the
*reverse* direction, though not for the reason given: upstream handles a
leading-underscore name, upjet does not.
`GetExternalNameFromTemplated("{{ .external_name }}_...", "_dmarc_example.com_TXT_v=DMARC1")`
returns `""` — **measured** — because the left separator is empty and the
function takes `strings.Split(val, "_")[0]`
(`upjet/pkg/config/externalname.go:183-186`). A `_dmarc` TXT record would lose
its external-name after create. That is a second, smaller defect in the same
resource.

## 5. R3 (with R4) — connection-secret keys for list and map sensitive attributes

**Real. Category: correctness, user-visible. Severity: medium. Fix belongs in
upjet, and it is API-breaking.**

`setSensitiveAttributesToValuesMap` trims a trailing `s` from the composed key
before appending the index or map key —
`k = strings.TrimSuffix(k, pluralSuffix)`, `pluralSuffix = "s"`
(`upjet/pkg/resource/sensitive.go:43,473-480`) — and it does so
unconditionally, for names that are not plurals as much as for names that are.
The branch runs only for `map[string]any` and `[]any` values; the `string`
branch does not trim (`:134-150`). **Read.**

This is not latent in this provider. Several sensitive attributes in the
connection-details mappings are `schema.TypeMap`:

| attribute | resource | secret key produced |
| --- | --- | --- |
| `connection_properties` | glue connection (`glue/connection.go:198`) | `attribute.connection_propertie.<k>` |
| `athena_properties` | glue connection (`:52`) | `attribute.athena_propertie.<k>` |
| `environment_variables` | amplify app/branch (`amplify/app.go:261`) | `attribute.environment_variable.<k>` |
| `token_signing_public_keys` | iot authorizer (`iot/authorizer.go:92`) | `attribute.token_signing_public_key.<k>` |
| `airflow_configuration_options` | mwaa environment (`mwaa/environment.go:59`) | `attribute.airflow_configuration_option.<k>` |

**Read** (attribute names taken from the generated
`GetConnectionDetailsMapping()` methods across `apis/cluster/*`; types from
the vendored schemas). Every consumer of those connection secrets reads a key
that does not name the attribute it came from.

The round-trip does not invert, as the lead says: `attribute.<name>.0` decodes
back to `<name>[0]`, a different attribute from `<name>s`
(`secretKeyToFieldPath`, `:428-437`). That half is **upjet-only and latent
here** — `GetSensitiveObservation` has exactly one caller,
`upjet/pkg/terraform/files.go:139`, on the Terraform-CLI workspace path, which
this provider never runs. **Read.**

**R4 folds into the same finding.** The encoding is ambiguous in a second way:
`fieldPathToSecretKey` renders both index and field segments as `.N`
(`:439-458`), and `secretKeyToFieldPath` turns any trailing or interior
`.<digits>` back into `[<digits>]` via `reEndsWithIndex`/`reMiddleIndex`
(`:48-49,428-437`). A digit-only map key — `airflow_configuration_options` and
`environment_variables` both take arbitrary user keys — decodes as an array
index, and `Paved.SetString` on `x[2022]` grows a 2023-element slice where a
map belongs. Same reachability: CLI path only, so **latent** here. **Read.**
A third case the lead missed and the same fix should cover: the map-key branch
does *not* apply the `...key...` escaping that `fieldPathToSecretKey` uses for
dotted field names (`:448-451`), so a dotted map key —
`airflow_configuration_options` keys are dotted by convention, e.g.
`core.dag_concurrency` — is equally ambiguous on the way back.

**The fix is not free.** `sensitive_test.go:143-192` asserts the trimmed keys
(`attribute.top_level_secret.0`), so upstream has locked the behaviour in, and
changing it renames keys in live connection secrets. This wants an upjet issue
proposing an unambiguous encoding plus a migration, not a quiet patch.

## 6. R2 — fix 11's flip costs more Secret reads than it was priced at

**Real. Category: waste / cost. Severity: medium. Correction to
[fix 11](fixes/11-scope-secret-informer.md)'s audit-cost note.**

Fix 11's review priced the added reads as "one `client.Get` per reconcile for
`source: Secret` only — IRSA, Pod Identity and Upbound read no Secret at all"
(`fixes/README.md`). Two other per-reconcile Secret reads go through the same
manager client and were not counted:

* **MR sensitive parameters.** `getExtendedParameters` calls
  `resource.GetSensitiveParameters` with `&APISecretClient{kube: kube}` on
  every Connect (`upjet/pkg/controller/external_tfpluginsdk.go:144`; FW twin
  `external_tfpluginfw.go:140`), and the state-reconstruction branch calls it a
  second time on any reconcile that rebuilds state
  (`external_tfpluginsdk.go:285`). `GetSensitiveParameters` resolves **one
  `client.Get` per populated `SecretRef`**, not one per MR — the list branch
  loops (`upjet/pkg/resource/sensitive.go:266-289`) — and does not deduplicate
  refs pointing at the same Secret. `APISecretClient.GetSecretData` is a plain
  `kube.Get` (`upjet/pkg/controller/api.go:57-63`). **Read.**
* **Password generators.** `common.PasswordGenerator` does an unconditional
  `client.Get` on the referenced Secret on every reconcile, on all credential
  sources, for RDS, DocDB and ElastiCache
  (`config/cluster/common/common.go:95`). **Read.**

Neither depends on the credential source, so the "IRSA reads no Secret at all"
line is wrong for any MR with a `passwordSecretRef`. The order of magnitude is
unchanged — these are still single-digit GET/s at fleet scale — but the flip is
not free for IRSA users, which is how it was sold.

**The follow-up in the fixes README is aimed at the wrong layer.** It proposes
"a bounded TTL cache in `internal/clients`, keyed on the ProviderConfig
secretRef". That cannot cover either read above: both go through upjet's
`APISecretClient`, constructed from the `client.Client` handed to the
connectors in generated controllers (`mgr.GetClient()` in every
`zz_controller.go`). Covering them means either a caching `client.Client`
wrapper injected at controller setup — 178 binaries — or re-enabling the
Secret cache. Say so in the fix rather than implying one cache closes both.

## 7. R10 — the policy `Version` suppression is real, and deliberate

**Partially right. Category: correctness. Severity: medium-low. No clean fix.**

**True.** `config/cluster/sns/config.go:32-54` and
`config/cluster/sqs/config.go:36-58` both strip `Version` from the old and the
new policy document via `common.RemovePolicyVersion`
(`config/cluster/common/common.go:133-145`) before calling
`awspolicy.PoliciesAreEquivalent`, and delete the `policy` attribute from the
diff when the remainder is equivalent. The stripping is load-bearing:
`awspolicyequivalence@v1.7.0` compares `Version` itself
(`aws_policy_equivalence.go:131`). So a change that touches only `Version` is
suppressed, `Synced` reads true, and the change is never sent. **Read.**

**Overstated.** The lead frames this as an oversight — "the collateral is the
user-initiated Version change". It is a knowingly taken trade: the commit that
introduced it is titled `fix(sqs): update loop` (`14626977fb`, cherry-picked as
`2c5f4810c5`), i.e. it exists to stop a permanent update loop caused by AWS
returning a `Version` the user did not set. **Read.**

And there is no obvious better fix. The custom diff receives `state` and
`config`, but both are already what `diff.Attributes["policy"].Old` and `.New`
carry, so a diff hook has no third data point with which to distinguish "AWS
normalised it" from "the user changed it". Anything better needs the previous
*spec* generation, which lives outside the hook. Record it as a known
limitation; do not open it as a fix.

## 8. R12 — `conditionalFilter`'s whole-path camel→snake

**Real, latent. Category: correctness. Severity: medium-low. Fix belongs in
upjet, folded into [fix 07](fixes/07-fieldpath-camel-snake.md)'s branch.**

The parent's open question was whether callers pass dotted paths. Answer:
**not today, but the configuration surface invites them.**

`conditionalFilter` applies `name.NewFromCamel(cName).Snake` to a whole
canonical name and hands the result to `fieldpath.GetValue`
(`upjet/pkg/resource/lateinit.go:195-208`). Canonical names are dotted for
nested fields — `getCanonicalName` joins with `.` (`:435-441`), and
`AddConditionalIgnoredCanonicalFields` is fed
`traverser.FieldPath(f.CanonicalPaths)` (`upjet/pkg/types/field.go:171-174`).
The conversion mangles those:

```
NewFromCamel("ScalingConfig")           -> "scaling_config"          (correct)
NewFromCamel("User")                    -> "user"                    (correct)
NewFromCamel("ScalingConfig.MaxSize")   -> "scaling_config_._max_size"
NewFromCamel("BlockDeviceMappings.Ebs") -> "block_device_mappings_._ebs"
```

**Measured**, by running upjet's `pkg/types/name` in a throwaway module. The
lead's predicted output is exact.

Reachability: `grep WithConditionalFilter apis/` yields six generated files and
two distinct names, `"ScalingConfig"` (EKS NodeGroup) and `"User"` (MQ Broker)
— both top-level and digit-free, so the filter works today. **Measured.** But
`LateInitializer.ConditionalIgnoredFields` takes Terraform paths "concatenated
with dots", and the doc comment for its sibling gives
`"block_device_mappings.ebs"` as the worked example
(`upjet/pkg/config/resource.go:258-268`). The first nested entry anyone adds
makes the filter silently never match, and late-init then overwrites a field
the user pinned through `initProvider`.

Note the sibling is safe: `nameFilter` compares canonical names as strings with
no conversion at all (`lateinit.go:97-101`). Only the conditional variant
converts, and only because it must look up `initProvider`, whose keys really
are snake_case (`json.TFParser` uses the `tf` tag —
`upjet/pkg/resource/json/json.go:10`). So the *intent* is right and only the
transform is wrong; fix 07's `convertFieldPathToSnake` drops straight in.

## 9. R16 — two lying breadcrumbs on the Framework replace path

**Real. Category: observability. Severity: low-medium. Fix belongs in upjet,
two lines.**

`upjet/pkg/controller/external_tfpluginfw.go:475`:

```go
return errors.New("cannot get the type at path from resource schema: %v")
```

`errors.New`, not `Errorf`, and the real `err` from
`TypeAtTerraformPath` is discarded — the user gets a literal `%v`. **Read.**

Twenty lines down, `:496`, the Debug line
`"TF plan reported a diff at path that require resource replacement, but the
prior and plan values are equal. Skipping..."` sits **outside** the
`if !plannedVal.Equal(priorVal)` block, so it fires once per RequiresReplace
path including the ones just appended to `filteredRequiresReplace`. **Read.**

Same family as closed L28 (SDK diagnostics through `%v`) but a different file
and different defects; L28's fix does not touch either line. This is the
Framework analogue of the surface L27 says is where ForceNew troubleshooting
happens, and both of its breadcrumbs are wrong.

## 10. R6 — `Update` returns no connection details

**Real. Category: correctness. Severity: low. Fix belongs in upjet.**

Both `Update` implementations end `return managed.ExternalUpdate{}, nil` with
the state map in hand and no `GetConnectionDetails` call —
`external_tfpluginsdk.go:807` and `external_tfpluginfw.go:950`. `Create` and
`Observe` both compute details from the same map
(`external_tfpluginfw.go:835`, `:700`). The reconciler publishes
`update.ConnectionDetails` (`crossplane-runtime/pkg/reconciler/managed/reconciler.go:1587`).
**Read.**

The secret is not wiped: `APISecretPublisher` assigns `s.Data = c` and applies,
and `corev1.Secret.Data` is `omitempty`, so an empty map produces a patch that
touches no keys (`crossplane-runtime/pkg/reconciler/managed/api.go:100-121`).
So the failure mode is genuinely the narrow one the lead claims — a rotated
credential reaches the secret one `Observe` late — and every operation here is
async with an immediate requeue, so the window is one reconcile. Severity low,
confirmed.

**The FW fix is not a one-liner.** Connection details must be computed from the
*pre-conversion* state map, as `Create` does at `:835` before converting at
`:840`; `Update` converts at `:929` before anything else, so the call has to be
inserted above that line, not appended at the end.

## 11. R17 — the doubled error prefix

**Real. Category: observability. Severity: low. Fix belongs in upjet, one
line.**

`planRequiresReplace` builds a string that already begins with
`"diff contains fields that require resource replacement: "`
(`external_tfpluginfw.go:863-874`), and `Update` wraps it with the same prefix
(`:882`). It also ends with a trailing `", "` from the builder loop. The
`Synced` condition message reads
`diff contains fields that require resource replacement: diff contains fields
that require resource replacement: eventTypeIds, `. **Read.**

## 12. R14 — inject-key matching corrupts paths containing the digit zero

**Real, latent. Category: correctness. Severity: low. Fix belongs in upjet.**

`strings.ReplaceAll(e, "0", "*")` at
`upjet/pkg/config/conversion/list_conversion.go:121` and `:149` replaces every
`'0'` **character** in the expanded path, not the zero index segment the
comment describes. `rule[10].filter` becomes `rule[1*].filter`; a field name
containing a literal `0` is corrupted; a non-zero index never matches a
`[*]`-keyed entry at all. **Read.**

Inert today: the only registered `ListInjectKeys` entry in this repository is
EKS `vpcConfig` (`config/registry_cluster.go:206-218`) — top level, no digits,
and singleton lists only ever have index 0, so the substitution is a no-op on
the one path that uses it. The failure mode when it does bite is a silent
non-match, i.e. the injected list-map key is not added, which is precisely the
server-side-apply data loss the mechanism was built to prevent. Cheap to fix
correctly (match on the fieldpath segments rather than by string substitution).

## 13. R4 — see R3

Folded into finding 5 above.

## 14. R11 — two places assume `Conversions[0]` is the identity converter

**Real. Category: maintainability. Severity: low. Fix belongs in this repo.**

`config/registry_cluster.go:227-235` comments "assumes the first element is the
identity conversion … and removes it", then does `r.Conversions =
r.Conversions[1:]` with nothing asserting the assumption. It holds today:
`config.DefaultResource` constructs `Conversions` with exactly one element, the
identity conversion (`upjet/pkg/config/common.go:96`), and every other
mutation in this repository appends (`config/overrides.go:194`,
`config/cluster/connect/config.go:82`, `kafka/config.go:43`,
`autoscaling/config.go:38`). **Read.**

The lead missed the more interesting half: **there is a second site making the
same assumption**, `config/cluster/elasticache/config.go:114-116`, which also
strips element zero and prepends its own identity converter excluding
`clusterMode`. The two do not currently compose, because
`configureSingletonListAPIConverters` only runs for resources listed in
`config/old-singleton-list-apis.txt`, and `aws_elasticache_replication_group`
is not in that 325-line list (**read**). If it were ever added, the second
`[1:]` would silently delete ElastiCache's `clusterMode`-excluding identity
converter. That is the concrete argument for the type assertion the lead asks
for, and it costs three lines.

## 15. R5 — `errors.Wrapf(nil, …)` in the `default` branch

**Partially right. Category: correctness. Severity: low, latent. Fix belongs in
upjet, one line.**

**True and certain.** `upjet/pkg/resource/sensitive.go:294-295` is
`default: return errors.Wrapf(err, errFmtCannotGetSecretKeySelector, expandedJSONPath)`.
The `err` in scope is the loop-local one declared by `v, err :=
pavedJSON.GetValue(expandedJSONPath)` at `:203`, freshly declared each
iteration and necessarily nil on this branch, because the `case` arms that
assign to it were not taken. `errors.Wrapf(nil, …)` returns nil, so the
function reports success — and because it is `return`, not `continue`, it also
abandons the remaining entries of `jsonPathSet`. **Read.**

**Unreachable as described.** The lead's scenario is "a plain string that
slipped into a secret-ref position". The values at these paths come from the
generated CRD types, where a sensitive parameter is a `*SecretKeySelector`, a
`[]SecretKeySelector` or a secret-reference object — always a JSON object or
array after `fieldpath.PaveObject`. The one path shape that could carry a bare
string, `status.atProvider.<field>` (handled specially at `:176-183`), is
absent from the observation struct precisely because it is sensitive — checked
on the only mapping of that shape in this provider,
`aws_iam_user_login_profile`, whose `UserLoginProfileObservation` has no
`Password` field (`apis/cluster/iam/v1beta1/zz_userloginprofile_terraformed.go:23-25`,
`zz_userloginprofile_types.go`). `ExpandWildcards` returns nothing for an
absent path, so the loop body never runs. **Read.**

Worth a one-line upstream fix (`errors.Errorf`) because it is unarguable, not
because anything reaches it.

## 16. R20 — the Dynamic endpoint's IAM special case

**Partially right. Category: correctness. Severity: low, sandbox only. Fix
belongs in this repo.**

**True.** `internal/clients/provider_config.go:182-187` builds the dynamic URL
as `<proto>://<service>.<region>.<host>` for every service except a hard-coded
`service == "IAM"`, which omits the region. Twenty lines below, the
`SigningRegion` half of the same closure handles the general case by testing
`region == "aws-global"` and mapping it per partition (`:199-208`). The two
halves of one function disagree about how region-less services are recognised,
which is the strongest evidence that the `IAM` literal is a stand-in. **Read.**

**The list of affected services is wrong.** `SetResolver` is applied last in
`GetAWSConfigWithoutTracking` (`:136`), so it attaches only to the
provider's *own* `aws.Config`, and [fix 06](fixes/06-dynamic-endpoint-ignored.md)
already establishes that config never reaches the Terraform CRUD client.
The direct SDK callers of the resolved config are `sts:GetCallerIdentity`
(`internal/clients/aws.go:119`, `internal/clients/cache.go:153`) and the EKS
`clusterauth` controller (`internal/controller/*/eks/clusterauth/eks.go:31`,
`controller.go:61`). Route 53 and CloudFront are reconciled through the
Terraform client and never see this resolver at all; global *API groups* are
given a concrete region such as `us-east-1` by `getGlobalRegion`, not
`aws-global` (`internal/clients/aws.go:229-263`). **Read.**

So the residue is one call: STS built with `stsRegionOrDefault`, which sets the
client region to `aws-global` when no region is configured (`:261-268`),
against a Dynamic endpoint. Real, narrow, and only in the LocalStack-style
scenario the feature exists for. Fix by testing the region rather than the
service name, matching the `SigningRegion` branch immediately below.

---

## The external-name template class — the ruling

The generator inferred a whole latent class from "one sweep of the 67
templated identifiers surfaced two breakages against upstream plus the typo".
The inference about *testing* is right; the inference about *prevalence* is
not.

**What is true.** Nothing executes these templates. `config/externalname_test.go`
is 50 lines and tests one hand-written `SetIdentifierArgumentFn` for
`ecsTaskDefinition`; no test renders a template or compares an ID against
upstream (**read**). Nothing in generation would catch it either:
`template.Parse` accepts `{{ .parameeters.target }}` happily, and execution
returns `<no value>` with a nil error (**measured**). A malformed template
reaches production silently.

**What is not.** The corrected tally is **two defects, not three**. The
lightsail entry is one defect — the typo — not two, because the underscore
separator is a format the vendored provider parses on purpose
(`domain_entry.go:220-266`). The other is R8's trailing slash. Everything else
I checked matches upstream: `aws_vpc_endpoint_connection_accepter` (`_`,
`ec2/vpc_endpoint_connection_accepter.go:139`), `aws_eks_access_policy_association`
(`#`, `eks/access_policy_association.go:194`), `aws_cloud9_environment_membership`
(`#`, `cloud9/environment_membership.go:172`), `aws_appstream_fleet_stack_association`
and `aws_appstream_user` (`/`). Across 90 distinct template strings and 100
templated registrations in `config/externalname.go`, that is two defects.
**Read.**

So: a real gap in test coverage with a ~2 % defect rate behind it, not a
systemic identity crisis. Do not open a sweep. Do open the cheap guard, which
this pass found by accident:

> A `{{ .parameters.X }}` action that does not resolve leaves `X` out of
> `IdentifierFields`, which moves the field out of `forProvider`-required and
> into `initProvider` with a CEL rule. That difference is **visible in the
> checked-in generated types** — compare `Target` and `Type` in
> `apis/cluster/lightsail/v1beta1/zz_domainentry_types.go`.

A generation-time assertion that every `{{ .parameters.* }}` action in a
template names a real attribute of that resource's Terraform schema would have
caught the lightsail typo at codegen, costs one traversal, and needs no live
account. A second assertion — that the rendered template contains no
`<no value>` when executed against a fully-populated dummy parameter map —
would catch the same class and is equally cheap. Neither catches R8; a trailing
separator needs a comparison against upstream's `…CreateResourceID`, which is
not mechanisable.

---

## Wrong

* **R13 — `provider-meta` and state upgraders.** The *reading* is right and
  worth recording: `SetCriticalAnnotations` is called only from the CLI path
  (`upjet/pkg/controller/external.go:260,418`), the SDK path puts nothing but
  timeouts in `InstanceState.Meta`
  (`external_tfpluginsdk.go:305-315`), and `RefreshWithoutUpgrade` never
  consults `SchemaVersion` or `StateUpgraders`
  (`terraform-plugin-sdk/v2@v2.37.0/helper/schema/resource.go:1107-1160`). The
  *consequence* does not follow. Upjet does not carry Terraform state across
  provider versions at all: it rebuilds state from `status.atProvider` on every
  cold start, interpreted through the **current** schema
  (`external_tfpluginsdk.go:270-315`), and then `Read` repopulates every
  attribute from AWS. There is no old-layout state to migrate and no error
  path to trip. A provider bump is not a fleet-wide correctness event.
* **R15 — panic on an explicit-null list.** The nil type assertion at
  `upjet/pkg/config/conversion/list_conversion.go:151` is really there, but
  the input cannot occur. The conversion paves an object produced by
  `runtime.DefaultUnstructuredConverter.ToUnstructured`
  (`conversions.go:326`), and every singleton-list field is a Go slice with
  `omitempty` (`apis/cluster/eks/v1beta1/zz_cluster_types.go:136,230,320`), so
  a nil slice yields an **absent** key, not a null one, and `ExpandWildcards`
  returns nothing for it. The runtime Terraform conversion passes `opts = nil`
  (`upjet/pkg/config/tf_conversion.go:74-77`), so it cannot reach the branch
  either. Hardening it is still free, but there is no bug to file.
* **R19 — `tfStateVlueIsEmpty`.** All three code observations are accurate
  (`config/overrides.go:213-262`): it runs only when `region` is non-empty, it
  discards `GetAttribute`/`SetAttribute` diagnostics, and it treats an empty
  string or empty map at depth one as existence. The consequence is inverted.
  Upjet's default for a non-null Framework state is already
  `resourceExists = true`; the hook can only override it to *false*
  (`upjet/pkg/controller/external_tfpluginfw.go:624-648`). Every hole the lead
  names therefore degrades to the pre-existing default — the check fails to
  help, it never flips a correct verdict to a wrong one. The only dangerous
  direction, a false "empty" for a live resource, needs every depth-one
  attribute except `region` to be null. `tftypes.Value.Copy()` is a deep copy
  (`terraform-plugin-go/tftypes/value.go:234-251`), so the `SetAttribute` that
  nulls `region` does not leak into the caller's state either. Nothing to do
  beyond renaming the function.

## Unfalsifiable as stated

* **R18 — `proposedState` as a fork of Terraform 1.5.5 `objchange`.** The fork
  is real and declared (`upjet/pkg/controller/proposed_state.go:16`, 668
  lines, feeding every Framework plan). The "no drift test" claim is nearly
  right — there is a `proposed_state_test.go`, but it is 101 lines with a
  single `TestProposedNewAttributes`, which is not a drift test. The risk
  claim cannot be settled from this environment: it requires diffing against
  `terraform/internal/plans/objchange` at v1.5.5 and at HEAD, and that source
  is not in the module cache and needs network access. **The experiment that
  would settle it:** fetch both revisions of `objchange.go`, diff them against
  each other to enumerate post-1.5.5 fixes, then check each fix against the
  fork — a bounded afternoon that produces either a list of divergences or a
  clean bill. Until someone runs it, this is a maintenance observation, not a
  lead.

---

## What this changes elsewhere

Two corrections to prior documents, both in
[`fixes/README.md`](fixes/README.md):

1. **Fix 11's added-read accounting is incomplete** (finding 6). It counts
   ProviderConfig credential Secrets only. MR sensitive-parameter refs and the
   RDS/DocDB/ElastiCache password generators read Secrets through the same
   uncached client on every reconcile, on every credential source including
   IRSA. The proposed remedy — a TTL cache in `internal/clients` keyed on the
   ProviderConfig secretRef — cannot reach either of them.
2. **The second-round summary's "two breakages against upstream plus the
   typo"** should read "two defects": the lightsail underscore separator is
   not a breakage (the ruling above).

Nothing else in `lead-triage.md`, `reconcile-workflow.md`,
`reconcile-workflow-detail.md`, `memory-footprint.md` or
`architecture-wins.md` is contradicted by this pass. R1, R7 and R12 were
spot-checked into `fixes/README.md` before this triage ran; all three survive,
with R7 narrowed to its first half.


---

# Round 3 leads: from the cluster work

Found while reviewing the provider's own code with the cluster measurements in
hand. Verified against source unless marked otherwise; none is measured.

## Correctness

**C1. `skip_credentials_validation` silently substitutes a fake account ID.**
`internal/clients/aws.go:126,150` return the constant `000000000000` for the
account whenever the flag is set. That account is templated into external names
and ARNs — `iamPolicy()` and `genericARNTemplate` in `config/externalname.go`
among others. The flag's Terraform meaning is only "do not call STS to validate";
a user who sets it against **real AWS** (say, because their policy forbids
`sts:GetCallerIdentity`) gets Observe against ARNs containing the wrong account,
hence NotFound, hence re-create loops. `docs/ideas.md` records the constant as a
LocalStack convenience and never records this failure. Fix: separate "skip
validation" from "fake the account", or refuse account-templated resources under
the flag with a clear error.

**C2. `spec.skip_requesting_account_id` is parsed, documented, and never read.**
Declared in both scopes' `types.go`; no code references it —
`internal/clients/aws.go:368` hardcodes `SkipRequestingAccountId: true` for the
Terraform client, and the provider's own STS call is gated only by
`SkipCredsValidation`. Setting it is a silent no-op. It is also exactly the knob
C1 needs. Fix: honour it, or delete it with a deprecation note.

**C3. Namespaced `ProviderConfig` requires `secretRef.namespace`, then ignores
it.** The CRD marks the field **required**;
`internal/clients/pc_resolver.go:53` then overwrites it unconditionally with the
managed resource's namespace (same for both webIdentity token refs). A user
pointing at a secret in another namespace is silently redirected to their own,
where a same-named secret may exist and be used instead. The namespaced
`PasswordGenerator` already made the equivalent API change to a local selector;
the ProviderConfig API did not. Fix: use a local selector type, or error when the
declared namespace differs.

**C4. `Source: IRSA` without `AWS_WEB_IDENTITY_TOKEN_FILE` now fails every
reconcile, with a misleading message.** `internal/clients/creds_cache.go:188-192`
hashes the token file unconditionally once the source is IRSA, returning "token
file name cannot be empty" when the variable is unset — EKS Pod Identity, a
missing service-account annotation, a non-EKS cluster. Before the credentials
cache this fell through to the default chain. Wrapped as "cannot calculate the
hash for the credentials file", which names neither IRSA nor the variable.

**C5. `eks/clusterauth` dereferences fields that are nil while a cluster is
CREATING.** `internal/controller/{cluster,namespaced}/eks/clusterauth/controller.go:144,185`
and `eks.go:59,66,89` dereference `CertificateAuthority` / `Endpoint`, which are
nil until the cluster is ACTIVE — the common composition case of creating Cluster
and ClusterAuth together. The `== ""` guards show the empty case was anticipated
and the nil case was not. Fix: nil checks returning a retryable error.

**C6. `clusterauth`'s documented `refreshPeriod` maximum is not enforced.** The
type comment says "The maximum is 10m0s"; nothing validates it. The token is
capped at 15 minutes while the freshness window is the uncapped `RefreshPeriod`,
so `refreshPeriod: 30m` is accepted and yields a connection secret whose token is
dead for half of every period. Fix: a CEL validation, or cap the deadline.

**C7. upjet's `MetricRecorder` is a `manager.Runnable` that nothing runs.** The
generated controllers create one per kind, but only the *state-metrics* recorder
is passed to `mgr.Add` (`config/templates/controller.go.tmpl:145`). The
`MetricRecorder`'s `Start` exists solely to register the Delete handler that
drops per-MR entries, so on a cluster with managed-resource churn the observation
map grows without bound. Fix: add it, or drop the dead `Start` upstream.

**C8. An inverted guard returns success on its failure path.**
`internal/clients/aws.go:111-113`: the `awsCfg == nil` branch wraps a
necessarily-nil `err`, so `errors.Wrap` yields nil and the guard returns an empty
`Setup` with no error. Unreachable today. One-line fix: `errors.New`.

## Waste

**W1. The `aws.Config` is rebuilt on every reconcile.**
`getAWSConfigWithDefaultRegion` runs at `internal/clients/aws.go:108`, *before*
the AWS client cache, so every reconcile re-parses the credentials secret with
go-ini and re-runs `config.LoadDefaultConfig` — which re-reads `~/.aws` files.
Measured at 154.7 MB per nine minutes of allocation that the client cache cannot
reach. Both pieces are pure functions of the ProviderConfig and could be keyed
beside the credentials cache.

**W2. `getRegion` converts the whole managed resource to unstructured to read one
string.** `internal/clients/provider_config.go:76-88` runs
`DefaultUnstructuredConverter.ToUnstructured` — a full reflection walk of the
spec — to fetch `spec.forProvider.region`. A generated getter would remove it.

**W3. The legacy ProviderConfig round trip, twice per reconcile.**
`legacyToModernProviderConfigSpec` (JSON marshal + unmarshal,
`pc_resolver.go:24-46`) plus the reconciliationpolicy wrapper re-resolving the
same config: together ~2% of CPU samples at 500 managed resources. A hand-written
converter and a (UID, generation) memo would erase both. This quantifies L9/L10,
which were established by reading.

**W4. The API-call counter resolves its Prometheus labels per call.**
`withExternalAPICallCounter` (`internal/clients/aws.go:313-346`) calls
`WithLabelValues(serviceID, operationName)` on every AWS API call — 0.96% of CPU
and of allocation, the second-largest provider-owned frame in both profiles. A
small map of pre-resolved counters removes it.

**W5. Each scope builds its own credentials cache.** `SelectTerraformSetup` is
called once per scope and news up an `AWSCredentialsProviderCache` each time, so
cluster- and namespaced-scoped resources sharing an identity pay separate STS
calls and hold duplicate entries.

## Dead

`config/externalnamenottested.go` (731 lines, `ExternalNameNotTestedConfigs`) is
referenced nowhere, and 18 of its entries duplicate live map entries — promoted
to tested and never removed here, so the copies can diverge unnoticed.
`skipList` in `config/registry_common.go` lists `aws_alb$` and
`aws_alb_target_group_attachment$` twice.
