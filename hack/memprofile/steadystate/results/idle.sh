#!/bin/bash
R=/tmp/claude-0/-home-user-provider-upjet-aws/6c23abbe-8a8d-548a-afbd-517aed77cb6a/scratchpad/runs
for rep in 1 2; do
  WORKLOAD=idle DURATION=900s INTERVAL=15s /tmp/steadystate > $R/idle-nolimit-rep$rep.txt 2>&1
  GOMEMLIMIT=300MiB WORKLOAD=idle DURATION=900s INTERVAL=15s /tmp/steadystate > $R/idle-gml300-rep$rep.txt 2>&1
done
echo DONE-IDLE
