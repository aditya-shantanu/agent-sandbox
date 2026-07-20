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

## 2026-07-19 — ROUND-3 A/B VERDICT (same cluster sandbox-20260719-053456)

Leg A (round-2 stack @838427e): p50 1554 / p90 2479 / p99 2940 / all-ready 3.85s.
Leg B (round-3 @e77ad2e: watch/write split + 4-way sharding + 2-write adoption
+ APF): **p50 966 / p90 1740 / p99 2582 / max 2624 / all-ready 2.62s**,
300/300, smoke floor 57ms. Overall ready throughput 124.8/s.

Round-3 gains vs round-2: p50 −38%, p90 −30%, all-ready −32%.
**Cumulative vs original baseline: p90 5.55s→1.74s (3.2×), p99 19.1s→2.58s
(7.4×), all-ready 20.25s→2.62s (7.7×), zero cold starts / conflicts / waste.**
Segment decomposition + rest_client/APF verification: TODO from
round3/B-round3 artifacts (controller.log, metrics.jsonl.gz) — next session.
Round-4 agenda: SCALE-ROADMAP.md R4.1-R4.7 + sustained-rate phase at 500/s.

---

## 2026-07-19 — ROUND-4/5 INTEGRATED VERDICT (single image e75295a, tuned n2-standard-16 CP, TUNE_CONTROL_PLANE, client-connections=4 both legs, cluster sandbox-20260719-195815)

Leg A (round-3 flag set): ack p50 101ms; e2e p50 610 / p90 1094 / p99 1130 / all-ready 1.15s.
Leg B (+ --cache-label-selectors --disable-claim-observability-annotations --disable-claim-events):
**ack p50 49ms; e2e p50 489 / p90 584 / p99 761 / max 856 / all-ready 0.86s.** Smoke floor 31-40ms.
300/300 both legs, zero cold starts.

Attribution confirmed: round-4's seat-wall + client-calibration diagnosis was right —
same code as round-3 gained 1740→1094ms p90 from CP tuning + calibrated client alone;
the round-4/5 flags then took it to 584ms.

## ALL-ROUNDS COMPARISON (300-claim burst vs 300 warm pool)

| | p50 | p90 | p99 | all-ready |
|---|---|---|---|---|
| baseline | 2539ms | 5546ms | 19.1s | 20.25s |
| round 1 | ~1000ms* | ~1700ms* | 6.5s | 6.74s |
| round 2 | 1490ms | 3116ms | 3.4s | 3.45s |
| round 3 | 966ms | 1740ms | 2.58s | 2.62s |
| **round 4/5** | **489ms** | **584ms** | **761ms** | **0.86s** |
| vs baseline | **5.2×** | **9.5×** | **25×** | **23.5×** |

*round-1 quantiles were storm-distorted (see earlier notes).

p90 584ms sits just above round-4's 230-340ms prediction: the residual is create-ack
(49ms) + watch hops + 2 write RTTs at ~100-150ms each under burst — exactly the L1
(one-write adoption) + create-path territory forecast. Next: L1 prototype + sustained
500/s runs (multi-pool + max-refill-rate + sharding per SCALE-ROADMAP).

---

## 2026-07-20 — GATE ZERO (ROUND7-PLAN §3 + GAP-AUDIT riders) — COMPLETE (verdict below)

Launched 06:23 UTC from the v8 orchestrator (perf-bench-runner reset; scripts
in `gs://kops-state-142966328212/perf-bench-scripts/`, canonical copies in
`optimizations/infra/`). Image/code: `perf-investigation-master` @ **2b60967**
(includes the pre-run GAP fixes: GAP-4 pool-delete UID+RV preconditions,
GAP-2/7 guaranteed controller pod + system-cluster-critical + GOMEMLIMIT=6GiB,
GAP-5 SDK watch resourceVersion fix, and the TUNE_NODES bench knob).

One cluster, three sequential legs, all with **CLEAN instrumentation**
(GAP-1: no `--zap-log-level=debug`, no `--zap-encoder=json`, no
`--enable-pprof-debug`, no client qps/burst pins → binary default -1, no
apiserver `-v=3`), `--profile-controller=false --client-connections=4` on the
stress client, 20-claim smoke before every leg:

- Cluster: NODE_COUNT=34 (n2-standard-8), CP n2-standard-16,
  TUNE_CONTROL_PLANE=true, **TUNE_NODES=true** (new: eventTTL=15m, kubelet
  systemReserved 2CPU/8Gi + podPidsLimit=4096, 200GB pd-ssd worker boot
  disks — SCALING-GUIDE-NOTES §4).
- **Leg A (CLEAN-A)** — burst-300 on the round-4/5 flag set (workers
  400/400/1, webhook off, separate-watch-connection, api-connections=4,
  cache-label-selectors, claim observability annotations+events off,
  replenish-delay=20s). Purpose: re-anchor the 584ms headline without the
  instrumentation tax; the clean smoke floor re-measures the ROUND7 §7
  physics floor.
- **Leg B (CLEAN-B)** — same + `--one-write-adoption
  --sandbox-write-behind-window=250ms --no-spec-adoption`. Purpose: first
  benchmark of the fully-composed tree (the §1.2 predictions have never been
  measured); A→B isolates the composed-flag delta on a clean anchor.
- **Leg SUST** — `claims-warm-sustained` only: 300/s Poisson × 60s across 4
  namespaces, replenish-delay=0, `--sandbox-warm-pool-max-refill-rate=100`,
  pool workers=4, dwell=1500ms, pool-headroom=5s. Purpose: the sustained-300
  number the <100ms p90 target is actually stated against, plus the GAP-3
  teardown-decomposition ride-along (dwell→delete ≈ 300 deletes/s).
  Capacity per the tool's own formula: pool 4×ceil(75×5)=1500 +
  in-flight ceil(300×(1.5+5))=1950 → 3450 ≤ spare ≈ 34×110 − ~72 system
  pods ≈ 3668. The originally-specified headroom 6s + dwell 2s computes to
  3900 > spare — exactly the round-6 capacity refusal — hence the
  adjustment. Cluster deleted after this leg.

Results land in `gs://kops-state-142966328212/perf-bench-results/gate0/`
(`STATUS.txt` + `hb-*` heartbeats every 3 min; per-leg folders
`A-clean-burst/`, `B-clean-composed/`, `S-sustained300/` with RESULTS.txt,
run.log, summary.json, metrics.jsonl.gz).

## 2026-07-20 — GATE-ZERO VERDICT (cluster sandbox-20260720-062747, image 2b60967)

All three legs ran to measurement completion on one cluster (06:23-06:59 UTC;
the rc=1 exits were cosmetic post-measurement failures: leg A/B report
generation ran past the leg timeout, and leg S's `generate_report.py` crashed
on a watch.jsonl.gz schema wrinkle — see the leg-S artifact-gap note below).
Cluster deleted cleanly.

### Legs A and B — burst-300, both clean, both 300/300 with zero failures

| | smoke p50/p90 | ack p50 | e2e p50 | e2e p90 | e2e p99 | max | all-ready |
|---|---|---|---|---|---|---|---|
| **A** (round-4/5 flag set) | 41 / 44ms | 48ms | 522ms | 1000ms | 1082ms | 1170ms | 1.18s |
| **B** (+ one-write-adoption, write-behind 250ms, no-spec-adoption) | **19 / 21ms** | 32ms | **273ms** | **301ms** | **309ms** | **312ms** | **0.31s** |
| A→B gain | 2.1× | 1.5× | 1.9× | **3.3×** | 3.5× | 3.8× | 3.8× |

The composed flags' first-ever benchmark delivered exactly the L1 forecast
(round-7 §1.2 predicted p90 ~205-345ms): **the A→B delta is a clean 3.3× on
p90 with a collapsed tail** (p99−p50 spread: A 560ms → B 36ms — the pipeline
is now conflict-free AND wave-free). Leg B's max (312ms) is under leg A's p50.

**Honest numbers vs history (all in-region client, like-for-like):**

| | p50 | p90 | p99 | all-ready |
|---|---|---|---|---|
| baseline | 2539ms | 5546ms | 19.1s | 20.25s |
| round 4/5 (instrumented) | 489ms | 584ms | 761ms | 0.86s |
| **gate-zero B (clean, composed)** | **273ms** | **301ms** | **309ms** | **0.31s** |
| vs baseline | **9.3×** | **18.4×** | **62×** | **65×** |

**GAP-1 confirmed:** the clean smoke floor is **14-21ms** (leg B: min 14 /
p50 19 / p90 21ms; leg S smoke: min 13 / p50 22ms) — LOWER than the 25-40ms
floor assumed from instrumented runs (31-40ms measured with debug logging +
pprof + JSON encoder). Instrumentation was overstating both the floor and the
burst numbers; every historical number above the gate-zero row carries that
tax. The uncontended adoption path is ~20ms of physics, and the 300-burst
p50 (273ms) is now within ~250ms of it.

Leg B validated with zero failures, zero cold starts, and a 3.3× composed
gain → per the standing decision the five flags flipped to default-on in
`cmd/agent-sandbox-controller/main.go` (commit "perf: flip validated
optimization defaults on (gate-zero leg B)").

### Leg S — sustained 300/s × 60s: FAILED (supply-side collapse) + diagnosis

Headline: 18,000 requested → 17,891 created (298.2/s achieved), only **3,804
ready, 14,117 failed** — every failure a 300s per-claim timeout, zero create
failures. e2e for the "ready" cohort: p50 64s / p90 261s. Phase wall 444s.

**What the artifacts show** (summary.json, run.log, watch.jsonl.gz,
sandboxes.jsonl; forensics 2026-07-20):

1. **The API server and cluster were healthy throughout.** Create ack p50
   8ms / p99 87ms at a steady 298/s; pod scheduling p50 21ms; pod start
   0.9-1.5s. Total sandboxes ever created: 5,812 (≈5,770 pods) — the
   3,450-pod capacity envelope was never approached. **No capacity
   exhaustion, no cold-start flood** (the claim cold-start fallback fired
   only 147 times, all minutes into the drain).
2. **The 1,500 pooled sandboxes drained in ~5s** (window [0,10s): 2,522 of
   3,086 arrivals served, p50 137ms — warm hits are fast even at 300/s).
3. **Pool refill then collapsed to ~2-3 creates/s per pool** — 30-50× below
   the configured `--sandbox-warm-pool-max-refill-rate=100` per-pool cap and
   ~10× below the ~70-85/s isolated per-pool ceiling measured in round 6.
   Refill sandbox ADDED rate per 30s after the drain: 379, 262, 289, 202,
   198, 180... (~8-13/s aggregate across 4 pools). Pods for those sandboxes
   started promptly once created — the bottleneck was purely the rate at
   which the warm-pool controller issued creates. Supply ~10/s vs demand
   300/s → the backlog (peak ~14k pending claims) could never clear.
4. **Adoption double-booking at scale:** of 5,457 sandboxes that claims
   bound, **3,204 were bound by TWO claims** (2 by three). 6,410 claims
   raced onto shared sandboxes; 2,096 won; **~4,300 losers stayed wedged
   bound-but-never-Ready until their 300s timeout** (their recorded binding
   never changed after the winner's dwell-delete removed the sandbox — the
   409-steal rebind path either didn't fire or couldn't keep up). The
   remaining 9,228 failures never bound at all (pool empty). Double-booking
   also wasted ~half of the scarce refill supply.
5. **Root cause (mechanism):** demand starved supply inside the controller.
   A growing pending-claim backlog kept 400 claim workers in adoption
   retry/rebinding storms (double-booking is the direct evidence) while the
   4-worker pool controller — which must re-list and re-filter every
   pool-labeled member each reconcile, including hundreds of
   adopted-but-not-yet-deleted ones — got a shrinking share of CPU/write
   budget; its refill create rate decayed as the backlog grew (52/s in the
   first 30s → ~7/s by t=90s). The refill token bucket was never the
   limiter; it was starved upstream of it. The original hypothesis (refill
   ready-rate < drain rate → pool exhaustion) is confirmed, but the
   magnitude is far worse than the 70-85/s ceiling math predicted, and the
   mechanism is controller-internal contention — NOT pod cold-start
   capacity.
6. **Artifact gaps that hurt:** `controller.log` in the S folder is 0 bytes
   (so the "Pacing pool replenishment" / deficit log lines that would nail
   the starvation point are missing), and `generate_report.py` died on a
   duckdb JSON-schema error over watch.jsonl.gz (`unknown key "sandbox"`)
   before emitting the capacity/APF/throttling sections. Fix both before
   the next sustained run.

**What the next sustained attempt needs:**

- **Refill feasibility math first:** sustain requires
  `pools × contended-per-pool-refill-READY-rate ≥ arrival rate` — using the
  CONTENDED rate, which this run measured at ~2-3/s/pool, not the isolated
  70-85/s. Until the contention is fixed, no pool count is feasible at
  300/s; fix supply isolation first, then re-measure the contended rate at
  ~100/s before attempting 300/s again (ramp legs: 100 → 200 → 300/s).
- **Isolate supply from demand:** dedicated API connection + worker/APF
  budget for warm-pool refill; bound the claim controller's empty-pool
  retry storm (rate-limited requeue while the pool is empty instead of hot
  adoption passes); reconsider 400 claim workers (contention beat
  parallelism here).
- **Fix the wedged-loser bug:** a claim bound to a sandbox that was stolen
  or deleted must unbind and re-enter adoption (this alone was ~4.3k of the
  14.1k failures); add an adoption reservation so two workers can't select
  the same candidate during the write-behind/one-write visibility window.
- **Bigger shock absorber:** pool replicas sized ≥ R × (refill_p99 +
  headroom) against the contended refill rate, and/or more, smaller pools
  so per-pool list/filter work stays bounded.
- **Harness:** capture controller.log reliably, fix the report generator's
  watch-stream schema handling, and add a live refill-rate gauge to the
  progress line so collapse is visible at +30s instead of post-mortem.

## 2026-07-20 — ROUND 8: leg-S root causes fixed, sustained-300 relaunched (PENDING)

Status: **PENDING** — sustained-300 rerun (single leg `S2-sustained300`)
relaunched on `perf-bench-runner` (startup marker v9, orchestrator pinned to
`d1ddac3`) against a fresh 34-node tuned cluster, same capacity math as leg S
(pool 1500 + in-flight 1950 = 3450 ≤ spare ≈ 3668). Results land in
`gs://kops-state-142966328212/perf-bench-results/round8/` (`STATUS.txt`,
`hb-*` heartbeats every 3 min, `S2-sustained300/` leg folder). Commits:
`650bc0e` (double-bind + wedged-loser fix), `c9bcc7c` (pool starvation fix),
`d1ddac3` (artifact-gap fixes).

### Root cause 1 — L1 one-write double-bind (the 3,204 double-bound sandboxes)

The queue pop WAS exclusive; the leak was a **re-add during the async
window**. Under exhaustion, refill sandboxes are adoptable at CREATE (labels
+ pool ownerRef; `isAdoptable` does not require Ready), so backlogged claims
pop them within milliseconds of the sandbox CREATE event — before the pod is
scheduled. The pod-scheduling `status.nodeName` write then fires
`sandboxEventHandler.Update` with `newAdoptable && nodeScheduled`, and the
handler **re-Added the popped key**: with one-write adoption the sandbox
still looks pool-owned/adoptable in every cache view until the async patch
lands, so a second claim popped it, passed `verifySandboxCandidate` against
the same stale view, and status-bound it too. A second amplifier lived in
the flusher: its post-409 "fresh read" went through the **informer cache**,
so under the watch backlog a genuine steal re-read as "still pool-owned" →
"benign conflict" → 5 rebuilt-stale attempts → attempts-exhausted path
**re-added the already-adopted candidate to the queue** (the triple-bound
sandboxes). The losers then wedged because `recoverLostAdoption` also read
the claim through the lagging cache, concluded its own status write "hadn't
appeared yet", exhausted its 10×100ms budget and gave up — leaving the
binding in place — while each subsequent reconcile pass misclassified as
crash-window recovery and fired a doomed optimistic-lock patch every 50ms
(the 409 storm), and the cold-start guard deferred against **reserved
phantom members** that could never materialize.

Fix (`650bc0e`): (a) queue pops now **reserve** the key — `Add` drops
reserved keys, give-backs go through `Release`, ghosts/deletes through
`Forget`, and reservations survive terminal adoption outcomes until the
sandbox DELETE event, so no watch event can re-queue an in-transaction or
adopted sandbox; (b) the flusher re-verify and `recoverLostAdoption` use a
**DirectReader** (`mgr.GetAPIReader()`) — recovery decisions never depend on
informer convergence and the loser's binding clears in one RTT; (c)
crash-window conflicts reclassify from a direct read in the same pass (no
more 409-storm loop); (d) `shouldDeferColdStart` excludes reserved members.
Regression tests reproduce both leg-S signatures pre-fix (double-bind via
the nodeScheduled re-add; wedged loser under a permanently stale claim
cache) and pass post-fix; `-race` clean.

### Root cause 2 — pool-controller starvation (2-3 creates/s vs 100/s cap)

Two compounding mechanisms, both demand-side pressure on shared resources —
the token bucket was never the limiter and (by design review) neither was
APF: refill creates ride `agent-sandbox-bulk` (~322 seats at tuned server
limits) while the claim-side sandbox patches ride `agent-sandbox-critical`,
so the observed queueing had to be **client-side, upstream of APF**
(leg-S flowcontrol metrics were unavailable — controller.log empty + report
crash, both fixed). (i) The pool's refill POSTs shared the 4 round-robin
HTTP/2 write shards with 400 claim workers; at backlog the claim side kept
hundreds of streams in flight (409 storm + status patches + cleanup
patches), so each pool create waited behind them, stretching each
`slowStartBatch` pass to tens of seconds — 100 granted tokens / ~40s ≈ the
measured 2-3 creates/s. (ii) The empty-pool claim polling was flat 50ms:
~14k pending claims × 20 polls/s ≈ 280k attempted reconciles/s of pure
spinning (each with an O(pool-members) cache scan), starving the 4 pool
workers of CPU and workqueue turns — the refill decay (52/s → 7/s) tracks
the backlog growth exactly.

Fix (`c9bcc7c`): (i) `--pool-dedicated-connection` (default on) gives pool
member creates/deletes their own HTTP/2 connection (the
`newIsolatedHTTPClient` mechanism `--separate-watch-connection` already
uses); (ii) backlog-aware polling — cold-start deferral requeues now grow
from 50ms to a 500ms cap with consecutive deferrals
(`coldStartDeferralRequeueDelay`), cutting per-claim empty-pool poll load
10× exactly when the pool needs the headroom. Note the fixes compound:
root-cause-1's reservation also stops double-binding from wasting ~half the
scarce refill supply, and killing the 409 storm removes most of the
competing write traffic.

### Artifact gaps (fixed in `d1ddac3`)

- `controller.log` 0 bytes in leg S: `kubectl logs deployment/... || true`
  resolves ONE pod and silently produces an empty file when that pod is
  evicted/failed (the controller runs BestEffort — GAP-2's exact hazard).
  Capture is now per-pod (+ `--previous`), with kubectl stderr preserved in
  `capture-errors.log` and a loud warning when the concatenated
  `controller.log` is empty.
- `generate_report.py` duckdb crash (`unknown key "sandbox"`):
  `read_json_auto` inferred a nested STRUCT for the watch record's `object`
  from a sample that never saw the sustained phase's `sandboxclaims` events
  (`status.sandbox`). The watch scans now pin the envelope schema and read
  `object` as raw JSON (`ignore_errors=true` for truncated tails) — any
  resource shape is tolerated.

Success criteria for the rerun: zero double-bound sandboxes
(`sandboxes.jsonl` forensics), zero claims wedged >10s after losing a
candidate, per-pool contended refill ≥ the ~70-85/s isolated ceiling (or a
measured new contended number to feed the §"refill feasibility math"), and
failed-claim count ≈ 0 at 300/s × 60s. If refill still can't sustain 300/s
aggregate, the next lever is the planned ramp (100 → 200 → 300/s) to measure
the contended per-pool rate before re-attempting 300.
