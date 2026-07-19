# ROUND4-FINDINGS — leg-B attribution, round-4 quick wins, and the L1 decision

Round-4 session against round-3 leg B (`perf-investigation-master` @ e77ad2e: p50 966 / p90 1740 / p99 2582 / all-ready 2.62s, 300-claim burst vs 300 warm, smoke floor 57-87ms). Forensics inputs: `gs://…/round3/B-round3/stress-test/{watch.jsonl.gz, metrics.jsonl.gz, controller.log, sandboxes.jsonl, pprof-controller-claims-warm-*.pprof}` (downloaded to `/tmp/r4/`, analysis scripts `/tmp/r4/{analyze,metrics_analyze,hist_analyze}.py`). Code: branch `perf-investigation-master-round4` (pushed, NOT merged), worktree `/tmp/perf-r4`.

## 1. Phase-1 attribution: where 966ms p50 / 1740ms p90 actually live

Pool was 300/300 Ready before the first claim landed (no fill overlap). All 300 adoptions warm, `attempts=1`, zero candidate rejections, zero 409s — the round-2/3 pathology stayed dead. 55 "Deferring cold start" guard hits, zero actual cold starts.

### Per-claim segments (client watch stream, 300 claims; controller timing lines, 224 claims)

| segment | n | p10 | p50 | p90 | p99 | max |
|---|---|---|---|---|---|---|
| S0 create→ack (client POST RTT) | 300 | 101 | **200** | 221 | 224 | 230 |
| S1 create-call→claim ADDED at client watch | 300 | 118 | 273 | 313 | 333 | 336 |
| S2 claim visible→sandbox adoption patch visible | 300 | 65 | **555** | **1841** | 1882 | 1903 |
| S3 adoption patch→claim Ready visible | 300 | −473* | 156 | 670 | 1003 | 1341 |
| true queue wait: claim-visible→winning-pass entry (est.) | 224 | −171* | **33** | 560 | 984 | 1012 |
| `patchMs` — sandbox adoption patch RTT (controller-observed) | 224 | 127 | **449** | **995** | 1544 | 1547 |
| `statusMs` — claim status patch RTT | 224 | 97 | **278** | 698 | 1044 | 1144 |
| `annotationFlushMs` (post-Ready, off critical path) | 224 | 115 | 284 | 716 | 1046 | 1095 |
| E2E create→Ready | 300 | 508 | 966 | 1744 | 2582 | 2624 |

\* negative values = per-resource watch streams deliver independently; the claim-Ready event sometimes reaches the client before the sandbox MODIFIED event. Not an anomaly.

**p50 critical path ≈ 200 (ack) + ~70 (watch-in) + 33 (wait) + 449 (adoption patch) + 278 (status patch) ≈ 1030 ≈ observed 966.** The story has inverted since round 2: queue wait is now nearly gone; **the two hot-path write RTTs themselves are 75% of the median**, at 15-30× the etcd commit time.

### Why the writes are slow: the split of one 449ms sandbox patch (p50)

| layer | p50 | p90 | source |
|---|---|---|---|
| etcd commit (`etcd_request update`) | 12-18ms | 42-59ms | apiserver metrics, burst window |
| APF wait, `agent-sandbox-critical` PL | 3.2ms | 63ms (p99 359) | 4,362 requests in window |
| apiserver execution (`apf execution`, critical PL) | 86ms | 321ms | CPU-inflated: 3-10× quiescent |
| server total (`apiserver_request PATCH sandboxes`) | 102ms | 340ms | n=2086 |
| controller-observed RTT (`patchMs`) | 449ms | 995ms | in-burst timing lines |
| **⇒ client-side transport/queueing gap** | **~350ms** | **~650ms** | difference |

**The APF "verify ~0" check FAILED — and that is the round-4 headline.** Our own insulation PLs are the queueing point: `agent-sandbox-critical` has 78 nominal seats (40/310 × 600 server seats) but the burst's **demand high-watermark was 272 seats** (bulk: 49 seats vs HWM 100). Requests queue at the server, seat-starved execution slows on the CPU-bound n2-standard-8 control plane, backpressure piles requests up client-side → the 350ms client-server gap. Client-side rate limiter waits: 0.00ms (n=5,356) — not the client's token bucket.

### Pipeline position is the p90

corr(ready-rank, e2e) = **0.961**; ready inter-arrival mean 8.0ms → drain rate 124.8/s overall / 196/s steady; first→last Ready span 2.40s. The p90 claim is not "slow" — it is ~270th in a queue draining at ~125-200/s. Write demand in the true burst window (2.85s): 300 claim POSTs + 931 claim writes + 581 sandbox writes + 300 pod patches + 300 events ≈ **846 writes/s** against an effective seat-limited service rate of roughly `(78+49) seats / ~0.1s ≈ 600-900/s` under contention-inflated exec — the system runs at its APF+CPU ceiling for the whole burst.

Attribution shares of the p50 (966ms): create-ack 21% (client POST under its own 50-way concurrency; server POST p50 112ms), watch-in ~7%, queue wait 3%, adoption-patch RTT 46%, status-patch RTT 29% (overlaps S3). Of the write RTTs: ~10% server APF wait, ~25% server exec (CPU), ~4% etcd, **~60% client-side in-flight queueing** — itself the backpressure shadow of the seat ceiling.

### Both instrumentation bugs root-caused

1. **`queueLatMs` (1s truncation):** reported p50 1025ms vs 33ms true — `CreationTimestamp` is second-truncated, and burst claims created at t+0.85s inherit ~850ms phantom wait.
2. **Missing timing lines (76/300, up from 40):** controller-runtime's production zap wraps the core in `NewSamplerWithOptions(core, 1s, 100, 100)` — at the burst peak (~2,100 log lines/s) the 101st+ "adoption timing" line per sampler window is silently dropped. The missing cohort had e2e p50 1382ms vs 909ms for survivors — the surviving sample is biased toward the fast cohort, so every log-derived quantile was wrong. (Line-count fingerprint: 77+123+24 across wall-seconds = 100+100+24 across sampler windows.)

## 2. Round-4 implementation (branch `perf-investigation-master-round4`, 3 commits, pushed)

`go build ./...`, `go vet ./...` clean; all unit packages pass. Only `test/e2e{,/extensions}` fail, identically to the base branch, for the environmental reason `stat bin/KUBECONFIG: no such file` (needs a live cluster).

| commit | content |
|---|---|
| `3bf19e3` **perf(r4.1): internal/rawpatch** | New `internal/rawpatch` package: builds `{"metadata":{"annotations"\|"labels":{…}}}` merge patches directly instead of DeepCopy+MergeFrom (serialize whole object twice, diff). Tests pin payloads **byte-for-byte** (sorted keys, JSON/HTML escaping) and prove byte-equivalence with `client.MergeFrom` output. Set-only by design (no JSON-null deletes). |
| `8f3bd9d` **perf(round4): controller changes** | **R4.1 call sites:** `persistStampedAnnotations`, `initializeSandboxLaunchTypeLabel`, sandbox pod-name annotation patch → rawpatch; interceptor tests capture actual wire bytes per call site and assert exactness + legacy equivalence (round-3 profile: `mergeFromPatch.Data` 11.4%, JSON 18.3% of burst CPU). **R4.2:** `Cache.DefaultTransform = TransformStripManagedFields()` for ALL cached objects (nothing reads managedFields; verified repo-wide); `PodCacheTransform` drops pod spec except `spec.nodeName` (the only spec field read — verified: `sandbox_controller.go:286` only); `--cache-label-selectors` (default false) scopes Pod+Service informers server-side to `agents.x-k8s.io/sandbox-name-hash exists`. Opt-in because the external `agents.x-k8s.io/adoptable=true` contract matches by name — such objects must also carry the tracking label under the flag (documented). `TestPodCacheTransformMergePatchUnaffected` proves merge patches from stripped cache objects are byte-identical to unstripped and contain no `spec`/`managedFields` keys. **R4.3:** `--disable-claim-observability-annotations` (default false) skips the deferred flush entirely (−1 write/claim, ~⅓ of claim-controller hot-path writes); in-memory stamping kept for metrics/traces; test proves a warm adoption completes with **zero non-status claim writes** and still binds. **Instrumentation:** `queueLatMs` now = controller-watch-receive (`getTimingPredicate`'s monotonic `observedTimes` entry) → winning-pass entry; old value kept as `sinceCreationMs`, documented as 1s-truncated. Timing line moved to a dedicated **never-sampled** zap core (RFC3339Nano ts, still V(1)-gated, `grep '"adoption timing"'`-compatible). |
| `14e6f1a` **perf(r4.5): bench tuning** | `CP_MACHINE_TYPE` env (default n2-standard-8) for kops control-plane size; `TUNE_CONTROL_PLANE=true` opt-in (creation-time, INSTRUMENT_CLUSTER-style): `--max-requests-inflight=3000`, `--max-mutating-requests-inflight=1000` (600→4000 APF seats), scheduler kubeAPIQPS/burst 800/1600, KCM 200/400 via `kops --set`. Events-etcd split documented as already-default in kops (separate `etcd-events` cluster). `k8s/apf-insulation.yaml` sizing comment replaced with round-3 measurements (78 seats vs demand HWM 272, wait p99 359ms) + the re-derivation formula: shares are fractions of the seat total, so 4000 seats ⇒ critical ≈ 516, bulk ≈ 322 — no manifest change needed, keep `seats(critical) ≥ 2× demand HWM`. |

## 3. Recommendation: is L1 (one-write adoption) required for 174ms p90?

**Arithmetic.** Removing the seat wall (R4.5: 516 seats ≥ 2× the 272 HWM) and ~35% of hot-path write demand (R4.3 flush + `--disable-claim-events`; R4.1/R4.2 cut controller CPU ~25-30%) collapses the queueing terms toward their uncontended values. Post-R4 p90 estimate at 300-burst:

| term | today (p90) | post-R4 estimate | basis |
|---|---|---|---|
| create-ack | 220 | 60-100 | server POST p50 112→~40ms with seats+CPU headroom; remainder is the stress client's own 50-way pipelining |
| watch-in to controller | ~90 | 40-60 | uncontended S1−S0 ≈ 70ms; less encode contention |
| queue wait | 560 | ~20 | wait was backpressure, not workqueue |
| adoption patch RTT | 995 | 40-60 | server exec 86→~30ms, APF wait→~0, client gap→~0 |
| status patch RTT | 698 | 40-60 | same |
| Ready delivery to client | ~50 | 30-40 | |
| **p90 total** | **1740** | **≈ 230-340ms** | |

So **R4.x + control-plane tuning alone gets ~5-7× of the required 10×, landing ~1.5-2× above the 174ms target.** The residual is NOT dominated by the second write (worth only ~40-60ms once the control plane is healthy): it is create-ack plus two watch hops — terms L1 does not touch. Therefore:

- **L1 is not the next lever, and is not strictly required to get close** — run the round-4 A/B first (both legs `TUNE_CONTROL_PLANE=true CP_MACHINE_TYPE=n2-standard-16`, candidate adds `--cache-label-selectors --disable-claim-observability-annotations`). Decision gate: if measured p90 ≤ ~250ms and `demand_seats_high_watermark ≪ limit`, the remaining gap sits in ack+watch delivery.
- **L1 becomes necessary to actually cross ≤174ms and to hold it at 500-1000/s**: it removes one write RTT (~40-60ms, closing most of the 230→174 gap), and — more importantly at 1000/s — halves hot-path seat demand per claim (2→1 writes before Ready), which is what keeps the seat ceiling unreachable as rate scales. Pair it with the R4.4 namespace sharding from SCALE-ROADMAP before any 1000/s attempt; single-process watch ingestion and the ~846 writes/s burst demand both scale linearly with rate.
- Independent of L1, the create-ack term (60-100ms) will bound p90 near ~150-200ms; if the target is strict, the benchmark should also raise stress-client `createConcurrency` and confirm claim POSTs stay on the exempt PL (admin kubeconfig, verified `system:masters`).

**Bottom line: ship the round-4 A/B first; expect p90 ≈ 230-340ms (5-7×). Approve L1 prototyping behind a flag now, targeted at the 500-1000/s rounds, since the arithmetic shows it is the difference between ~230ms and ~174-190ms at 300/s and between linear and super-linear seat demand growth beyond it.**

