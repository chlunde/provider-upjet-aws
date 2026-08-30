#!/usr/bin/env python3
"""Generate a family's scheme-registration closure from its resolvers.

UPJET_SCHEME_FAMILY registers only the API groups a family needs. Hand-picking
that list is unsafe: the s3 family reaches s3control **v1beta2**, a hand-written
list registered only v1beta1, and reference resolution failed at runtime with
  no kind "AccessPoint" is registered for version "s3control.aws.upbound.io/v1beta2"
which no arm caught because none resolved a cross-group reference.

The closure is derivable: every cross-group target appears as a literal
apisresolver.GetManagedResource("<group>", "<version>", ...) call in the
generated resolvers. This walks those, adds the family's own groups and the
ProviderConfig APIs, and emits the import list and registration calls.

  python3 hack/clustermeasure/gen-family-scheme.py s3
"""
import glob, re, sys, collections

family = sys.argv[1] if len(sys.argv) > 1 else "s3"
want = collections.defaultdict(set)          # (scope, shortgroup) -> {versions}

for scope in ("cluster", "namespaced"):
    for f in glob.glob(f"apis/{scope}/{family}/*/zz_generated.resolvers.go"):
        version = f.split("/")[-2]
        want[(scope, family)].add(version)   # the family's own group
        for group, ver in re.findall(r'GetManagedResource\("([^"]+)",\s*"([^"]+)"', open(f).read()):
            short = group.split(".")[0]
            # a *.aws.m.upbound.io group is namespaced, *.aws.upbound.io is cluster-scoped
            tgt = "namespaced" if ".aws.m." in group else "cluster"
            want[(tgt, short)].add(ver)

# the ProviderConfig APIs are needed in both scopes
lines_import, lines_call = [], []
for (scope, short), versions in sorted(want.items()):
    for v in sorted(versions):
        alias = f"{scope[:2]}{short.replace('-','')}{v}"
        lines_import.append(f'\t{alias} "github.com/upbound/provider-aws/v2/apis/{scope}/{short}/{v}"')
        lines_call.append(f"\t\t\t{alias}.AddToScheme,")
for scope, alias in (("cluster", "clpc"), ("namespaced", "nspc")):
    lines_import.append(f'\t{alias}v1beta1 "github.com/upbound/provider-aws/v2/apis/{scope}/v1beta1"')
    lines_call.append(f"\t\t\t{alias}v1beta1.SchemeBuilder.AddToScheme,")

print(f"// {len(lines_call)} registrations for family {family!r}, derived from its resolvers")
print("\n".join(lines_import))
print("---")
print("\n".join(lines_call))
