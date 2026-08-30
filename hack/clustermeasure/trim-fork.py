#!/usr/bin/env python3
"""Trim the fork to one family's service closure, in a vendored tree.

Two generated files root the 267 services, and they root different things:

  internal/provider/sdkv2/service_packages_gen.go
      imports internal/service/* (the Terraform resource implementations) and
      calls each one's ServicePackage(ctx) from a flat slice literal.

  internal/conns/awsclient_gen.go
      imports aws-sdk-go-v2/service/* (the SDK clients) for the return types of
      266 typed accessors.

Trimming only the first leaves every SDK client package imported, and Go
initialises every imported package whether or not any symbol is reachable - so
their endpoints.init work stays on the heap. Both files have to go.
"""
import os, re, sys

VENDOR = "vendor/github.com/hashicorp/terraform-provider-aws"
KEEP = {
    "s3", "s3control", "s3outposts", "account", "organizations",
    "sts", "iam", "signin", "sso", "ssoadmin", "ssooidc",
    "resourcegroupstaggingapi", "resourcegroups", "dynamodb", "sns", "sqs",
    "apigatewayv2", "kms", "ec2", "cloudwatchlogs", "outposts", "meta",
}

def trim_service_packages():
    p = f"{VENDOR}/internal/provider/sdkv2/service_packages_gen.go"
    out, dropped, kept = [], 0, 0
    for line in open(p).read().split("\n"):
        m = re.match(r'\t"github.com/hashicorp/terraform-provider-aws/internal/service/([a-z0-9]+)"$', line)
        if m:
            if m.group(1) in KEEP: kept += 1
            else: dropped += 1; continue
        m2 = re.match(r'\t\t([a-z0-9]+)\.ServicePackage\(ctx\),$', line)
        if m2 and m2.group(1) not in KEEP:
            continue
        out.append(line)
    open(p, "w").write("\n".join(out))
    print(f"service_packages_gen.go: kept {kept}, dropped {dropped}")

def referenced_accessors():
    """Accessor names still called by code that survives the trim."""
    names = set()
    for root, dirs, files in os.walk(VENDOR):
        if "/internal/service/" in root:
            svc = root.split("/internal/service/")[1].split("/")[0]
            if svc not in KEEP:
                dirs[:] = []
                continue
        for f in files:
            if not f.endswith(".go") or f == "awsclient_gen.go":
                continue
            names.update(re.findall(r'\.([A-Za-z0-9]+)Client\(ctx\)', open(os.path.join(root, f), errors="ignore").read()))
    return names

def trim_awsclient():
    p = f"{VENDOR}/internal/conns/awsclient_gen.go"
    keep_names = referenced_accessors()
    lines = open(p).read().split("\n")
    out, used_pkgs, dropped = [], set(), 0
    i = 0
    hdr = re.compile(r'^func \(c \*AWSClient\) ([A-Za-z0-9]+)Client\(ctx context\.Context\) \*([a-z0-9_]+)\.Client \{$')
    while i < len(lines):
        m = hdr.match(lines[i])
        if not m:
            out.append(lines[i]); i += 1; continue
        # consume the whole function, to its closing brace at column 0
        j = i
        while j < len(lines) and lines[j] != "}":
            j += 1
        name, pkg = m.group(1), m.group(2)
        if name in keep_names:
            used_pkgs.add(pkg)
            out.extend(lines[i:j+1])
        else:
            dropped += 1
            # also drop a single trailing blank line so the file stays tidy
            if j + 1 < len(lines) and lines[j+1] == "":
                j += 1
        i = j + 1
    kept_lines, dropped_imports = [], 0
    imp = re.compile(r'^\t(?:[a-z0-9_]+ )?"github.com/aws/aws-sdk-go-v2/service/([a-z0-9]+)"$')
    for line in out:
        m = imp.match(line)
        if m and m.group(1) not in used_pkgs:
            dropped_imports += 1
            continue
        kept_lines.append(line)
    open(p, "w").write("\n".join(kept_lines))
    print(f"awsclient_gen.go: dropped {dropped} accessors and {dropped_imports} SDK client imports, kept {len(used_pkgs)}")

trim_service_packages()
trim_awsclient()
