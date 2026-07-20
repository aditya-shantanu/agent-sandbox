#!/bin/bash
# v11: ROUND 9b (second wave) — sustained-300 rerun with the scheduler
# ACTUALLY tuned. SUST3 (v10) found the supply wall: kops v1.35 silently
# ignores cluster.spec.kubeScheduler.kubeAPIQPS (flag-tagged field; modern
# kube-scheduler only reads clientConnection from its config file), so every
# "tuned" cluster ran the scheduler at default 50 QPS = the measured
# 47-50 binds/s ceiling on 34 AND 150 nodes. Fixed at pin 8a57aa7 via
# kubeScheduler.qps=800/burst=1600.
#   SUST4  claims-warm-sustained 300/s x 60s, 150x e2-standard-8, 8
#          namespaces/pools, refill 100/pool, workers 8, dwell 1500ms,
#          headroom 10s, + --per-namespace-claim-watch (9a item 5b; pairs
#          with the 8 namespaces). Pin carries 9a's claim-side no-spec +
#          segment histograms + create-ack riders + apfVerification.
#   SUST5-cbor  (conditional: only if SUST4 meets criteria — failed <= 900
#          AND ready >= 17100) same config + TUNE_CBOR=true on a fresh
#          cluster: CBOR serving/storage A/B at rate. Verify
#          application/cbor on the wire before attributing deltas.
# Capacity math per leg (tool formula): pool ceil(37.5*10s)*8 = 3000;
# inflight ceil(300*(1.5+5)) = 1950; needed 4950 <= spare ~16180.
set -uo pipefail
export PATH=/usr/local/go/bin:/usr/local/bin:$PATH HOME=/root
R="gs://kops-state-142966328212/perf-bench-results/round9b"
ART="/root/artifacts/round9b"
PIN=8a57aa77d1ee0e19bc622261f2b07fa3c8c0ef33
mkdir -p "$ART/SUST4-sustained300" "$ART/SUST5-cbor"
export KOPS_STATE_STORE=gs://kops-state-142966328212

status() { echo "[$(date -u +%FT%TZ)] $*" | tee -a /root/round9b-status.txt; gcloud storage cp /root/round9b-status.txt "$R/STATUS.txt" -q 2>/dev/null || true; }

# Heartbeats: run.log + orchestrate log + status, every 3 min.
( while true; do sleep 180; for l in "$ART"/*/run.log /root/orchestrate.log /root/round9b-status.txt; do [ -f "$l" ] && gcloud storage cp "$l" "$R/hb-$(basename "$(dirname "$l")")-$(basename "$l")" -q 2>/dev/null; done; done ) &
HB=$!
trap 'kill $HB 2>/dev/null' EXIT

status "round9b v11 orchestrator starting (SUST4 sched-fix; conditional SUST5-cbor)"

# Quota preflight (shared project; re-verify the sizing's headroom).
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

# Clean instrumentation (GAP-1). Composed defaults are default-on at this
# pin (incl. the 9a claim-side no-spec half); kept explicit where they were
# explicit in v9/v10 for artifact self-documentation. Pool workers = pool
# count (8).
S_ARGS="--leader-elect=true --sandbox-concurrent-workers=400 --sandbox-claim-concurrent-workers=400 --enable-webhook=false --separate-watch-connection --api-connections=4 --cache-label-selectors --disable-claim-observability-annotations --disable-claim-events --sandbox-warm-pool-concurrent-workers=8 --sandbox-warm-pool-max-refill-rate=100 --one-write-adoption --sandbox-write-behind-window=250ms --no-spec-adoption"
CLEAN_STRESS="--client-connections=4 --profile-controller=false"
SUST_STRESS="--sustained-rate=300 --sustained-seconds=60 --sustained-namespaces=8 --claim-dwell=1500ms --sustained-pool-headroom=10s --per-namespace-claim-watch"

run_leg() { # $1 leg-dir  $2... env assignments (run script invoked in repo)
  local leg="$1"; shift
  status "START $leg"
  (cd /root/repo-round9b && env ARTIFACTS="$ART/$leg" "$@" ./test/benchmarks/scenarios/benchmarks-kops-gcp/run) > "$ART/$leg/run.log" 2>&1
  local rc=$?
  status "DONE $leg rc=$rc"
  grep -A24 'STRESS TEST RESULTS' "$ART/$leg/run.log" > "$ART/$leg/RESULTS.txt" 2>/dev/null || true
  gcloud storage cp -r "$ART/$leg" "$R/" -q || true
  return $rc
}

# Belt-and-braces cluster delete for a given leg's run.log.
delete_leg_cluster() { # $1 leg-dir
  local cl
  cl=$(grep -oE 'sandbox-[0-9]{8}-[0-9]{6}' "$ART/$1/run.log" | head -1)
  [ -z "$cl" ] && { echo "unknown"; return; }
  GOBIN=/root/kops-bin go install k8s.io/kops/cmd/kops@v1.35.0 2>/dev/null
  if /root/kops-bin/kops get cluster --name "$cl" >/dev/null 2>&1; then
    status "cluster $cl still present; deleting"
    /root/kops-bin/kops delete cluster --name "$cl" --yes || true
  fi
  echo "$cl"
}

# ---- Leg 1: SUST4 — scheduler fixed, 9a items aboard. ----
run_leg SUST4-sustained300 \
  NODE_COUNT=150 NODE_MACHINE_TYPE=e2-standard-8 NODE_VOLUME_SIZE=100 \
  CP_MACHINE_TYPE=n2-standard-16 TUNE_CONTROL_PLANE=true TUNE_NODES=true \
  SKIP_E2E_SUITE=true STRESS_SMOKE_COUNT=20 \
  STRESS_PHASES=claims-warm-sustained STRESS_TIMEOUT=60m \
  STRESS_EXTRA_ARGS="$CLEAN_STRESS $SUST_STRESS" \
  CONTROLLER_ARGS="$S_ARGS"
RC_S4=$?
CL4=$(delete_leg_cluster SUST4-sustained300)

# Gate SUST5-cbor on SUST4 meeting the round-9b success criteria: a CBOR
# claim-path delta is unreadable under a supply collapse. Criteria: failed
# <= 900 (5% of 18000) and ready >= 17100 (95%).
S4LINE=$(grep -E 'claims-warm-sustained: [0-9]+ requested' "$ART/SUST4-sustained300/RESULTS.txt" 2>/dev/null | head -1)
S4READY=$(echo "$S4LINE" | grep -oE '[0-9]+ ready' | grep -oE '[0-9]+')
S4FAILED=$(echo "$S4LINE" | grep -oE '[0-9]+ failed' | grep -oE '[0-9]+')
status "SUST4 rc=$RC_S4 ready=${S4READY:-?} failed=${S4FAILED:-?} cluster=$CL4"

CL5=skipped
RC_S5=skipped
if [ "$RC_S4" = 0 ] && [ -n "${S4READY:-}" ] && [ -n "${S4FAILED:-}" ] && \
   [ "$S4READY" -ge 17100 ] && [ "$S4FAILED" -le 900 ]; then
  run_leg SUST5-cbor \
    NODE_COUNT=150 NODE_MACHINE_TYPE=e2-standard-8 NODE_VOLUME_SIZE=100 \
    CP_MACHINE_TYPE=n2-standard-16 TUNE_CONTROL_PLANE=true TUNE_NODES=true TUNE_CBOR=true \
    SKIP_E2E_SUITE=true STRESS_SMOKE_COUNT=20 \
    STRESS_PHASES=claims-warm-sustained STRESS_TIMEOUT=60m \
    STRESS_EXTRA_ARGS="$CLEAN_STRESS $SUST_STRESS" \
    CONTROLLER_ARGS="$S_ARGS"
  RC_S5=$?
  CL5=$(delete_leg_cluster SUST5-cbor)
else
  status "SUST5-cbor SKIPPED (SUST4 did not meet criteria)"
fi

status "ROUND9B-V11 COMPLETE SUST4=$RC_S4($CL4) SUST5=$RC_S5($CL5)"
gcloud storage cp /root/orchestrate.log "$R/orchestrate.log" -q || true
