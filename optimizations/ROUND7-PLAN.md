# ROUND7-PLAN — the last latency round, the floor, and the rate tracks

Status: round-7 planning deliverable (2026-07-19), branch
`perf-investigation-master-round7-plan`, base `perf-investigation-master`
@ 43fb706 (L1 one-write adoption + round-6 coalescing/no-spec both merged).
Inputs: every prior findings doc in `optimizations/` (RESULTS, ANALYSIS,
ROUND2/4-FINDINGS, ROUND5-NO-SERVICE, ROUND6-COALESCING, SCALE-ROADMAP,
UPSTREAM-ISSUE-DRAFT) plus the community scan (COMMUNITY-SCAN.md, landing on
this branch's base — key items folded in below). Docs-only branch; no code.

State entering round 7: measured burst-300 p90 = **584ms** on the round-4/5
stack (PRE-L1, PRE-coalesce — the composed tree at tip 43fb706 has never been
benchmarked). Composed hot path = **2 critical writes** (claim CREATE + claim
status PATCH); the adoption patch is deferred/coalesced (L1), pod hygiene is
write-behind (round 6), and the sandbox status echo dies once the claim-side
half of no-spec adoption lands (§4 item 2 below). Targets: **p90 < 100ms at
sustained 300/s**, and *holding* (not necessarily <100ms) at 500-1000/s.
Guardrail unchanged: no breaking CRD changes.

---

## 1. Latency budget — fully-composed tree vs the physics floor

### 1.1 Method

Segments are the measured round-4 decomposition (ROUND4-FINDINGS §1),
re-projected for the composed tree: one in-window write instead of two after
the CREATE (L1 moves the adoption patch off the critical path; write-behind
moves the pod patch out of the window; claim-side no-spec removes the sandbox
status echo), and in-window write demand per claim drops 5 → 2 (ROUND6 §6
table), which deflates every queueing-sensitive term. Anchors, all measured:

- round-4/5 leg B (584ms p90): ack p50 49ms; residual explicitly attributed
  to "create-ack + watch hops + 2 write RTTs at ~100-150ms each under burst"
  (RESULTS, all-rounds comparison note).
- uncontended smoke floor (20 claims, same composed-ish stack): **31-40ms**
  create→Ready (round-4/5), 57-87ms on round-3.
- healthy-server write costs: PATCH 23-37ms, POST ~50ms quiescent
  (ROUND2-FINDINGS); etcd commit p50 12-18ms *under burst*, ~30ms server
  total; APF wait ~0 once seats ≥ 2× demand HWM (round-4 R4.5, verified in
  the round-4/5 A/B).
- pipeline-position effect at burst: corr(ready-rank, e2e) = 0.961 — burst
  p90 is queue *rank*, not per-claim slowness (ROUND4 §1).

### 1.2 The budget table

All numbers ms. "Floor" = irreducible per-segment physics (§1.3). Burst-300 =
all 300 POSTs in <1s (current `claims-warm` shape). Sustained-300 = 300/s
arrivals shaped per §5 (ρ ≈ 0.4-0.6 against the composed drain rate).

| segment | burst-300 p50 / p90 (predicted) | sustained-300 p50 / p90 (predicted) | floor p50 / p90 | notes |
|---|---|---|---|---|
| S0 create → ack (claim POST) | 45 / 80 | 30 / 55 | 12 / 20 | 1 raft commit + CR decode/validate/CEL + encode; client exempt-PL, calibrated connections |
| S1 ack → controller watch-in | 25 / 55 | 15 / 35 | 4 / 8 | cacher dispatch is immediate; cost = per-watcher JSON encode + in-zone RTT + informer decode |
| S2 queue wait + in-pass compute | 5 / 25 | 2 / 10 | 1 / 2 | workqueue + candidate pop (~0.1ms) + patch build; backpressure gone with 1 in-window write |
| S3 claim status PATCH RTT (the one in-window write) | 60 / 140 | 35 / 75 | 14 / 25 | 1 raft commit + patch apply; burst number is seat-share of a queue draining ~300-500/s |
| S4 Ready → client watch delivery | 20 / 45 | 12 / 30 | 4 / 8 | same physics as S1 |
| **E2E create→Ready** | **≈155 / ≈345** | **≈95 / ≈205** | **≈35 / ≈60** | burst p90 is rank-dominated (corr 0.961), so segment p90s do NOT add — the E2E p90 row is the pipeline estimate, not the column sum |

Readings:

- **Burst-300 all-at-once cannot meet <100ms p90 and that is arithmetic, not
  code**: 300 simultaneous arrivals against a composed drain rate of
  ~350-500/s puts the 270th claim ~0.55-0.75s deep in the pipeline no matter
  how cheap each pass is. §5 (arrival shaping) is the only lever for that
  shape; the <100ms target is correctly stated against *sustained* 300/s.
- **Sustained-300 lands at ≈95/205 (p50/p90) on the composed tree as-is** —
  above target. The gap to <100ms p90 is exactly the round-7 items: no-spec
  (removes residual echo traffic + one reconcile), create-ack work and CBOR
  (S0, S1, S4 encode terms), and CP tuning as table stakes
  (TUNE_CONTROL_PLANE + n2-standard-16 CP). With items 2-5 of §4 landed, the
  sustained prediction is **p50 ≈ 65-80, p90 ≈ 90-140** — the target is
  achievable-borderline, and every further ms is bought inside 2× of the
  floor.
- One audit caveat on the ledger behind this table: community issue **#594**
  reports NetworkPolicy writes on the adoption path in some configurations —
  uncounted in the ROUND6 §2 write ledger. §4 item 7 audits it before we
  trust "2 critical writes" as system-wide truth for non-bench deployments.

### 1.3 The physics floor, stated explicitly

The Ready signal structurally requires **two persisted writes and two watch
deliveries in series**, because the contract is "a durable SandboxClaim whose
status a client can watch":

1. claim CREATE — 1 etcd raft commit (the request itself; cannot be removed);
2. watch hop apiserver→controller — the controller can only act on a
   committed claim;
3. claim status PATCH — 1 etcd raft commit (the binding must be durable
   before it is announced; anything less is optimistic-ready, rejected in §6);
4. watch hop apiserver→client — the client can only observe a committed
   status.

Per-component floor on this environment (1 CP node, co-located single-member
etcd — no quorum network RTT; pd-ssd fdatasync ~1-3ms; in-zone TCP RTT
~0.3-1ms; TLS on warm HTTP/2 connections amortizes to ~0 per request):
commit ≈ 3-8ms each incl. apiserver storage txn; request processing
(authn/authz + CR schema/CEL validation + encode/decode) ≈ 5-12ms per write;
watch hop (cacher dispatch <1ms + per-watcher encode 0.5-2ms + RTT + client
decode) ≈ 3-8ms each; controller pass ≈ 1-2ms. Sum: **≈ 30-45ms p50, ≈
60-80ms p90 under sustained load** (fsync variance, GC, scheduler jitter).

This is not hypothetical: the measured 20-claim smoke floor of **31-40ms** IS
this floor, observed. It is also why the GKE CL2 reference (p50 154ms with a
webhook in path, WAN-adjacent) is a ceiling, not a floor.

---

## 2. Upstream reconciliation — mandatory before any round-7 code

The community scan changes the ground under two of our merged optimizations
and pre-empts one planned one. This section is a *blocking prerequisite* for
every code item in §4.

1. **PR #1118 (lgtm + approved, merge imminent) rewrites adoption
   finalization** — it removes the `triggeredAdoptions` map and the 50ms
   conflict sentinel that our stale-pass guard (upstream-draft Task 3) and
   bounded-requeue (Task 5) are built on. Strategy:
   - freeze all fork-side claim-controller changes until #1118 merges
     upstream; rebase `perf-investigation-master` onto it immediately after;
   - re-express the two invariants (never re-enter adoption from a stale
     cache view; conflicts get bounded requeue, not exponential backoff) in
     #1118's structure rather than porting our data structures — the
     *invariants* are the deliverable, the maps were implementation;
   - re-run gate zero (§3) after the rebase before trusting any number; the
     re-record/echo pathologies rounds 1-3 killed can regress silently under
     a rewritten finalizer;
   - **§4 item 2 (claim-side no-spec) must be implemented ON TOP of the
     rebased tree**, not before — it edits `completeAdoption`, which #1118
     touches. Rebase-watch: #1118.
2. **PR #1213 (lgtm) already ships a namespaced mode.** Our R4.4
   `--watch-namespaces` static sharding must **compose with #1213's flag,
   not compete**: adopt its flag/config surface as the namespace-scoping
   mechanism, keep only the per-shard `LeaderElectionID` + deployment
   topology + bench harness from R4.4, and drop any duplicate flag on the
   fork at rebase time. §4 item 6 is re-scoped accordingly. Rebase-watch:
   #1213.
3. **Novelty check (for the upstream issue draft):** nobody upstream is
   pursuing CBOR for CRs, APF insulation for controller traffic classes, or
   client transport sharding — those three are novel contributions of this
   investigation and should stay prominent in UPSTREAM-ISSUE-DRAFT (tasks
   11, 12 + the CBOR remaining-work note) rather than being assumed to
   arrive from the community.

---

## 3. Decision gate zero: benchmark the composed tree first

Nothing in §4 should be ordered until tip 43fb706 (+ flags:
`--sandbox-write-behind-window=250ms --no-spec-adoption` + round-4/5 flag
set + L1 flag) is A/B'd at burst-300 AND at a sustained-300 phase — and
re-run after the §2 rebase. The 584ms headline predates both merged
branches; every §4 saving below is relative to the §1.2 predictions and must
be re-anchored to this run. Effort S (the reuse+smoke workflow, one
cluster). This also forces the `claims-warm-sustained` phase
(SCALE-ROADMAP measurement item 1) into existence, which items 6-9 all
require.

---

## 4. Ordered next optimizations

Ordering rule: (a) upstream reconciliation first (§2), (b) close the
sustained-300 <100ms gap, (c) then what only matters at 500-1000/s,
(d) measurement enablers interleaved where they gate a decision.
"Link" = community issue/PR to reference or rebase-watch.

| # | item | mechanism | expected saving | effort | risk | prerequisite | link |
|---|---|---|---|---|---|---|---|
| 1 | **Rebase onto #1118 + #1213** (§2) | re-express stale-pass + bounded-requeue invariants in #1118's finalization; adopt #1213's namespace flag | none directly — protects all prior gains (3-9.5×) from silent regression | **M** | med: rewritten finalizer can reintroduce echo/re-record storms — gate-zero re-run is the detector | #1118, #1213 merged upstream | PR #1118, PR #1213 |
| 2 | **Claim-side no-spec adoption** (design complete: ROUND6-COALESCING §3.3) | when `additionalPodMetadata` is empty: adoption patch becomes metadata-only (labels+annotations+ownerRef) → no generation bump → sandbox status echo never happens (`updateStatus` DeepEqual short-circuit) and the adoption-MODIFIED sandbox reconcile goes read-only; `reconcileActive` drift check compares modulo identity keys | −1 system-wide write/claim (ledger #7) + −1 sandbox reconcile; at 300/s that is −300 writes/s of background seat demand → est. −15-40ms on burst p90, −5-15ms sustained; −20% hot-path writes at 1000/s | **S-M** (claim controller; §3.3 spells out the 3 changes + crash-window audit; tests already specified) | low-med: drift-check relaxation must not reopen the back-door bump; KEP-0174 users keep today's path byte-for-byte | item 1 (edits `completeAdoption`, which #1118 rewrites); sandbox-side `--no-spec-adoption` (merged) | rebase-watch #1118 |
| 3 | **Create-ack reduction** (S0: 49ms → target 25-35ms) | (a) verify claim POSTs ride the exempt PL every run (`kubectl auth whoami` → `system:masters`); (b) stress-client `--client-connections` ≥ ceil(createConcurrency/100); (c) audit CR admission cost: schema + CEL validation per POST — `apiserver_request_duration_seconds{verb="POST"}` minus etcd time, quiescent; trim provably-hot rules only; (d) CBOR POST bodies+responses (item 4); (e) CP headroom = TUNE_CONTROL_PLANE + n2-standard-16 as table stakes | −15-25ms p50 on S0; the single largest floor-adjacent term | **S** | low; (c) touches validation only with data | gate-zero baseline for S0 | — |
| 4 | **CBOR serving/storage bench experiment** (KEP-4222; novel — nobody upstream is doing this for CR controllers) | kops 1.35 / k8s 1.35: server gate **alpha, off by default** (beta retargeted 1.37) — enable at cluster creation: `--set cluster.spec.kubeAPIServer.featureGates.CBORServingAndStorage=true`. Client side (controller AND stress client are plain client-go, so the env-var gate provider applies): `KUBE_FEATURE_ClientsAllowCBOR=true` + `KUBE_FEATURE_ClientsPreferCBOR=true` (case-sensitive; client-go v0.36.2 has both gates). Covers CRs (serving + storage). Safe fallback: server without the gate → 415 → process-wide JSON fallback | upstream benchmarks: up to ~8× faster CR encode / ~2× decode; expect −5-15ms on S1+S4 under load, −5-10ms on S0, real apiserver CPU cut (per-watcher json.compact tax on CR watch events, k8s #110146, disappears) | **S** (flags + one A/B leg) | med: alpha gate (bench-only until beta); **two traps:** (i) controller-runtime v0.24.1 builds its own client — verify on the wire (apiserver `-v=3` or `apiserver_request_total`) that `application/cbor`/`application/cbor-seq` is actually negotiated; if pinned to JSON, override at rest.Config before measuring; (ii) **no CBOR merge-patch media type exists** — hot-path PATCH *bodies* stay JSON; do not expect S3 to move | gate zero; fresh cluster (kops gates apply at creation) | KEP-4222; k8s #110146 |
| 5 | **Watch-path micro-opts — the real subset** | (a) keep watcher count on `sandboxclaims`/`sandboxes` minimal — every full watcher pays a separate CR JSON encode per event (cachingObject helps only ≥3 watchers; CR JSON watchers still pay per-watcher compaction); (b) verify watches serve from the watch cache, never etcd (no `TOO OLD` restarts; `apiserver_watch_cache_lag` flat); (c) `--separate-watch-connection` stays on for client AND controller; (d) informer decode cost: covered by CBOR + merged cache transforms | individually 2-10ms; collectively keeps S1/S4 near their 3-8ms floor at rate — hygiene that prevents regression more than a new win | **S** | low | none | k8s PR #84043 (cachingObject threshold) |
| 6 | **Sharding-at-rate benchmark** (8 shards × sustained, composed with #1213) | use **#1213's namespaced-mode flag** for scoping (not a competing fork flag); keep from R4.4: per-shard `LeaderElectionID`, 8-replica deployment topology, pool-per-namespace layout, per-shard scrape aggregation + max/mean queue-depth imbalance metric. Sustained 300 → 500 → 1000/s aggregate | proves (or falsifies) near-linear controller scale: 8 × ~150/s ≥ 1000/s hot path; splits watch-ingestion decode 8× | **M** (manifests + bench phase; zero adoption-logic code) | low-med: static assignment leaves hot-namespace skew (accepted until L2 dynamic sharding); watch item: k8s 1.36 KEP-5866 server-side sharded list/watch (`shardRange()` on uid/namespace, alpha) is the eventual successor — not usable on the 1.35 bench | sustained phase; item 1 (adopt #1213 flag); TUNE_CONTROL_PLANE | PR #1213; KEP-5866 |
| 7 | **Write-ledger audit: NetworkPolicy (#594) + pod-patch elimination via #435** | (a) audit whether NetworkPolicy writes ride the adoption path in any supported configuration (#594 says yes for some) — if so they join the ledger and the write-behind/coalescing scope decision must be revisited; (b) track/support **#435 (safe-to-evict = on-completion semantics)**: if the autoscaler marker no longer needs stripping *at adoption*, ledger #6 (the pod metadata patch) is **deleted, not deferred** — which also removes the 1s write-behind bound rationale and de-risks L1's async pod patch entirely | (a) correctness of every budget in this file for non-bench deployments; (b) −1 write/claim system-wide and one less crash-window to reason about | **S** (audit) + upstream-dependent (b) | low; (b) is upstream-paced — implement the fork switch behind the existing flag surface when #435 lands | none for (a); #435 merged for (b) | #594, #435 |
| 8 | **Refill/churn track for 1000/s** | (a) **node-local I/O wall**: justinsb's containerd fdatasync / overlayfs-volatile / NVMe work (**PRs #1203-#1208**) explains part of the measured 70-85/s per-pool refill ceiling — pull those into the bench nodes (NVMe ephemeral + containerd tuning) and re-measure the ceiling before buying more etcd; (b) **multi-etcd on kops**: extra `spec.etcdClusters` shards (documented `leases` shard is the precedent; `main`/`events` default) + `spec.kubeAPIServer.etcdServersOverrides` for `pods` and the `agents.x-k8s.io`/`extensions.agents.x-k8s.io` groups (~3-4× commit ceiling); validate on a throwaway cluster (nodeup only special-cases `/events` DNS; on-CP apiserver uses 127.0.0.1 ports, so moot); (c) **scheduler/KCM QPS**: already in TUNE_CONTROL_PLANE (800/1600, 200/400); confirm at 1000 pods/s via `scheduler_pending_pods` + EndpointSlice workqueue dashboards; (d) **Cilium identity explosion (#836)**: P99 pod-start >20s from high-cardinality labels — our per-sandbox `sandbox-name-hash` pod label (relied on by `--cache-label-selectors`) is exactly that shape; mitigation to bench: cilium label-filter excluding `agents.x-k8s.io/*` from identity computation; treat #836 as a compatibility risk for both the claim-UID label index and cache-label-selectors at scale; (e) **template→pools fan-out (#1182)**: same O(N²) shape as our fixed pool→claims fan-out — needed before the multi-pool sizing guidance (several 1-3k pools) is real; (f) **recycling decision gate (L6)**: one-page decision memo to the security owner (fresh netns/MAC required? node-local scrubber acceptable?) — engineering explicitly blocked on the answer; recycling converts 20-25 writes/claim → ~6 and is the only path where 1000/s doesn't imply a ~500-node pod pipeline | raises the churn wall from ~8k writes/s (one etcd) toward ~25-30k (sharded) AND removes the node-side refill brake — the difference between 500/s and 1000/s sustained | **M-L** (infra + bench; no controller code) | med operational (more etcds, cilium config); #836 mitigation must be verified not to break NetworkPolicy semantics that reference our labels; L6 high (isolation) — memo only | items 3-6 pointless above 500/s without this; sustained phase | PRs #1203-#1208; #836; #1182 |
| 9 | **Arrival-shaping guidance + SDK idempotent create (#1089)** | publish the §5 jitter formula in the stress tool AND SDK latency guidance; align with **#1089** (SDK idempotent-create / adopt-on-409) so client retries at sustained rate dedupe instead of double-claiming — retry storms are the arrival-shape failure mode the jitter math assumes away | converts burst-300 (p90 ≈ 345 predicted) into the sustained shape (p90 ≈ 90-205) for clients that can spread; #1089 keeps the sustained math valid under client retries | **S** | none — guidance + SDK review | none | #1089 |

Sequencing: gate zero → 1 (rebase) → gate-zero re-run → 2 → 3+4 (same
cluster pair) → 5 (continuous) → 6 → 7a alongside, 7b upstream-paced →
8a-e → 8f memo drafted from day one. Item 9 ships with gate zero's
sustained phase.

---

## 5. Arrival shaping: what spread makes burst-300 ≈ sustained-300

Model the composed system as a pipeline with drain rate **μ ≈ 350-500
claims/s** (measured: 300 ready in 0.86s pre-L1 ⇒ ~350/s; L1 halves
in-window write demand, so ≥400/s is conservative post-compose; re-anchor at
gate zero). For N simultaneous arrivals, the k-th ready claim waits ≈ k/μ, so:

- burst-300 p90 queue-position cost ≈ 0.9·300/μ ≈ **540-770ms** — this term
  alone dwarfs the <100ms target and no server-side work removes it;
- spreading N arrivals uniformly over T seconds gives arrival rate R = N/T.
  The queue stays ~empty when ρ = R/μ ≤ ~0.6, i.e. **T ≥ N/(0.6·μ)**. With
  μ = 400/s: **T ≥ 1.25s for N=300**. At ρ ≤ 0.6 each claim sees ≈ the
  uncontended path (§1.2 sustained column); between 0.6 < ρ < 1 delay grows
  roughly as ρ/(1−ρ) × service time (heavy-traffic approximation) — at
  ρ = 0.86 (300/s vs 350/s) expect ~2-3× the uncontended write RTTs, which
  is exactly the sustained-300 p90 gap in §1.2.

Client guidance to publish: **jitter each create by
`Uniform(0, max(1s, N/(0.6·μ_published)))`** when firing N claims
programmatically; per-claim create→Ready latency is measured from its own
POST, so the jitter costs the client only aggregate completion time
(≤ T + p99), not per-claim latency. Poisson arrivals at rate R behave the
same for ρ ≤ 0.6 (burstiness within the window is absorbed at that
utilization). The stress tool keeps the all-at-once phase deliberately — it
is the adversarial shape that finds pathologies (rounds 1-3 all hid there).

---

## 6. Dead ends researched and rejected this round

| rejected | reason |
|---|---|
| server-side batch/multi-object writes | final since ROUND6 §1: no API verb, no KEP through the 1.36 cycle; per-object raft commits are structural |
| protobuf for CRs | not available by design; CBOR is the successor (item 4) |
| **bookmark tuning** | the cacher's 1-1.25s bookmark timer affects only watch-*list* initialization and resumption, never steady-state event dispatch (each cacheWatcher dispatches real events immediately, non-blocking); there is no knob that moves per-event delivery latency |
| **WatchList / sendInitialEvents on the hot path** | streaming lists (KEP-3157) cut informer (re)LIST memory/CPU at startup — not per-event latency; server gate off-by-default in 1.33+ (superseded for lists by streaming collection encoding); worth flags only for restart/failover behavior, which is not the bottleneck (failover already ≤2s via lease release) |
| watch-cache size tuning (`--default-watch-cache-size` etc.) | CR watch cache is dynamic; zero `TOO OLD` restarts observed — not our failure mode |
| CBOR for hot-path PATCH bodies | no CBOR merge-patch media type (`application/apply-patch+cbor` is SSA-only, and SSA is a rejected regression here); S3's request body stays JSON — item 4 scoped accordingly |
| admission-time binding (mutating webhook picks the warm sandbox at claim CREATE) | adds an admission hop to S0 (GKE CL2 ran a webhook in path at p90 245ms), cannot write status at create, still needs the sandbox-side patch, and re-introduces the webhook subsystem the alpha-removal deleted — strictly worse than L1 |
| client-side "optimistic ready" (SDK treats POST ack as usable) | breaks the durability contract (§1.3 write #3); a crash between ack and bind strands users on a nonexistent sandbox |
| raising HTTP/2 max streams client-side | server-side flag only; transport sharding (merged) is the fix — re-confirmed, still dead |
| KEP-5866 server-side sharded list/watch, this round | alpha in k8s 1.36; bench pinned to kops/k8s 1.35 — tracked as successor to namespace sharding (item 6) |
| per-sandbox Service elimination as a round-7 item | already done/moot for the bench (ROUND5: measured zero Service/EndpointSlice churn; router name-index path shipped) |
| fork-private namespace-scoping flag | superseded by community PR #1213 — keeping ours would fork the config surface for zero gain (§2) |
| porting `triggeredAdoptions`/sentinel forward across #1118 verbatim | the maps are implementation, not contract; re-express the invariants in #1118's structure instead (§2) |

---

## 7. Why we can't do better than X — the stopping rule

**X ≈ 35ms p50 / 60-80ms p90 sustained.** The composed Ready path is
irreducibly: one durable commit to accept the claim, one watch hop so a
correct controller can act on committed state, one durable commit to bind,
one watch hop so the client observes committed state — 2 raft commits + 2
watch deliveries + 4 request-processing traversals over warm TLS. On this
environment those components sum to ≈30-45ms p50, and the measured
20-claim smoke floor (**31-40ms**) confirms the estimate is not theoretical:
we have already touched the floor when contention is absent. Every proposal
that beats it either drops a commit (optimistic ready — contract-breaking),
drops a hop (admission binding — adds a slower hop instead), or batches
commits (no such API — final).

Therefore: **when a sustained-300 run measures p90 ≤ ~100ms with S0 ≤ 35ms
and S3 ≤ 60ms, stop optimizing latency.** Remaining engineering effort at
that point buys *rate-holding*, not milliseconds — sharding (item 6), the
churn walls (item 8), and the L6 recycling decision are what stand between
300/s and 1000/s, and none of them move p90 at 300/s. Conversely, if the
target were ever revised below ~2× the smoke floor (<60-70ms p90 sustained),
the honest answer is "not on this architecture" — that regime requires
either co-located binding (custom apiserver = CRD break, out of bounds) or
optimistic contracts, both rejected.

References for the round-7 external claims: KEP-4222 CBOR gates and env-var
client gating (kubernetes.io feature-gates + client-go features docs; alpha
since 1.32, CRs covered, 415-fallback), kops extra etcd shards + overrides
(kops cluster_spec docs, `leases` shard; nodeup `kube_apiserver.go`), CR
watch per-watcher encode tax (k8s #110146; cachingObject ≥3 watchers, k8s PR
#84043), WatchList status (KEP-3157; disabled-by-default server-side in
1.33+), KEP-5866 sharded list/watch (k8s 1.36 alpha). Community links per
§2/§4: PRs #1118, #1213, #1203-#1208; issues #836, #594, #435, #1182, #1089
(details in COMMUNITY-SCAN.md).
