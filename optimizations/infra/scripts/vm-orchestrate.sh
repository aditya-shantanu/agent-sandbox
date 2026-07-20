#!/bin/bash
# v10: ROUND 9b — sustained-300 with supply >= demand (node-scale leg).
# Single leg SUST3 on a fresh 150-worker e2-standard-8 tuned cluster,
# image perf-investigation-master @ 5c49ed1 (round-8-verified controller;
# adds only the NODE_MACHINE_TYPE/NODE_VOLUME_SIZE bench knobs):
#   SUST3  claims-warm-sustained 300/s x 60s, 8 namespaces/pools,
#          refill-rate 100/pool + 8 pool workers, dwell 1500ms,
#          pool headroom 10s.
# Sizing (RESULTS.md round-9b entry has the full math):
#   pool = ceil(37.5*10s)*8 = 3000; inflight = ceil(300*(1.5s+5)) = 1950;
#   needed 4950 <= spare ~150*110 - ~320 system pods ~= 16180 (31%).
#   Steady-state slot demand ~6000 (pool 3000 + 300/s x ~10s residence);
#   refill capacity 8 pools x 70-85/s isolated >= 560/s vs 300/s demand.
# Quotas (us-central1, verified 2026-07-20): e2 workers draw from CPUS
# (3386/10000 used; +1200 fits), NOT the exhausted N2_CPUS (7708/8000 —
# CP n2-standard-16 still fits); 100GB pd-ssd x150 = 15TB fits SSD_TOTAL_GB
# (163251/184305); +151 IPs fit IN_USE_ADDRESSES (328/575).
set -uo pipefail
export PATH=/usr/local/go/bin:/usr/local/bin:$PATH HOME=/root
R="gs://kops-state-142966328212/perf-bench-results/round9b"
ART="/root/artifacts/round9b"
PIN=5c49ed1d61e76d2e0cbf9c24ab3f685667e8dfe8
mkdir -p "$ART/SUST3-sustained300"
export KOPS_STATE_STORE=gs://kops-state-142966328212

status() { echo "[$(date -u +%FT%TZ)] $*" | tee -a /root/round9b-status.txt; gcloud storage cp /root/round9b-status.txt "$R/STATUS.txt" -q 2>/dev/null || true; }

# Heartbeats: run.log + orchestrate log + status, every 3 min.
( while true; do sleep 180; for l in "$ART"/*/run.log /root/orchestrate.log /root/round9b-status.txt; do [ -f "$l" ] && gcloud storage cp "$l" "$R/hb-$(basename "$(dirname "$l")")-$(basename "$l")" -q 2>/dev/null; done; done ) &
HB=$!
trap 'kill $HB 2>/dev/null' EXIT

status "round9b v10 orchestrator starting"

# Quota preflight: the sizing above assumed the 2026-07-20 quota headroom;
# other tenants share the project, so re-verify before paying for a cluster.
# Needs: 1200 e2 vCPUs (CPUS), 16 N2 vCPUs (N2_CPUS), ~15.2TB SSD, ~152 IPs.
QJSON=$(gcloud compute regions describe us-central1 --project=gke-ai-eco-dev --format=json 2>/dev/null)
QFREE=$(echo "$QJSON" | jq -r '[.quotas[] | {(.metric): (.limit - .usage)}] | add | "CPUS=\(.CPUS) N2_CPUS=\(.N2_CPUS) SSD_GB=\(.SSD_TOTAL_GB) IPS=\(.IN_USE_ADDRESSES)"')
QOK=$(echo "$QJSON" | jq -r '[.quotas[] | {(.metric): (.limit - .usage)}] | add | (.CPUS >= 1250) and (.N2_CPUS >= 16) and (.SSD_TOTAL_GB >= 15600) and (.IN_USE_ADDRESSES >= 155)')
status "quota free: $QFREE (ok=$QOK)"
if [ "$QOK" != "true" ]; then
  status "FATAL: insufficient quota headroom for the 150-node leg; aborting before cluster create"
  gcloud storage cp /root/orchestrate.log "$R/orchestrate.log" -q || true
  exit 1
fi

[ -d /root/repo-round9b/.git ] || git clone -q /root/repo-candidate /root/repo-round9b
git -C /root/repo-round9b remote set-url origin https://github.com/aditya-shantanu/agent-sandbox.git
git -C /root/repo-round9b fetch -q origin perf-investigation-master
if ! git -C /root/repo-round9b checkout -qf "$PIN"; then
  status "FATAL: pinned commit $PIN not found after fetch"
  gcloud storage cp /root/orchestrate.log "$R/orchestrate.log" -q || true
  exit 1
fi
status "repo pinned to $(git -C /root/repo-round9b rev-parse --short HEAD)"

# Clean instrumentation (GAP-1 discipline): NO debug logs, NO pprof-debug,
# NO client qps/burst pins. Composed defaults (one-write-adoption,
# write-behind 250ms, no-spec-adoption, pool-dedicated-connection) are
# default-on at this pin; kept explicit for self-documenting artifacts.
# vs v9: pool workers 4 -> 8 (8 pools this round, workers >= pool count).
S_ARGS="--leader-elect=true --sandbox-concurrent-workers=400 --sandbox-claim-concurrent-workers=400 --enable-webhook=false --separate-watch-connection --api-connections=4 --cache-label-selectors --disable-claim-observability-annotations --disable-claim-events --sandbox-warm-pool-concurrent-workers=8 --sandbox-warm-pool-max-refill-rate=100 --one-write-adoption --sandbox-write-behind-window=250ms --no-spec-adoption"
CLEAN_STRESS="--client-connections=4 --profile-controller=false"

run_leg() { # $1 leg-dir  $2... env assignments (run script invoked in repo)
  local leg="$1"; shift
  status "START $leg"
  (cd /root/repo-round9b && env ARTIFACTS="$ART/$leg" "$@" ./test/benchmarks/scenarios/benchmarks-kops-gcp/run) > "$ART/$leg/run.log" 2>&1
  local rc=$?
  status "DONE $leg rc=$rc"
  grep -A16 'STRESS TEST RESULTS' "$ART/$leg/run.log" > "$ART/$leg/RESULTS.txt" 2>/dev/null || true
  gcloud storage cp -r "$ART/$leg" "$R/" -q || true
  return $rc
}

# ---- Single leg: fresh 150-node tuned cluster, sustained 300/s with
# supply >= demand; the run script's cleanup trap deletes the cluster on
# exit (no KEEP_CLUSTER). STRESS_TIMEOUT 60m: pool provisioning is 3000
# pods + first-pull on 150 nodes, and a degraded run must still drain. ----
run_leg SUST3-sustained300 \
  NODE_COUNT=150 NODE_MACHINE_TYPE=e2-standard-8 NODE_VOLUME_SIZE=100 \
  CP_MACHINE_TYPE=n2-standard-16 TUNE_CONTROL_PLANE=true TUNE_NODES=true \
  SKIP_E2E_SUITE=true STRESS_SMOKE_COUNT=20 \
  STRESS_PHASES=claims-warm-sustained STRESS_TIMEOUT=60m \
  STRESS_EXTRA_ARGS="$CLEAN_STRESS --sustained-rate=300 --sustained-seconds=60 --sustained-namespaces=8 --claim-dwell=1500ms --sustained-pool-headroom=10s" \
  CONTROLLER_ARGS="$S_ARGS"
RC_S=$?

CL=$(grep -oE 'sandbox-[0-9]{8}-[0-9]{6}' "$ART/SUST3-sustained300/run.log" | head -1)

# Belt-and-braces cluster delete (the run's cleanup should already have done it).
if [ -n "$CL" ]; then
  GOBIN=/root/kops-bin go install k8s.io/kops/cmd/kops@v1.35.0 2>/dev/null
  if /root/kops-bin/kops get cluster --name "$CL" >/dev/null 2>&1; then
    status "cluster $CL still present; deleting"
    /root/kops-bin/kops delete cluster --name "$CL" --yes || true
  fi
fi

status "ROUND9B COMPLETE SUST3=$RC_S cluster=${CL:-unknown}"
gcloud storage cp /root/orchestrate.log "$R/orchestrate.log" -q || true
