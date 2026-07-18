# Sandbox Claim Latency Optimization — Analysis

Goal: minimize SandboxClaim → Ready latency when ~300 claims arrive
simultaneously against a fully provisioned warm pool (adoption path, no pod
creation in the hot path). Constraint: no breaking API changes; dropping
v1alpha1 was explicitly authorized. All line references below are to
upstream/main @ 5320421 (pre-optimization) unless noted.

## Hot-path map (from the exploration phase, 2026-07-18)

Warm-adoption happy path per claim, all in one claim-controller reconcile:
observability annotation patch → adoption `Update` (optimistic lock) →
`completeAdoption` sandbox patch → status patch. The claim's Ready condition
is forwarded verbatim from the already-Ready warm sandbox in the same pass —
there is no extra watch hop on the happy path. All reads are informer-cache;
zero API reads. Services/DNS/router registration are NOT in the Ready path;
adoption never renames objects (labels + ownerRef only).

### Ranked bottlenecks for the 300-burst

1. **Write storm sharing one API budget.** ~4 writes/claim + 1-2 pod/status
   writes downstream + 1 event ≈ 1,800-2,400 writes, WHILE the warm pool
   immediately creates ~300 replacement sandboxes (→ +300 pods, +300
   services) in the same window.
2. **Watch fan-out amplification (O(N²)).** `Watches(SandboxWarmPool, …,
   ResourceVersionChangedPredicate)` + `mapWarmPoolToClaims` re-enqueued ALL
   claims referencing a pool on EVERY pool write; pool status churns
   constantly during the burst. Cheap no-op reconciles, but they saturate the
   400 workers and delay first-pass reconciles.
3. **Adoption conflicts → exponential backoff.** The adoption optimistic-lock
   `Update` routed 409s through per-item exponential failure backoff instead
   of the bounded 50ms requeue the repo already used for cache lag (#1107).
4. **Sandbox `Status().Update` races.** Every adoption patch bumps sandbox
   generation → core reconcile → plain status Update from possibly-stale
   cache racing the adoption patch → 409 + backoff. On the fallback path
   (adopting a not-yet-Ready sandbox) this sits on the pod-Ready → claim-Ready
   chain.
5. **Non-factors, verified:** controller-runtime v0.24.1 priority queue means
   the legacy global 10 qps workqueue bucket is NOT active (stale comment in
   code); no admission webhooks exist; conversion webhook only fires for
   non-storage-version requests (none on a beta-only cluster); resync is ~10h.

## The five optimization branches

All merged into `perf-investigation-master`; each branch validated with
build/vet + unit suite before merge.

### 1. `perf-investigation-master-remove-alpha` (baseline per user decision)
Drop v1alpha1 from all four CRDs; beta-only, `conversion.strategy: None`;
delete conversion-handler registrations. −22,713 lines. Steady-state hot-path
win is small on a clean cluster (conversion was latent), but it removes the
webhook subsystem entirely and makes `--enable-webhook=false` safe, skipping
cert bootstrap + CRD caBundle patching at startup. Risk: clusters with
`v1alpha1` in `status.storedVersions` need storage migration before CRD apply;
migration periodics that upgrade *from* alpha need an upstream decision.

### 2. `…-claim-writes` (claim controller)
- Fan-out fix: `GenerationChangedPredicate` on the pool watch + skip
  bound/deleting claims in `mapWarmPoolToClaims`. Verified safe: adoptable-
  sandbox wakeups come from the Sandbox watch → in-memory queue, never from
  pool fan-out; a claim finding the queue empty cold-starts in the same pass.
- Write folding: first-observed-at annotations stamped in-memory and carried
  by the adoption Update (4 → 3 writes/claim warm path); deferred merge patch
  after status on other paths. Subtlety: `Status().Patch` refreshes the local
  object and drops unpersisted annotations — `restoreStampedAnnotations`
  re-applies them post-status-write.
- Adoption 409 → `errAdoptionConflictRetry` sentinel: nil error + 50ms
  requeue + candidate back in queue (no failure backoff).
- Residual risks: pool spec changes no longer re-enqueue bound claims (metadata
  re-sync waits for the next claim/sandbox event); first-observed metric can
  be recorded one pass later in rare interleavings.

### 3. `…-sandbox-status` (core sandbox controller)
- `Status().Update` → `Status().Patch(MergeFrom)` (no optimistic lock).
  Safe because this controller is the ONLY Sandbox-status writer and all
  fields are recomputed from observed state each pass; eliminates adoption-race
  409s. Tests reproduce the 409 against old code.
- No-op reconciles verified write-free (interceptor-counted). ObservedGeneration
  bump per adoption kept (API conventions; kstatus waiters) — made cheap
  instead of removed.
- Adoption-triggered pod patch stays synchronous: it strips
  `cluster-autoscaler.kubernetes.io/safe-to-evict=true` (deferral risks the
  autoscaler evicting a just-claimed pod) and applies claim-uid labels used by
  discovery/NetworkPolicy.

### 4. `…-warmpool-refill` (warm pool controller)
`--sandbox-warm-pool-replenish-delay` (duration, default 0 = today's
behavior). On observed member drops, defer replacement creates and re-arm
while drops continue; refill the full deficit in one batch after the burst
settles. Created replacements are counted into the observed baseline so stale
informer reads defer briefly instead of double-creating (also fixes the
cache-lag refill trickle). Pool status stays truthful; GC unaffected. Risk:
pool briefly under-provisioned during the hold (accepted); a sliding
continuous drop trickle keeps deferring (bounded by burst-shaped traffic).

### 5. `…-benchmark` (measurement harness, no controller changes)
New stress phase `claims-warm`: provision pool of N (default 300), wait
ReadyReplicas=N, fire N claims simultaneously (unthrottled client), record
create-ack separately from create→Ready (p50/p90/p95/p99/max) and
time-to-all-Ready; controller metrics scraped throughout. kops script gains
`NODE_COUNT`, `STRESS_CLAIMS_WARM`, `CONTROLLER_ARGS`, `SKIP_E2E_SUITE`.

## Measurement policy (user decision 2026-07-18)

Primary metric: **`agent_sandbox_claim_controller_startup_latency_ms`**
(controller-internal, first controller observation → Ready) — excludes all
client-side delays; identical to the GKE CL2 job's
`ControllerStartupLatency`. Secondary: client-observed create→Ready and
time-to-all-Ready from the stress tool. Caveat tracked: pre-optimization
controllers re-record the histogram on redundant reconciles (782 observations
for 300 claims in baseline1), inflating tail quantiles; report observation
counts alongside quantiles.

## Reference point: GKE CL2 rapid-burst run (2026-07-02)

ControllerStartupLatency p50=154ms / p90=245ms / p99=478ms on: 100 nodes,
warm pool 3,300 for a 300-claim burst (no meaningful refill contention),
dedicated e2-standard-32 controller node, GKE scalability-project control
plane, CL2-paced creates (QPS=300, 2 bursts). Not comparable to the 6-node
kops runs in absolute terms; useful as a ceiling. Two of the five
optimizations (refill deferral, fan-out fix) specifically attack the
contention sources that this environment sidesteps by size.
