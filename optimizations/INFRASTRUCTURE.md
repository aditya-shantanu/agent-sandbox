# Perf Investigation Infrastructure

Inventory of the VMs, clusters, and cloud resources used by the sandbox-claim
latency investigation (branch `perf-investigation-master`). GCP project:
`gke-ai-eco-dev` (number `142966328212`), region `us-central1`.

## Benchmark runner VM

| | |
|---|---|
| Name | `perf-bench-runner` |
| Zone | `us-central1-a` |
| Machine type | `n2-standard-16` |
| Image | `ubuntu-2404-lts-amd64` (100 GB boot disk) |
| Scopes | `cloud-platform` (uses the default compute service account) |
| Created | 2026-07-18 |
| Purpose | Runs the ENTIRE benchmark pipeline in-cloud (image builds via docker buildx + qemu-arm64, kops cluster lifecycle, the stress-test client, artifact upload). Created so runs survive the operator's laptop going offline, and so client-observed latencies do not include a residential WAN link. |

Bootstrap + orchestration scripts live in
`gs://kops-state-142966328212/perf-bench-scripts/` (`vm-bootstrap.sh`,
`vm-orchestrate.sh`) — canonical copies are checked in under
`optimizations/infra/`: Terraform for the VM in `infra/terraform/` (import the
live VM or apply fresh), the VM scripts and helpers in `infra/scripts/`
(`launch-runner.sh` uploads scripts + relaunches without SSH,
`run-ab-reuse.sh` is the sequential one-cluster A/B, `cleanup-leaks.sh`
sweeps leaked clusters/VM). Repo clones on the VM: `/root/repo-baseline` (branch
`perf-baseline-bench`) and `/root/repo-candidate`
(`perf-investigation-master`). SSH requires interactive corp auth
(IAP/OS Login); monitoring therefore goes through GCS heartbeats (below), not SSH.

## Benchmark kops clusters (ephemeral)

Created and torn down by `test/benchmarks/scenarios/benchmarks-kops-gcp/run`
per run; named `sandbox-<YYYYMMDD-HHMMSS>` from the run start time.

| | |
|---|---|
| Shape | 1× control plane `n2-standard-8` + `NODE_COUNT` (6 for the 300-claim scenario) × `n2-standard-8` workers |
| CNI | cilium, with endpoint-create rate limit raised to 100/s and k8s-client QPS 50/burst 100 (script does this) |
| kops state store | `gs://kops-state-142966328212` |
| Cleanup | trap in the run script deletes the cluster on exit. **If a run is killed mid-flight, check for leaks**: `gcloud compute networks list --filter="name~sandbox-2026"`, then `kops delete cluster --name <name> --yes` with `KOPS_STATE_STORE=gs://kops-state-142966328212`. Instances cost the most — verify with `gcloud compute instances list --filter="name~sandbox-2026"`. |

Clusters known to have been created so far (all deleted): `sandbox-20260718-124152`
(first pipeline validation, died on pip), `sandbox-20260718-133603`-era cluster
(baseline1, successful measurement), `sandbox-20260718-145351` (leaked on
network timeout, cleaned manually), `sandbox-20260718-151446` (interrupted for
the move to the runner VM, cleaned).

## Registries and artifacts

- Images: `gcr.io/gke-ai-eco-dev/agent-sandbox/{agent-sandbox-controller,chrome-sandbox}:<git-describe>` plus `:buildcache` tags.
- Benchmark results: `gs://kops-state-142966328212/perf-bench-results/latest/` — per-run folders (`candidate1`, `baseline2`, `candidate2`) with `RESULTS.txt`, `run.log`, `summary.json`, `metrics.jsonl.gz`; `heartbeat-*.log` refreshed every 3 min while runs are live. Later rounds use per-round folders (`round2/`, `round3/`, ..., `gate0/`) with the same shape plus `STATUS.txt` (one line per orchestrator state change) and `hb-<leg>-run.log` heartbeats. Superseded orchestrators are archived under `perf-bench-scripts/archive/`.
- Startup-script marker: the VM startup script is idempotent via `/root/.bench-started-vN`; bump the marker (v8 = gate zero, v9 = round 8, v10/v11 = round 9b, v12 = round 10), `gcloud compute instances add-metadata perf-bench-runner --metadata-from-file startup-script=optimizations/infra/terraform/vm-startup.sh`, then `reset` the VM to relaunch.
- Quota watch (shared project; measured 2026-07-20): **N2_CPUS ~exhausted (7,708/8,000)** — the effective 34-worker cap on n2-standard-8 legs; node-scale legs use `NODE_MACHINE_TYPE=e2-standard-8` (CPUS quota, ~6,600 free) and `NODE_VOLUME_SIZE=100` (SSD_TOTAL_GB headroom ~21TB). The v11 orchestrator runs a quota preflight before cluster create.
- Round-9b clusters (all created and deleted 2026-07-20, verified no leaks): `sandbox-20260720-181027` (SUST3, 150 nodes), `sandbox-20260720-195719` (SUST4, 150 nodes), SUST5-cbor never created (kops --set featureGates failure).
- Cost anchors (measured 2026-07-20): 150× e2-standard-8 + n2-standard-16 CP + 100GB pd-ssd ≈ **$45/h all-in**; a full bring-up→measure→teardown supply leg = 33-37 min ≈ **$25-28**. A 70-node leg of the same shape ≈ $22-24/h, ≈ $15-20/leg (round-10 sizing, from the corrected ~60-80-node/$12-16k-per-month 300/s estimate).
- Round-10 clusters (2026-07-20, verified no leaks post-run): `sandbox-20260720-225023` (70 nodes, shared by legs A and B, deleted by leg B's cleanup + belt-and-braces check); `sandbox-r10dryrun` (kops CONFIG only for the cluster-spec-patch dry-run validation — no cloud resources ever created — deleted). Two legs total ≈ 43 cluster-minutes ≈ **~$17** against the $60-90 budget; leg C (CBOR) auto-skipped per its gate.
- Baseline1 artifacts (run from the laptop before the VM existed): local only, `/tmp/bench-artifacts-baseline/stress-test/` on the operator's Mac.

## Standard invocation

```
NODE_COUNT=6 SKIP_E2E_SUITE=true STRESS_PHASES=claims-warm STRESS_CLAIMS_WARM=300 \
CONTROLLER_ARGS="--leader-elect=true --enable-pprof-debug --zap-log-level=debug \
  --zap-encoder=json --kube-api-qps=1000 --kube-api-burst=2000 \
  --sandbox-concurrent-workers=400 --sandbox-claim-concurrent-workers=400 \
  --sandbox-warm-pool-concurrent-workers=1 --enable-webhook=false" \
test/benchmarks/scenarios/benchmarks-kops-gcp/run
```

Candidate runs add `--sandbox-warm-pool-replenish-delay=20s` to `CONTROLLER_ARGS`.

### Cluster reuse + smoke testing (preferred A/B workflow)

Cluster bring-up is ~20 min of every run; reusing one cluster for a
baseline/candidate pair halves wall time AND removes cluster-to-cluster
variance from the comparison. Env knobs on the run script:

- `KEEP_CLUSTER=true` — cleanup leaves the cluster running (delete manually
  when done: `KOPS_STATE_STORE=gs://kops-state-142966328212 kops delete
  cluster --name <name> --yes`).
- `EXISTING_CLUSTER=<name>` — skip create/validate/cilium tuning; export
  kubeconfig and go straight to deploy + stress. Controller redeploys with
  the current tree's image, so switching branches between invocations is the
  A/B mechanism.
- `STRESS_SMOKE_COUNT=20` — run a 20-claim claims-warm smoke first; aborts
  before the full-size run if the pipeline is broken.

Sequential A/B on one cluster:
```
# 1. baseline: creates cluster, keeps it
KEEP_CLUSTER=true STRESS_SMOKE_COUNT=20 ... (baseline tree)/run
# 2. candidate: reuses it
EXISTING_CLUSTER=sandbox-<ts> STRESS_SMOKE_COUNT=20 ... (candidate tree)/run
# 3. delete the cluster manually
```
`SKIP_E2E_SUITE=true` exists because laptop/VM pip mirrors could not install the
Python SDK deps (setuptools/duckdb unavailable on `us-python.pkg.dev` mirror);
the stress phase is pure Go and unaffected. The post-run HTML report generator
fails for the same pip reason — cosmetic; `summary.json` and stdout tables are
written before it.

## Known operator-laptop pitfalls (for local runs only — prefer the VM)

- Docker daemon is colima: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock`; the `colima` docker context had to be recreated (`docker context create colima --docker host=...`), and the buildx builder `agent-sandbox-builder` references it.
- gcloud tokens expire and hang the gcr push with `failed to authorize: DeadlineExceeded` — rerun `gcloud auth login`.
- Concurrent local runs saturate the uplink (layer-push EOFs).
