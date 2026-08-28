# Report the changed Terraform attributes in change-log entries

The one item from [ideas.md](../ideas.md) §I8 that was small enough to just do.
Scoped deliberately to the `AdditionalDetails` mechanism only — **not** the status
field and **not** the Kubernetes Event, both of which need an API version, a size
bound and a story about not re-introducing no-op status writes.

## The gap

Every `Observe` computes a full Terraform diff — `InstanceDiff.Attributes` is
`map[string]*ResourceAttrDiff` with `Old`, `New`, `RequiresNew`, `Sensitive` — and
reduces it to one boolean (`external_tfpluginsdk.go:559`). The only escape is a
Debug-level Go struct dump at `:480`, off in every real deployment.

Meanwhile `managed.ExternalUpdate` has carried `AdditionalDetails map[string]string`
all along, forwarded verbatim to the change-log service
(`crossplane-runtime/pkg/reconciler/managed/changelogger.go:111`), and providers
ship it enabled behind `--enable-changelogs`. **`grep -rn AdditionalDetails` across
all of `upjet/pkg` returns zero hits** — independently confirmed. No upjet code
path has ever populated it, so every entry says only "an update happened".

## Two corrections to the analysis this came from

1. In crossplane-runtime v2.3.3 (which upjet pins) the type is **`ExternalDelete`**,
   not `ExternalDeletion`.
2. **`reconciler.go:1571` logs `update.AdditionalDetails` on the *error* path too**,
   not only on success at `:1584`. This was not in the brief and it changed the
   design: a *refused* update — `assertNoForceNew`, the `UpdateLoopPrevention`
   guard, an apply failure — is the single most valuable place to report the diff,
   because `UpdateLoopPrevention` today reports only an opaque `Reason` string.
   Details are therefore attached to **every** return of `Update`, error paths
   included, via a named return plus a `defer` — which required touching no
   existing `return` statement.

## Redaction: structural, not conditional

**No attribute value is ever emitted** — not `Old`, not `New`, not for any
attribute, sensitive or otherwise. Verified by inspection: the helper contains
**zero** references to `.Old` or `.New`. There is no code path along which a value
can reach a change-log entry.

Paths are **allowlisted by construction**:

* **SDK** — each flatmap key (`disk.0.kms_key`, `tags.Owner`, `subnets.1234567`,
  `tags.%`) is walked segment-by-segment against the compiled-in Terraform schema.
  A segment survives verbatim **only** where the schema declares an attribute of
  that name; list indices, set element hashes and map keys become `*`; a key the
  schema does not recognise is **dropped**, not guessed at. So a secret in a tag
  key yields `tags.*`, never the key.
* **Framework** — `tftypes.AttributeName` steps are schema identifiers by
  construction; `ElementKeyString`/`ElementKeyInt`/`ElementKeyValue` (the last is a
  whole set element *value*) all become `*`.
* **Belt and braces** — a final character-class filter replaces anything outside
  `[A-Za-z0-9_-]`, so a hole in the schema walk still cannot leak user text.

Sensitive attributes are still **named**, with a `(sensitive)` marker drawn from
the union of three models: `ResourceAttrDiff.Sensitive`, the Terraform schema's
`Sensitive` flag (checked at the path *and* every ancestor), and upjet's own
`config.Resource.Sensitive` field paths. The marker gates nothing, since no values
are emitted — it is informational. Naming them is deliberate: a write-only
credential field that never round-trips is one of the most common real causes of
an upjet update loop, and the field's name is a compiled-in schema identifier, not
a secret.

## Bounds

32 attribute paths **and** 2048 bytes, whichever binds first, with 48 bytes held
in reserve so the truncation notice fits inside the cap. Truncation is never
silent: the value ends `, ... (68 more omitted)` and `changedAttributeCount`
always reports the true total. Paths are deduplicated after normalization, which
itself bounds wide resources hard.

```
changedAttributes:     "disk.*.size, name, subnets.*, tags, tags.*"
changedAttributeCount: "5"
```

## Update only

`Create` reports nothing: the diff there is the entire desired configuration,
which the entry's resource snapshot already carries. `Delete` reports nothing:
`instanceDiff` is empty but for `Destroy = true`. Both documented in the method
comments.

## Verification

Three leak mutations, each caught:

* emit `ad.New` alongside the path → 7 subtests fail, leak assertion fires on all
  three sensitive cases;
* disable **both** allowlist layers at once → `MapKeysAreNotReported` reports
  `tags.hunter2-do-not-log`;
* **(independent, third site)** bypass `normalizeFlatmapPath` entirely and emit the
  raw flatmap key → 5 subtests fail with
  `sensitive value leaked into AdditionalDetails["changedAttributes"] = "tags.hunter2-do-not-log"`.

Every test case additionally runs `assertNoSecretLeak`, scanning both keys and
values of the returned map for the literal used as every sensitive value in the
file. The framework cases drive a real `Observe` → `Update`, exercising the actual
`getDiffPlanResponse` wiring rather than a hand-set field; `RequiresReplace`
asserts details come back *alongside* the force-new error.

`golangci-lint` could not run (built with go1.25 against a repo targeting 1.26.7);
the change was hand-checked against the enabled set in `.golangci.yml`.

## Composes with fix 17

No textual conflict — verified by an actual test merge, not by eye: *"Automatic
merge went well"*, and `go build ./...` plus `go test ./pkg/controller/` pass on
the merged tree. Semantically it composes because the named return receives the
returned literal *before* the deferred function runs, so the success path ends up
carrying both `ConnectionDetails` and `AdditionalDetails`.

**Branch** `chlunde/upjet` `feat-changelog-attribute-details` @ `e337a43`.
