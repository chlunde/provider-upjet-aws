#!/usr/bin/env python3
"""Rewrite the two blobs config/ embeds down to what the *runtime* provider
actually reads, so the cost of parsing them can be measured.

  config/schema.json          19.0 MB -> ~0.4 MB
  config/provider-metadata.yaml 7.8 MB -> ~0.2 MB

Why this is sound, per upjet pkg/config/provider.go NewProvider:

* The loop over the JSON schema replaces the JSON-derived schema with the Go
  one (`terraformResource = p.TerraformProvider.ResourcesMap[name]`) for every
  Terraform Plugin SDK resource - 960 of the 1,029 this provider configures. So
  those entries only need to *exist*, with a non-empty Schema, to survive the
  `len(terraformResource.Schema) == 0` check. A one-attribute stub does.
* The 69 Terraform Plugin Framework resources genuinely use the JSON schema.
  They are kept whole - 0.24 MB.
* 654 resources in the file are configured by neither list. They are parsed,
  converted by GetV2ResourceMap and then skipped: 12.5 MB of pure waste.
* data_source_schemas (1.8 MB) is never read by upjet at all.
* The metadata is only read by configurators that edit documentation, and
  provider-upjet-aws nils it out after ConfigureResources anyway. But several
  configurators dereference r.MetaResource and index ArgumentDocs without a nil
  check (config/cluster/s3/config.go:45 among them), so the entries have to
  survive with a non-nil ArgumentDocs map. Everything else goes.

Run from the repository root. Writes .orig backups; --restore puts them back.
"""
import json, os, re, sys

SCHEMA = "config/schema.json"
META = "config/provider-metadata.yaml"
STUB = {"version": 0, "block": {"attributes": {"id": {"type": "string", "computed": True}}}}


def restore():
    for f in (SCHEMA, META):
        if os.path.exists(f + ".orig"):
            os.replace(f + ".orig", f)
            print("restored", f)


def framework_names():
    s = open("config/externalname.go").read()
    m = re.search(r"TerraformPluginFrameworkExternalNameConfigs\s*=\s*map\[string\]config\.ExternalName\{(.*?)\n\}", s, re.S)
    return set(re.findall(r'^\s*"(aws_[a-z0-9_]+)"\s*:', m.group(1), re.M))


def trim_schema():
    fw = framework_names()
    d = json.load(open(SCHEMA))
    key = list(d["provider_schemas"])[0]
    blk = d["provider_schemas"][key]
    rs = blk["resource_schemas"]
    blk.pop("data_source_schemas", None)
    blk["resource_schemas"] = {n: (v if n in fw else STUB) for n, v in rs.items()}
    os.replace(SCHEMA, SCHEMA + ".orig")
    with open(SCHEMA, "w") as f:
        json.dump(d, f, separators=(",", ":"))
    print(f"{SCHEMA}: {os.path.getsize(SCHEMA + '.orig')/1e6:.1f} MB -> {os.path.getsize(SCHEMA)/1e6:.1f} MB "
          f"({len(fw)} framework schemas kept, {len(rs) - len(fw)} stubbed)")


def trim_meta():
    out = ["name: hashicorp/terraform-provider-aws", "resources:"]
    n = 0
    for line in open(META):
        ind = len(line) - len(line.lstrip(" "))
        if line.startswith("    ") and ind == 4 and line.rstrip().endswith(":"):
            name = line.strip()[:-1]
            out += [f"    {name}:", f"        name: {name}", f"        title: {name}", "        argumentDocs: {}"]
            n += 1
    os.replace(META, META + ".orig")
    open(META, "w").write("\n".join(out) + "\n")
    print(f"{META}: {os.path.getsize(META + '.orig')/1e6:.1f} MB -> {os.path.getsize(META)/1e6:.1f} MB ({n} resources kept)")


if __name__ == "__main__":
    if "--restore" in sys.argv:
        restore()
    else:
        if "--schema" in sys.argv or "--all" in sys.argv:
            trim_schema()
        if "--meta" in sys.argv or "--all" in sys.argv:
            trim_meta()
