# [DRAFT — not yet filed] Umbrella: SandboxClaim warm-adoption latency at high burst rates — findings and fixes

**Scenario:** N SandboxClaims created simultaneously against a fully provisioned
SandboxWarmPool of N (measured at N=300; targets: 500, then 1000 claims/s
sustained). Goal: minimize create→Ready for every claim. Guardrail honored
throughout: **no breaking CRD changes** (the one authorized exception: dropping
v1alpha1, task 1).

**Measured outcome** (kops GCP, 1 control plane + 6× n2-standard-8 workers,
in-region client; identical harness across runs):

| | p50 | p90 | p99 | time to ALL 300 Ready |
|---|---|---|---|---|
| before | 2.54s | 5.55s | 19.1s | 20.25s |
| after (all fixes) | 489ms | **584ms** | 761ms | **0.86s** |
| improvement | 5.2× | 9.5× | 25× | 23.5× |

Uncontended floor (20-claim runs): 31-87ms — everything above that is burst
contention, and each task below removes a piece of it. Full data, per-round
A/Bs, and forensic artifacts: `optimizations/` on the investigation fork
(`aditya-shantanu/agent-sandbox`, branch `perf-investigation-master`).

Each task below is independently reviewable and roughly maps to one
investigation branch. Ordered by theme, not priority.

---

## Theme A — API surface

### Task 1: Serve v1beta1 only; drop v1alpha1 and the conversion webhook
**Problem:** All four CRDs served alpha+beta with `conversion.strategy: Webhook`,
forcing the webhook server, cert bootstrap, and CRD caBundle patching to exist
even though every in-repo actor speaks v1beta1 (storage version).
**Fix:** Delete `api/v1alpha1` + `extensions/api/v1alpha1`, single-version CRDs
with `conversion: None`, remove conversion-handler registrations; makes
`--enable-webhook=false` fully safe. −22,713 lines.
**Caveats for master:** clusters with `v1alpha1` in `status.storedVersions`
need storage migration before applying the new CRDs; migration periodics that
upgrade *from* alpha need a decision.

## Theme B — SandboxClaim controller hot path

### Task 2: Kill the pool→claims watch fan-out (O(N²) reconciles)
**Problem:** `Watches(SandboxWarmPool, mapWarmPoolToClaims,
ResourceVersionChangedPredicate)` re-enqueued **every** claim referencing a
pool on **every** pool write; pool status churns continuously during a burst.
Measured: workers saturated with no-op reconciles, delaying first-pass
adoption.
**Fix:** `GenerationChangedPredicate` on the pool watch (status churn no
longer fans out) + `mapWarmPoolToClaims` skips already-bound/deleting claims.
Verified safe: adoptable-sandbox wakeups flow through the Sandbox watch → the
in-memory warm queue, never the pool fan-out.

### Task 3: Stale-cache re-adoption guard (the biggest single fix)
**Problem:** A reconcile reading a stale pre-adoption claim view re-entered
adoption or the cold path; the dedup map was only consulted on the
annotation-present branch. Measured at 300-burst: **4,357 reconciles for 300
claims (14.5×), 45% adoption-write conflict rate, 161 claims' status flapping
Ready→AdoptionConflict→Ready after readiness, 144 duplicate cold-path
sandboxes (138 wasted pods created and thrown away)** — ~half the burst's
write load was waste.
**Fix:** In-memory per-claim-UID fingerprints (`triggeredAdoptions` +
last-written-status) consulted **before** adoption/cold-start entry and before
status writes; stale passes perform zero writes; duplicate metric records
suppressed (histogram had 911 observations for 300 claims).

### Task 4: Cold-start guard + adoption optimistic lock
**Problem:** Momentary in-memory queue exhaustion (failed attempts held popped
keys for their full ~400ms attempt; doomed stale adopters checked out more)
sent claims to the cold path **with 300 adoptable sandboxes present** — there
was no List fallback. Separately, the adoption sandbox patch had no optimistic
lock, so two claims could silently adopt the same sandbox; the loser burned
its candidate and cold-started. Measured: 6 cold-bound claims = the entire
p99 tail (4.4-6.7s).
**Fix:** Before cold-starting, an indexed cache List for adoptable pool
members → bounded 50ms requeue if any exist (attempt-capped); keys returned
to the queue immediately on failure; `MergeFromWithOptimisticLock` on the
adoption patch (lost race = 409 = re-adopt a different candidate). Cold
starts at 300-burst: 0 in every run since.

### Task 5: Adoption conflicts → bounded requeue, not exponential backoff
**Problem:** A 409 on the adoption write routed the claim through per-item
exponential failure backoff (5ms·2^k), producing retry waves at
0.8/1.2/1.6s.
**Fix:** Conflict sentinel → nil error + 50ms `RequeueAfter` with the
candidate returned to the queue (mirrors the existing #1107 cache-lag
pattern).

### Task 6: Collapse the adoption transaction 3 writes → 2
**Problem:** Critical path was claim Update (annotation lock) → sandbox patch
→ claim status patch: three serial RTTs, each inflated under burst.
**Fix:** The optimistically-locked sandbox patch (which stamps
`claim-uid` label + controllerRef atomically) **is** the adoption lock; the
annotation moved to a deferred post-Ready flush (observability hint only).
Crash recovery via a new field index on the claim-UID label +
`IsControlledBy` check — covered by restart tests at N=50 concurrency.
**Trade-offs:** annotation may land a beat after Ready; cross-process
double-bind protection now rests on leader election + recovery List.

### Task 7: Write-payload and CPU reductions
**Problem:** Small metadata patches were built via full-object
DeepCopy+MergeFrom diffing — `persistStampedAnnotations` alone was 15.8% of
burst CPU; ~33% of controller CPU was JSON.
**Fix:** `internal/rawpatch` builds targeted
`{"metadata":{"annotations":{…}}}` merge patches directly (byte-identical to
the old wire format, pinned by tests); optional
`--disable-claim-observability-annotations` removes the flush write entirely
(−1 write/claim); optional `--disable-claim-events` removes 300 event POSTs
per burst.

## Theme C — Sandbox controller

### Task 8: Status writes race-proofed
**Problem:** `Status().Update` from a possibly-stale cache raced the claim
controller's adoption patch → 409s → backoff on the pod-Ready→claim-Ready
chain.
**Fix:** `Status().Patch(MergeFrom)` (this controller is the sole
Sandbox-status writer; all fields recomputed each pass); no-op reconciles
verified write-free by interceptor-counted tests.

### Task 9: Informer cache diet
**Problem:** Pod/Service informers were cluster-wide and full-object —
label predicates filtered events *after* decode; managedFields (SSA-inflated
by kubelet) decoded and cached everywhere.
**Fix:** `TransformStripManagedFields` on all cached objects; pod cache
transform drops the spec except `spec.nodeName` (the only spec field read);
opt-in `--cache-label-selectors` scopes Pod/Service watches server-side to
the sandbox tracking label. Caveat documented: externally pre-provisioned
`adoptable=true` resources must carry the tracking label under the flag.

## Theme D — Warm pool

### Task 10: Refill must yield to the burst (and flow under sustained load)
**Problem (burst):** the pool refilled the full deficit immediately,
racing 300 sandbox+pod creates against the adoption burst for API budget.
**Problem (sustained):** the burst fix (deferral) **re-arms on every drop**,
so under continuous arrivals refill never starts and the pool drains to zero.
**Fix:** `--sandbox-warm-pool-replenish-delay` (defer refill until drops
settle; default 0 = legacy) + `--sandbox-warm-pool-max-refill-rate`
(per-pool token bucket; default 0 = legacy). Delay defers the START, rate
shapes the FLOW; they compose. Measured per-pool refill ceiling ~70-85/s
(scheduler QPS and APF-shaped creates are the limiters) → sizing formula
documented: pools ≥ ceil(rate / per-pool ceiling), replicas ≥ rate ×
(refill p99 + hold).

## Theme E — Transport & control plane

### Task 11: Watch/write connection separation + connection sharding
**Problem:** kube-apiserver advertises `SETTINGS_MAX_CONCURRENT_STREAMS=100`;
the controller's informer watches shared one HTTP/2 connection with hundreds
of concurrent write streams. Measured: watch events reached the controller
~1s late (independent watcher: 20-305ms) — ~60% of end-to-end latency — and
effective write concurrency plateaued at ~100-110 despite 800 workers.
**Fix:** `--separate-watch-connection` (dedicated connection for the
manager's cache via `Cache.HTTPClient`) + `--api-connections=N` (round-robin
write sharding over N pre-established connections). Defaults preserve stock
behavior exactly.

### Task 12: APF insulation — and the seat-sizing lesson
**Problem:** All controller traffic shared `workload-low` with every other
tenant; and once insulated, the first sizing (40 shares ≈ 78 seats) became
the bottleneck itself — measured demand high-watermark 272 seats, requests
queueing at our own priority level.
**Fix:** Dedicated PriorityLevelConfigurations/FlowSchemas (latency-critical
claim path above bulk refill above events), with the sizing rule now
documented: `seats(critical) ≥ 2× demand HWM`; raising
`--max-mutating-requests-inflight` multiplies the seat pool (bench: 200→1000
seats on an n2-standard-16 control plane took p90 1740→1094ms with zero code
change). Bench-only manifest today (excluded from release kustomization).

### Task 13: Graceful failover
**Problem:** `LeaderElectionReleaseOnCancel` unset → rolling updates waited
out the full 15s LeaseDuration with no active controller (≈7,500 queued
claims at 500/s).
**Fix:** Release the lease on clean shutdown (~0-2s handover); crash failover
unchanged by design. Restart blast radius audited and pinned by a 660-line
regression suite: recovery of adopted-but-unwritten claims proven at N=50,
echo relapse bounded to one write per claim.

### Task 14: Horizontal scale without API changes
**Problem:** One active process must ingest every watch event — a vertical
ceiling as rates rise.
**Fix:** `--watch-namespaces` static sharding (server-side scoped informers +
per-shard leader Lease). Safe because adoption exclusivity rests on the
optimistic-lock patch (task 4), not process-local state, and adoption is
namespace-local by construction — full safety-audit table in the fork's docs.

## Theme F — Clients & router

### Task 15: SDK ready-wait was paying an extra round trip
**Problem:** The Python SDK ran two sequential watches (claim → then sandbox)
although the claim's first status event already carries name + forwarded
Ready; dev-mode port-forward polled at 500ms.
**Fix:** Single claim-watch `wait_for_claim_ready()` (legacy methods kept,
API-compatible); poll 500→50ms; latency guidance in the SDK README.

### Task 16: Router resolution without per-sandbox Services
**Problem:** Per-sandbox headless Services (+~2 EndpointSlice writes each,
~20-25% of churn writes for deployments using them) existed largely because
the router's UID-less fallback was DNS.
**Fix:** Router gains a namespace/name cache-resolution path
(PodIP header → UID cache → name cache → DNS), so `spec.service: false`
(field already exists — no CRD change) becomes viable; benchmark template
pinned service-free. Legacy DNS-only consumers documented.

## Theme G — Measurement (bench/dev tooling; partly bench-only)

### Task 17: Benchmark honesty
Burst phase (`claims-warm`), sustained phase (`claims-warm-sustained`,
Poisson arrivals + rolling windows), per-adoption phase-timing instrumentation
(never-sampled log line; two measurement bugs found and fixed: 1s-truncated
CreationTimestamp math, zap sampler dropping the slow cohort's lines),
REST-client latency histograms (controller-runtime exports none), pprof
captured during the burst, and — critically — **the stress client itself
shared one HTTP/2 connection** (same 100-stream cap), inflating reported
create-ack ~2-3×; `--client-connections=N` calibrates it. Every prior
public-facing number should be read with that caveat.

---

## Remaining work (known, scoped, not in this issue)
- **L1 one-write adoption** (status-first commit on shard-exclusive pools):
  the arithmetic path from p90 ~584ms to ≤174ms and the seat-demand halver
  needed for 1000/s.
- Create-path cost (ack ~49ms calibrated) and CBOR serving/storage
  (KEP-4222) once beta.
- etcd/apiserver write-domain sharding for the ~20k writes/s churn budget at
  1000/s; per-claim churn reduction (service-free defaults, recycling —
  security-gated).
