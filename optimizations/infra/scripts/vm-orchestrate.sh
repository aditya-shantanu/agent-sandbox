#!/bin/bash
# Orchestrates the benchmark queue on the runner VM (run as root):
#   1. candidate1  — first candidate measurement
#   2. baseline2 + candidate2 in parallel — fresh A/B pair to double-check both numbers
# Artifacts (summary.json, logs, metrics) are uploaded to GCS after each run so
# results survive even if the VM is deleted.
set -uo pipefail
export PATH=/usr/local/go/bin:/usr/local/bin:$PATH
export HOME=/root

RESULTS_GCS="gs://kops-state-142966328212/perf-bench-results/latest"
echo "Results will upload to: ${RESULTS_GCS}"

# Heartbeat: upload orchestrator + per-run logs every 3 minutes so progress is
# visible from GCS without SSH access to this VM.
( while true; do
    sleep 180
    gcloud storage cp /root/orchestrate.log "${RESULTS_GCS}/orchestrate.log" -q 2>/dev/null || true
    for l in /root/artifacts/*/run.log; do
      [ -f "$l" ] && gcloud storage cp "$l" "${RESULTS_GCS}/heartbeat-$(basename "$(dirname "$l")").log" -q 2>/dev/null
    done
  done ) &
HEARTBEAT_PID=$!
trap 'kill ${HEARTBEAT_PID} 2>/dev/null' EXIT

ARGS_COMMON="--leader-elect=true --enable-pprof-debug --zap-log-level=debug --zap-encoder=json --kube-api-qps=1000 --kube-api-burst=2000 --sandbox-concurrent-workers=400 --sandbox-claim-concurrent-workers=400 --sandbox-warm-pool-concurrent-workers=1 --enable-webhook=false"
ARGS_CANDIDATE="${ARGS_COMMON} --sandbox-warm-pool-replenish-delay=20s"

run_bench() { # $1=name $2=tree $3=controller-args
  local name="$1" tree="$2" args="$3"
  local art="/root/artifacts/${name}"
  mkdir -p "${art}"
  echo "[$(date -u +%FT%TZ)] START ${name}"
  (cd "${tree}" && ARTIFACTS="${art}" NODE_COUNT=6 SKIP_E2E_SUITE=true \
     STRESS_PHASES=claims-warm STRESS_CLAIMS_WARM=300 \
     CONTROLLER_ARGS="${args}" \
     ./test/benchmarks/scenarios/benchmarks-kops-gcp/run) > "${art}/run.log" 2>&1
  local rc=$?
  echo "[$(date -u +%FT%TZ)] DONE ${name} exit=${rc}"
  grep -A14 'STRESS TEST RESULTS' "${art}/run.log" > "${art}/RESULTS.txt" 2>/dev/null || true
  gcloud storage cp -r "${art}" "${RESULTS_GCS}/" -q || true
  return ${rc}
}

run_bench candidate1 /root/repo-candidate "${ARGS_CANDIDATE}"

# Fresh A/B pair, parallel on separate clusters. Stagger so the
# timestamp-derived cluster names and image pushes don't collide.
run_bench baseline2 /root/repo-baseline "${ARGS_COMMON}" &
P1=$!
sleep 120
run_bench candidate2 /root/repo-candidate "${ARGS_CANDIDATE}" &
P2=$!
wait ${P1}; RC1=$?
wait ${P2}; RC2=$?

echo "FINAL: candidate1 done, baseline2 rc=${RC1}, candidate2 rc=${RC2}"
echo "ALL RESULTS: ${RESULTS_GCS}"
gcloud storage cp /root/orchestrate.log "${RESULTS_GCS}/" -q || true
echo "ORCHESTRATION COMPLETE"
