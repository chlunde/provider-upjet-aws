# `Update` returns no connection details

Both external clients finish `Update` holding the post-update Terraform state
map and then return an empty `managed.ExternalUpdate`
(`pkg/controller/external_tfpluginsdk.go:807`,
`external_tfpluginfw.go:950`). `Observe` and `Create` both derive connection
details from that same map (`external_tfpluginfw.go:700`, `:835`). The managed
reconciler publishes `update.ConnectionDetails`
(`crossplane-runtime/pkg/reconciler/managed/reconciler.go:1587`).

**Be accurate about the severity: this is narrow.** The connection secret is not
wiped. `APISecretPublisher` assigns `s.Data = c` and applies, and
`corev1.Secret.Data` is `omitempty`, so an empty map produces a patch that
touches no keys (`crossplane-runtime/pkg/reconciler/managed/api.go:100-121`).
The actual failure is that a credential rotated by an `Update` reaches the
connection secret one `Observe` late. These operations are async with an
immediate requeue, so the window is a single reconcile.

## The ordering trap

The sensitive field paths in `GetConnectionDetailsMapping` are written against
the **native Terraform schema**, so the details must be computed *before*
`ApplyTFConversions`. `Create` gets this right (details at `:835`, conversion at
`:840`). The framework `Update` converts at `:929`, first thing after
unmarshalling the new state, so the call has to be inserted above that line, not
appended at the end of the method. The SDK client converts immediately after
`fromInstanceStateToJSONMap` and has the same constraint.

Computing the details on the converted map is worse than the original bug: the
sensitive paths silently fail to resolve and nothing is published, with no error.

## Making the ordering an assertion

The tests register a `config.TerraformConversion` that renames the sensitive
attribute in `FromTerraform` mode. If the details are computed on the converted
map the path no longer resolves and the result is empty, so the test fails. That
turns the ordering constraint into something the compiler-adjacent machinery
enforces rather than a comment someone can refactor past.

Confirmed by mutation: moving the call below `ApplyTFConversions` in the
framework client fails `TestTPFUpdateConnectionDetails`. Reverting the
production change entirely fails both new tests.

**Branch** `chlunde/upjet` `fix-update-connection-details` @ `edfc8db`.
