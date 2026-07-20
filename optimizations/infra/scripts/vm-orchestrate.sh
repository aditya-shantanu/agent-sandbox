#!/bin/bash
# v8: GATE ZERO — clean-instrumentation benchmark of the composed tree
# (ROUND7-PLAN §3 + GAP-AUDIT riders GAP-1/2/7 + SCALING-GUIDE §4 infra).
# One cluster (34 nodes, tuned CP + tuned nodes), three legs, same image
# (perf-investigation-master @ 2b60967):
#   CLEAN-A  burst-300, clean instrumentation (no debug logs, no pprof-debug,
#            no client qps/burst pins, apiserver at default verbosity)
#   CLEAN-B  same + --one-write-adoption --sandbox-write-behind-window=250ms
#            --no-spec-adoption (the composed candidate)
#   SUST     claims-warm-sustained 300/s x 60s, 4 namespaces, replenish
#            delay 0 + refill-rate 100/pool + 4 pool workers.
#            Capacity math (tool formula, main.go checkClusterCapacity):
#            pool = ceil(75*5s)*4 = 1500; inflight = ceil(300*(1.5s+5)) = 1950;
#            needed 3450 <= spare ~3740-~72 system pods. The specified
#            headroom=6s/dwell=2s would have needed 3900 > spare (the exact
#            round-6 refusal), hence headroom=5s + dwell=1500ms.
set -uo pipefail
export PATH=/usr/local/go/bin:/usr/local/bin:$PATH HOME=/root
R="gs://kops-state-142966328212/perf-bench-results/gate0"
ART="/root/artifacts/gate0"
PIN=2b60967adf929e793e80b79fa489efe5a64d1636
mkdir -p "$ART/A-clean-burst" "$ART/B-clean-composed" "$ART/S-sustained300"
export KOPS_STATE_STORE=gs://kops-state-142966328212

status() { echo "[$(date -u +%FT%TZ)] $*" | tee -a /root/gate0-status.txt; gcloud storage cp /root/gate0-status.txt "$R/STATUS.txt" -q 2>/dev/null || true; }

# Heartbeats: run.log per leg + orchestrate log + status, every 3 min.
( while true; do sleep 180; for l in "$ART"/*/run.log /root/orchestrate.log /root/gate0-status.txt; do [ -f "$l" ] && gcloud storage cp "$l" "$R/hb-$(basename "$(dirname "$l")")-$(basename "$l")" -q 2>/dev/null; done; done ) &
HB=$!
trap 'kill $HB 2>/dev/null' EXIT

status "gate0 v8 orchestrator starting"

[ -d /root/repo-gate0/.git ] || git clone -q /root/repo-candidate /root/repo-gate0
git -C /root/repo-gate0 remote set-url origin https://github.com/aditya-shantanu/agent-sandbox.git
git -C /root/repo-gate0 fetch -q origin perf-investigation-master
if ! git -C /root/repo-gate0 checkout -qf "$PIN"; then
  status "FATAL: pinned commit $PIN not found after fetch"
  gcloud storage cp /root/orchestrate.log "$R/orchestrate.log" -q || true
  exit 1
fi
status "repo pinned to $(git -C /root/repo-gate0 rev-parse --short HEAD)"

# Clean-instrumentation controller args (GAP-1): NO --zap-log-level=debug,
# NO --zap-encoder=json, NO --enable-pprof-debug, NO --kube-api-qps/burst
# (binary default -1 = client throttling off, per scaling-guide 3.5).
BASE_ARGS="--leader-elect=true --sandbox-concurrent-workers=400 --sandbox-claim-concurrent-workers=400 --enable-webhook=false --separate-watch-connection --api-connections=4 --cache-label-selectors --disable-claim-observability-annotations --disable-claim-events"
A_ARGS="$BASE_ARGS --sandbox-warm-pool-concurrent-workers=1 --sandbox-warm-pool-replenish-delay=20s"
B_ARGS="$A_ARGS --one-write-adoption --sandbox-write-behind-window=250ms --no-spec-adoption"
S_ARGS="$BASE_ARGS --sandbox-warm-pool-concurrent-workers=4 --sandbox-warm-pool-max-refill-rate=100 --one-write-adoption --sandbox-write-behind-window=250ms --no-spec-adoption"
CLEAN_STRESS="--client-connections=4 --profile-controller=false"

run_leg() { # $1 leg-dir  $2... env assignments (run script invoked in repo)
  local leg="$1"; shift
  status "START $leg"
  (cd /root/repo-gate0 && env ARTIFACTS="$ART/$leg" "$@" ./test/benchmarks/scenarios/benchmarks-kops-gcp/run) > "$ART/$leg/run.log" 2>&1
  local rc=$?
  status "DONE $leg rc=$rc"
  grep -A16 'STRESS TEST RESULTS' "$ART/$leg/run.log" > "$ART/$leg/RESULTS.txt" 2>/dev/null || true
  gcloud storage cp -r "$ART/$leg" "$R/" -q || true
  return $rc
}

# ---- Leg A: create + keep cluster, clean burst-300 ----
run_leg A-clean-burst \
  NODE_COUNT=34 CP_MACHINE_TYPE=n2-standard-16 TUNE_CONTROL_PLANE=true TUNE_NODES=true \
  KEEP_CLUSTER=true SKIP_E2E_SUITE=true STRESS_SMOKE_COUNT=20 \
  STRESS_PHASES=claims-warm STRESS_CLAIMS_WARM=300 STRESS_TIMEOUT=30m \
  STRESS_EXTRA_ARGS="$CLEAN_STRESS" \
  CONTROLLER_ARGS="$A_ARGS"
RC_A=$?

CL=$(grep -oE 'sandbox-[0-9]{8}-[0-9]{6}' "$ART/A-clean-burst/run.log" | head -1)
status "leg A rc=$RC_A cluster=$CL"

if [ -z "$CL" ]; then
  status "FATAL: no cluster name found in leg A log; aborting legs B/S"
  gcloud storage cp /root/orchestrate.log "$R/orchestrate.log" -q || true
  exit 1
fi

# ---- Leg B: reuse cluster, composed flags ----
run_leg B-clean-composed \
  EXISTING_CLUSTER="$CL" KEEP_CLUSTER=true NODE_COUNT=34 SKIP_E2E_SUITE=true \
  STRESS_SMOKE_COUNT=20 STRESS_PHASES=claims-warm STRESS_CLAIMS_WARM=300 STRESS_TIMEOUT=30m \
  STRESS_EXTRA_ARGS="$CLEAN_STRESS" \
  CONTROLLER_ARGS="$B_ARGS"
RC_B=$?

# ---- Leg S: reuse cluster, sustained 300/s; cleanup deletes the cluster ----
run_leg S-sustained300 \
  EXISTING_CLUSTER="$CL" NODE_COUNT=34 SKIP_E2E_SUITE=true \
  STRESS_SMOKE_COUNT=20 STRESS_PHASES=claims-warm-sustained STRESS_TIMEOUT=45m \
  STRESS_EXTRA_ARGS="$CLEAN_STRESS --sustained-rate=300 --sustained-seconds=60 --sustained-namespaces=4 --claim-dwell=1500ms --sustained-pool-headroom=5s" \
  CONTROLLER_ARGS="$S_ARGS"
RC_S=$?

# Belt-and-braces cluster delete (leg S's cleanup should already have done it).
GOBIN=/root/kops-bin go install k8s.io/kops/cmd/kops@v1.35.0 2>/dev/null
if /root/kops-bin/kops get cluster --name "$CL" >/dev/null 2>&1; then
  status "cluster $CL still present; deleting"
  /root/kops-bin/kops delete cluster --name "$CL" --yes || true
fi

status "GATE0 COMPLETE A=$RC_A B=$RC_B S=$RC_S cluster=$CL"
gcloud storage cp /root/orchestrate.log "$R/orchestrate.log" -q || true
