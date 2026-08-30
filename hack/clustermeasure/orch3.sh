#!/bin/sh
set -u
K=/hostbin/kubectl
OUT=/hostout/samples.tsv
SHIM="10.96.194.60"
mark() { echo "# $(date +%s) $*" >> $OUT; echo "$(date +%H:%M:%S) $*"; }

deploy_arm() {
  arm="$1"; shift
  envs=""
  lim=""
  bin=provider
  mrs=50
  for kv in "$@"; do
    k=$(echo "$kv" | cut -d= -f1); v=$(echo "$kv" | cut -d= -f2-)
    if [ "$k" = "BIN" ]; then bin="$v"; continue; fi
    if [ "$k" = "MRS" ]; then mrs="$v"; continue; fi
    if [ "$k" = "PODLIMIT" ]; then
      lim="        resources:
          limits: {memory: ${v}}
"
      continue
    fi
    envs="${envs}        - name: ${k}
          value: \"${v}\"
"
  done
  cat > /tmp/p.yaml <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: provider-aws-s3, namespace: mem}
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector: {matchLabels: {app: provider-aws-s3}}
  template:
    metadata: {labels: {app: provider-aws-s3, arm: "$arm"}}
    spec:
      serviceAccountName: provider-aws-s3
      terminationGracePeriodSeconds: 5
      hostAliases:
      - ip: "$SHIM"
        hostnames: ["000000000000.localstack.mem.svc.cluster.local"]
      containers:
      - name: provider
        image: alpine:3.22
        command: ["/opt/provider/${bin}"]
        args: ["--certs-dir=", "--poll=1m", "--skip-default-tags"]
        env:
        - name: ARM
          value: "$arm"
${envs}        readinessProbe:
          httpGet: {path: /metrics, port: 8080}
          initialDelaySeconds: 2
          periodSeconds: 2
          failureThreshold: 300
${lim}        volumeMounts:
        - {name: bin, mountPath: /opt/provider, readOnly: true}
      volumes:
      - name: bin
        hostPath: {path: /provider-bin, type: Directory}
EOF
  $K apply -f /tmp/p.yaml >/dev/null
}

run_arm() {
  arm="$1"; shift
  mark "ARM $arm ENV $*"
  # clear MRs
  $K delete buckets.s3.aws.upbound.io --all --wait=false >/dev/null 2>&1
  $K delete deploy provider-aws-s3 -n mem --ignore-not-found >/dev/null 2>&1
  while $K get pods -n mem -l app=provider-aws-s3 --no-headers 2>/dev/null | grep -q .; do sleep 2; done
  for b in $($K get buckets.s3.aws.upbound.io -o name 2>/dev/null); do
    $K patch $b --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1
  done
  $K delete buckets.s3.aws.upbound.io --all --wait=true --timeout=60s >/dev/null 2>&1
  t0=$(date +%s)
  deploy_arm "$arm" "$@"
  $K wait --for=condition=Ready pod -n mem -l app=provider-aws-s3 --timeout=900s >/dev/null 2>&1
  t1=$(date +%s)
  mark "READY $arm ready_s=$((t1-t0))"
  $K logs -n mem -l app=provider-aws-s3 --tail=50 2>/dev/null | grep -E "configuration built|startup heap" | while read -r l; do mark "LOG $arm $l"; done
  sleep 90
  mark "PHASE $arm idle-done"
  $K apply -f /cfg/buckets-${mrs}.yaml >/dev/null 2>&1
  n=0
  i=0
  while [ $i -lt 90 ]; do
    n=$($K get buckets.s3.aws.upbound.io -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -c True)
    [ "$n" -ge "$mrs" ] && break
    sleep 10
    i=$((i+1))
  done
  t2=$(date +%s)
  mark "CREATED $arm ready_buckets=$n in $((t2-t1))s"
  sleep 200
  rc=$($K get pods -n mem -l app=provider-aws-s3 -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null)
  lr=$($K get pods -n mem -l app=provider-aws-s3 -o jsonpath='{.items[0].status.containerStatuses[0].lastState.terminated.reason}' 2>/dev/null)
  uid=$($K get pods -n mem -l app=provider-aws-s3 -o jsonpath='{.items[0].metadata.uid}' 2>/dev/null | tr - _)
  d=$(find /hostcgroup -maxdepth 5 -type d -name "*$uid*" 2>/dev/null | head -1)
  c=$(find $d -maxdepth 1 -type d -name "cri-*" 2>/dev/null | head -1)
  mark "LIMITS $arm restarts=${rc:-0} lastTerm=${lr:-none} max=$(cat ${c:-$d}/memory.max 2>/dev/null) peak=$(cat ${c:-$d}/memory.peak 2>/dev/null) events: $(tr '\n' ' ' < ${c:-$d}/memory.events 2>/dev/null)"
  mark "END $arm"
}

mark "MATRIX DONE"

mark "MATRIX DONE"

run_arm e5-ctrl BIN=provider-v5 UPJET_FAMILY_FILTER=s3 GODEBUG=disablethp=1 GOGC=25 PROVIDER_MAX_RECONCILE_RATE=10 UPJET_PPROF_ADDR=:6060
run_arm e5-lazy BIN=provider-v5 UPJET_FAMILY_FILTER=s3 GODEBUG=disablethp=1 GOGC=25 PROVIDER_MAX_RECONCILE_RATE=10 UPJET_PPROF_ADDR=:6060 UPJET_LAZY_CONVERT=1
run_arm e5-nosampler BIN=provider-v5 UPJET_FAMILY_FILTER=s3 GODEBUG=disablethp=1 GOGC=25 PROVIDER_MAX_RECONCILE_RATE=10 UPJET_PPROF_ADDR=:6060 UPJET_NO_LOG_SAMPLER=1
run_arm e5-ctrl-500 BIN=provider-v5 UPJET_FAMILY_FILTER=s3 GODEBUG=disablethp=1 GOGC=25 PROVIDER_MAX_RECONCILE_RATE=10 UPJET_PPROF_ADDR=:6060 MRS=500
run_arm e5-strip-500 BIN=provider-v5 UPJET_FAMILY_FILTER=s3 GODEBUG=disablethp=1 GOGC=25 PROVIDER_MAX_RECONCILE_RATE=10 UPJET_PPROF_ADDR=:6060 MRS=500 UPJET_STRIP_CACHE_METADATA=1
run_arm e5-all-500 BIN=provider-v5 UPJET_FAMILY_FILTER=s3 GODEBUG=disablethp=1 GOGC=25 PROVIDER_MAX_RECONCILE_RATE=10 UPJET_PPROF_ADDR=:6060 MRS=500 UPJET_STRIP_CACHE_METADATA=1 UPJET_SHARE_SCHEME=1 UPJET_NO_LOG_SAMPLER=1 UPJET_LAZY_CONVERT=1 UPJET_CLEAR_SCHEMAFUNC=1
mark "MATRIX DONE"
