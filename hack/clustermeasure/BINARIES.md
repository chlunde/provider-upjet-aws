<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# Which binary produced which arm

The measurement used ten binaries, not one. `docs/cluster-measurement.md` said
"one binary for every arm" in its Setup section - that is true **within** round 1
and false from round 2 onward. This file is the manifest that was missing, so the
arms can be reproduced and so cross-binary deltas can be told apart from
single-variable ones.

Every binary is `./cmd/provider/s3`, `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`,
`-ldflags="-s -w"`.

| binary | arms | source state |
| --- | --- | --- |
| `provider` | round 1 (`baseline` … `filter-madvise`), 500-MR arms | branch as committed; `UPJET_FAMILY_FILTER` + `UPJET_SCAVENGE_AFTER_STARTUP` only |
| `provider` (pprof) | `lim*`, `nothp-*`, `filter-nothp-*` | + `UPJET_PPROF_ADDR` |
| `provider-trim` | none (panicked) | + `trim-embeds.py --all`, metadata stubbed too aggressively |
| `provider-trim2` | `trim2*`, `s100-*` | + `trim-embeds.py --all` (configurator-safe metadata) |
| `provider-trim3` | `t3-*` | + upjet `matches()` precompile, `UPJET_CLEAR_SCHEMAFUNC` |
| `provider-sdk` | `sdk`, `sdk2`, `sdk-gml120`, `sdk-gogc25-gml120` | vendored, AWS SDK partition regexes hoisted; **no upjet patch** |
| `provider-v4` | `v4-*` | trimmed embeds + `UPJET_SHARE_SCHEME`, `UPJET_NO_LOG_SAMPLER` (incomplete), `UPJET_LAZY_CONVERT` |
| `provider-v5` | `e5-*` | **stock embeds** + all v4 knobs + `UPJET_STRIP_CACHE_METADATA` + completed sampler patch |
| `provider-v6` | `e6-*` | v5 + `trim-fork.py` (vendored) |
| `provider-v7` | `e7-*` | v6 + `UPJET_SCHEME_FAMILY` |
| `provider-v8` | `e8-*` | v7 + `UPJET_CACHE_AWS_CLIENT` |
| `provider-v9` | `e9-*`, `r10-*`, `r100-*` | v8 + `UPJET_CACHE_IMPLIED_TYPE` |

## Reproducing v4 and later

```console
python3 hack/clustermeasure/trim-embeds.py --all          # v4 only; v5+ use stock embeds
cp -r $(go env GOMODCACHE)/github.com/crossplane/upjet/v2@v2.4.1-0.20260728103920-4f6e6e10dff2 /tmp/upjet-local
chmod -R u+w /tmp/upjet-local
patch -p1 -d /tmp/upjet-local < hack/clustermeasure/upjet-measurement.patch   # strip the a/upjet b/upjet prefixes to taste
go mod edit -replace github.com/crossplane/upjet/v2=/tmp/upjet-local
go mod vendor && python3 hack/clustermeasure/trim-fork.py                    # v6 and later
go build -mod=vendor -ldflags="-s -w" -o provider ./cmd/provider/s3
```

`upjet-measurement.patch` carries all three upjet changes. The
`UPJET_CACHE_IMPLIED_TYPE` one measured as a null and is kept only so v9 can be
rebuilt.

## Known measurement defect

**Rounds 2-9 ran at `--max-reconcile-rate=100`, not 10.** `kingpin`'s
`DefaultEnvars()` derives variable names from `filepath.Base(os.Args[0])`
(`cmd/provider/s3/zz_main.go:103`), and the orchestrators run the binary as
`/opt/provider/${bin}` - so `PROVIDER_MAX_RECONCILE_RATE` was ignored for every
arm with a non-default `BIN`, and so were `PROVIDER_POLL_STATE_METRIC` and
`PROVIDER_ENABLE_SECRET_CACHE`. Confirmed directly: 6,042 goroutines and
26.3 MiB of stacks with the variable set, against 1,540 and 7.0 MiB when the flag
is passed in `args`. `orch3.sh` now passes `--max-reconcile-rate=${rate}` as a
flag.
