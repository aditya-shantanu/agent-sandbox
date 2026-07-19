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

### candidate1 (`perf-investigation-master`, run 2026-07-19 02:23-02:41 UTC) — COMPLETE

300/300 Ready, 0 failed, phase wall 56.5s. Client on VM (in-region):

| metric | value |
|---|---|
| create ack | p50 103ms / p90 125ms / max 144ms |
| create→Ready | p50 952ms / **p90 1489ms** / p99 5597ms / max 6735ms |
| time to ALL 300 Ready | **6.74s** (baseline1: 13.82s, but that client was WAN-inflated) |
| ready throughput | steady 210.8/s (baseline1: 45.8/s) |

**Anomaly:** ~16 claims cold-started (48 cold-launch metric observations;
baseline1 had zero). Cold pod creation (~3-6s) explains the p99/max tail.
Under investigation — with pool=300 for 300 claims, any adoption race that
burns a candidate forces a cold start.

**Measurement notes discovered on this run:**
- The cumulative `claim_controller_startup_latency_ms` histogram is polluted
  by re-records (911 warm observations for 300 claims; re-recorded durations
  grow with time since first-observed, so tail quantiles from the cumulative
  histogram exceed true end-to-end latency). Do not read absolute quantiles
  off the cumulative histogram; compare like-for-like or use windowed rate()
  as CL2 does — or rely on the in-region client-observed table, whose client
  overhead is ~100ms ack + in-region watch delivery.
- Server-side condition timestamps cannot measure warm adoption: the claim
  forwards the warm sandbox's Ready condition, whose lastTransitionTime
  predates claim creation.

### baseline2 (`perf-baseline-bench`, fresh cluster, 2026-07-19 02:41 UTC start) — COMPLETE

300/300 Ready, 0 failed, phase wall 113.8s. VM client (in-region; ack p50 147ms).

| metric | value |
|---|---|
| create→Ready | p50 2539ms / **p90 5546ms** / p99 19.1s / max 20.2s |
| time to ALL 300 Ready | 20.25s |
| ready throughput | steady 48.9/s, overall 15.0/s |

Confirms baseline1's magnitude from an in-region client (baseline1's Mac WAN
was not the story). Note the tail (p99 19.1s vs baseline1's 6.7s) — the
unoptimized tail is run-to-run unstable, consistent with conflict/backoff
storms.

**A/B so far (both VM-measured):** p90 5.55s → 1.49s (3.7×), all-ready
20.25s → 6.74s (3.0×), steady throughput 48.9/s → 210.8/s (4.3×).

### candidate2 (`perf-investigation-master`, fresh cluster, 2026-07-19 02:43 UTC start) — COMPLETE

300/300 Ready, 0 failed, phase wall 56.7s. VM client (ack p50 143ms).

| metric | value |
|---|---|
| create→Ready | p50 1057ms / **p90 1970ms** / p99 6506ms / max 6742ms |
| time to ALL 300 Ready | 6.74s |
| ready throughput | steady 147.7/s, overall 46.3/s |

Reproduces candidate1 (p90 1.49s vs 1.97s; all-ready identical 6.74s). The
p99 ~6.5s tail matches candidate1's cold-start signature — the cold-start
anomaly likely recurred (forensics in progress).

## FINAL A/B (in-region client, like-for-like; controller-internal histograms
## unusable for absolute quantiles per the re-record pollution note above)

| metric | baseline (pre-opt) | candidate (optimized, 2 runs) | gain |
|---|---|---|---|
| create→Ready p50 | 2.54s | 0.95-1.06s | ~2.5× |
| create→Ready **p90** | **5.55s** | **1.49-1.97s** | **~3.2×** |
| create→Ready p99 | 19.1s | 5.6-6.5s | ~3× |
| time to ALL 300 Ready | 20.25s | 6.74s (both runs) | 3.0× |
| steady throughput | 48.9/s | 148-211/s | 3-4.3× |

Remaining known issues driving the candidate tail: ~16 cold-started claims
per candidate run (should be 0). Round 2 targets the ~1s median and this tail.

Artifacts: `gs://kops-state-142966328212/perf-bench-results/latest/`.

---

## Reference: GKE CL2 rapid-burst (2026-07-02, different environment — ceiling, not baseline)

100 nodes, warm pool 3,300, dedicated e2-standard-32 controller node,
CL2-paced 300 QPS × 2 bursts. ControllerStartupLatency p50=154ms / p90=245ms /
p99=478ms (thresholds 300/300/500 — passed). PodStartupLatency p90=872ms.
Claim throughput max 54.2/s.

---

## 2026-07-19 — Round-2 A/B (launched, one reused cluster, both legs instrumented)

Quick-wins branch merged at 838427e (stale-pass guard, echo suppression,
cold-start deferral guard, instant key return, adoption optimistic lock,
duplicate-recompute removal, --disable-claim-events flag; see
ROUND2-FINDINGS.md). A/B on the runner VM via the reuse+smoke workflow:

- Leg A (pre-quickwins): perf-investigation-master @ 6ca3422 — creates+keeps cluster
- Leg B (+quickwins): @ 838427e — reuses the same cluster
- Both: NODE_COUNT=6, smoke 20 first, 300-claim burst, INSTRUMENT_CLUSTER=true,
  identical CONTROLLER_ARGS (debug logging + pprof + 20s replenish delay)
- Artifacts: gs://kops-state-142966328212/perf-bench-results/round2/

Results: PENDING (legs A then B, sequential).

### Leg A (pre-quickwins @ 6ca3422) — COMPLETE (cluster sandbox-20260719-042602, kept)

Smoke (20 claims, uncontended): create→Ready p50 62ms / p90 79ms — proof that
warm adoption itself is fast; everything above it is burst contention.

300-burst: p50 1107ms / p90 1610ms / p99 7654ms / all-ready 7.86s (ack p50 152ms).

Adoption segment decomposition (220 sampled "adoption timing" lines):
creation→winning-pass-entry p50 1169 / p90 1953ms (dominant; retry waves +
doomed stale passes); pop ~0.1ms; Update p50 99ms; sandbox patch p50 116ms;
status patch p50 136ms; in-pass total p50 382 / p90 551ms (writes run 3-5×
the ~30ms server commit — in-flight ceiling). Cold-start fallthroughs logged:
**183 × "warm pool queue empty"** (the stale-storm duplicate generator,
directly observed). Leg B (quickwins) pending on the same cluster.

### Leg B (+quickwins @ 838427e, same cluster) — COMPLETE, ROUND-2 VERDICT

Smoke: p50 79ms. 300-burst: p50 1490 / p90 3116 / p99 3432 / **all-ready 3.45s**.

| | Leg A (pre) | Leg B (quickwins) |
|---|---|---|
| cold-start fallthroughs | 183 | **0** |
| adoption conflicts | ~45% PUT 409 | **~0** |
| stale passes doing writes | hundreds | 0 (521 suppressed by guard) |
| p99 / max | 7.65s / 7.86s | **3.43s / 3.45s** |
| time to ALL ready | 7.86s | **3.45s (2.3×)** |
| p50 / p90 | 1107 / 1610ms | 1490 / 3116ms (regressed) |

Verdict: the quick-wins eliminate ALL pathology (cold starts, conflicts,
wasted pods, unstable tail) and cut all-ready 2.3×, at the cost of a fairer
but slower median: leg A's low median was lucky racers profiting from a storm
that starved the tail. Leg B is a clean pipeline whose bound is now visible
in the segments: write RTTs double under 300 simultaneous passes (update
110 / patch 216 / status 179ms p50; in-pass total 617ms) vs ~30ms server
commits → the ~100-stream in-flight ceiling × 3 serial writes IS the
remaining bottleneck. queueLat p50 1739ms = waiting for a pipeline slot.

Round-3 levers (design items from ROUND2-FINDINGS.md, now data-confirmed):
1. Collapse 3 writes → 2 (sandbox patch as the adoption lock).
2. Raise the in-flight ceiling (HTTP/2 transport sharding / MaxConcurrentStreams).
Both attack the 617ms in-pass cost and the pipeline depth directly.
Full-pool A vs B artifacts: gs://kops-state-142966328212/perf-bench-results/round2/
