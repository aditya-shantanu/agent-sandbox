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
`vm-orchestrate.sh`). Repo clones on the VM: `/root/repo-baseline` (branch
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
- Benchmark results: `gs://kops-state-142966328212/perf-bench-results/latest/` — per-run folders (`candidate1`, `baseline2`, `candidate2`) with `RESULTS.txt`, `run.log`, `summary.json`, `metrics.jsonl.gz`; `heartbeat-*.log` refreshed every 3 min while runs are live.
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
`SKIP_E2E_SUITE=true` exists because laptop/VM pip mirrors could not install the
Python SDK deps (setuptools/duckdb unavailable on `us-python.pkg.dev` mirror);
the stress phase is pure Go and unaffected. The post-run HTML report generator
fails for the same pip reason — cosmetic; `summary.json` and stdout tables are
written before it.

## Known operator-laptop pitfalls (for local runs only — prefer the VM)

- Docker daemon is colima: `DOCKER_HOST=unix://$HOME/.colima/default/docker.sock`; the `colima` docker context had to be recreated (`docker context create colima --docker host=...`), and the buildx builder `agent-sandbox-builder` references it.
- gcloud tokens expire and hang the gcr push with `failed to authorize: DeadlineExceeded` — rerun `gcloud auth login`.
- Concurrent local runs saturate the uplink (layer-push EOFs).
