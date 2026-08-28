# Three misleading messages on the Framework replace path

All in `upjet/pkg/controller/external_tfpluginfw.go`. None changes behaviour;
all three actively mislead someone reading logs to find out why an update was
refused.

**The doubled prefix.** `planRequiresReplace` (`:863-874`) builds a string that
already starts with `"diff contains fields that require resource replacement: "`,
and `Update` (`:882`) wraps it with the same prefix. The builder loop also leaves
a trailing `", "`. The helper now returns just the joined field list and the
prefix lives only at its single caller.

**A format string handed to `errors.New`.** `filterRequiresReplace` (`:475`)
called `errors.New("cannot get the type at path from resource schema: %v")`,
printing a literal `%v` and discarding the underlying error — on the code path
whose entire job is explaining why the update was refused. Now `errors.Wrap`.

**A Debug line that reports the opposite of what happened.** The same loop logs
*"the prior and plan values are equal. Skipping..."*, but it sat below the
`append` with no conditional, so it fired for every path including the ones just
**kept**. The message text is unambiguous about its intent, so the fix is a
`continue` after the append rather than a rewording — one line, and it preserves
the string operators may already be grepping for.

## Correction to the round-2 triage

[lead-triage-round2.md](../lead-triage-round2.md) renders the resulting condition
message as `...: eventTypeIds,`. That is not literally reproducible.
`tftypes.AttributePath.String()` (terraform-plugin-go v0.28.0,
`tftypes/attribute_path.go:59`) emits `AttributeName("eventTypeIds")`, so the
real message is

```
diff contains fields that require resource replacement: diff contains fields that require resource replacement: AttributeName("name"),
```

The two defects are real; only the field rendering in the quoted example was
simplified.

**Left alone deliberately:** improving that rendering is a separate design call —
it means deciding on a field-path format and handling `ElementKeyInt` /
`ElementKeyString` steps — and it deserves its own issue rather than riding along
with three message fixes.

## Tests

Neither helper had any coverage. Added: a table test on `planRequiresReplace`
pinning the exact string (which catches both the prefix and the trailing
separator), an end-to-end assertion of the user-visible message through `Update`,
and a test using a small recording logger that asserts the skip breadcrumb is
emitted for the equal-values case and *not* for the kept case.

The `errors.Wrap` path is untested: reaching a `TypeAtTerraformPath` failure
needs a `RequiresReplace` path that resolves against the state value but not the
schema, i.e. a contrived state/schema mismatch. One-line `New` → `Wrap`; the
fixture cost was judged not worth it.

**Branch** `chlunde/upjet` `fix-error-message-defects` @ `95385db`.
