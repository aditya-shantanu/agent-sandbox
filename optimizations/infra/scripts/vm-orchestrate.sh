#!/bin/bash
# v9: ROUND 8 — sustained-300 rerun after the leg-S fixes (double-bind
# reservation + direct-read recovery, pool dedicated write connection,
# backlog-aware claim polling, artifact-gap fixes). Single leg on a fresh
# 34-node tuned cluster, image perf-investigation-master @ d1ddac3:
#   SUST2  claims-warm-sustained 300/s x 60s, 4 namespaces, replenish
#          delay 0 + refill-rate 100/pool + 4 pool workers.
#          Capacity math unchanged from gate zero (tool formula,
#          main.go checkClusterCapacity):
#          pool = ceil(75*5s)*4 = 1500; inflight = ceil(300*(1.5s+5)) = 1950;
#          needed 3450 <= spare ~3740-~72 system pods. (headroom=6s/dwell=2s
#          would need 3900 > spare — the round-6 refusal — hence
#          headroom=5s + dwell=1500ms, same as leg S.)
set -uo pipefail
export PATH=/usr/local/go/bin:/usr/local/bin:$PATH HOME=/root
R="gs://kops-state-142966328212/perf-bench-results/round8"
ART="/root/artifacts/round8"
PIN=d1ddac3a666cc463e1f1dad5ecdc9f45c9e18e61
mkdir -p "$ART/S2-sustained300"
export KOPS_STATE_STORE=gs://kops-state-142966328212

status() { echo "[$(date -u +%FT%TZ)] $*" | tee -a /root/round8-status.txt; gcloud storage cp /root/round8-status.txt "$R/STATUS.txt" -q 2>/dev/null || true; }

# Heartbeats: run.log + orchestrate log + status, every 3 min.
( while true; do sleep 180; for l in "$ART"/*/run.log /root/orchestrate.log /root/round8-status.txt; do [ -f "$l" ] && gcloud storage cp "$l" "$R/hb-$(basename "$(dirname "$l")")-$(basename "$l")" -q 2>/dev/null; done; done ) &
HB=$!
trap 'kill $HB 2>/dev/null' EXIT

status "round8 v9 orchestrator starting"

[ -d /root/repo-round8/.git ] || git clone -q /root/repo-candidate /root/repo-round8
git -C /root/repo-round8 remote set-url origin https://github.com/aditya-shantanu/agent-sandbox.git
git -C /root/repo-round8 fetch -q origin perf-investigation-master
if ! git -C /root/repo-round8 checkout -qf "$PIN"; then
  status "FATAL: pinned commit $PIN not found after fetch"
  gcloud storage cp /root/orchestrate.log "$R/orchestrate.log" -q || true
  exit 1
fi
status "repo pinned to $(git -C /root/repo-round8 rev-parse --short HEAD)"

# Clean instrumentation (GAP-1 discipline, same as gate zero): NO debug logs,
# NO pprof-debug, NO client qps/burst pins. Round-8 defaults already include
# --one-write-adoption, --sandbox-write-behind-window=250ms,
# --no-spec-adoption and the new --pool-dedicated-connection.
S_ARGS="--leader-elect=true --sandbox-concurrent-workers=400 --sandbox-claim-concurrent-workers=400 --enable-webhook=false --separate-watch-connection --api-connections=4 --cache-label-selectors --disable-claim-observability-annotations --disable-claim-events --sandbox-warm-pool-concurrent-workers=4 --sandbox-warm-pool-max-refill-rate=100 --one-write-adoption --sandbox-write-behind-window=250ms --no-spec-adoption"
CLEAN_STRESS="--client-connections=4 --profile-controller=false"

run_leg() { # $1 leg-dir  $2... env assignments (run script invoked in repo)
  local leg="$1"; shift
  status "START $leg"
  (cd /root/repo-round8 && env ARTIFACTS="$ART/$leg" "$@" ./test/benchmarks/scenarios/benchmarks-kops-gcp/run) > "$ART/$leg/run.log" 2>&1
  local rc=$?
  status "DONE $leg rc=$rc"
  grep -A16 'STRESS TEST RESULTS' "$ART/$leg/run.log" > "$ART/$leg/RESULTS.txt" 2>/dev/null || true
  gcloud storage cp -r "$ART/$leg" "$R/" -q || true
  return $rc
}

# ---- Single leg: fresh 34-node tuned cluster, sustained 300/s; the run
# script's cleanup trap deletes the cluster on exit (no KEEP_CLUSTER). ----
run_leg S2-sustained300 \
  NODE_COUNT=34 CP_MACHINE_TYPE=n2-standard-16 TUNE_CONTROL_PLANE=true TUNE_NODES=true \
  SKIP_E2E_SUITE=true STRESS_SMOKE_COUNT=20 \
  STRESS_PHASES=claims-warm-sustained STRESS_TIMEOUT=45m \
  STRESS_EXTRA_ARGS="$CLEAN_STRESS --sustained-rate=300 --sustained-seconds=60 --sustained-namespaces=4 --claim-dwell=1500ms --sustained-pool-headroom=5s" \
  CONTROLLER_ARGS="$S_ARGS"
RC_S=$?

CL=$(grep -oE 'sandbox-[0-9]{8}-[0-9]{6}' "$ART/S2-sustained300/run.log" | head -1)

# Belt-and-braces cluster delete (the run's cleanup should already have done it).
if [ -n "$CL" ]; then
  GOBIN=/root/kops-bin go install k8s.io/kops/cmd/kops@v1.35.0 2>/dev/null
  if /root/kops-bin/kops get cluster --name "$CL" >/dev/null 2>&1; then
    status "cluster $CL still present; deleting"
    /root/kops-bin/kops delete cluster --name "$CL" --yes || true
  fi
fi

status "ROUND8 COMPLETE S2=$RC_S cluster=${CL:-unknown}"
gcloud storage cp /root/orchestrate.log "$R/orchestrate.log" -q || true
