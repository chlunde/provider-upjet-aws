<!--
SPDX-FileCopyrightText: 2024 The Crossplane Authors <https://crossplane.io>

SPDX-License-Identifier: CC-BY-4.0
-->

# In-cluster memory measurement harness

Throwaway instrumentation that measures what a **family provider pod** actually
costs, as opposed to what a process linking the same packages costs. Companion
to [`hack/memprofile`](../memprofile/README.md); results in
[`docs/cluster-measurement.md`](../../docs/cluster-measurement.md).

The difference that motivates it: every figure in `hack/memprofile` comes from
`/proc/self/smaps_rollup` and `runtime.MemStats`. Kubernetes charges a pod from
its **cgroup**, and on a node with `transparent_hugepage=always` the two
disagree by up to 180 MiB. This harness reads the cgroup.

## Shape

* **kind** cluster, one node (`kind.yaml`).
* **LocalStack** for S3/STS/IAM (`localstack.yaml`), plus `tagshim.yaml` - a
  4-line HTTP server answering the S3 Control `ListTagsForResource` call that
  `aws_s3_bucket` makes after create and that LocalStack 4.14 fails with a 500.
  It is reached through a `hostAliases` entry, because the S3 Control client
  prefixes the account ID to the endpoint hostname.
* The provider runs as a **plain Deployment**, not a Crossplane package: the
  binary is `docker cp`'d to `/provider-bin` on the node and mounted by
  `hostPath`, so an arm can be re-run in seconds without building an image. Only
  the family's CRDs are applied, which is what the family package ships.
* `sampler.yaml` is a privileged `hostPID` pod that samples every 20 s:
  `/proc/<pid>/{status,smaps_rollup}`, the container's `memory.current` and
  `memory.stat`, and `go_memstats_*` from the provider's own `/metrics`. Each
  row is labelled with the `ARM` environment variable read out of the provider
  process, so arms self-label.
* `orch.sh` (run by `orch-pod.yaml`) sequences the arms **inside the cluster**.
  That is not incidental: macOS suspends background processes, which stretched a
  120 s `sleep` on the host to 27 minutes and corrupted the first run.

One binary serves every arm; the arms differ only by environment variable, so
nothing about the link shape changes between them.

## Running it

```console
kind create cluster --config hack/clustermeasure/kind.yaml
kubectl apply -f hack/clustermeasure/localstack.yaml -f hack/clustermeasure/tagshim.yaml
kubectl apply -f package/crds/s3.aws.upbound.io_*.yaml -f package/crds/s3.aws.m.upbound.io_*.yaml \
              -f package/crds/aws.upbound.io_*.yaml   -f package/crds/aws.m.upbound.io_*.yaml
kubectl apply -f hack/clustermeasure/rbac.yaml -f hack/clustermeasure/creds.yaml

GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o provider-s3 ./cmd/provider/s3
docker exec memtest-control-plane mkdir -p /provider-bin
docker cp provider-s3 memtest-control-plane:/provider-bin/provider
curl -sL -o kubectl https://dl.k8s.io/release/v1.35.1/bin/linux/arm64/kubectl
docker cp kubectl memtest-control-plane:/provider-bin/kubectl

kubectl apply -f hack/clustermeasure/sampler.yaml
kubectl create configmap orch -n mem --from-file=orch.sh=hack/clustermeasure/orch.sh \
                                     --from-file=hack/clustermeasure/buckets-50.yaml
kubectl apply -f hack/clustermeasure/orch-pod.yaml
python3 hack/clustermeasure/analyze.py     # medians per arm and phase
```

`orch.sh` hardcodes the tag shim's ClusterIP in a `hostAliases` entry; re-read it
with `kubectl get svc -n mem tagshim` before running.

`samples.tsv` is the raw output behind `docs/cluster-measurement.md`: 400
samples, 11 arms, ~2.5 h of wall clock.

## Building the binary

`go build ./cmd/provider/s3` needs roughly 20 GB of transient space in `$TMPDIR`
on top of a build cache that grows to ~24 GB. It fails with "no space left on
device" well before that; the compile is resumable, so re-running after freeing
space picks up from the build cache.
