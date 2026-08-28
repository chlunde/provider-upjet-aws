# String surgery on structured field paths (R14 + R12)

Two places transform a structured field path — dot-separated segments plus `[n]`
indices — with plain string operations, corrupting any path whose *text* happens
to contain the character being manipulated. Same mistake made twice, same fix
both times: parse with `fieldpath`, transform the segments, render back.

**Both are latent.** Neither is reachable with the configuration that ships in
provider-upjet-aws today. Say so plainly in the PR — overselling these is the
fastest way to lose a reviewer.

## R14 — `pkg/config/conversion/list_conversion.go:121,149`

```go
strings.ReplaceAll(e, "0", "*")
```

The comment says it replaces the zero index segment; it replaces every `0`
**character**. `rule[10].filter` becomes `rule[1*].filter`, a field named
`x509_config` becomes `x5*9_config`, and any index other than zero never matches
a `[*]` key at all. The `ListInjectKeys` lookup then silently misses and the
configured key is not injected — which is precisely the server-side-apply problem
the mechanism exists to prevent.

Inert today because the one registered inject key is EKS `vpcConfig`
(`config/registry_cluster.go:208`) — top-level, digit-free, singleton list, so
index 0 is the only expansion it ever sees.

Replaced by `wildcardIndexedPath`, which parses and swaps every `SegmentIndex`
for `fieldpath.Field("*")`. `Segments.String()` renders a field segment whose
value is `*` as `[*]`, so it round-trips exactly to the wildcard form the `paths`
parameter uses. The two duplicated call sites collapse into one lookup.

## R12 — `pkg/resource/lateinit.go:202`

Whole-name camelCase→snake_case in one shot before `fieldpath.GetValue`.
Canonical names are dotted (`getCanonicalName` joins with `.` at `:435`), and the
converter treats `.` as a word boundary. Measured with upjet's own
`pkg/types/name`:

```
NewFromCamel("ScalingConfig").Snake           = "scaling_config"
NewFromCamel("ScalingConfig.MaxSize").Snake   = "scaling_config_._max_size"
NewFromCamel("BlockDeviceMappings.Ebs").Snake = "block_device_mappings_._ebs"
```

So any nested path can never resolve, and the consequence when it bites is that
late-initialization overwrites a field the user deliberately pinned through
`spec.initProvider`.

Inert today: the only registered names are the single segments `ScalingConfig`
and `User`.

### On "invited by the config docs" — the claim holds, but weaker than stated

`ConditionalIgnoredFields` (`pkg/config/resource.go:266-268`) has no worked
example of its own. Its immediate sibling three lines above, `IgnoredFields`,
does: *"Terraform field paths concatenated with dots... e.g.
`block_device_mappings.ebs`"*. The two are described as the same kind of thing,
so a user following the neighbouring example lands on the broken path.

But `nameFilter`, which consumes `IgnoredFields`, does a plain string compare
with no conversion (`lateinit.go:97-101`) — so the sibling is safe and its doc is
accurate *for it*. The invitation is by adjacency, not by a false statement.

**Partly fixed, and worth knowing:** for a field that is a genuine Terraform list
(`block_device_mappings` really is one) the canonical name carries no index, so
`block_device_mappings.ebs` still will not resolve against a list-shaped
`initProvider`. This fix makes dotted paths work wherever the intermediate
segments are objects — the common case on the v1beta2 embedded-object APIs. The
list-shaped case is a separate design question about whether canonical names
should carry wildcards, and was deliberately not attempted.

## Duplicate helper — a known, accepted cost

R12's fix reuses `convertFieldPathToSnake` **verbatim** from
`fix-fieldpath-segmentwise-camel-snake` (`046b8f2`), which put it in
`pkg/controller/annotation_conversions.go` as an unexported function. R12 lives
in package `resource` and cannot call it, so if both branches land there are two
identical unexported copies.

A test merge reports *"Automatic merge went well"* — different files entirely, no
textual conflict, either can land first. The cheap follow-up is to hoist it into
`pkg/types/name` (both packages already import it and it owns `NewFromCamel`).
That was deliberately not done here, because it would mean editing the other
branch and coupling two otherwise independent PRs. Offer it in the PR description
instead and let the maintainer choose.

## Tests

`TestWildcardIndexedPath` (10 cases: empty, no index, index 0, index 1, index 10,
field name with a literal `0`, digit-bearing name *with* an index, multiple
indices, already-wildcard idempotence, unparsable path) plus six new `TestConvert`
cases in both directions. `TestConvertFieldPathToSnake` (7) and
`TestConditionalFilter` (6).

Note on test-data choice: the literal-zero case uses `x509_config`, not
`s3Bucket` — `s3Bucket` contains a `3`, not a `0`, and would not trip the bug.

Mutation-verified. Reverting the helper to `strings.ReplaceAll(fp, "0", "*")`
fails **12 subtests**; reverting only the call site fails the six new `TestConvert`
cases while every pre-existing case still passes, which is exactly the expected
signal — the old code is correct at index 0 and nowhere else. Reverting R12 fails
precisely the two nested `TestConditionalFilter` cases and neither top-level one.

**Branch** `chlunde/upjet` `fix-path-string-surgery` @ `48677c6`, `97467d6`.
