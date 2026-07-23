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

## 2026-07-20 — ROUND 8: leg-S root causes fixed, sustained-300 rerun (COMPLETE — verdict below)

Sustained-300 rerun (single leg `S2-sustained300`) ran on `perf-bench-runner`
(startup marker v9, orchestrator pinned to `d1ddac3`) against a fresh 34-node
tuned cluster `sandbox-20260720-155728`, same capacity math as leg S
(pool 1500 + in-flight 1950 = 3450 ≤ spare ≈ 3668). STATUS: **DONE rc=0,
ROUND8 COMPLETE**; cluster deleted cleanly (run.log `Deleted cluster:
"sandbox-20260720-155728"`; GCE instance/network/address lists verified
empty post-run). Artifacts:
`gs://kops-state-142966328212/perf-bench-results/round8/` (`STATUS.txt`,
`hb-*` heartbeats, `S2-sustained300/` leg folder). Commits:
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

## 2026-07-20 — ROUND-8 VERDICT (leg S2-sustained300, cluster sandbox-20260720-155728, image d1ddac3)

Smoke on the fixed tree (20 claims, uncontended): ack p50 10ms,
create→Ready **p50 21ms / p90 22ms / max 23ms** — the clean floor is intact
after the round-8 fixes.

**Sustained 300/s × 60s (4 pools × 375, refill cap 100/pool, dwell 1.5s):**

| metric | value |
|---|---|
| requested / created / ready / failed | 18,000 / 17,822 / **15,821** / 2,006 (521.0s phase wall) |
| arrival rate held | 296.7/s overall, 296.8/s steady (Poisson target 300/s) |
| create ack | p50 **39ms** / p90 194ms / p99 573ms / max 1143ms |
| create→Ready (ready cohort) | p50 141.0s / p90 246.6s / p99 277.0s |
| ready throughput | overall 46.8/s, best60s **65.7/s**, best10s 152/s |

Rolling 10s windows by arrival time (create→Ready):

| window | arrivals | ready | p50 | p90 |
|---|---|---|---|---|
| [0-10s) | 2,956 | 2,956 | **119ms** | 176.2s |
| [10-20s) | 2,999 | 2,999 | 67.2s | 76.5s |
| [20-30s) | 2,970 | 2,681 | 98.3s | 133.7s |
| [30-40s) | 2,893 | 2,335 | 153.5s | 169.3s |
| [40-50s) | 2,993 | 1,847 | 207.8s | 220.8s |
| [50-60s) | 3,011 | 3,003 | 246.9s | 274.9s |

The pool lasted ~5-10s at 300/s; the first window's p50 119ms is the claim
path performing at rate while supply existed. Every later window is
queueing behind refill, not adoption cost.

**Success criteria — forensics (sandboxes.jsonl, all 17,822 bindings):**

- **Double-bound sandboxes: 0 of 17,822** (leg S: 3,204 of 5,457). The
  reservation + DirectReader fix (`650bc0e`) is verified at full scale.
  Controller-log counters concur: `DOUBLE-BIND` 0, `wedged` 0, `ONE-WRITE
  ADOPTION LOST` 0. (Caveat: the per-pod controller log capture — the
  `d1ddac3` fix worked, `capture-errors.log` clean — still only retained the
  final ~17s / 9,758 lines of the run at default kubectl limits; the
  sandboxes.jsonl forensic is the definitive zero, the log counters are
  corroboration. Next harness tweak: `--tail=-1` + since-start capture.)
- **Wedged losers: 0.** All 2,006 failures were 300s per-claim timeouts of
  claims **uniquely bound** to a sandbox whose pod never reached Ready in
  time — pure supply starvation. Zero never-bound failures (the leg-S
  empty-pool storm is gone), zero bindings that changed or pointed at
  deleted sandboxes.
- **Contended refill rate (timeseries.jsonl, aggregate pod creates):**
  183/s in [10-30s), 156/s in [30-60s), 103/s in [60-120s), best-10s
  259.9/s — i.e. **~26-46 creates/s per pool contended** vs leg S's 2-3/s
  (**15-60× recovery**; `c9bcc7c` verified). Below the 70-85/s isolated
  ceiling but no longer the binding constraint: creates outran Ready 3-4×
  throughout.

**Where the time went (segment decomposition, summary.json):** ack p50
39ms → sandbox-create→pod-create p50 18.7s (refill queue) →
**pod-create→scheduled p50 129.0s / p90 206.5s** (the wall) →
scheduled→running p50 1.1s → pod-Ready→sandbox-Ready p50 0.1s. The
scheduler/node pipeline, not the API server (ack healthy throughout) and
not the controller (double-bind-free, refill flowing), absorbed the queue.

**Fix effectiveness — old leg S vs S2, same scenario and capacity math:**

| | leg S (gate zero) | S2 (round 8) |
|---|---|---|
| completion | 3,804/18,000 (**21%**), rc=1 | 15,821/17,822 created (**89%**), rc=0 |
| double-bound sandboxes | 3,204 | **0** |
| wedged losers | ~4,300 | **0** |
| never-bound failures | 9,228 | **0** |
| contended refill creates | ~2-3/s per pool | ~26-46/s per pool |
| ready throughput | ~10/s decaying | 47/s overall / 65.7/s best60s |
| create ack p50/p99 | 8ms / 87ms | 39ms / 573ms (healthy under 3-4× more live churn) |

**Verdict.** The round-8 fixes eliminated the catastrophic failure mode
entirely — no double-binding, no wedging, no starvation collapse, clean
exit. What remains is **supply-side and empirical**: sustaining 300/s
needs 300 pods/s reaching Ready, and a 34-node pipeline delivers ~50-65
ready/s (~1.5-2 pods/s/node) — the SCALE-ROADMAP §1.4 churn wall,
now confirmed with numbers. The three recorded paths forward:

1. **Nodes-scale:** at ~1.5-2 ready/s per node, 300/s sustained implies
   ~150-200 nodes plus scheduler QPS raises (§1.4) — a deployment-sizing
   line item, not an engineering unknown.
2. **Per-node ready-rate levers:** the node-local I/O wall work
   (PRs #1203-#1208: containerd fdatasync, overlayfs-volatile, NVMe),
   cilium client QPS, kubelet tuning — raises ready/s/node so the node
   count shrinks.
3. **L6 recycling (SCALE-ROADMAP):** decouple claim rate from pod churn
   entirely — the only path where sustained high rates don't imply a
   large pod pipeline; still gated on the security decision memo.

**Definitive investigation scoreboard:**

- **Burst-300: SOLVED.** p90 301ms, 18.4× vs baseline, all-ready 0.31s,
  zero failures/cold-starts/conflicts (gate-zero leg B, clean).
- **Sustained-300 claim path: PROVEN.** 119ms p50 at 300/s while pool
  supply lasted; ack p50 39ms holding 296.7/s for 60s; zero controller
  pathology at 14k-claim backlog.
- **Sustained-300 end-to-end: SUPPLY-BOUND.** Beyond pool depth it is a
  pods-to-Ready throughput problem (cluster sizing per paths 1-3 above),
  not a controller defect. Feasibility rule confirmed:
  `nodes × per-node-ready-rate ≥ arrival rate` once the pool drains.

---

## 2026-07-20 — ROUND 9b: sustained-300 with supply ≥ demand (node-scale leg SUST3) — PENDING

Goal: make sustained 300/s DEMONSTRABLE for the full 60s window (supply ≥
demand) and quantify the supply ceiling. Single leg `SUST3-sustained300`,
v10 orchestrator (v9 archived), fresh cluster, controller code held at the
round-8-verified tree (no controller changes in this round — 9a owns those).

**Supply math that sized the config (new forensics on round-8 artifacts):**

- `timeseries.jsonl` shows **livePods peaked at 12,549 pod objects against
  3,740 slots** — the 34-node cluster was slot-saturated with a ~9k Pending
  queue. The measured ~47-65 ready/s was **slot turnover**
  (slots ÷ residence, with residence inflated to ~60-75s by the
  delete-pipeline backlog), NOT a kubelet/scheduler per-node pipeline limit:
  scheduled→running held p50 1.1s while nodes absorbed ~4.4 pod starts/s
  each whenever slots freed. Node/slot count therefore scales the ceiling
  ~linearly; the "1.5-2 ready/s/node" figure was an artifact of the
  collapsed regime, and §1.4's 150-200-node estimate is the
  worst-case-pessimistic bound, not the expectation.
- Healthy-regime slot demand at 300/s: pool 3,000 + 300/s × ~10s residence
  (start 1.1s + ready + dwell 1.5s + delete pipeline + termination grace 1s)
  ≈ **6,000 slots**; 150 workers provide ~16,180 spare slots (2.7×
  headroom against residence inflation).
- **Quota reality check (us-central1, measured 2026-07-20):** N2_CPUS
  7,708/8,000 used — the true reason every round ran 34 workers; ~292 free
  N2 vCPUs cannot host a node-scale leg at all. CPUS (E2/N1 pool)
  3,386/10,000 → 150 × e2-standard-8 = 1,200 vCPUs fits; SSD_TOTAL_GB
  163,251/184,305 → 200GB pd-ssd × 150 (30TB) does NOT fit, 100GB (15TB)
  does; IN_USE_ADDRESSES 328/575 → 151 more fits. Hence two new run-script
  knobs: `NODE_MACHINE_TYPE` (e2-standard-8 here) and `NODE_VOLUME_SIZE`
  (100). Cost ≈ $45/h all-in, ~2.5h ≈ ~$115/run.
- Refill per R4.6 / the MaxRefillRate sizing comment: pools ≥
  ceil(300 / 70-85 isolated per-pool rate) = 5; run **8 pools** (8
  namespaces × 375 replicas, headroom 10s) × cap 100/s, pool workers = 8 —
  aggregate cap 800/s, expected ≥560/s isolated, ≥208-368/s even at
  round-8's worst-case contended 26-46/s/pool, vs 300/s demand.
- Capacity-check formula (tool refuses otherwise): pool 8×ceil(37.5×10s) =
  3,000 + in-flight ceil(300×(1.5s+5)) = 1,950 → 4,950 ≤ spare ≈ 150×110 −
  ~320 system ≈ 16,180 (31%, under the 90% warning line).

Config: NODE_COUNT=150 NODE_MACHINE_TYPE=e2-standard-8 NODE_VOLUME_SIZE=100,
CP n2-standard-16, TUNE_CONTROL_PLANE + TUNE_NODES, composed defaults on,
clean instrumentation (GAP-1), smoke 20 first;
`--sustained-rate=300 --sustained-seconds=60 --sustained-namespaces=8
--claim-dwell=1500ms --sustained-pool-headroom=10s`; controller args as
round-8 v9 with `--sandbox-warm-pool-concurrent-workers=8`.

Success criteria: ≥95% of ~18,000 ready; steady-state ready throughput
≥295/s; rolling-window p90s flat across the 60s (no degradation cliff);
plus a measured supply-ceiling estimate (slot residence, refill margin,
per-node rates) for SCALE-ROADMAP §1.4.

Artifacts: `gs://kops-state-142966328212/perf-bench-results/round9b/`.
Results: COMPLETE — SUST3 verdict below (supply wall found: untuned scheduler).

## 2026-07-20 — ROUND 9a: claim-path items landed (code complete; measurement rides 9b) — PENDING

Goal (PATH-TO-100MS §3.2): sustained-300 claim-path p90 from the measured
95-96ms (S2 warm-hit cohort; p50 41ms) to 60-80ms. All four items + one
read-path closer landed on `perf-investigation-master`; upstream PR #1118
re-checked before touching the claim controller: **still OPEN** (no rebase
required; the completeAdoption/applyAdoptionMutations split is preserved so
re-expression on #1118's structure stays mechanical).

| item | commit | expected on sustained p90 | notes |
|---|---|---|---|
| claim-side no-spec adoption (ROUND6 §3.3; task 12 claim half) | `a3b61bd` | **−5 to −15ms** + −1 sandbox status write & −1 spec-bearing reconcile per adoption system-wide | metadata-only adoption patch when additionalPodMetadata is empty; drift check modulo system-reserved keys (no back-door bump); KEP-0174 path byte-for-byte; one-write async + crash recovery pinned metadata-only; same `--no-spec-adoption` flag (default on) drives both halves |
| create-ack riders (item 3) | `b1b3337` | holds S0 at floor (verification); remaining −10-15ms rides CBOR + CP headroom | leg-B scrape decomposition: server 22.7ms = etcd ~11.6ms + handler/encode ~10ms + admission ~0.7ms; APF wait ~0; payloads thin (POST body 203B) ⇒ spec-trim rider DEAD; implemented APF exempt-PL preflight (dry-run + PF headers → summary.json `apfVerification`) and client-connections calibration warning |
| CBOR A/B wiring (item 4) | `ab0db41` | **−5 to −10ms** (S0/S1/S4 encode; S3 stays JSON) | `TUNE_CBOR=true`: apiserver CBORServingAndStorage at creation + client env gates on controller Deployment and harness; both traps documented (verify content-type on the wire; no CBOR merge-patch) |
| adoption segment histograms (measurement rider a) | `ccd7712` | decision data | `agent_sandbox_claim_adoption_segment_latency_ms{segment=queue_wait\|sandbox_patch\|status_write\|annotation_flush\|total\|async_queue_wait\|async_patch}` — not V(1)-gated; clean legs self-decompose (no more RV-interleave forensics) |
| sharded session claims watch (SUB-FLOOR 5b) | `26f0898` | brackets B2's S4 share | `--per-namespace-claim-watch` (default off): one watch stream per claims namespace incl. sustained `-s1..sN`; splits the ~0.7-0.9ms/event per-stream backlog |

Read-path follow-up NOT built (too large for this round): 3′ router-held
first request (needs a SandboxClaim informer + claim-header path in
`sandbox-router`); tracked in UPSTREAM-ISSUE-DRAFT remaining work.

Validation: build/vet clean; full unit suite green including `-race`
(extensions/controllers, controllers, internal/*, test/stress, cmd);
cluster-backed e2e excluded (no cluster in the dev loop). New tests:
`nospec_adoption_test.go` (6 invariants), `apfcheck_test.go`.

**For the 9b combined run:** controller flags unchanged (no-spec claim half
rides the existing default-on `--no-spec-adoption`); optional stress flags
`--per-namespace-claim-watch` (recommended with `--sustained-namespaces=8`)
and env `TUNE_CBOR=true` for the CBOR leg (fresh cluster only; remember the
wire-format verification before attributing deltas). Projection with items
composed: sustained-300 p90 ≈ **60-80ms** (PATH-TO-100MS §3.2).
Results: PENDING (measured by the 9b leg on this tree).

## 2026-07-20 — ROUND-9B SUST3 VERDICT (cluster sandbox-20260720-181027, 150× e2-standard-8, image 5c49ed1): supply wall FOUND — it was never the nodes

Run 18:06-18:39 UTC end-to-end (VM reset → cluster up → smoke → sustained →
report → cluster deleted; verified `DONE rc=0`). Smoke: ack p50 7ms,
create→Ready p50 19 / p90 20ms — floor intact on E2 nodes.

**Sustained 300/s × 60s (8 pools × 375, refill cap 100/pool, workers 8,
dwell 1.5s):** 18,000 requested / 17,837 created (296.8/s held) / 17,205
ready / 881 failed (594s wall). Ready throughput **47.4/s overall, best60s
75.7/s** — on a cluster with 16,500 pod slots and ~16,180 spare.

| window (arrival) | n | ready | p50 | p90 |
|---|---|---|---|---|
| [0-10s) | 2,987 | 2,987 | **56ms** | **122ms** |
| [10-20s) | 2,985 | 2,985 | 160.5s | 203.1s |
| [30-40s) | 2,936 | 2,936 | 182.5s | 204.3s |
| [50-60s) | 3,054 | 2,496 | 281.4s | 299.4s |

**The demonstration failed for a reason that changes the roadmap:** ready
throughput on 150 nodes (47.4/75.7) is statistically identical to round-8's
34 nodes (46.8/65.7). The supply ceiling is **node-count-independent**.
Segment + scheduler-metrics forensics:

- `podCreatedToScheduled` p50 **183.5s / min 98.5s** (n=11,119) with ~16k
  slots free the whole run; `scheduledToPodRunning` p50 1.0s (nodes bored).
- kube-scheduler `schedule_attempts` rate plateaued at **47-50/s** exactly;
  `rest_client_rate_limiter_duration_seconds` (POST) climbed to **100+s
  mean throttle wait** — the scheduler's own CLIENT-SIDE rate limiter.
- **Root cause (kops v1.35 source):** `cluster.spec.kubeScheduler.kubeAPIQPS`
  carries only a `flag:"kube-api-qps"` tag — a CLI flag modern
  kube-scheduler does not consume; the fields kops maps into the generated
  `KubeSchedulerConfiguration.clientConnection` are **`qps`/`burst`**
  (componentconfig.go:798-800, kubescheduler/model.go MapToUnstructured).
  TUNE_CONTROL_PLANE therefore NEVER tuned the scheduler: every cluster
  since round 4/5 ran it at the componentconfig default **50 QPS/100
  burst** = the measured 47-50 binds/s wall. (KCM's `kubeAPIQPS` IS a real
  KCM flag; only the scheduler key was broken.) Fixed in the run script:
  `kubeScheduler.qps=800` / `burst=1600`.
- Corollary: round-8's "~1.5-2 ready/s/node churn wall" and SCALE-ROADMAP
  §1.4's "150-200 nodes for 300/s" were **misattributed** — the per-node
  pipeline demonstrated ~4.4 starts/s/node (round 8) and 1.0s
  scheduled→running here; §1.4's own warning that the scheduler at default
  QPS "is the refill ceiling all by itself" was the correct prediction all
  along, hidden by the silently-inert kops key.
- What held at rate: arrival 296.8/s for 60s; ack p50 33ms at 300/s + full
  churn; first-window (pool-backed) create→Ready **p50 56 / p90 122ms** at
  a true 300/s; refill creates ~143/s average during the pile-up (above
  demand-share until the scheduler backlog swamped everything); zero
  double-binds, zero wedged losers (round-8 fixes hold at a second full-scale
  run). 881 failures = 300s per-claim timeouts, pure scheduler-queue
  starvation, zero controller pathology.

150-node E2 sizing/quota notes for the record: N2_CPUS 7,708/8,000 used
forced e2-standard-8 (CPUS pool); SSD_TOTAL_GB forced 100GB pd-ssd boot
disks; quota preflight added to the v10 orchestrator; bring-up→teardown of
the 150-node cluster fit in a 33-minute wall clock.

## 2026-07-20 — ROUND 9b SUST4: rerun with the scheduler actually tuned (v11, image 0b92308 + sched-fix) — COMPLETE (verdict below)

Same 150× e2-standard-8 shape as SUST3, plus: **kubeScheduler.qps=800/
burst=1600 applied via the fixed keys**; pinned at the 9a tip so the leg
carries claim-side no-spec adoption, segment histograms, create-ack
riders, and `--per-namespace-claim-watch` (8 shards, pairs with
`--sustained-namespaces=8`). Conditional second leg SUST5-cbor
(`TUNE_CBOR=true`, fresh cluster) runs only if SUST4 passes its criteria —
CBOR claim-path deltas are unreadable under a supply collapse.
`apfVerification` in summary.json to be checked (harness client must ride
an exempt PL). Success criteria unchanged from the SUST3 design entry.
Results: COMPLETE — round-9b verdict below.

## 2026-07-20 — ROUND-9B VERDICT (SUST4 cluster sandbox-20260720-195719 @ 8a57aa7; SUST5-cbor aborted pre-cluster): scheduler wall peeled; next walls measured; 300/s NOT yet demonstrable

Smoke on the 9a tree (claim-side no-spec aboard): ack p50 8ms, create→Ready
p50 21 / p90 23 / max 23ms — floor intact.

**SUST4 vs SUST3 (identical 150× e2-standard-8 / 16,500-slot shape, 8×375
pools, same arrival process; deltas: kubeScheduler.qps=800/burst=1600
actually applied, pin 8a57aa7 = 9a items + `--per-namespace-claim-watch`):**

| | SUST3 (sched @ default 50 QPS) | SUST4 (sched @ 800 QPS) |
|---|---|---|
| created / ready / failed | 17,837 / 17,205 / 881 | 17,958 / **17,595 (97.7%)** / 492 |
| arrival rate held | 296.8/s | 297.8/s |
| ready throughput (overall / steady / best60s / best10s) | 47.4 / 42.8 / 75.7 / 296.6 | 45.9 / **78.6** / **109.5** / 303.2 |
| scheduler bind rate | 47-50/s plateau; rl-POST wait >100s mean | **104-222/s** while etcd healthy; throttle gone |
| pod-create→scheduled p50 | 183.5s (min 98.5s) | **0.82s** (p90 2.2s) |
| claim→pod-created p50/p90 | 20.3s / 35.5s | 41.9s / 75.3s |
| pod-Ready→sandbox-Ready p50/p90 | 14ms / 4.5s | **50.5s / 57.8s** |
| first-window [0-10s) p50/p90 | 56 / 122ms | 124 / 304ms |
| etcd main DB | survived | **137MB→2,028MB in 10.5min → NOSPACE**, binds→0 at 20:15, cleanup delete failed |
| window p90 trend | 122ms → 299s | 304ms → 232s |

**Success criteria:** ≥95% ready **PASS** (97.7%); steady ready ≥295/s
**FAIL** (78.6/s steady, 109.5 best60s; 303/s only while pool-backed);
flat rolling p90s **FAIL** (monotonic degradation after pool drain);
supply ceiling **MEASURED** (below). Zero double-binds / wedges again
(third full-scale confirmation).

**The supply-wall ladder, now measured layer by layer (the round-9b
deliverable):**

1. ~~Nodes/slots~~ — exonerated: 150 nodes idled in both legs
   (scheduled→running p50 1.0-2.3s; ≤2 starts/s/node vs ≥4.4 demonstrated).
2. ~~kube-scheduler client QPS~~ — the 47-50/s wall of rounds 8 + SUST3;
   peeled by the kops key fix (`kubeScheduler.qps`, commit 8a57aa7). Binds
   reached 222/s burst.
3. **Controller supply pipelines ≈ 100-150/s aggregate** (the current
   binding constraint): refill issuance (claim→pod-created p50 41.9s under
   backlog) and sandbox-Ready marking (pod-Ready→sandbox-Ready p50 50.5s
   at ~13k live pods). Ready throughput steady 78.6/s / best60s 109.5/s.
   Next levers: controller sharding (R4.4/task 19), refill/ready-marking
   profiling; NOT more nodes.
4. **etcd default quota-backend-bytes (2GiB) — a hard *duration* wall:**
   ~3.2MB/s of main-DB revision growth at ~21k pod + 18k sandbox + 18k
   claim churn ⇒ NOSPACE in ~10 min; even a fixed 100/s run dies in ~30
   min. Any future sustained leg MUST raise ETCD_QUOTA_BACKEND_BYTES
   (8GiB+) and set `--etcd-compaction-interval=2m` (R4.5 item, never
   applied), and should consider the L4 pods/CR-group etcd split.

Caveat for 9a: the first-window regression (56/122 → 124/304ms) and ack
p50 33→118ms are **confounded** — scheduler at 800 QPS adds concurrent
bind+event write load from t=0 and etcd bloat raises write latency
run-wide; do NOT read a claim-path regression (or the no-spec/per-ns-watch
delta) from SUST3 vs SUST4.

**SUST5-cbor: aborted before cluster creation, zero cost, zero leak.**
`kops create cluster --set
cluster.spec.kubeAPIServer.featureGates.CBORServingAndStorage=true` fails
with "field not found in *Cluster" — kops v1.35 `--set` cannot address
featureGates map keys (same limitation as kubelet.systemReserved; the
TUNE_CBOR comment's contrary claim was wrong). Fix for 9a: patch the
cluster yaml between `kops create cluster` and `kops update cluster --yes`
(the systemReserved pattern in the same script).

**Cost & path recommendation (with the L6 comparison, per the mandate):**

- Measured cost of a 150-node supply leg: ~$45/h all-in; 33-37 min
  bring-up→teardown ≈ **~$25-28/leg**. Standing 150-node churn cluster:
  ~$32k/mo — and it does NOT deliver 300/s (walls 3-4 are control-plane).
- Corrected node sizing for 300/s churn: slots needed ≈ pool 3,000 + 300/s
  × ~10s healthy residence ≈ 6,000 ⇒ **~60-80 e2-standard-8 nodes
  (~$12-16k/mo)** once walls 3-4 fall — half the §1.4 estimate; nodes were
  never the cost driver.
- **L6 recycling comparison:** scrub-and-reuse removes pod churn from
  steady state: no scheduler binds, no refill pipeline, etcd revision
  growth ~0 instead of 3.2MB/s, ~6 writes/claim instead of 20-25. Cluster
  size = concurrent live sandboxes = R × mean lifetime (at 300/s: 60s
  lifetimes ⇒ ~18k pods ≈ 165 nodes — same iron as churn but with a
  healthy control plane; short bench-like lifetimes ⇒ ~25-30 nodes,
  ~$5-6k/mo). It remains the ONLY path where sustained rate decouples from
  control-plane write ceilings; still gated on the security memo.
- **Do not relaunch supply legs until**: (a) etcd quota/compaction knobs
  are in the run script, (b) the ~100-150/s controller pipeline ceiling is
  profiled (sharding per R4.4 is the standing hypothesis). Then a 60-80
  node leg (~half the cost of SUST3/4) is the right next attempt.

Artifacts: `gs://kops-state-142966328212/perf-bench-results/round9b/`
(SUST3, SUST4 complete with reports; SUST5-cbor run.log only). All three
clusters verified deleted (GCE instances/networks/addresses: zero
`sandbox-2026*` remaining).

---

## 2026-07-20 — ROUND 10: sustained-300 with every measured wall addressed (v12 orchestrator, pin 273d9d7) — PENDING

Design (per the 9b "do not relaunch until" gate — both conditions now met):

- **Phase-0 doc audit** committed (f67c4d5): 9b ladder propagated to
  PATH-TO-100MS/GAP-AUDIT/ROUND7-PLAN/UPSTREAM-ISSUE-DRAFT (+ the kops
  inert-`kubeAPIQPS` bug recorded as its own upstream-filing item — no
  existing kubernetes/kops issue found 2026-07-20).
- **Pre-run fixes** (273d9d7): (a) TUNE_CONTROL_PLANE now applies
  ETCD_QUOTA_BACKEND_BYTES=8GiB + 2m periodic auto-compaction to both etcd
  clusters via a `kops get -o json`→jq→`kops replace` cluster-spec patch
  (no kops spec field exists for the apiserver's
  `--etcd-compaction-interval`, so compaction is enforced etcd-side);
  validated by a state-store round-trip + `--target=terraform` render
  (env present in both etcd-manager manifests); hard-fails if dropped.
  (b) TUNE_CBOR server gate moved to the same patch mechanism (the SUST5
  `--set` failure). (c) `agent_sandbox_supply_segment_latency_ms{segment=
  pool_member_create|sandbox_create_to_pod_create|pod_ready_to_sandbox_ready}`
  so the ~100-150/s supply-pipeline ceiling decomposes from the standard
  scrape. (d) `SHARD_B_NAMESPACES` run-script knob: clones the deployed
  controller as `agent-sandbox-controller-shard2` with its own
  `--watch-namespaces` list (sharding-example recipe; distinct
  auto-derived leader Lease).
- **Cluster (one, reused):** 70× e2-standard-8 workers (corrected 9b
  sizing; spare ≈ 7,550 slots vs 4,950 needed = 65%), CP n2-standard-16,
  TUNE_CONTROL_PLANE + TUNE_NODES + new etcd knobs, clean instrumentation.
- **Leg A (SUST300-single):** 300/s × 60s, 8 namespaces × 375-replica
  pools, refill cap 100/pool, workers 8, dwell 1.5s, headroom 10s,
  `--per-namespace-claim-watch`, single controller. Success: ≥95% of
  ~18,000 ready; steady ready ≥295/s; flat window p90s <100ms.
- **Leg B (SUST300-shard2, only if A fails its criteria = controller
  pipeline ceiling binding):** same cluster, 2 controller shards
  (`--watch-namespaces` r10b-s1..4 / SHARD_B_NAMESPACES r10b-s5..8),
  `--namespace=r10b`, no smoke (smoke ns outside shard lists) — the
  sharding-at-rate proof for ladder item 3.
- **Leg C (SUST300-cbor, only if A or B passed):** TUNE_CBOR=true repeat
  of the winner on a FRESH cluster (CBOR server gate is creation-time —
  a reused-cluster CBOR leg measures nothing). Wire-format verification
  required before attributing deltas.
- Budget: ~$60-90 (leg ≈ $15-20 at ~$22-24/h for this shape); one retry
  per leg on transient pre-measurement infra failure, nothing else.
- **Regression rider (leg A analysis):** compute BOTH warm-hit cohort
  definitions — (a) arrivals [0,3s) matching the S2 methodology, (b) full
  first 10s window — and compare S2/SUST3/SUST4/round-10 with per-run
  load context (bind rate during the window); attribute any delta via the
  segment histograms; state a definitive claim-path regression verdict.

Artifacts: `gs://kops-state-142966328212/perf-bench-results/round10/`.
Results: COMPLETE — verdict below.

## 2026-07-20 — ROUND-10 VERDICT (cluster sandbox-20260720-225023, 70× e2-standard-8, pin 273d9d7, run 22:46-23:34 UTC): etcd wall RESOLVED; sharding FALSIFIED at 300/s; the supply wall is apiserver CPU

Orchestrator v12 ran fully autonomously: quota preflight OK → leg A →
(criteria failed) → leg B on the same cluster → leg C correctly skipped
(no winner) → cluster deleted → `gce-instance-leaks=0` (independently
re-verified: zero `sandbox-2026*` instances/networks/addresses). Wall
clock 48 min; ~43 cluster-minutes ≈ **~$17 total** (vs $60-90 budget).
The new cluster-spec patch applied and verified on the real run
("Cluster-spec patch verified."; ETCD env in both etcd-manager manifests).
Smoke floor intact: ack p50 7-8ms, create→Ready p50 21 / p90 24ms.

**Leg A (single controller, all TUNE knobs incl. etcd 8GiB + 2m compaction):**
18,151 created (298.0/s held) / **14,456 ready / 4,112 failed**; ready
throughput overall 39.2/s, best60s 79.7/s, best10s 310/s. Ack p50 105ms
run-wide (22ms in the first window). **FAIL** on ≥95%/≥295-per-s criteria.

| window (arrival) | n | ready | p50 | p90 |
|---|---|---|---|---|
| [0-10s) | 3,062 | **3,062** | **72ms** | **190ms** |
| [10-20s) | 3,058 | 2,849 | 36.3s | 52.9s |
| [30-40s) | 2,996 | 2,406 | 206.7s | 292.8s |
| [50-60s) | 2,984 | 1,237 | 241.9s | 267.3s |

**Leg B (2 shards via --watch-namespaces + SHARD_B_NAMESPACES, 4 ns each,
same cluster):** shard2 deployed and rolled out; work split evenly (ready
per namespace 1,525-1,676 across all 8). 17,899 created / **12,865 ready /
5,972 failed** — **WORSE than leg A** (ready best60s 65.9 vs 79.7/s;
first window p50 64 / p90 333ms; ack p50 137ms). Zero double-binds in
36,050 bindings across both legs (4th/5th full-scale confirmation).

**The round-10 finding — the supply-wall ladder, re-attributed with the
new instrumentation:**

1. **etcd 2GiB quota (ladder wall 4): RESOLVED.** With 8GiB +
   2m periodic auto-compaction, two back-to-back 10.6-min full-churn runs
   (~36k sandboxes + ~36k claims + ~33k pods each) completed rc=0 with
   working deletes (created=deleted) and no NOSPACE signature — SUST4
   died at exactly this duration. The knobs are now table stakes.
2. **"Controller supply pipelines" (ladder wall 3): RE-ATTRIBUTED — it
   was never the controller process.** The supply-segment histograms
   decompose it: refill CREATE RTT is healthy (`pool_member_create` p50
   146-217ms), but `sandbox_create_to_pod_create` (p50 4.8s in the first
   window → 46s late) and `pod_ready_to_sandbox_ready` (p50 9-16s, p90
   →73s) absorb everything. **Sharding did NOT move them**: per-shard
   with HALF the namespaces each, leg B measured the SAME or worse
   per-process lags (14-15s early, 70-73s late) and aggregate pod-create
   issuance identical to leg A (~6.5k in the first 63s both legs) — the
   lag is not process-CPU; it is queueing against a shared resource.
3. **The shared resource is control-plane CPU: kube-apiserver measured
   at ~12.8 of 16 cores (1280%) during the phase** (in-burst pprof, leg
   A), ~40% of samples in GC (mallocgc/scanobject/findObject) driven by
   allocation in encode paths, `encoding/json` visibly hot. Every
   supply-pipeline stage (watch fan-out to controller+KCM+scheduler,
   pod/sandbox status writes, deletes) queues on this box. A second
   controller adds watch/encode fan-out and write concurrency → leg B
   made the CP MORE saturated → worse. This is SCALE-ROADMAP §1.1's
   "~25 cores at 1000/s" arriving early: at 300/s CHURN (~20-25
   cluster-wide writes/claim + ~33k pod objects/leg + 12-13k live pod
   objects at peak) one n2-standard-16 apiserver is the wall.
4. Slot saturation is the SECONDARY effect: livePods peaked 12.6-13.3k
   pod OBJECTS vs 7,553 slots (podCreatedToScheduled p50 72.7s = Pending
   queue, scheduledToPodRunning p50 1.7s = nodes still bored). It is
   downstream of (3): pods pile up because the delete pipeline lags
   through the saturated CP. Scheduler attempts ran 50-150/s
   unthrottled — wall 2 (kops QPS key) stays peeled.

**Regression rider — sustained claim-path regression verdict: NO
(quantified; cohort definition + load regime fully account for it).**
Matched-cohort comparison across runs:

| run (code, sched, etcd, nodes) | [0,3s) cohort p50/p90 | first-10s window p50/p90 | load during window |
|---|---|---|---|
| S2 round-8 (pre-9a, sched@50-inert, 2GiB, 34n) | 38 / 96ms | 119ms p50 (incl. pool cliff) | binds capped 47-50/s; ack p50 11-14ms |
| SUST3 (pre-9a, sched@50-inert, 2GiB, 150n) | — | 56 / 122ms | binds capped ~50/s |
| SUST4 (9a tree, sched@800, 2GiB→NOSPACE, 150n) | — | 124 / 304ms | binds 104-222/s + etcd degrading from t=0 |
| **R10-A (9a tree + etcd knobs, sched@800, 70n)** | **72 / 195ms** | **72 / 190ms** (all 3,062 ready, every one <2s) | sched attempts 52-72/s; refill creates ~34/s; ack p50 22ms |

Segment attribution (adoption histograms, leg A, first-21s delta,
n=5,330): `queue_wait` p50 **0.21ms** / p90 70ms — the claim controller
is NOT backlogged; `status_write` p50 **39.8ms / p90 372ms** vs the
~15ms healthy server-side PATCH — the entire first-window elevation is
the one critical write's RTT (+ watch fan-out) through a CP that is
simultaneously absorbing supply churn. Three independent confirmations
of no code regression: (a) same 9a tree, etcd fixed: SUST4 124/304 →
R10-A 72/190 (code constant, environment moved); (b) smoke floor
identical pre/post-9a (21-23ms); (c) S2's famous 41/95 was the [0,3s)
warm-hit cohort at near-zero concurrent churn — its matched R10 cohort
is 72/195 with the delta living in `status_write` RTT, not in any
adoption segment the 9a items touched. (Side note: one-write
`async_queue_wait` p50 reached 2.2s under churn — the deferred sandbox
patch lags but never gates Ready; by design.)

**Scoreboard after round 10:**

- Sustained-300 flat-window <100ms p90: **NOT DEMONSTRATED.** Even the
  supply-present first window is 190ms p90 in this regime — the claim
  path <100ms at 300/s needs CP headroom, not more claim-path code.
- Walls peeled so far: scheduler QPS (9b) ✅, etcd quota/compaction
  (10) ✅, nodes exonerated (9b) ✅, controller-process supply ceiling
  **falsified** (10) ✅. **Standing wall: apiserver CPU at 300/s churn.**
- Controller correctness at scale: 5 consecutive full-scale runs with
  zero double-binds/wedges.

**What remains for 300 → 500/1000 (recommended order):**

1. **CP headroom leg:** n2-standard-32 control plane (R4.5's original
   spec; ~$0.9/h delta) — the single cheapest test of wall 3. Pair with
   **CBOR (L5)**: the pprof shows exactly its target (JSON encode + GC
   from encode allocs); the TUNE_CBOR wiring is now fixed and unused.
   Expected: both first-window p90 <100ms and a higher ready plateau.
2. **Multi-apiserver / write-domain sharding (L4)** if one 32-core box
   still saturates — controller sharding composes with it (shards pinned
   to different apiservers), which is where leg B's topology becomes
   useful; it is NOT useful against a single shared apiserver.
3. **Churn reduction beats churn service:** L6 recycling remains the
   only path where sustained rate decouples from CP write ceilings
   (~6 writes/claim, no pod churn) — **the security decision memo is now
   the critical path to 500/1000**, since round 10 proves the ceiling is
   CP-capacity-per-write, and 500-1000/s multiplies exactly that.
4. Cheap bankable demo: a 100-150/s leg inside the measured ceiling for
   a full-window <100ms p90 result (PATH-TO-100MS §3.3 option).

Artifacts: `gs://kops-state-142966328212/perf-bench-results/round10/`
(A-sust300-single/, B-sust300-shard2/ with RESULTS.txt, summary.json
incl. apfVerification=exempt, sandboxes.jsonl, timeseries.jsonl,
metrics.jsonl.gz with both shards' scrapes, apiserver pprof, reports;
STATUS.txt + heartbeats). Cluster deleted; zero leaks.

---

## 2026-07-22 — ROUND 1256 ADJUDICATION (3-arm: BASE 4357fa4 / PR#1256-v1 160e00f / optimistic-lock PROTO 8b5408f) — rep1 + rep2s COMPLETE; PROTO isolation leg PENDING

Full write-up: `ROUND-1256-ADJUDICATION.md`. Identical tuned 12-node kops
clusters, e2-standard-16 CP, identical harness. Zero failures in every
arm/phase.

**Rep1 (burst-300 + fill + mif50/100 + sustained 45/s × 60s):**

| arm | burst p50/p90 (all-ready) | sustained 45/s p50/p90 | mif50/100 p50 |
|---|---|---|---|
| BASE | 1082/2096ms (2.44s) | 108/236ms | 1064/1144ms |
| ARM-1256 (v1) | 838/1660ms (1.91s) | **727/4177ms** (p99 6.1s, monotonic degradation, 46 cold starts, 23,249 wq retries) | 1136/1220ms |
| PROTO | 904/2019ms (2.65s) | 113/261ms | 890/904ms |

#940 inflation: BASE 4503 obs/2955 claims (+52%; 333 vs 320 on the burst
label); ARM-1256 ~exact; PROTO exact on every label. Byte-identical-patch
watch probe: **0 identical MODIFIED events in ALL arms** — apiserver no-op
short-circuit confirmed (k8s.io/apiserver v0.36.2 etcd3
`store.go:553-576`). Retry fingerprints: BASE conflict-scattered 0-8s;
ARM-1256 fixed-requeue churn (57/72 gaps <1s on only 19 conflicts);
PROTO optimistic-409 + per-item exponential 5-11s stragglers.

**Rep2s (sustained-only control + manual chaos; clusters reused,
controllers 11h/0 restarts):**

- ARM-1256's sustained collapse VANISHES without a preceding burst
  (windows 118/288/109/106/129ms, 0 cold starts) → rep1 collapse is
  preceding-burst debris in the guard machinery (matches SV-P4's clean
  192ms, also burst-free).
- Manual chaos, 5 status wipes/arm landed: BASE repaired 5/5 in 18-52ms;
  PROTO 5/5 in 144-381ms; **ARM-1256 0/5** (no restore events in the
  115-1445ms dwell-bounded windows; 7 watch events/target vs
  8-with-restore elsewhere) — the lastWrittenStatusMap write-suppression
  signature, **empirically confirming igooch's self-healing objection**.
  Annotation-removal prediction ungraded (all targets NotFound-raced by
  the 2s dwell; re-run rides the isolation leg).
- #940 independent leg: BASE 4232/2680 = **1.58×** (reproducible);
  ARM-1256 2635/2635 = 1.000; PROTO 2627/2664 = 0.986 (37 claims deleted
  pre-sample during backlog).

**OPEN (no conclusion recorded):** PROTO rep2s sustained-only ramp — p50
637→2089ms from window 2, p99 12.3s, 2,043 claim retries, beginning
BEFORE the +26s wipes; inverse of its clean rep1. Isolation leg running
(fresh cluster+deploy; burst+sustained then sustained-after-idle; no
chaos; dedicated long-lived claims for annotation grading). Candidate
attributions held open: stale-reuse conditions vs intrinsic
(suspect: APIReader uncached-GET retry feedback).

> **RESOLVED same day (isolation-leg RCA):** the ramp is an
> assignment-flip amplification loop — completeAdoption 404/steal on a
> stale candidate view caused the pass to overwrite the already-committed
> adoption annotation with the next candidate (340 flips leg B vs 0 leg A;
> 323 status rebinds; 74 Ready regressions; sandbox-controller error churn
> 10,880 PUT 409 + 8,200 PUT 404). APIReader GETs (432) and claim
> workqueue (1,064) exonerated as suspected. Fixed statelessly on
> `proto-1256-optimistic-status` @ `ecdba21` (adoption-patch optimistic
> lock + authoritative resolveAdoptionCompletion; terminal cleanup of dead
> references; no in-pass rebinding). Full RCA:
> `ROUND-1256-ADJUDICATION.md` resolved section.

**Interim verdict:** BASE's #940 inflation reproducible (1.52-1.58×);
ARM-1256 v1 fixes #940 exactly but fails self-healing 0/5 and collapses
post-burst under sustained load — not shippable as designed; PROTO
matches the #940 fix, self-heals 5/5, and is comparable-or-better on
every latency leg, gated only on the open isolation item. PR #1256
rework to the optimistic-lock design proceeds.

Ops note: the adjarm chaos_loop wedge (9.6h held `wait` in all arms),
v21 artifact recovery, v22 loop bounding, and the kops instance-name
deletion pitfall are recorded in `INFRASTRUCTURE.md`. Sidebar P2SUST leg
(replenish delay=0 / rate=100 at sustained 45/s): p50 92ms / p90 182ms
flat windows — posted as the two-knob comment on PR #1251. All 3
clusters reused across reps then deleted (verified 0 IGMs/state/instances).

Artifacts:
`gs://kops-state-142966328212/perf-bench-results/round-1256-adjudication/<ARM>/{rep1,rep2s}/`.

## 2026-07-22 — PR #1256 REWORK SHIPPED: v24 final validation gates all 6 PASS (fresh cluster + deploy of `ecdba21`; rebased and force-pushed as PR head `2588c32`)

All 6 v24 gates PASS (full table: `ROUND-1256-ADJUDICATION.md`, v24
section). 0 failed claims in every phase of every run:

- **#940 exactly-once:** rework 3,022 samples / 3,022 claims, exact on
  every label (revalidation); independently 2,773 samples for exactly
  2,740+20+8+5 accountable events in a second leg. BASE over-records
  reproducibly: 4,503/2,955 (+52%) and 4,232/2,680 (1.58×), incl. 333
  samples for the 320-claim burst label.
- **Burst-300 parity:** 1097/2394/2571ms p50/p90/p99, all-300 in 2.63s
  (revalidation) vs BASE 1082/2096/2401ms, 2.44s (3-arm run) — within
  cross-cluster noise; correctness PR, not a burst speedup.
- **Sustained parity:** 45/s × 60s p50/p90 BASE 108/236ms → rework
  108/221ms; second back-to-back run 115/262ms, flat 92-139ms windows,
  no post-burst degradation (v1's 23k-retry collapse absent).
- **Watch probe:** 0 byte-identical MODIFIED events across all runs
  (normalized minus resourceVersion/managedFields) — server-side
  short-circuit confirmed; the PR's wins are client-side + non-identical
  stale overwrites.
- **Chaos self-healing:** 20 external status wipes across runs, zero
  restarts — BASE repairs 18-52ms, rework 19-77ms, v1 memory-guard 0/5
  (never) — igooch's objection empirically confirmed.
- **Retry fingerprint:** rework drops optimistic 409s benignly with
  workqueue-rate-limited retry; assignment-flip loop eliminated with
  regression tests.

**Shipped:** rebase onto upstream main `9dcbe62` (one conflict in
`sandboxclaim_controller.go` — #1114's error-returning
`recordCreationLatencyMetric` + one-shot annotation composed with the
`statusAuthoritative` gate, both kept); full validation green
(build/vet/gofmt/lint-go 0 issues/unit+race). Force-pushed to
`upstream-p4-claim-write-suppression` → PR #1256 head `2588c32`; PR
retitled ("fix(sandboxclaim): duplicate startup-latency metrics and
stale-status overwrites via optimistic-locked status writes") with the
final body (Fixes #940); igooch reply posted
(pull/1256#issuecomment-5051235861); CodeRabbit V(4) and Copilot
ordering threads answered and resolved. /hold stays until igooch
reviews the rework.

## 2026-07-22 — PR #1252 THREE-WAY A/B (immediate / flusher / RequeueAfter): flusher has a churn-collapse mode; RequeueAfter ADOPTED, flusher RETIRED

Direct comparison justinsb asked for on the PR: three legs on ONE fresh
tuned 12-node kops cluster (redeploy only between legs; e2-standard-16
CP; each leg: 300-claim warm burst + 45/s × 60s sustained Poisson churn,
2s claim dwell, 0 failed claims everywhere). Arms: `window=0` immediate
(control), the PR's `internal/writebehind` flusher at 250ms, and the
RequeueAfter deferral variant (`p6-requeueafter-variant` @ `b81b504`) at
250ms. Posted verbatim:
[pull/1252#issuecomment-5052059674](https://github.com/kubernetes-sigs/agent-sandbox/pull/1252#issuecomment-5052059674).

| | immediate (window=0) | write-behind flusher (250ms) | **RequeueAfter (250ms)** |
|---|---|---|---|
| burst p50/p90 | 1465/3201ms | **1049/2243ms** | 1271/3158ms |
| sustained p50/p90 | **320/680ms** | 2139/7530ms (windows 399→4068ms, monotonic) | 507/1027ms (no collapse) |
| pod metadata PATCHes | 2,953 ok | **181 ok + 3,072 hit 404** | 2,997 ok |
| total controller writes | 48,624 | 64,823 (+33%) | 49,836 |
| optimistic 409s | 5,447 | 7,877 | **3,077 (fewest)** |
| sandbox reconciles | 53,462 | 55,641 | 58,819 (+10% — the "extra pass per deferred write" cost, confirmed and absorbed) |

**Finding — flusher churn-collapse mode:** under sustained churn the
flusher's deferred writes routinely fire AFTER their objects are deleted
(2s dwell < in-flight lifetime + window backlog): the intended write
reduction inverts into write amplification (181 useful patches vs 3,072
404s; +33% total writes) plus a monotonic latency spiral. The
RequeueAfter form kills the failure mode structurally — the flush pass
re-reads current state before writing, so a deleted/changed object
produces no patch — at a measured cost of ~+10% reconcile passes (and it
recorded the FEWEST 409s of the three arms). Honest residual case for
the flusher: it won the burst leg (1049/2243ms) — burst-heavy,
long-lived-sandbox profiles — but that upside is not worth a mechanism
with a collapse mode.

**SV-P6 reconciliation:** SV-P6's earlier clean sustained leg for the
flusher does NOT contradict this — that run had no preceding burst, the
same preceding-churn trigger pattern as the ARM-1256 rep1 collapse
(clean in rep2s burst-free control, collapsed after a burst). The
flusher's pathology needs in-flight deferred writes racing deletions;
a from-idle sustained leg never builds that backlog.

**Decision + rework SHIPPED (user-approved):** PR #1252 reworked to the
RequeueAfter mechanism. New single-commit series on upstream main
`9dcbe62` (variant was already based on it; no conflicts): kept
`controllers/writebehind_requeue.go` (deferredWriteClock, timestamp-only
— no mutation payloads), `SandboxReconciler.WriteBehindWindow` deferral
+ RequeueAfter return in `sandbox_controller.go`, same
`--sandbox-write-behind-window` flag in `main.go` (no flusher), tests
incl. the readiness-never-gated pin and crash recovery
(`TestRequeueDeferralCoalescesAdoptionPodPatch`,
`TestRequeueDeferralCrashRecovery`, `TestWriteBehindDisabledIsSynchronous`).
REMOVED: the entire `internal/writebehind` package (+tests) and
DESIGN-NOTE.md (content moved to the PR body). Validation green
(build/vet/gofmt/unit+race/lint-go 0 issues). Force-pushed → PR head
`e5a83ea`; PR retitled ("perf(sandbox): opt-in coalescing of recoverable
metadata writes via RequeueAfter deferral") with new body (three-way
table + "Why not the flusher", credits justinsb's RequeueAfter
suggestion); reply posted in his `writebehind.go` thread
([discussion_r3634195788](https://github.com/kubernetes-sigs/agent-sandbox/pull/1252#discussion_r3634195788)),
left unresolved for his call.

## 2026-07-22 — ISSUE #1215 FIX SHIPPED (PR #1266): warm-pool over-creation — expectations gate + terminating counting + unschedulable hold + self-scheduled grace requeue; v26 null repro / v27 decisive

**Issue:** [kubernetes-sigs/agent-sandbox#1215](https://github.com/kubernetes-sigs/agent-sandbox/issues/1215)
— SandboxWarmPool over-creates replicas (reporter: ~5x pods vs Σ spec.replicas,
cluster-wide scheduler/apiserver overload). tomergee's log-confirmed RCA:
create-path read-after-write — `toCreate = replicas − len(cachedSandboxes)`
computed off the informer cache, which lags the controller's own just-issued
creates; every create's watch event re-enqueues the pool while the cache is
still stale. workers=1000 amplifies. NOT the 5-min grace GC loop (that fired
once in the runaway window vs ≥5,000 creates).

**Fix (branch `fix-1215-warmpool-expectations`, PR
[#1266](https://github.com/kubernetes-sigs/agent-sandbox/pull/1266), head
`788b90b` on upstream `dcd2fb4`):**
1. Self-contained ReplicaSet-style expectations tracker
   (`extensions/controllers/warmpool_expectations.go`): counted creates
   recorded pre-issue, UID-tracked deletes, watch-observed lowering via a
   wrapping `Watches` handler (replaces `Owns`), 5-min timeout fallback,
   ATOMIC check-and-record create gate (stricter than kube — race-free even
   under overlapping reconciles).
2. Hard invariant: creates gated on active + terminating-still-present
   population; pool can never overshoot spec.replicas while deletes drain.
   Consequence: delete-and-replace defers one watch round-trip.
3. Unschedulable-aware stuck GC: PodScheduled=False/Unschedulable → hold +
   1-min requeue instead of delete-and-replace (was an unbounded churn loop
   under capacity shortfall).
4. Self-scheduled post-grace evaluation: reconciles seeing young not-Ready
   sandboxes return RequeueAfter = earliest grace deadline (+2s slack),
   min-composed with other requeues. Fixes a LATENT UPSTREAM DEFECT the
   quiet-cluster forensics exposed: the 5-min stuck GC only ever fired on
   ambient traffic (pod FailedScheduling events never touch Sandbox objects;
   next guaranteed reconcile ~10h resync).
5. WarmPoolNotProgressing (Warning) / WarmPoolProgressing (Normal) transition
   Events (status has no conditions field; CRD schema change deliberately out
   of scope — flagged as additive follow-up).

**v26 (null repro, diagnostic; quiet cluster, 500×replicas:1, fast image):**
reproduced NEITHER symptom — the amplifier is cache lag under load. But E2
exposed the reachability gap above: pool settled Ready=False got NO
post-grace evaluation for 12+ min; first cut's NotProgressing event was
empirically unreachable → self-scheduled requeue added (commit `37fea85`,
now `5bb457d` post-rebase).

**v27 (decisive; kops/GCE k8s v1.35.6, 12-node tuned; artifacts
`gs://kops-state-142966328212/perf-bench-results/round-1215-repro/v27/`):**

E1 — 20 pools × replicas:25 (500 target), cold python-runtime-sandbox,
workers=1000:

| metric | BEFORE (main 9dcbe62) | AFTER (37fea85) |
|---|---|---|
| controller sandbox POSTs | 5,537 (~5.5×) | 1,004 = 502+502 EXACT |
| peak concurrent sandboxes | 624 (+24.8%) | 502 |
| peak pods | 1,230 (2.46×) | 502 |
| events | Scheduled 1,153 / Killing 292 / Failed 854 / FailedScheduling 1,453 | Killing 2 |
| watch stream | churn | 502 ADDED / 2 DELETED (2 legitimate replacements) |
| all-ready | +48s | +19s |

E2 — 1 pool replicas:3 cpu:64 (unschedulable): BEFORE stable until single
post-grace touch at +367s → delete all 3 + recreate, cadence re-arms per
trigger (real clusters: ambient traffic). AFTER: zero deletes, same 3 UIDs
for 420s, exactly ONE WarmPoolNotProgressing Warning at +5m02s with NO
external trigger.

**Status:** PR [#1266](https://github.com/kubernetes-sigs/agent-sandbox/pull/1266)
filed 2026-07-22 (MERGEABLE, base dcd2fb4); issue comment crediting
tomergee's RCA + headline numbers:
[#1215 (comment)](https://github.com/kubernetes-sigs/agent-sandbox/issues/1215#issuecomment-5053997976).
Unit coverage: laggingClient stale-cache repro (proof: gate disabled → 15
creates for replicas=3), concurrent -race, terminating-counting,
unschedulable hold + exactly-once events, fake-clock quiet-cluster
walk-through. All non-e2e -race green, lint-go 0 issues. Semantic deltas to
watch in review: replace-on-next-pass (stuck GC / Recreate rollouts), Owns →
Watches wrapper.

---

## 2026-07-23 — Y16 APF-INSULATION A/B (overlay off/on; stock + tuned inflight) — COMPLETE; PR #1270 FILED

Setup: kops, 12 worker nodes, k8s v1.35.6, controller at upstream main
`dcd2fb4`, 45 claims/s sustained workload; overlay =
`examples/apf-insulation/apf-insulation.yaml` (branch
`upstream-y16-apf-insulation` @ `e55e2b3`).

**Routing verified (same-cluster A/B, stock limits):** controller traffic
moved from `workload-low` (75.0k reqs, demand HWM 161 seats) to
`agent-sandbox-critical` (51.0k, HWM 86) + `agent-sandbox-bulk` (21.1k,
HWM 52); `workload-low` left with events only (3.4k). APF wait p99 ≤20ms
on all levels; 0 rejections.

**Neutrality:** `system` / `workload-high` / `leader-election` /
`node-high` all held wait p50+p99 ≤5ms, demand unchanged, 0 rejections.

**Sizing rule confirmed (second cluster, mutating inflight 1000):**
critical level ≈516 seats vs demand HWM 115 — the `>=2x` rule holds with
~4.5x headroom; wait p99 ≤5ms every level in both configs; 0 rejections.

**HONEST FINDING — no latency delta from the overlay in either
configuration** (APF waits ~0 in all legs). WHY this differs from
round-4/5's p90 1740→1094ms: that win came from raising the SERVER
inflight limits when the pre-optimization controller (several times more
writes per claim) saturated the 600-seat pool (272-seat demand HWM vs 77
seats, wait p99 359ms). The current optimized write volumes measure an
86-seat critical HWM at 45/s and never queue at stock limits — the seat
wall the overlay+limits combination once removed no longer exists at
typical rates. The overlay's standing value is isolation + guaranteed
claim-path capacity under multi-tenant contention and higher-rate
regimes; branch docs and PR body were reframed accordingly (`e55e2b3`)
before filing, with the historical numbers kept only as clearly-labeled
context motivating the sizing rule.

PR: https://github.com/kubernetes-sigs/agent-sandbox/pull/1270 (shipped
body archived in `pr-drafts/Y16.md`).

## 2026-07-23 — PR #1266 adversarial proof pass (v31): 6/6 pre-registered expectations PASS

Arms: BEFORE = main 95380d9 (warm-pool controller byte-identical to 9dcbe62);
AFTER = merge 9f7c649 (PR head 0d0aa75 onto 95380d9 — ship simulation). One
fresh tuned 12-node cluster, redeploy between arms.

- E1 over-creation (20x25, heavy image, workers=1000): BEFORE peak 597
  sandboxes / 1,292 pods (2.58x), 3,799 POSTs (~3.8x); AFTER 1,002 POSTs
  exact (501+501), peak 500/501, 500 + exactly 1 legitimate replacement,
  all-ready +19s vs +28s. PASS both directions.
- E2 unschedulable hold: BEFORE churned on the first post-grace touch (3
  deleted + 3 recreated); AFTER untouched 540s: 0 deletes, one
  WarmPoolNotProgressing at +5m14.3s (inside the jittered 5m02s-5m30s
  window), self-triggered. PASS.
- E3 jitter herd (30 pools simultaneous): exactly 30 events, 0 duplicates,
  spread 138.3s near-uniform — no synchronized spike. PASS.
- E4 phantom-delete stall (6->3 raced with out-of-band deletes, 3x):
  reconvergence <=~0.5s each, no expectation stall. PASS x3.

Evidence comment posted: PR #1266 issuecomment-5064242380. Artifacts:
gs://kops-state-142966328212/perf-bench-results/round-1266-proof/.
