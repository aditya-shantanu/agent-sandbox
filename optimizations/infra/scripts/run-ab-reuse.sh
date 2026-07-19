#!/bin/bash
# Sequential A/B benchmark on ONE reused cluster (the preferred workflow):
#   baseline run creates the cluster and keeps it; candidate run reuses it;
#   the cluster is deleted at the end. Each run smoke-tests with a small
#   claims-warm burst before the full 300.
#
# Run this ON THE RUNNER VM (as root) with the two trees cloned by
# vm-bootstrap.sh, or locally with equivalent checkouts:
#   BASELINE_TREE=/root/repo-baseline CANDIDATE_TREE=/root/repo-candidate ./run-ab-reuse.sh
set -uo pipefail
export PATH=/usr/local/go/bin:/usr/local/bin:$PATH

BASELINE_TREE="${BASELINE_TREE:-/root/repo-baseline}"
CANDIDATE_TREE="${CANDIDATE_TREE:-/root/repo-candidate}"
ARTROOT="${ARTROOT:-/root/artifacts/ab-$(date +%Y%m%d-%H%M%S)}"
RESULTS_GCS="${RESULTS_GCS:-gs://kops-state-142966328212/perf-bench-results/latest}"

ARGS_COMMON="--leader-elect=true --enable-pprof-debug --zap-log-level=debug --zap-encoder=json --kube-api-qps=1000 --kube-api-burst=2000 --sandbox-concurrent-workers=400 --sandbox-claim-concurrent-workers=400 --sandbox-warm-pool-concurrent-workers=1 --enable-webhook=false"
ARGS_CANDIDATE="${ARGS_COMMON} --sandbox-warm-pool-replenish-delay=20s"

export KOPS_STATE_STORE=gs://kops-state-142966328212

mkdir -p "${ARTROOT}/baseline" "${ARTROOT}/candidate"

echo "=== [1/3] baseline (creates + keeps cluster) ==="
(cd "${BASELINE_TREE}" && \
  ARTIFACTS="${ARTROOT}/baseline" KEEP_CLUSTER=true NODE_COUNT=6 SKIP_E2E_SUITE=true \
  STRESS_SMOKE_COUNT=20 STRESS_PHASES=claims-warm STRESS_CLAIMS_WARM=300 \
  CONTROLLER_ARGS="${ARGS_COMMON}" \
  ./test/benchmarks/scenarios/benchmarks-kops-gcp/run) > "${ARTROOT}/baseline/run.log" 2>&1
RC_BASE=$?
gcloud storage cp -r "${ARTROOT}/baseline" "${RESULTS_GCS}/ab-baseline/" -q || true

CLUSTER=$(grep -oE 'sandbox-[0-9]{8}-[0-9]{6}' "${ARTROOT}/baseline/run.log" | head -1)
if [[ ${RC_BASE} -ne 0 || -z "${CLUSTER}" ]]; then
  echo "baseline failed (rc=${RC_BASE}); attempting cluster cleanup"
  [[ -n "${CLUSTER}" ]] && kops delete cluster --name "${CLUSTER}" --yes || true
  exit 1
fi
echo "=== baseline done; reusing cluster ${CLUSTER} ==="

echo "=== [2/3] candidate (reuses ${CLUSTER}) ==="
(cd "${CANDIDATE_TREE}" && \
  ARTIFACTS="${ARTROOT}/candidate" EXISTING_CLUSTER="${CLUSTER}" KEEP_CLUSTER=true \
  NODE_COUNT=6 SKIP_E2E_SUITE=true \
  STRESS_SMOKE_COUNT=20 STRESS_PHASES=claims-warm STRESS_CLAIMS_WARM=300 \
  CONTROLLER_ARGS="${ARGS_CANDIDATE}" \
  ./test/benchmarks/scenarios/benchmarks-kops-gcp/run) > "${ARTROOT}/candidate/run.log" 2>&1
RC_CAND=$?
gcloud storage cp -r "${ARTROOT}/candidate" "${RESULTS_GCS}/ab-candidate/" -q || true

echo "=== [3/3] deleting cluster ${CLUSTER} ==="
kops delete cluster --name "${CLUSTER}" --yes || true

echo "A/B complete: baseline rc=${RC_BASE}, candidate rc=${RC_CAND}"
echo "Artifacts: ${ARTROOT} and ${RESULTS_GCS}/ab-{baseline,candidate}/"
exit $(( RC_BASE > RC_CAND ? RC_BASE : RC_CAND ))
