#!/bin/bash
# v12: ROUND 10 — sustained-300 with ALL measured walls addressed:
# scheduler qps=800 (9b key fix), etcd quota 8GiB + 2m auto-compaction
# (NEW — wall 4, applied via the run script's cluster-spec patch), 70x
# e2-standard-8 (corrected sizing: nodes exonerated, ~6,000 slots needed),
# supply-segment histograms (pin 273d9d7) so the ~100-150/s controller
# pipeline ceiling decomposes.
#   Leg A  SUST300-single: 300/s x 60s, 8 namespaces/pools, single
#          controller. PASS = ready >= 17100 (95%) AND failed <= 900.
#   Leg B  SUST300-shard2 (only if A FAILS its criteria, i.e. the
#          controller supply ceiling still binds): same cluster (reuse), 2
#          controller shards via --watch-namespaces (4 namespaces each,
#          SHARD_B_NAMESPACES run-script knob) — the sharding-at-rate proof.
#   Leg C  SUST300-cbor (only if A or B PASSED and budget remains): fresh
#          cluster (CBOR server gate is creation-time), TUNE_CBOR=true
#          repeat of the winning config.
# One retry per leg on TRANSIENT infra failure (rc!=0 with no measurement
# reached); no other relaunches.
# Capacity math per leg (tool formula): pool ceil(37.5*10s)*8 = 3000;
# inflight ceil(300*(1.5+5)) = 1950; needed 4950 <= spare ~70*110-~150
# system ~= 7550 (65%, under the 90% warning line).
set -uo pipefail
export PATH=/usr/local/go/bin:/usr/local/bin:$PATH HOME=/root
R="gs://kops-state-142966328212/perf-bench-results/round10"
ART="/root/artifacts/round10"
PIN=273d9d7f0cd15b2835f531c6a643acbeabeb87a6
mkdir -p "$ART/A-sust300-single" "$ART/B-sust300-shard2" "$ART/C-sust300-cbor"
export KOPS_STATE_STORE=gs://kops-state-142966328212

status() { echo "[$(date -u +%FT%TZ)] $*" | tee -a /root/round10-status.txt; gcloud storage cp /root/round10-status.txt "$R/STATUS.txt" -q 2>/dev/null || true; }

# Heartbeats: run.log + orchestrate log + status, every 3 min.
( while true; do sleep 180; for l in "$ART"/*/run.log /root/orchestrate.log /root/round10-status.txt; do [ -f "$l" ] && gcloud storage cp "$l" "$R/hb-$(basename "$(dirname "$l")")-$(basename "$l")" -q 2>/dev/null; done; done ) &
HB=$!
trap 'kill $HB 2>/dev/null' EXIT

status "round10 v12 orchestrator starting (A single -> B shard2 if A fails -> C cbor if winner)"

# Quota preflight (shared project). 70x e2-standard-8 = 560 CPUS + CP 16
# N2_CPUS; 70x100GB pd-ssd workers + CP/etcd disks ~= 7.4TB SSD; 71 IPs.
QJSON=$(gcloud compute regions describe us-central1 --project=gke-ai-eco-dev --format=json 2>/dev/null)
QFREE=$(echo "$QJSON" | jq -r '[.quotas[] | {(.metric): (.limit - .usage)}] | add | "CPUS=\(.CPUS) N2_CPUS=\(.N2_CPUS) SSD_GB=\(.SSD_TOTAL_GB) IPS=\(.IN_USE_ADDRESSES)"')
QOK=$(echo "$QJSON" | jq -r '[.quotas[] | {(.metric): (.limit - .usage)}] | add | (.CPUS >= 620) and (.N2_CPUS >= 16) and (.SSD_TOTAL_GB >= 7500) and (.IN_USE_ADDRESSES >= 75)')
status "quota free: $QFREE (ok=$QOK)"
if [ "$QOK" != "true" ]; then
  status "FATAL: insufficient quota headroom for the 70-node leg; aborting before cluster create"
  gcloud storage cp /root/orchestrate.log "$R/orchestrate.log" -q || true
  exit 1
fi

[ -d /root/repo-round10/.git ] || git clone -q /root/repo-candidate /root/repo-round10
git -C /root/repo-round10 remote set-url origin https://github.com/aditya-shantanu/agent-sandbox.git
git -C /root/repo-round10 fetch -q origin perf-investigation-master
if ! git -C /root/repo-round10 checkout -qf "$PIN"; then
  status "FATAL: pinned commit $PIN not found after fetch"
  gcloud storage cp /root/orchestrate.log "$R/orchestrate.log" -q || true
  exit 1
fi
status "repo pinned to $(git -C /root/repo-round10 rev-parse --short HEAD)"

# Clean instrumentation (GAP-1). Composed defaults on at this pin; explicit
# where prior rounds were explicit, for artifact self-documentation.
S_ARGS="--leader-elect=true --sandbox-concurrent-workers=400 --sandbox-claim-concurrent-workers=400 --enable-webhook=false --separate-watch-connection --api-connections=4 --cache-label-selectors --disable-claim-observability-annotations --disable-claim-events --sandbox-warm-pool-concurrent-workers=8 --sandbox-warm-pool-max-refill-rate=100 --one-write-adoption --sandbox-write-behind-window=250ms --no-spec-adoption"
CLEAN_STRESS="--client-connections=4 --profile-controller=false"
SUST_STRESS="--sustained-rate=300 --sustained-seconds=60 --sustained-namespaces=8 --claim-dwell=1500ms --sustained-pool-headroom=10s --per-namespace-claim-watch"
CLUSTER_ENV=(NODE_COUNT=70 NODE_MACHINE_TYPE=e2-standard-8 NODE_VOLUME_SIZE=100 CP_MACHINE_TYPE=n2-standard-16 TUNE_CONTROL_PLANE=true TUNE_NODES=true SKIP_E2E_SUITE=true STRESS_PHASES=claims-warm-sustained STRESS_TIMEOUT=60m)

run_leg() { # $1 leg-dir  $2... env assignments (run script invoked in repo)
  local leg="$1"; shift
  status "START $leg"
  (cd /root/repo-round10 && env ARTIFACTS="$ART/$leg" "$@" ./test/benchmarks/scenarios/benchmarks-kops-gcp/run) > "$ART/$leg/run.log" 2>&1
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

leg_cluster() { grep -oE 'sandbox-[0-9]{8}-[0-9]{6}' "$ART/$1/run.log" 2>/dev/null | head -1; }
leg_measured() { [ -s "$ART/$1/RESULTS.txt" ]; }
leg_ready()  { grep -E 'claims-warm-sustained: [0-9]+ requested' "$ART/$1/RESULTS.txt" 2>/dev/null | head -1 | grep -oE '[0-9]+ ready'  | grep -oE '[0-9]+'; }
leg_failed() { grep -E 'claims-warm-sustained: [0-9]+ requested' "$ART/$1/RESULTS.txt" 2>/dev/null | head -1 | grep -oE '[0-9]+ failed' | grep -oE '[0-9]+'; }
leg_pass() { # $1 leg-dir: round-10 criteria ready>=17100 failed<=900
  local rd fl; rd=$(leg_ready "$1"); fl=$(leg_failed "$1")
  [ -n "$rd" ] && [ -n "$fl" ] && [ "$rd" -ge 17100 ] && [ "$fl" -le 900 ]
}

# ---- Leg A: single controller, all TUNE knobs incl. the new etcd pair. ----
run_leg A-sust300-single "${CLUSTER_ENV[@]}" KEEP_CLUSTER=true \
  STRESS_SMOKE_COUNT=20 \
  STRESS_EXTRA_ARGS="$CLEAN_STRESS $SUST_STRESS" \
  CONTROLLER_ARGS="$S_ARGS"
RC_A=$?
if [ $RC_A -ne 0 ] && ! leg_measured A-sust300-single; then
  # Transient infra failure before measurement: ONE retry (fresh cluster).
  status "leg A failed before measurement; one retry after cleanup"
  delete_leg_cluster A-sust300-single >/dev/null
  mv "$ART/A-sust300-single/run.log" "$ART/A-sust300-single/run.attempt1.log" 2>/dev/null
  run_leg A-sust300-single "${CLUSTER_ENV[@]}" KEEP_CLUSTER=true \
    STRESS_SMOKE_COUNT=20 \
    STRESS_EXTRA_ARGS="$CLEAN_STRESS $SUST_STRESS" \
    CONTROLLER_ARGS="$S_ARGS"
  RC_A=$?
fi
CL_A=$(leg_cluster A-sust300-single)
A_READY=$(leg_ready A-sust300-single); A_FAILED=$(leg_failed A-sust300-single)
status "leg A rc=$RC_A ready=${A_READY:-?} failed=${A_FAILED:-?} cluster=${CL_A:-?}"

# ---- Leg B: 2-shard controller, same cluster — only if A's supply ceiling
# still binds (A failed its criteria but measured). Smoke off: the smoke
# namespace is outside every shard's list by construction. ----
WINNER=""
RC_B=skipped
if leg_pass A-sust300-single; then
  status "leg A PASSED criteria — sharding leg B not needed"
  WINNER=A
  CL_A_FINAL=$(delete_leg_cluster A-sust300-single)
  status "leg A cluster deleted ($CL_A_FINAL)"
elif leg_measured A-sust300-single && [ -n "${CL_A:-}" ]; then
  run_leg B-sust300-shard2 "${CLUSTER_ENV[@]}" EXISTING_CLUSTER="$CL_A" \
    STRESS_SMOKE_COUNT=0 \
    STRESS_EXTRA_ARGS="$CLEAN_STRESS $SUST_STRESS --namespace=r10b" \
    CONTROLLER_ARGS="$S_ARGS --watch-namespaces=r10b-s1,r10b-s2,r10b-s3,r10b-s4" \
    SHARD_B_NAMESPACES="r10b-s5,r10b-s6,r10b-s7,r10b-s8"
  RC_B=$?
  if [ $RC_B -ne 0 ] && ! leg_measured B-sust300-shard2; then
    status "leg B failed before measurement; one retry on the same cluster"
    mv "$ART/B-sust300-shard2/run.log" "$ART/B-sust300-shard2/run.attempt1.log" 2>/dev/null
    run_leg B-sust300-shard2 "${CLUSTER_ENV[@]}" EXISTING_CLUSTER="$CL_A" \
      STRESS_SMOKE_COUNT=0 \
      STRESS_EXTRA_ARGS="$CLEAN_STRESS $SUST_STRESS --namespace=r10b" \
      CONTROLLER_ARGS="$S_ARGS --watch-namespaces=r10b-s1,r10b-s2,r10b-s3,r10b-s4" \
      SHARD_B_NAMESPACES="r10b-s5,r10b-s6,r10b-s7,r10b-s8"
    RC_B=$?
  fi
  status "leg B rc=$RC_B ready=$(leg_ready B-sust300-shard2 || echo '?') failed=$(leg_failed B-sust300-shard2 || echo '?')"
  leg_pass B-sust300-shard2 && WINNER=B
  CL_B_FINAL=$(delete_leg_cluster B-sust300-shard2)
  status "shared cluster deleted after leg B ($CL_B_FINAL)"
else
  status "leg A did not reach measurement — skipping legs B and C"
  delete_leg_cluster A-sust300-single >/dev/null
fi

# ---- Leg C: TUNE_CBOR=true repeat of the winner, FRESH cluster (the CBOR
# server gate is creation-time — the reused-cluster variant measures
# nothing; round-9b SUST5 lesson). Only when a winner exists: a CBOR delta
# is unreadable under a supply collapse. ----
RC_C=skipped
if [ -n "$WINNER" ]; then
  C_CONTROLLER="$S_ARGS"; C_SHARDS=""; C_STRESS="$CLEAN_STRESS $SUST_STRESS"; C_SMOKE=20
  if [ "$WINNER" = B ]; then
    C_CONTROLLER="$S_ARGS --watch-namespaces=r10c-s1,r10c-s2,r10c-s3,r10c-s4"
    C_SHARDS="r10c-s5,r10c-s6,r10c-s7,r10c-s8"
    C_STRESS="$CLEAN_STRESS $SUST_STRESS --namespace=r10c"
    C_SMOKE=0
  fi
  run_leg C-sust300-cbor "${CLUSTER_ENV[@]}" TUNE_CBOR=true \
    STRESS_SMOKE_COUNT="$C_SMOKE" \
    STRESS_EXTRA_ARGS="$C_STRESS" \
    CONTROLLER_ARGS="$C_CONTROLLER" \
    SHARD_B_NAMESPACES="$C_SHARDS"
  RC_C=$?
  if [ $RC_C -ne 0 ] && ! leg_measured C-sust300-cbor; then
    status "leg C failed before measurement; one retry after cleanup"
    delete_leg_cluster C-sust300-cbor >/dev/null
    mv "$ART/C-sust300-cbor/run.log" "$ART/C-sust300-cbor/run.attempt1.log" 2>/dev/null
    run_leg C-sust300-cbor "${CLUSTER_ENV[@]}" TUNE_CBOR=true \
      STRESS_SMOKE_COUNT="$C_SMOKE" \
      STRESS_EXTRA_ARGS="$C_STRESS" \
      CONTROLLER_ARGS="$C_CONTROLLER" \
      SHARD_B_NAMESPACES="$C_SHARDS"
    RC_C=$?
  fi
  status "leg C rc=$RC_C ready=$(leg_ready C-sust300-cbor || echo '?') failed=$(leg_failed C-sust300-cbor || echo '?')"
  CL_C_FINAL=$(delete_leg_cluster C-sust300-cbor)
  status "leg C cluster deleted ($CL_C_FINAL)"
else
  status "leg C SKIPPED (no leg met the pass criteria — CBOR delta unreadable under supply collapse)"
fi

# Final leak sweep: any sandbox-2026* GCE artifacts left is a loud failure.
LEAKS=$(gcloud compute instances list --project=gke-ai-eco-dev --filter="name~sandbox-2026" --format="value(name)" 2>/dev/null | wc -l)
status "ROUND10-V12 COMPLETE A=$RC_A B=$RC_B C=$RC_C winner=${WINNER:-none} gce-instance-leaks=$LEAKS"
gcloud storage cp /root/orchestrate.log "$R/orchestrate.log" -q || true
