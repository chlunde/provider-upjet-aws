# Two malformed external-name templates, and a guard for the class

## The defects

**`aws_appstream_user_stack_association`** (`config/externalname.go:488`) appends
a literal `/` after the last action. Upstream composes the ID as
`strings.Join([]string{userName, authType, stackName}, "/")` with nothing
appended, and parses it back with `strings.SplitN(id, "/", 3)` taking `parts[2]`
verbatim (`internal/service/appstream/user_stack_association.go`, used by both
Read `:108` and Delete `:136`). So the trailing separator is absorbed into the
stack name and both operations look up `my-stack/`, which matches nothing.

**`aws_lightsail_domain_entry`** (`:1894`) says `.parameeters.target`.
`text/template` renders an unknown root as the literal `<no value>` and returns a
**nil error**, so the ID has always been `name_domain_A_<no value>`. The
underscore separator in this one is correct — upstream falls back to splitting on
`_` when the ID has a single flex part (`domain_entry.go:220-266,322-353`) — and
was left alone.

## Why AppStream does not stay fixed after Create

The bug survives a successful create. Upjet rebuilds the Terraform state from
`status.atProvider` whenever the operation tracker is cold and overwrites the ID
with a **fresh render of the template**
(`external_tfpluginsdk.go:157-162`, `:289`), so after any pod restart the
mis-composed ID — not the one the AWS provider recorded at create time — is what
Read is handed.

## Migration: nothing breaks, and the upgrade repairs things

For this resource the external-name annotation is effectively write-only:

* `TemplatedStringAsIdentifier("")` with an empty name field path makes
  `SetIdentifierArgumentFn` a no-op (`upjet/pkg/config/externalname.go:126-129`),
  so the annotation is never injected into the Terraform parameters;
* the template contains no `{{ .external_name }}`, so `GetIDFn` never reads it
  either, and `GetIDFn` re-executes the template on every call (`:143-152`) with
  no memoisation;
* `setExternalName` only writes the annotation, and only when `resourceExists`.

An existing MR therefore already holds the **correct**, slash-free annotation —
it was written from the ID the AWS provider composed inside `Create`. For it to
hold a trailing-slash ID, a `Refresh` would have had to succeed with the bad ID,
which cannot happen. And even then it is self-healing: `setExternalName` rewrites
it on the first successful Observe.

After the fix the first reconcile renders `user/USERPOOL/stack`, Read finds the
association, and the MR goes Ready — for many MRs, the first time since the pod
that created them restarted.

**Worth a release note, not a blocker:** the visible effect of upgrading is MRs
changing state. Before the fix, a cold start either re-created an association
that already existed or errored out of Observe, and Delete passed the wrong stack
name to `BatchDisassociateUserStack`, so an MR deleted under an affected build
either stuck on its finalizer or left the association behind in AWS. Which of the
two happens was not verified — that needs an account. **Anyone who deleted these
MRs recently should check AWS for orphaned associations.**

## The lightsail migration edge

`IdentifierFields` is consumed only by `upjet/pkg/types/field.go:121` — it is a
pure code-generation input with **no runtime effect**, so the branch is coherent
without regenerating. But the next `make generate` will make
`spec.forProvider.target` **required** on `DomainEntry` and remove
`spec.initProvider.target`, breaking any manifest that supplies `target` through
`initProvider`. That is a deliberate API decision for the maintainers and was
left to them. The generated shape today, for the record:

```go
type DomainEntryInitParameters struct {
	Target *string `json:"target,omitempty" tf:"target,omitempty"`   // demoted
}
type DomainEntryParameters struct {
	// +kubebuilder:validation:Optional
	Target *string `json:"target,omitempty" tf:"target,omitempty"`   // should be Required
	// +kubebuilder:validation:Required
	Type   *string `json:"type" tf:"type,omitempty"`                 // matched, so required
}
```

Regeneration was not run: `cmd/generator` imports `xpprovider`
(2918 packages, 352 from terraform-provider-aws) and `generate.init` needs a
`terraform init` download plus a `pull-docs` clone. A unit assertion pins
`IdentifierFields == [domain_name target type]` instead, so the regeneration
lands the right shape whenever it happens.

## The guard — the part that matters longer term

A mistyped action is invisible: it parses, it executes, it returns nil. Two
assertions in package `config` (`config/externalnametemplate_test.go`) close
that, and they run in the ordinary `go test ./config/` flow.

**`TestExternalNameTemplateActionsAreWellFormed`** AST-walks the external-name
source, parses every string literal containing `{{`, and walks the `parse` tree
collecting every `*parse.FieldNode` — including ones nested in pipelines, so
`{{ (index .parameters.lex_bot 0).name }}` is covered. Each access must start
from `external_name`, `parameters` or `setup`; `.setup.` must name a real key of
`terraform.Setup.Map()`. This catches a misspelt root, with file:line.

**`TestExternalNameTemplateParametersExistInSchema`** builds a parameter map from
the resource's **real Terraform schema** and fails if the render contains
`<no value>`. This was cheap because package `config` already embeds the full
provider schema (`config/schema.json`, 19 MB) behind `getProviderSchema`
(`config/registry_common.go:45`): 1683 resource schemas in ~0.7s, and **no new
dependency** — in particular not terraform-provider-aws. It only renders entries
the AST identifies as template-built, since hand-written `GetIDFn`s legitimately
reject synthetic input (`PermissionSetIdAsExternalName` panics on it).

Independently confirmed: changing `authentication_type` to `authentication_typ` —
well-formed, plausible, nonexistent — fails only the schema-backed test, with the
resource, the template and the offending action named.

Neither guard can catch R8, since a trailing separator resolves fine, so both
known-good IDs are also pinned by name, and the AppStream one additionally
asserts the ID survives upstream's `SplitN(id, "/", 3)` round-trip.

## A third defect found in passing, out of scope

`ExternalNameNotTestedConfigs` contains `{{ .setup.configuration.account_id }}`,
a key the provider does not populate (`internal/clients/aws.go:143-155` sets
`client_metadata.account_id`, `client_metadata.partition`,
`configuration.region`). That map is referenced by no registry, so it is not
shipped — but it is a latent defect and the guard deliberately excludes the map,
with the reason in a comment.

**Branch** `fix-external-name-template-defects` @ `e867a27`, `7189335`,
`453072d`.
