#!/bin/bash
R=/tmp/claude-0/-home-user-provider-upjet-aws/6c23abbe-8a8d-548a-afbd-517aed77cb6a/scratchpad/runs
D=10m; I=15s
# Batch 1: post-startup FreeOSMemory, realistic rate, two reps
for rep in 1 2; do
  SCAVENGE_AFTER_STARTUP=1 WORKLOAD=reconcile QPS=2 DURATION=$D INTERVAL=$I \
    /tmp/steadystate > $R/recon-scav-nolimit-rep$rep.txt 2>&1 &
  A=$!
  GOMEMLIMIT=300MiB SCAVENGE_AFTER_STARTUP=1 WORKLOAD=reconcile QPS=2 DURATION=$D INTERVAL=$I \
    /tmp/steadystate > $R/recon-scav-gml300-rep$rep.txt 2>&1 &
  B=$!
  wait $A $B
  echo "BATCH1-rep$rep done"
done
# Batch 2: no post-startup scavenge (does load alone drive release?) + ticker shape
WORKLOAD=reconcile QPS=2 DURATION=$D INTERVAL=$I \
  /tmp/steadystate > $R/recon-noscav-nolimit.txt 2>&1 &
A=$!
GOMEMLIMIT=300MiB WORKLOAD=reconcile QPS=2 DURATION=$D INTERVAL=$I \
  /tmp/steadystate > $R/recon-noscav-gml300.txt 2>&1 &
B=$!
SCAVENGE_AFTER_STARTUP=1 SCAVENGE_EVERY=2m WORKLOAD=reconcile QPS=2 DURATION=$D INTERVAL=$I \
  /tmp/steadystate > $R/recon-ticker-nolimit.txt 2>&1 &
C=$!
wait $A $B $C
echo "BATCH2 done"
# Batch 3: flat out, serially, as an upper bound on churn
SCAVENGE_AFTER_STARTUP=1 WORKLOAD=reconcile QPS=0 DURATION=$D INTERVAL=$I \
  /tmp/steadystate > $R/recon-max-nolimit.txt 2>&1
echo "BATCH3a done"
GOMEMLIMIT=300MiB SCAVENGE_AFTER_STARTUP=1 WORKLOAD=reconcile QPS=0 DURATION=$D INTERVAL=$I \
  /tmp/steadystate > $R/recon-max-gml300.txt 2>&1
echo "DONE-RECON"
