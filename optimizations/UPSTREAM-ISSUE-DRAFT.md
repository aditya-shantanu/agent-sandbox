# [DRAFT — not yet filed] Umbrella: SandboxClaim warm-adoption latency at high burst rates — findings and fixes

**Scenario:** N SandboxClaims created simultaneously against a fully
provisioned SandboxWarmPool of N (measured at N=300; targets: 500, then
1000 claims/s sustained). Goal: minimize create→Ready for every claim.
Guardrail honored throughout: **no breaking CRD changes** (the one authorized
exception: dropping v1alpha1, task 20).

**Measured outcome** (kops GCP, 1 control plane + 6× n2-standard-8 workers,
in-region client; identical harness across runs):

| | p50 | p90 | p99 | time to ALL 300 Ready |
|---|---|---|---|---|
| before | 2.54s | 5.55s | 19.1s | 20.25s |
| after (round-4/5 stack) | 489ms | **584ms** | 761ms | **0.86s** |
| improvement | 5.2× | 9.5× | 25× | 23.5× |

The 584ms/0.86s row is the latest **verified** number (round-4/5 A/B).
Tasks 10, 12 and 17 (write-behind coalescing, no-spec adoption sandbox half,
L1 one-write adoption) merged after that run — **the composed tree is not yet
benchmarked** (gate zero in `ROUND7-PLAN.md`). Uncontended floor (20-claim
runs): 31-87ms — everything above that is burst contention, and each task
below removes a piece of it. Full data, per-round A/Bs, and forensic
artifacts: `optimizations/` on the investigation fork
(`aditya-shantanu/agent-sandbox`, branch `perf-investigation-master`).

## Legend

- **Safety** — 🟢 completely safe (behavior-identical or default-off flag,
  fully tested, no semantic change) · 🟡 medium (documented semantic
  trade-offs, or needs an upstream decision) · 🔴 risky / needs design
  discussion (none currently).
- **Impact on claim latency** — `+` small (<5%) · `++` moderate · `+++`
  large (measured big win; number cited inline) · `++++` structural.
- **Status** — *merged* = on `perf-investigation-master` · *bench-only* =
  manifests excluded from release kustomization · *design* = written up,
  not implemented. "(unbenchmarked)" = merged after the 584ms run.

Tasks are sorted by **safety first, then impact**. Each is independently
reviewable and roughly maps to one investigation branch.

## Summary

| # | task | safety | impact | status | community |
|---|---|---|---|---|---|
| 1 | Watch/write connection split + sharding | 🟢 | ++++ | merged | none — novel |
| 2 | Stale-cache re-adoption guard | 🟢 | +++ | merged | [#527](https://github.com/kubernetes-sigs/agent-sandbox/issues/527), [#418](https://github.com/kubernetes-sigs/agent-sandbox/issues/418), [#940](https://github.com/kubernetes-sigs/agent-sandbox/issues/940) |
| 3 | Cold-start guard + adoption optimistic lock | 🟢 | +++ | merged | [#418](https://github.com/kubernetes-sigs/agent-sandbox/issues/418), [#478](https://github.com/kubernetes-sigs/agent-sandbox/issues/478), [#1042](https://github.com/kubernetes-sigs/agent-sandbox/issues/1042) |
| 4 | Kill pool→claims watch fan-out | 🟢 | ++ | merged | [#527](https://github.com/kubernetes-sigs/agent-sandbox/issues/527) |
| 5 | Adoption conflicts → bounded requeue | 🟢 | ++ | merged | [#527](https://github.com/kubernetes-sigs/agent-sandbox/issues/527), [#1042](https://github.com/kubernetes-sigs/agent-sandbox/issues/1042)/[PR #1072](https://github.com/kubernetes-sigs/agent-sandbox/pull/1072) |
| 6 | Write-payload & CPU reductions (rawpatch + flags) | 🟢 | ++ | merged | [#527](https://github.com/kubernetes-sigs/agent-sandbox/issues/527), [#350](https://github.com/kubernetes-sigs/agent-sandbox/issues/350), [#940](https://github.com/kubernetes-sigs/agent-sandbox/issues/940) |
| 7 | Informer cache diet | 🟢 | ++ | merged | [#836](https://github.com/kubernetes-sigs/agent-sandbox/issues/836), [#484](https://github.com/kubernetes-sigs/agent-sandbox/issues/484) |
| 8 | Warm-pool refill shaping (delay + rate) | 🟢 | ++ | merged | [PR #913](https://github.com/kubernetes-sigs/agent-sandbox/pull/913), [#1182](https://github.com/kubernetes-sigs/agent-sandbox/issues/1182) |
| 9 | SDK single-watch ready wait | 🟢 | ++ | merged | [#574](https://github.com/kubernetes-sigs/agent-sandbox/issues/574), [#286](https://github.com/kubernetes-sigs/agent-sandbox/issues/286), [PR #565](https://github.com/kubernetes-sigs/agent-sandbox/pull/565) |
| 10 | Write-behind coalescing (sandbox controller) | 🟢 | ++ | merged (unbenchmarked) | [#350](https://github.com/kubernetes-sigs/agent-sandbox/issues/350), [#594](https://github.com/kubernetes-sigs/agent-sandbox/issues/594) |
| 11 | Sandbox status writes race-proofed | 🟢 | + | merged | [#527](https://github.com/kubernetes-sigs/agent-sandbox/issues/527), [#350](https://github.com/kubernetes-sigs/agent-sandbox/issues/350), [PR #882](https://github.com/kubernetes-sigs/agent-sandbox/pull/882) |
| 12 | Generation-bump elimination (sandbox half) | 🟢 | + | merged (unbenchmarked); claim half: design | none |
| 13 | Leader-lease release on shutdown | 🟢 | + | merged | none — file fresh |
| 14 | Router name-cache (service-free resolution) | 🟢 | + | merged | [#883](https://github.com/kubernetes-sigs/agent-sandbox/issues/883), [#836](https://github.com/kubernetes-sigs/agent-sandbox/issues/836), [PR #850](https://github.com/kubernetes-sigs/agent-sandbox/pull/850) |
| 15 | Benchmark honesty (measurement fixes) | 🟢 | + | merged | [#403](https://github.com/kubernetes-sigs/agent-sandbox/issues/403), [#624](https://github.com/kubernetes-sigs/agent-sandbox/issues/624), [#781](https://github.com/kubernetes-sigs/agent-sandbox/issues/781), [#1182](https://github.com/kubernetes-sigs/agent-sandbox/issues/1182) |
| 16 | APF insulation + seat sizing | 🟡 | +++ | bench-only | none — novel |
| 17 | L1 one-write adoption | 🟡 | +++ (projected) | merged (unbenchmarked) | [#435](https://github.com/kubernetes-sigs/agent-sandbox/issues/435); rebase-watch [PR #1118](https://github.com/kubernetes-sigs/agent-sandbox/pull/1118) |
| 18 | Collapse adoption transaction 3 writes → 2 | 🟡 | ++ | merged | [#478](https://github.com/kubernetes-sigs/agent-sandbox/issues/478); rebase-watch [PR #1118](https://github.com/kubernetes-sigs/agent-sandbox/pull/1118) |
| 19 | Horizontal scale via namespace sharding | 🟡 | ++ (rate-holding) | merged; re-scope onto [PR #1213](https://github.com/kubernetes-sigs/agent-sandbox/pull/1213) | [#484](https://github.com/kubernetes-sigs/agent-sandbox/issues/484), [PR #924](https://github.com/kubernetes-sigs/agent-sandbox/pull/924) |
| 20 | Serve v1beta1 only; drop v1alpha1 + conversion webhook | 🟡 | + | merged | [#751](https://github.com/kubernetes-sigs/agent-sandbox/issues/751); conflicts [PR #1188](https://github.com/kubernetes-sigs/agent-sandbox/pull/1188), [PR #1106](https://github.com/kubernetes-sigs/agent-sandbox/pull/1106) |

Novelty note (verified in the 2026-07-19 community scan): nobody upstream is
pursuing transport sharding, APF insulation for controller traffic classes,
or CBOR for CR controllers — tasks 1, 16 and the CBOR follow-up are entirely
novel contributions.

---

## 🟢 Completely safe

### Task 1: 🟢 ++++ — Watch/write connection separation + sharding
**Problem:** kube-apiserver's `SETTINGS_MAX_CONCURRENT_STREAMS=100` meant the
informer watches shared one HTTP/2 connection with hundreds of write streams:
watch events arrived ~1s late (~60% of e2e latency) and effective write
concurrency plateaued at ~100-110 despite 800 workers.
**Fix:** `--separate-watch-connection` (dedicated connection for the manager
cache) + `--api-connections=N` (round-robin write sharding). Defaults
preserve stock behavior exactly. Core of round-3's p90 2479→1740ms.

### Task 2: 🟢 +++ (all-ready 7.86→3.45s with task 3) — Stale-cache re-adoption guard
**Problem:** Reconciles on stale pre-adoption claim views re-entered adoption
or the cold path: 4,357 reconciles for 300 claims, 45% adoption-write
conflicts, 144 duplicate cold-path sandboxes, 911 histogram observations for
300 claims (the [#940](https://github.com/kubernetes-sigs/agent-sandbox/issues/940) bug).
**Fix:** In-memory per-claim-UID fingerprints consulted before
adoption/cold-start entry and before status writes; stale passes perform
zero writes (521 suppressed per burst); duplicate metric records suppressed.
**Rebase note:** [PR #1118](https://github.com/kubernetes-sigs/agent-sandbox/pull/1118)
rewrites adoption finalization — the invariant (never re-enter adoption from
a stale cache view) must be re-expressed in its structure, not the map ported.

### Task 3: 🟢 +++ (cold starts 183→0; was the entire p99 tail 4.4-6.7s) — Cold-start guard + adoption optimistic lock
**Problem:** Momentary warm-queue exhaustion sent claims to the cold path
with 300 adoptable sandboxes present (no List fallback); the adoption patch
had no optimistic lock, so two claims could silently adopt the same sandbox
(exactly [#418](https://github.com/kubernetes-sigs/agent-sandbox/issues/418)).
**Fix:** Indexed cache List for adoptable members before cold-starting →
bounded 50ms requeue if any exist; keys returned to the queue immediately on
failure; `MergeFromWithOptimisticLock` on the adoption patch (409 loser
re-adopts a different candidate). Cold starts at 300-burst since: 0.

### Task 4: 🟢 ++ — Kill the pool→claims watch fan-out (O(N²) reconciles)
**Problem:** Every pool status write re-enqueued every claim referencing the
pool; pool status churns continuously during a burst — workers saturated with
no-op reconciles, delaying first-pass adoption
([#527](https://github.com/kubernetes-sigs/agent-sandbox/issues/527)'s storm).
**Fix:** `GenerationChangedPredicate` on the pool watch + the mapper skips
already-bound/deleting claims. Verified safe: adoptable-sandbox wakeups flow
through the Sandbox watch → warm queue, never the pool fan-out.

### Task 5: 🟢 ++ — Adoption conflicts → bounded requeue, not exponential backoff
**Problem:** A 409 on the adoption write routed the claim through per-item
exponential backoff (5ms·2^k), producing retry waves at 0.8/1.2/1.6s.
**Fix:** Conflict sentinel → nil error + 50ms `RequeueAfter` with the
candidate returned to the queue (same pattern as
[PR #1072](https://github.com/kubernetes-sigs/agent-sandbox/pull/1072) for
[#1042](https://github.com/kubernetes-sigs/agent-sandbox/issues/1042)). This
data argues against [#1059](https://github.com/kubernetes-sigs/agent-sandbox/issues/1059)'s
CRD retry knob. Rebase-watch: [PR #1118](https://github.com/kubernetes-sigs/agent-sandbox/pull/1118).

### Task 6: 🟢 ++ (with task 7, round-4/5 flags: p90 1094→584ms) — Write-payload and CPU reductions
**Problem:** Small metadata patches were built via full-object
DeepCopy+MergeFrom diffing — one call site alone was 15.8% of burst CPU;
~33% of controller CPU was JSON.
**Fix:** `internal/rawpatch` builds targeted metadata merge patches
(byte-identical wire format, pinned by tests); optional
`--disable-claim-observability-annotations` (−1 write/claim) and
`--disable-claim-events` (−300 event POSTs/burst). Note:
[PR #1087](https://github.com/kubernetes-sigs/agent-sandbox/pull/1087)/[PR #1114](https://github.com/kubernetes-sigs/agent-sandbox/pull/1114)
fix the same metrics bug with a +1-write annotation — worth reconciling.

### Task 7: 🟢 ++ (with task 6 flags: p90 1094→584ms) — Informer cache diet
**Problem:** Pod/Service informers were cluster-wide and full-object: label
predicates filtered after decode; kubelet-inflated managedFields decoded and
cached everywhere.
**Fix:** `TransformStripManagedFields` everywhere; pod cache transform keeps
only `spec.nodeName`; opt-in `--cache-label-selectors` scopes Pod/Service
watches server-side to the tracking label (documented caveat: externally
pre-provisioned adoptables must carry it). Watch
[#836](https://github.com/kubernetes-sigs/agent-sandbox/issues/836): if
high-cardinality labels move to annotations, this flag must adapt.

### Task 8: 🟢 ++ — Warm-pool refill must yield to the burst (and flow under sustained load)
**Problem:** Refill raced 300 sandbox+pod creates against the adoption burst
for API budget; the naive deferral fix re-arms on every drop, draining the
pool to zero under sustained arrivals.
**Fix:** `--sandbox-warm-pool-replenish-delay` (defer refill start) +
`--sandbox-warm-pool-max-refill-rate` (per-pool token bucket); both default
0 = legacy, and compose. Measured per-pool refill ceiling ~70-85/s → sizing
formula documented. Supersedes stale
[PR #913](https://github.com/kubernetes-sigs/agent-sandbox/pull/913); the
[#1182](https://github.com/kubernetes-sigs/agent-sandbox/issues/1182)
template→pools fan-out is the adjacent unfixed cliff.

### Task 9: 🟢 ++ — SDK ready-wait was paying an extra round trip
**Problem:** The Python SDK ran two sequential watches (claim → sandbox)
although the claim's first status event already carries name + forwarded
Ready; dev-mode port-forward polled at 500ms
([#574](https://github.com/kubernetes-sigs/agent-sandbox/issues/574),
[#286](https://github.com/kubernetes-sigs/agent-sandbox/issues/286)).
**Fix:** Single claim-watch `wait_for_claim_ready()` (legacy methods kept,
API-compatible); poll 500→50ms; latency guidance in the SDK README.

### Task 10: 🟢 ++ — Write-behind coalescing for recoverable writes *(merged, unbenchmarked)*
**Problem:** Recoverable sandbox-controller writes (pod safe-to-evict strip,
pod-name annotation) competed for APF seats inside the latency-critical burst
window (~846 writes/s against a ~600-900/s seat-limited service rate);
repeated mutations to the same object each paid a full write
([#350](https://github.com/kubernetes-sigs/agent-sandbox/issues/350)'s theme).
**Fix:** `internal/writebehind` flusher, `--sandbox-write-behind-window`
(default 0 = stock): per-object N→1 merge-patch coalescing, pod patches
bounded ≤1s (autoscaler-safe), graceful drain on shutdown, crash recovery =
the normal level-based reconcile (test-pinned). Only recoverable metadata
writes are routed; status/creates/deletes stay synchronous. Related ledger
gap: [#594](https://github.com/kubernetes-sigs/agent-sandbox/issues/594)
(NetworkPolicy writes on the adoption path) still needs auditing.

### Task 11: 🟢 + — Sandbox status writes race-proofed
**Problem:** `Status().Update` from a possibly-stale cache raced the claim
controller's adoption patch → 409s → backoff on the pod-Ready→claim-Ready
chain.
**Fix:** `Status().Patch(MergeFrom)` (sole status writer; all fields
recomputed each pass); no-op reconciles verified write-free by
interceptor-counted tests. Same change as
[PR #882](https://github.com/kubernetes-sigs/agent-sandbox/pull/882)'s item 1
— coordinate, and adopt its `agent_sandbox_writes_total` metric idea.

### Task 12: 🟢 + — Generation-bump elimination, sandbox half *(merged, unbenchmarked; claim half: design)*
**Problem:** Adoption's `spec.podTemplate` rewrite bumps
`metadata.generation`, forcing one sandbox status write (observedGeneration
echo) per adoption — a write a metadata-only adoption never needs.
**Fix:** `--no-spec-adoption` (default off): ownership-derived safe-to-evict
hygiene in the sandbox controller, making the spec rewrite unnecessary when
`additionalPodMetadata` is empty (KEP-0174 users keep today's path
byte-for-byte). Claim-side half is design-complete (`ROUND6-COALESCING.md`
§3.3); both halves = −1 write/claim system-wide, est. −15-40ms burst p90.

### Task 13: 🟢 + — Graceful failover (leader-lease release)
**Problem:** `LeaderElectionReleaseOnCancel` unset → rolling updates waited
out the full 15s LeaseDuration with no active controller (≈7,500 queued
claims at 500/s).
**Fix:** Release the lease on clean shutdown (~0-2s handover); crash failover
unchanged by design. Restart blast radius pinned by a 660-line regression
suite; echo relapse bounded to one write per claim.

### Task 14: 🟢 + — Router resolution without per-sandbox Services
**Problem:** Per-sandbox headless Services (+~2 EndpointSlice writes each,
~20-25% of churn writes where used) existed largely because the router's
UID-less fallback was DNS — the exact 502/NXDOMAIN failure in
[#883](https://github.com/kubernetes-sigs/agent-sandbox/issues/883).
**Fix:** Router gains namespace/name cache resolution (PodIP header → UID
cache → name cache → DNS), making `spec.service: false` viable (field exists
— no CRD change); benchmark template pinned service-free. Complements the
Envoy direction ([PR #850](https://github.com/kubernetes-sigs/agent-sandbox/pull/850)):
framed as making service-free viable for any router.

### Task 15: 🟢 + — Benchmark honesty
**Problem:** Multiple measurement bugs hid the truth: 1s-truncated
CreationTimestamp math ([#781](https://github.com/kubernetes-sigs/agent-sandbox/issues/781)),
zap sampler dropping the slow cohort's log lines, and the stress client
sharing one HTTP/2 connection (same 100-stream cap) inflating reported
create-ack ~2-3×.
**Fix:** Burst + sustained (Poisson) stress phases, never-sampled per-adoption
phase timing, REST-client latency histograms, in-burst pprof, and
`--client-connections=N` calibration. Should be filed against the benchmark
umbrella ([#403](https://github.com/kubernetes-sigs/agent-sandbox/issues/403),
[#624](https://github.com/kubernetes-sigs/agent-sandbox/issues/624)) and
rebased onto [PR #1148](https://github.com/kubernetes-sigs/agent-sandbox/pull/1148)'s
harness split.

---

## 🟡 Medium — documented trade-offs or needs an upstream decision

### Task 16: 🟡 +++ (p90 1740→1094ms) — APF insulation and the seat-sizing lesson
**Problem:** All controller traffic shared `workload-low` with every tenant;
once insulated, the first sizing (≈78 seats) became the bottleneck itself —
measured demand high-watermark 272 seats, queueing at our own priority level.
**Fix:** Dedicated PriorityLevelConfigurations/FlowSchemas (claim path >
bulk refill > events) with the sizing rule `seats(critical) ≥ 2× demand HWM`;
raising `--max-mutating-requests-inflight` multiplies the seat pool (bench:
200→1000 seats took p90 1740→1094ms with zero code change).
**Why 🟡:** bench-only manifest today (excluded from release kustomization);
shipping defaults need an upstream sizing decision.

### Task 17: 🟡 +++ (projected: p90 584→~205-345ms burst; <100ms sustained target) — L1 one-write adoption *(merged, unbenchmarked)*
**Problem:** Even fully optimized, Ready waits on two serial in-window writes
(sandbox adoption patch → claim status patch); the 584ms residual is
create-ack + watch hops + 2 write RTTs at ~100-150ms each under burst.
**Fix:** `--one-write-adoption` (default off = today's path exactly):
claim-status-first commit (the single critical write), sandbox patch deferred
to bounded flush workers keeping the optimistic lock; 409-steal recovery
rebinds the claim; crash window recovered idempotently (pinned at N=50).
**Why 🟡:** sub-second autoscaler-eviction window until the async patch lands
— [#435](https://github.com/kubernetes-sigs/agent-sandbox/issues/435)
(`safe-to-evict=on-completion`) would eliminate it; multi-process window
widened (leader election makes it rare, namespace sharding removes it).
Rebase-watch: [PR #1118](https://github.com/kubernetes-sigs/agent-sandbox/pull/1118).

### Task 18: 🟡 ++ — Collapse the adoption transaction 3 writes → 2
**Problem:** Critical path was claim Update (annotation lock) → sandbox patch
→ claim status patch: three serial RTTs, each 100-216ms under burst vs ~30ms
server commits.
**Fix:** The optimistically-locked sandbox patch **is** the adoption lock;
the annotation moved to a deferred post-Ready flush. Crash recovery via a
claim-UID label index + `IsControlledBy` (restart tests at N=50) — also
closes [#478](https://github.com/kubernetes-sigs/agent-sandbox/issues/478)'s
double-adoption-on-failed-status-write.
**Why 🟡:** annotation may land a beat after Ready; cross-process double-bind
protection now rests on leader election + recovery List.

### Task 19: 🟡 ++ (rate-holding at 500-1000/s, not p90 at 300/s) — Horizontal scale without API changes
**Problem:** One active process must ingest every watch event — a vertical
ceiling as rates rise.
**Fix:** Static namespace sharding (server-side scoped informers + per-shard
leader Lease). Safe because adoption exclusivity rests on the optimistic-lock
patch (task 3), not process-local state, and adoption is namespace-local.
**Why 🟡:** [PR #1213](https://github.com/kubernetes-sigs/agent-sandbox/pull/1213)
already ships a namespaced mode — this task must adopt its flag surface
(keeping only per-shard Lease + topology + the safety audit) rather than
introduce a competing flag ([#484](https://github.com/kubernetes-sigs/agent-sandbox/issues/484)).

### Task 20: 🟡 + — Serve v1beta1 only; drop v1alpha1 and the conversion webhook
**Problem:** All four CRDs served alpha+beta with webhook conversion, forcing
the webhook server, cert bootstrap, and caBundle patching to exist even
though every in-repo actor speaks v1beta1 (storage version).
**Fix:** Delete both v1alpha1 trees, single-version CRDs with
`conversion: None`; makes `--enable-webhook=false` fully safe. −22,713 lines.
**Why 🟡:** clusters with `v1alpha1` in `status.storedVersions` need storage
migration first; migration periodics from alpha need a decision;
[#751](https://github.com/kubernetes-sigs/agent-sandbox/issues/751)'s
webhook-stamped metric becomes permanently impossible unless SDK-stamped;
conflicts with [PR #1188](https://github.com/kubernetes-sigs/agent-sandbox/pull/1188)/[PR #1106](https://github.com/kubernetes-sigs/agent-sandbox/pull/1106).

---

## Remaining work (known, scoped, not in this issue)

- **Gate zero:** benchmark the composed tree (tasks 10, 12, 17 + all flags)
  at burst-300 AND sustained-300 before trusting any projection
  (`ROUND7-PLAN.md` §3).
- **Rebase onto [PR #1118](https://github.com/kubernetes-sigs/agent-sandbox/pull/1118)
  and [PR #1213](https://github.com/kubernetes-sigs/agent-sandbox/pull/1213)**
  before any further claim-controller code (ROUND7 §2).
- Claim-side no-spec adoption (design: `ROUND6-COALESCING.md` §3.3) — kills
  the sandbox status echo entirely.
- Create-path cost (ack ~49ms calibrated) and CBOR serving/storage
  (KEP-4222) — novel, nobody upstream pursuing it for CR controllers.
- etcd/apiserver write-domain sharding for the ~20k writes/s churn budget at
  1000/s; per-claim churn reduction (service-free defaults,
  recycling — security-gated); node-local I/O wall
  ([PRs #1203-#1208](https://github.com/kubernetes-sigs/agent-sandbox/pull/1203));
  Cilium identity cardinality
  ([#836](https://github.com/kubernetes-sigs/agent-sandbox/issues/836)).
- Write-ledger audit for
  [#594](https://github.com/kubernetes-sigs/agent-sandbox/issues/594)
  (NetworkPolicy on the adoption path) and arrival-shaping/SDK
  idempotent-create guidance
  ([#1089](https://github.com/kubernetes-sigs/agent-sandbox/issues/1089)).

---

*Maintenance: this document is updated at the end of every investigation
round (latest: round 6/7 absorption, 2026-07-19 — L1, write-behind
coalescing, no-spec sandbox half, community scan). Numbers in the results
table are always the latest verified A/B, with merged-but-unbenchmarked work
flagged in the status column.*
