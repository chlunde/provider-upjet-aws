# The Tagger initializer writes the spec on every reconcile

`(*Tagger).Initialize` (`upjet/pkg/config/resource.go:351`) ends with an
unconditional `t.kube.Update(ctx, mg)`. crossplane-runtime's managed reconciler
calls `Initialize` on every pass (`pkg/reconciler/managed/reconciler.go:1115`),
so after the first reconcile — once `crossplane-kind`, `crossplane-name` and
`crossplane-providerconfig` are already in `spec.forProvider.tags` — this is a
full spec PUT that changes nothing, once per taggable managed resource per poll
interval, forever.

There are two distinct costs, and it is worth keeping them apart.

The one that shows up in logs: the write races the reconciler's own updates and
loses on resource version, producing

```
Cannot initialize managed resource ... "error": "Operation cannot be fulfilled on
securitygroups.ec2.aws.upbound.io \"example4\": the object has been modified;
please apply your changes to the latest version and try again"
```

on essentially every resource creation.

The one that shows up on a bill: the API server byte-compares the incoming
object and discards an identical update before it reaches etcd, so this is *not*
etcd write amplification or resource-version churn. What it is, is an API call —
and write calls are audited at a higher level than reads and carry the request
body. On a cluster with a few thousand managed resources and audit logs shipped
to something that charges for ingest, this is a continuous bill for events that
describe nothing happening.

## The trap in the fix

The obvious implementation — marshal the paved object before and after — is
wrong, and wrong in a way that looks right.

`setExternalTagsWithPaved` (`resource.go:374`) builds a map containing *only* the
three external tags and calls `paved.SetValue("spec.forProvider.<field>", tags)`.
That **replaces** the tags map in the paved JSON: any tags the user set are gone
from those bytes. They survive only because the following
`json.Unmarshal(pavedByte, mg)` merges into the existing non-nil Go map rather
than replacing it.

So a byte comparison of the paved JSON reports a difference on every call for any
resource carrying user tags — the skip would never fire for exactly the resources
most likely to exist in a real cluster, while the plain steady-state case would
pass and make the fix look correct. This was confirmed by mutation testing: with
the byte-comparison implementation in place, only the user-tags test case fails.

The comparison has to be against the managed resource itself:

```go
before := mg.DeepCopyObject()
if err := json.Unmarshal(pavedByte, mg); err != nil {
	return err
}
if equality.Semantic.DeepEqual(before, mg) {
	return nil
}
return t.kube.Update(ctx, mg)
```

`equality.Semantic.DeepEqual` matches what crossplane-runtime's managed
reconciler already uses for a similar suppression.

## Prior art — this was already fixed once

`chlunde/upjet` carries commit `a9c2cc9`, *"Avoid setting tags if there are no
changes, prevent conflict on every resource creation"* (2025-03-06), on the
branch named `fix`. It solves the same defect by returning a `changed` bool from
`setExternalTagsWithPaved` (backed by a `tagsUpToDate` helper) and bailing
*before* the unmarshal, so `mg` is never mutated on a no-op. It is not an
ancestor of `upstream/main` and never landed. Behaviour is equivalent; that shape
is tidier and touches less. Reviving it with the tests below on top is probably
the better upstream PR, since it comes with the field report attached.

## Tests

Table-driven, asserting on the *number* of `Update` calls rather than on the
returned error:

* first reconcile, tags absent — 1 update;
* first reconcile, update fails — 1 update, error propagates;
* steady state, external tags already set — 0 updates;
* steady state with user tags — 0 updates, and the user's tags still present
  afterwards (this is the case that catches the wrong fix);
* an external tag value changed — 1 update;
* `managementPolicies: [Observe]` — 0 updates, tags untouched.

The pre-existing test used `fake.Managed{}`, which has no
`spec.forProvider.tags`, so the unmarshal was silently a no-op and the test could
not distinguish the fix from its absence. It needs a type with a real
`Tags map[string]*string` and a `DeepCopyObject` that actually deep-copies it.

## Pairs with fix 14

Fix 14 suppresses the no-op *status* PUT; this suppresses the no-op *spec* PUT.
Together they take a steady-state reconcile of an unchanged taggable resource
from two guaranteed writes to zero. Neither is worth much alone.

**Branch** `chlunde/upjet` `fix-tagger-skip-noop-spec-update` @ `43f8c2d`.
