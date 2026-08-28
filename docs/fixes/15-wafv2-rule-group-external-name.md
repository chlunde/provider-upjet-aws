# WebACLRuleGroupAssociation never records its external name and leaks associations

Creating a `WebACLRuleGroupAssociation` fails with

```
failed to set the external-name of the managed resource during create: failed to compute the
external-name from the state map: either rule_group_reference or managed_rule_group must be
present in state file
```

even though the spec clearly has a `ruleGroupReference` (or `managedRuleGroup`) set. The
resource never becomes ready. Worse, the association *is* actually created in AWS before the
error is raised, and because no external name is ever written to the managed resource, the next
Observe reports the resource as non-existent and the controller calls Create again. Every retry
leaves another orphaned rule group association attached to the Web ACL.

The cause is that `GetExternalNameFn` is handed the Terraform state in two different shapes.
In upjet's plugin framework external client, Observe runs `ApplyTFConversions(..., FromTerraform)`
*before* calling `setExternalName`, so by then the singleton-list-to-embedded-object conversion has
turned `rule_group_reference` into a `map[string]any`. Create calls `setExternalName` *before* that
conversion, so the same field is still in its raw Terraform shape: a `[]any` holding a single map.
The two helpers in `config/externalname.go` only did a `map[string]interface{}` type assertion, so
on the Create path the assertion failed, both helpers returned the empty string, and the function
fell through to its "neither block present" error. The SDK-based external client orders the two
calls the same way, so the problem is not specific to the framework path.

The fix is a small `singletonBlock` helper that accepts either shape — a `map[string]any`, or a
`[]any` whose first element is a map — and returns nil for anything else. Both
`customRuleGroupIdentifier` and `managedRuleGroupIdentifier` now go through it, so the external
name is computed identically on the Create and Observe paths. Nothing in upjet changes and no
call sites move.

Existing resources that were stuck in this loop will still need their duplicate associations
cleaned up in AWS by hand; the fix only stops new ones from accumulating.
