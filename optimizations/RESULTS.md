# Benchmark Results Log

Scenario unless noted: kops GCP, 1× control plane + 6× n2-standard-8, cilium
(tuned), warm pool = 300, 300 SandboxClaims fired simultaneously
(`claims-warm` stress phase), controller flags:
`--kube-api-qps=1000 --kube-api-burst=2000 --sandbox-concurrent-workers=400
--sandbox-claim-concurrent-workers=400 --sandbox-warm-pool-concurrent-workers=1
--enable-webhook=false` (+ `--sandbox-warm-pool-replenish-delay=20s` on
candidate runs). Primary metric = controller-internal
`agent_sandbox_claim_controller_startup_latency_ms`; secondary = client-observed.

---

## 2026-07-18 13:36 PDT — baseline1 (branch `perf-baseline-bench` = main@5320421 + alpha removal + harness)

Cluster: `sandbox-20260718-133603`-era. Stress client ran on the operator's
laptop (pre-VM), so client-observed numbers include a residential WAN link.
300/300 claims Ready, 0 failed, phase wall time 92.3s.

**Controller-internal (histogram_quantile over final cumulative snapshot):**

| p50 | p90 | p99 | mean | observations |
|---|---|---|---|---|
| ~1.67s | ~8.33s | ~12.7s | 3.30s | **782** for 300 claims (re-record inflation — see ANALYSIS.md) |

**Client-observed (WAN-inflated):**

| metric | value |
|---|---|
| create ack | p50 544ms / p90 698ms / max 708ms |
| create→Ready | p50 2.38s / **p90 5.97s** / p99 6.73s / max 13.8s |
| time to ALL 300 Ready | 13.82s |
| ready throughput | steady 45.8/s, overall 21.9/s |

Artifacts: operator laptop `/tmp/bench-artifacts-baseline/stress-test/`.

---

## 2026-07-18 (evening PDT) — candidate1, then baseline2 + candidate2 (A/B verification pair)

Runner: `perf-bench-runner` VM (all client-side components in us-central1 —
client-observed numbers from here on have no WAN component and are NOT
directly comparable to baseline1's client-observed row; the controller-internal
metric is comparable across all runs).

- candidate1 (`perf-investigation-master` @ 910dd2f): PENDING
- baseline2 (`perf-baseline-bench` @ 36f8820, fresh cluster): PENDING
- candidate2 (`perf-investigation-master` @ 910dd2f, fresh cluster): PENDING

Artifacts: `gs://kops-state-142966328212/perf-bench-results/latest/`.

---

## Reference: GKE CL2 rapid-burst (2026-07-02, different environment — ceiling, not baseline)

100 nodes, warm pool 3,300, dedicated e2-standard-32 controller node,
CL2-paced 300 QPS × 2 bursts. ControllerStartupLatency p50=154ms / p90=245ms /
p99=478ms (thresholds 300/300/500 — passed). PodStartupLatency p90=872ms.
Claim throughput max 54.2/s.
