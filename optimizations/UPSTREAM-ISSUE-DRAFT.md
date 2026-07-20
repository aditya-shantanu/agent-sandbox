# [DRAFT — not yet filed] Umbrella: SandboxClaim warm-adoption latency at high burst rates — findings and fixes

**Scenario:** N SandboxClaims created simultaneously against a fully
provisioned SandboxWarmPool of N (measured at N=300; targets: 500, then
1000 claims/s sustained). Goal: minimize create→Ready for every claim.
Guardrail honored throughout: **no breaking CRD changes** (the one authorized
exception: dropping v1alpha1, task 20).

**Measured outcome** (kops GCP, in-region client; identical harness across
runs; baseline and round-4/5 rows on 6× n2-standard-8 workers, gate-zero row
on 34 workers + tuned control plane with clean instrumentation):

| | p50 | p90 | p99 | time to ALL 300 Ready |
|---|---|---|---|---|
| before | 2.54s | 5.55s | 19.1s | 20.25s |
| round-4/5 stack (instrumented) | 489ms | 584ms | 761ms | 0.86s |
| **gate-zero composed tree (clean)** | **273ms** | **301ms** | **309ms** | **0.31s** |
| improvement | 9.3× | **18.4×** | 62× | 65× |
| **sustained 300/s, warm supply present** (round-8 S2 warm-hit cohort) | **41ms** | **95-96ms** | 134ms | n/a (open-loop 60s arrivals) |

The sustained row is the claim path at a true 300/s arrival rate while pool
supply exists (first-10s warm-hit cohort of leg S2, n=1,517; window p90
drifts 60→96→105ms as refill contention builds) — the <100ms p90 target is
already being touched at rate; the burst rows are the adversarial
all-at-once shape. Decomposition: `PATH-TO-100MS.md`.

**Where the remaining milliseconds live (2026-07-20, PATH-TO-100MS.md):
writes are exonerated.** Server-side under burst: claim-status PATCH mean
15.8ms (100% ≤100ms), POST 22.7ms, APF wait 0.10-0.13ms on every priority
level, workqueue <1ms, zero conflicts. The binding burst constraint is the
**claims watch-event fan-out** — ~0.7-0.9ms/event (~1,350 events/s) per
watcher stream, traversed twice (apiserver→controller, apiserver→client) —
plus queue-rank domination (corr 0.767, drain ~1,120/s ⇒ ~240ms rank term
at 300-burst). That is why the open work below targets event-path encode
(CBOR), stream sharding, and per-adoption write elimination rather than
write RTTs.

The 301ms/0.31s row is the latest **verified** number (gate-zero leg B,
2026-07-20): the fully-composed tree — tasks 10, 12 and 17 on top of the
round-4/5 stack — benchmarked with clean instrumentation, 300/300 ready,
zero failures, zero cold starts, max 312ms. The composed flags alone (A→B on
the same cluster) were a 3.3× p90 gain (1000→301ms). Uncontended floor
(20-claim smoke, clean): **14-21ms** — the previously assumed 25-40ms floor
was instrumentation tax (see the observability section). Everything above
~20ms is burst contention, and each task below removes a piece of it.

**Sustained-rate honesty (updated after round 8, 2026-07-20):** gate zero's
300/s × 60s sustained leg FAILED (3,804/18,000 ready; 3,204 double-bound
sandboxes; refill starved to ~2-3 creates/s per pool). The round-8 fixes —
adoption reservation + DirectReader recovery (task 17) and pool-dedicated
connection + backlog-aware polling (task 8) — were then verified by a full
rerun (leg S2): **zero double-binds in 17,822 bindings, zero wedged losers,
refill recovered 15-60× to ~26-46 creates/s per pool, 15,821/17,822 ready,
rc=0**. The controller-side failure modes are gone. What remains at 300/s
sustained is **supply-side**: 300 claims/s beyond pool depth requires 300
pods/s reaching Ready, and a 34-node cluster delivers ~50-65 ready/s
(pod-create→scheduled p50 129s under queue). The claim path itself ran at
**119ms p50 at 300/s** while pool supply lasted. Sustained deployments must
be sized for pods-to-Ready throughput (nodes × per-node ready-rate ≥ arrival
rate); see task 8's caveat and `RESULTS.md` (round-8 verdict).

Full data, per-round A/Bs, and forensic artifacts: `optimizations/` on the
investigation fork (`aditya-shantanu/agent-sandbox`, branch
`perf-investigation-master`).

## Legend

- **Safety** — 🟢 completely safe (behavior-identical or default-off flag,
  fully tested, no semantic change) · 🟡 medium (documented semantic
  trade-offs, or needs an upstream decision) · 🔴 risky / needs design
  discussion (none currently).
- **Impact on claim latency** — `+` small (<5%) · `++` moderate · `+++`
  large (measured big win; number cited inline) · `++++` structural.
- **Status** — *merged* = on `perf-investigation-master` · *bench-only* =
  manifests excluded from release kustomization · *design* = written up,
  not implemented. As of gate zero (2026-07-20) every merged task has been
  benchmarked as part of the composed tree.

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
| 8 | Warm-pool refill shaping + supply isolation | 🟢 | ++ | merged (isolation verified round-8 S2) | [PR #913](https://github.com/kubernetes-sigs/agent-sandbox/pull/913), [#1182](https://github.com/kubernetes-sigs/agent-sandbox/issues/1182) |
| 9 | SDK single-watch ready wait | 🟢 | ++ | merged | [#574](https://github.com/kubernetes-sigs/agent-sandbox/issues/574), [#286](https://github.com/kubernetes-sigs/agent-sandbox/issues/286), [PR #565](https://github.com/kubernetes-sigs/agent-sandbox/pull/565) |
| 10 | Write-behind coalescing (sandbox controller) | 🟢 | ++ | merged | [#350](https://github.com/kubernetes-sigs/agent-sandbox/issues/350), [#594](https://github.com/kubernetes-sigs/agent-sandbox/issues/594) |
| 11 | Sandbox status writes race-proofed | 🟢 | + | merged | [#527](https://github.com/kubernetes-sigs/agent-sandbox/issues/527), [#350](https://github.com/kubernetes-sigs/agent-sandbox/issues/350), [PR #882](https://github.com/kubernetes-sigs/agent-sandbox/pull/882) |
| 12 | Generation-bump elimination (both halves) | 🟢 | + | merged (claim half: round 9a) | none |
| 13 | Leader-lease release on shutdown | 🟢 | + | merged | none — file fresh |
| 14 | Router name-cache (service-free resolution) | 🟢 | + | merged | [#883](https://github.com/kubernetes-sigs/agent-sandbox/issues/883), [#836](https://github.com/kubernetes-sigs/agent-sandbox/issues/836), [PR #850](https://github.com/kubernetes-sigs/agent-sandbox/pull/850) |
| 15 | Benchmark honesty (measurement fixes) | 🟢 | + | merged | [#403](https://github.com/kubernetes-sigs/agent-sandbox/issues/403), [#624](https://github.com/kubernetes-sigs/agent-sandbox/issues/624), [#781](https://github.com/kubernetes-sigs/agent-sandbox/issues/781), [#1182](https://github.com/kubernetes-sigs/agent-sandbox/issues/1182) |
| 16 | APF insulation + seat sizing | 🟡 | +++ | bench-only | none — novel |
| 17 | L1 one-write adoption (+ reservation/DirectReader hardening) | 🟡 | +++ (measured) | merged (hardening verified round-8 S2) | [#435](https://github.com/kubernetes-sigs/agent-sandbox/issues/435), [#418](https://github.com/kubernetes-sigs/agent-sandbox/issues/418), [#478](https://github.com/kubernetes-sigs/agent-sandbox/issues/478); rebase-watch [PR #1118](https://github.com/kubernetes-sigs/agent-sandbox/pull/1118) |
| 18 | Collapse adoption transaction 3 writes → 2 | 🟡 | ++ | merged | [#478](https://github.com/kubernetes-sigs/agent-sandbox/issues/478); rebase-watch [PR #1118](https://github.com/kubernetes-sigs/agent-sandbox/pull/1118) |
| 19 | Horizontal scale via namespace sharding | 🟡 | ++ (rate-holding) | merged; re-scope onto [PR #1213](https://github.com/kubernetes-sigs/agent-sandbox/pull/1213) | [#484](https://github.com/kubernetes-sigs/agent-sandbox/issues/484), [PR #924](https://github.com/kubernetes-sigs/agent-sandbox/pull/924) |
| 20 | Serve v1beta1 only; drop v1alpha1 + conversion webhook | 🟡 | + | merged | [#751](https://github.com/kubernetes-sigs/agent-sandbox/issues/751); conflicts [PR #1188](https://github.com/kubernetes-sigs/agent-sandbox/pull/1188), [PR #1106](https://github.com/kubernetes-sigs/agent-sandbox/pull/1106) |

Novelty note (verified in the 2026-07-19 community scan): nobody upstream is
pursuing transport sharding, APF insulation for controller traffic classes,
or CBOR for CR controllers — tasks 1, 16 and the CBOR follow-up are entirely
novel contributions.

**Default-flip note (2026-07-20):** after gate-zero leg B validated the
composed tree with zero failures, five flags flipped to default-on in
`cmd/agent-sandbox-controller/main.go`: `--separate-watch-connection`,
`--api-connections=4`, `--one-write-adoption`,
`--sandbox-write-behind-window=250ms`, `--no-spec-adoption`. Every flip is
reversible per-flag (help text documents the legacy value). The per-task
"Default" lines below reflect the post-flip state.

---

## 🟢 Completely safe

### Task 1: 🟢 ++++ — Watch/write connection separation + sharding

**Problem.** controller-runtime gives the manager's informer cache and every
write the same single HTTP/2 connection, and kube-apiserver's
`SETTINGS_MAX_CONCURRENT_STREAMS` (server default 100) caps in-flight
requests per connection. Two measured consequences at burst-300: (1) watch
events for the informer cache queued behind hundreds of write streams and
arrived ~1.0-1.1s late at p50 — ~60% of end-to-end latency was the
controller literally not yet knowing what had happened; (2) effective write
concurrency plateaued at ~100-110 regardless of `--sandbox-*-workers=400/400`
or QPS settings, so 800 configured workers behaved like ~100.

**Fix (mechanism).** Two flags in `cmd/agent-sandbox-controller/main.go` +
`transport.go`: `--separate-watch-connection` moves the manager cache's
list/watch streams onto a dedicated HTTP/2 connection so watch delivery can
never stall behind write bursts; `--api-connections=N` pre-establishes N
independent connections for non-watch traffic and shards requests
round-robin (~N×100 in-flight ceiling). No behavior change other than
transport topology; the API surface, ordering and retry semantics are
untouched.

**Validated.** `transport_test.go` proves `--api-connections=N` dials
exactly N distinct TCP connections, all serving requests. Round-3 A/B:
p90 2479→1740ms with the split+sharding as the core change. Every
subsequent round including gate zero ran on 4 connections; gate-zero leg B
(301ms p90) includes both flags.

**Default (post-flip):** `--separate-watch-connection=true`,
`--api-connections=4`. Set `=false` / `=1` to restore the stock
single-shared-connection client exactly.

### Task 2: 🟢 +++ (all-ready 7.86→3.45s with task 3) — Stale-cache re-adoption guard

**Problem.** Reconciles triggered on stale pre-adoption claim views
re-entered adoption or the cold path. Measured at burst-300 (round 2, leg
A): **4,357 reconciles for 300 claims**; ~45% of adoption writes ended in
409 conflicts; 144 duplicate cold-path sandboxes created and thrown away;
911 startup-latency histogram observations for 300 claims (re-record
pollution — the [#940](https://github.com/kubernetes-sigs/agent-sandbox/issues/940)
bug), which had been silently corrupting every histogram-derived quantile.

**Fix (mechanism).** In-memory per-claim-UID fingerprints in the claim
controller, consulted before adoption entry, before cold-start entry, and
before status writes: a reconcile pass whose cached view is older than the
last committed transition performs **zero writes**. Duplicate metric records
are suppressed by the same fingerprint.

**Validated.** Round-2 A/B on one cluster: adoption conflicts ~45%→~0,
stale passes doing writes hundreds→0 (521 suppressed per burst), all-ready
7.86s→3.45s (with task 3). Unit tests pin the guard; interceptor-counted
tests prove stale passes are write-free.

**Default:** always on (no flag — removing wasted writes has no trade-off).
**Rebase note:** [PR #1118](https://github.com/kubernetes-sigs/agent-sandbox/pull/1118)
rewrites adoption finalization — the invariant (never re-enter adoption from
a stale cache view) must be re-expressed in its structure, not the map
ported.

### Task 3: 🟢 +++ (cold starts 183→0; was the entire p99 tail 4.4-6.7s) — Cold-start guard + adoption optimistic lock

**Problem.** Two compounding bugs. (a) Momentary warm-queue exhaustion sent
claims to the cold path even with 300 adoptable sandboxes present — the
queue was the only source consulted, there was no List fallback. Round-2
leg A's cold-start reason logging counted **183 "warm pool queue empty"
fallthroughs in one 300-burst**; the resulting cold pod creations (~3-6s
each) were the entire 4.4-6.7s p99 tail of rounds 1-2. (b) The adoption
patch carried no optimistic lock, so two claims could silently adopt the
same sandbox (exactly
[#418](https://github.com/kubernetes-sigs/agent-sandbox/issues/418)).

**Fix (mechanism).** Before cold-starting, the claim controller does an
indexed cache List for adoptable pool members; if any exist it requeues
with a bounded 50ms delay instead of creating a pod. Popped candidate keys
return to the queue immediately on failed adoption. The adoption patch uses
`MergeFromWithOptimisticLock`, so a double-adoption becomes a 409 whose
loser re-adopts a different candidate instead of a silent steal.

**Validated.** Cold starts at burst-300 in every run since: **0** (round
4/5: 55 "Deferring cold start" guard hits, zero actual cold starts; gate
zero: zero). Covered by claim-controller unit tests including the
409-loser path.

**Default:** always on (no flag).

### Task 4: 🟢 ++ — Kill the pool→claims watch fan-out (O(N²) reconciles)

**Problem.** The claim controller watched SandboxWarmPool and mapped every
pool event to **every claim referencing that pool**. Pool status churns
continuously during a burst (replicas/readyReplicas counters), so each
status write re-enqueued all N claims: O(N²) no-op reconciles that
saturated the workers and delayed first-pass adoption — a large share of
the 4,357-reconciles-for-300-claims measurement in task 2, and
[#527](https://github.com/kubernetes-sigs/agent-sandbox/issues/527)'s storm.

**Fix (mechanism).** `GenerationChangedPredicate` on the pool watch
(status-only writes no longer map) plus the mapper skips already-bound and
deleting claims. Adoptable-sandbox wakeups were verified to flow through
the Sandbox watch → warm queue path, never the pool fan-out, so no wakeup
is lost.

**Validated.** Reconcile counts collapsed to ~1 winning pass per claim
(round-2 leg B); mapper behavior pinned by unit tests.

**Default:** always on (no flag).

### Task 5: 🟢 ++ — Adoption conflicts → bounded requeue, not exponential backoff

**Problem.** A 409 on the adoption write routed the claim through
controller-runtime's per-item exponential backoff (5ms·2^k). Under burst
contention this produced visible retry waves at 0.8/1.2/1.6s — losing a
race once pushed a claim's latency out by whole backoff steps, and the
waves re-synchronized the herd.

**Fix (mechanism).** Conflict sentinel → `nil` error + 50ms `RequeueAfter`,
with the popped candidate returned to the queue so the retry pass has a
fresh candidate immediately (same pattern as
[PR #1072](https://github.com/kubernetes-sigs/agent-sandbox/pull/1072) for
[#1042](https://github.com/kubernetes-sigs/agent-sandbox/issues/1042)).

**Validated.** Retry-wave signature gone from round-2 leg B latency
distribution; with tasks 2-3 conflicts themselves are ~0 so the path is
rarely exercised — it remains as the graceful floor. This data argues
against [#1059](https://github.com/kubernetes-sigs/agent-sandbox/issues/1059)'s
CRD retry knob. Rebase-watch:
[PR #1118](https://github.com/kubernetes-sigs/agent-sandbox/pull/1118).

**Default:** always on (no flag).

### Task 6: 🟢 ++ (with task 7, round-4/5 flags: p90 1094→584ms) — Write-payload and CPU reductions

**Problem.** Small metadata patches were built via full-object
DeepCopy+MergeFrom diffing. Burst profiles: `persistStampedAnnotations`'s
full-object merge patch alone was **15.8% of controller CPU** (round 2);
round-3 profile: `mergeFromPatch.Data` 11.4%, total JSON encode/decode
18.3% of burst CPU (~33% in round 2). Two further write classes rode the
hot path: the claim observability-annotation flush (1 write per claim,
~1/3 of claim-controller hot-path writes) and claim Events (~300 POSTs
per burst).

**Fix (mechanism).** `internal/rawpatch` builds targeted metadata merge
patches directly (no DeepCopy, no diff) for the three hot call sites
(`persistStampedAnnotations`, `initializeSandboxLaunchTypeLabel`, sandbox
pod-name annotation). Two opt-in flags remove the optional write classes:
`--disable-claim-observability-annotations` (−1 write/claim; in-memory
stamping kept so metrics/traces still work) and `--disable-claim-events`
(hot-path `Eventf` becomes a no-op via nil recorder).

**Validated.** Interceptor tests capture actual wire bytes per call site
and assert byte-identical wire format vs the legacy path; a test proves a
warm adoption completes with zero non-status claim writes and still binds.
Round-4/5 A/B (with task 7's flags): p90 1094→584ms. Note:
[PR #1087](https://github.com/kubernetes-sigs/agent-sandbox/pull/1087)/[PR #1114](https://github.com/kubernetes-sigs/agent-sandbox/pull/1114)
fix the same metrics bug with a +1-write annotation — worth reconciling.

**Default:** rawpatch always on (byte-identical). The two flags remain
default-off; benchmarks run with both on.

### Task 7: 🟢 ++ (with task 6 flags: p90 1094→584ms) — Informer cache diet

**Problem.** Pod and Service informers were cluster-wide and full-object:
label predicates filtered **after** decode, so the controller paid list/
watch volume, JSON decode CPU and cache memory O(cluster) instead of
O(sandboxes); kubelet-inflated `managedFields` were decoded and cached on
every object despite nothing in the repo reading them.

**Fix (mechanism).** `Cache.DefaultTransform = TransformStripManagedFields()`
for all cached objects; `PodCacheTransform` drops the pod spec except
`spec.nodeName` (verified the only spec field read); opt-in
`--cache-label-selectors` scopes Pod/Service watches **server-side** to the
sandbox tracking label. Documented caveat: externally pre-provisioned
adoptables must carry the tracking label under the flag (watch
[#836](https://github.com/kubernetes-sigs/agent-sandbox/issues/836): if
high-cardinality labels move to annotations, the flag must adapt).

**Validated.** `TestPodCacheTransformMergePatchUnaffected` proves merge
patches built from stripped cache objects are byte-identical to unstripped
ones and contain no `spec`/`managedFields` keys. Round-4/5 leg B (with
task 6 flags): p90 1094→584ms.

**Default:** transforms always on; `--cache-label-selectors` default-off
(external-adoptable contract), on in all benchmarks.

### Task 8: 🟢 ++ — Warm-pool refill must yield to the burst (and flow under sustained load)

**Problem.** Refill raced 300 sandbox+pod creates against the adoption
burst for API budget the moment the pool drained; the naive
"defer-on-drop" fix re-arms its hold on every drop, so under sustained
arrivals refill would never start and the pool drains to zero.

**Fix (mechanism).** Two composing flags on the SandboxWarmPool controller
(`extensions/controllers/sandboxwarmpool_controller.go`):
`--sandbox-warm-pool-replenish-delay` defers the START of refill after
member drops (burst gets the API budget first);
`--sandbox-warm-pool-max-refill-rate` is a per-pool token bucket
(capacity = 1s of creates) that shapes refill FLOW into a smooth stream
instead of full-deficit bursts. Both default 0 = exact legacy behavior.
Isolated per-pool refill ceiling measured at ~70-85/s (create→Ready incl.
pod start); sizing formula documented in the flag comments.

**Refill starvation fix (round 8, verified by leg S2).** Gate-zero leg S
showed shaping is necessary but not sufficient: at 300/s × 60s the refill
loop was starved to ~2-3 creates/s per pool (30-50× under its own token
bucket) by claim-side contention on shared resources — refill POSTs shared
the round-robin HTTP/2 write shards with 400 claim workers' backlog
traffic, and ~14k pending claims polling the empty pool at a flat 50ms
burned the pool workers' CPU/workqueue turns. Two additional mechanisms
fix this: **`--pool-dedicated-connection`** (default on) gives pool member
creates/deletes their own HTTP/2 connection (same `newIsolatedHTTPClient`
mechanism as `--separate-watch-connection`), and **backlog-aware polling**
— cold-start deferral requeues grow 50ms→500ms with consecutive deferrals,
cutting empty-pool poll load ~10× exactly when the pool needs headroom.

**Validated.** Unit tests pin bucket accrual/requeue math;
replenish-delay=20s ran in every burst benchmark since round 2 (refill
observably deferred past the burst window). The round-8 sustained rerun
(leg S2, 300/s × 60s, same scenario that collapsed in leg S) measured the
contended refill at **~26-46 creates/s per pool (103-183/s aggregate,
best-10s 260/s)** — a 15-60× recovery that moved refill off the critical
constraint (creates outran pod-Ready 3-4×). Supersedes stale
[PR #913](https://github.com/kubernetes-sigs/agent-sandbox/pull/913); the
[#1182](https://github.com/kubernetes-sigs/agent-sandbox/issues/1182)
template→pools fan-out is the adjacent unfixed cliff.

**⚠ Sustained-load caveat (updated after round 8).** With the controller
fixed, the honest remaining constraint at sustained rates is the
**cluster's pods-to-Ready throughput**: leg S2 completed 15,821/17,822
(89%, rc=0, zero double-binds/wedges) but a 34-node pipeline delivers only
~50-65 pods/s to Ready against 300/s arrivals (pod-create→scheduled p50
129s under queue), so claims beyond pool depth wait on supply (e2e p50
141s) and the slowest 2,006 hit per-claim timeouts. The claim path itself
held 119ms p50 while pool supply lasted. Sizing rule for sustained R/s:
pool absorbs the transient; beyond it, `nodes × per-node-ready-rate ≥ R`
(≈1.5-2 ready/s per n2-standard-8 node measured) — or raise per-node rates
(node-local I/O work,
[PRs #1203-#1208](https://github.com/kubernetes-sigs/agent-sandbox/pull/1203))
or remove pod churn from the steady state (recycling, security-gated).
This is a deployment-sizing constraint, not a controller defect. Full
data: `RESULTS.md` round-8 verdict.

**Default:** shaping flags default 0 (legacy immediate full-deficit
refill); `--pool-dedicated-connection` defaults on (set `=false` for the
legacy shared transport).

### Task 9: 🟢 ++ — SDK ready-wait was paying an extra round trip

**Problem.** The Python SDK ran two sequential watches (claim → then
sandbox) although the claim's first status event already carries the bound
sandbox name and the forwarded Ready condition — one full watch
setup/teardown of pure overhead per claim; dev-mode port-forward polled at
500ms ([#574](https://github.com/kubernetes-sigs/agent-sandbox/issues/574),
[#286](https://github.com/kubernetes-sigs/agent-sandbox/issues/286)).

**Fix (mechanism).** Single claim-watch `wait_for_claim_ready()` that
resolves on the claim's own status (legacy two-watch methods kept,
API-compatible); port-forward poll interval 500→50ms; latency guidance
added to the SDK README.

**Validated.** SDK unit tests; used by the stress harness's claim phases
in every round since.

**Default:** new method available; existing call sites migrated in-repo.

### Task 10: 🟢 ++ — Write-behind coalescing for recoverable writes

**Problem.** Recoverable sandbox-controller writes (pod safe-to-evict
strip, pod-name annotation) competed for APF seats inside the
latency-critical burst window — ~846 writes/s offered against a ~600-900/s
seat-limited service rate — and repeated mutations to the same object each
paid a full write RTT
([#350](https://github.com/kubernetes-sigs/agent-sandbox/issues/350)'s theme).

**Fix (mechanism).** `internal/writebehind` flusher +
`--sandbox-write-behind-window`: pending mutations to the same object merge
into ONE merge patch flushed within the window; pod patches additionally
bounded ≤1s so the adoption-path safe-to-evict strip cannot lag the cluster
autoscaler; pending patches drain on graceful shutdown; crash recovery is
the normal level-based reconcile (informer state is the source of truth).
Only recoverable metadata writes are routed; status writes, creates/deletes
and ownership transfers stay synchronous.

**Validated.** `internal/writebehind` unit tests + `round6_coalesce_test.go`
pin merge/flush/drain/crash-recovery semantics. Benchmarked in gate-zero
leg B as part of the composed tree (250ms window): 300/300, zero failures,
p90 301ms — the A→B 3.3× composed gain includes this task. Related ledger
gap: [#594](https://github.com/kubernetes-sigs/agent-sandbox/issues/594)
(NetworkPolicy writes on the adoption path) still needs auditing.

**Default (post-flip):** `--sandbox-write-behind-window=250ms`; set `=0` to
restore the fully-synchronous stock write path.

### Task 11: 🟢 + — Sandbox status writes race-proofed

**Problem.** The sandbox controller's `Status().Update` from a possibly
stale cache raced the claim controller's adoption patch → 409s → backoff
sitting directly on the pod-Ready→claim-Ready chain.

**Fix (mechanism).** `Status().Patch(MergeFrom)` — the sandbox controller
is the sole status writer and recomputes all fields each pass, so a merge
patch is always safe and conflict-free.

**Validated.** Interceptor-counted tests verify no-op reconciles are
write-free. Same change as
[PR #882](https://github.com/kubernetes-sigs/agent-sandbox/pull/882)'s item
1 — coordinate, and adopt its `agent_sandbox_writes_total` metric idea.

**Default:** always on (no flag).

### Task 12: 🟢 + — Generation-bump elimination, both halves *(claim half landed round 9a)*

**Problem.** Adoption's `spec.podTemplate` rewrite bumps
`metadata.generation`, forcing one sandbox status write (observedGeneration
echo) per adoption — a write a metadata-only adoption never needs.

**Fix (mechanism).** `--no-spec-adoption`, one flag driving both halves.
Sandbox half: ownership-derived safe-to-evict hygiene — when a Sandbox is
claim-controlled, the safe-to-evict template annotation is treated as
absent (never propagated, stripped if present) regardless of whether the
claim controller rewrote `spec.podTemplate`. Claim half (round 9a): when
`additionalPodMetadata` is empty, the adoption patch is METADATA-ONLY
(labels/annotations/ownerRef; no `spec` key → no generation bump → the
sandbox status echo never fires and the adoption-MODIFIED sandbox reconcile
goes read-only), and the steady-state drift check compares the pod template
modulo system-reserved keys so the spec is never rewritten through the back
door. KEP-0174 users (non-empty `additionalPodMetadata`) keep today's spec
rewrite byte-for-byte. Behavioral deltas: claim-owned cold-start pods that
explicitly set safe-to-evict=true also have it suppressed (strictly safer);
pool bookkeeping labels remain in the (pod-inert) spec template of
fast-path-adopted sandboxes.

**Validated.** Unit tests pin both halves (hygiene: write-count proof in
`round6_coalesce_test.go`; claim side: metadata-only patch byte-identity,
back-door-bump absence, KEP-0174 contrast, one-write async + crash-recovery
metadata-onlyness in `nospec_adoption_test.go`). Sandbox half benchmarked in
gate-zero leg B; the composed −1 write/claim is measured in the 9b run.

**Default (post-flip):** `--no-spec-adoption=true`; set `=false` to restore
legacy behavior of both controllers.

### Task 13: 🟢 + — Graceful failover (leader-lease release)

**Problem.** `LeaderElectionReleaseOnCancel` was unset, so every rolling
update waited out the full 15s LeaseDuration with **no active controller**
— at a sustained 500/s that is ≈7,500 claims queued during a routine
deploy.

**Fix (mechanism).** Release the lease on clean shutdown → ~0-2s handover.
Crash failover (lease expiry) is unchanged by design.

**Validated.** 660-line regression suite pins restart blast radius; echo
relapse after restart bounded to one write per claim.

**Default:** always on.

### Task 14: 🟢 + — Router resolution without per-sandbox Services

**Problem.** Per-sandbox headless Services (+~2 EndpointSlice writes each,
~20-25% of churn writes where used) existed largely because the router's
UID-less fallback was DNS — the exact 502/NXDOMAIN failure mode of
[#883](https://github.com/kubernetes-sigs/agent-sandbox/issues/883).

**Fix (mechanism).** The router gains namespace/name cache resolution
(PodIP header → UID cache → name cache → DNS last), making
`spec.service: false` viable — the field already exists, no CRD change.
The benchmark template is pinned service-free.

**Validated.** Router unit tests for the resolution chain; service-free
benchmark runs since round 5 (`ROUND5-NO-SERVICE.md`). Complements the
Envoy direction
([PR #850](https://github.com/kubernetes-sigs/agent-sandbox/pull/850)):
framed as making service-free viable for any router.

**Default:** cache resolution always on; `spec.service` default unchanged.

### Task 15: 🟢 + — Benchmark honesty

**Problem.** Multiple measurement bugs hid the truth — detailed with their
individual payoffs in the observability section below. Headlines:
1s-truncated CreationTimestamp math inflated queue-wait ~30× (reported
p50 1025ms vs 33ms true —
[#781](https://github.com/kubernetes-sigs/agent-sandbox/issues/781)); the
zap sampler silently dropped the slow cohort's timing lines (survivor bias
on every log-derived quantile); the stress client shared one HTTP/2
connection (the same 100-stream cap as task 1) and inflated reported
create-ack ~2-3×; instrumented controller builds overstated the latency
floor itself (gate-zero clean smoke floor 14-21ms vs 31-40ms instrumented).

**Fix (mechanism).** Burst + sustained (Poisson) stress phases;
never-sampled per-adoption phase timing; monotonic queue-latency
measurement; REST-client latency histograms; in-burst pprof;
`--client-connections=N` calibration; clean-instrumentation benchmark
discipline (gate zero runs stock logging, no pprof-debug, no `-v=3`).

**Validated.** Each fix cross-checked against the client watch stream
(gate zero: clean legs vs instrumented history). Should be filed against
the benchmark umbrella
([#403](https://github.com/kubernetes-sigs/agent-sandbox/issues/403),
[#624](https://github.com/kubernetes-sigs/agent-sandbox/issues/624)) and
rebased onto
[PR #1148](https://github.com/kubernetes-sigs/agent-sandbox/pull/1148)'s
harness split.

**Default:** harness-side; no controller behavior change.

---

## 🟡 Medium — documented trade-offs or needs an upstream decision

### Task 16: 🟡 +++ (p90 1740→1094ms) — APF insulation and the seat-sizing lesson

**Problem.** All controller traffic shared `workload-low` with every
tenant's; and once insulated, the first sizing (≈78 seats for the critical
class) became the bottleneck itself — measured demand high-watermark **272
seats** with APF wait p99 359ms, i.e. queueing at our own priority level.
The deeper finding: at default `--max-mutating-requests-inflight=200`
(≈600 seats total) the seat pool, not the CPU, is the write-side wall —
round-4 attribution showed ~60% of each write RTT was client-side in-flight
queueing, the backpressure shadow of the seat ceiling.

**Fix (mechanism).** Dedicated PriorityLevelConfigurations/FlowSchemas
(`k8s/apf-insulation.yaml`): claim path > bulk refill > events, with the
sizing rule `seats(critical) ≥ 2× demand HWM`. Because APF shares are
fractions of the total, raising `--max-mutating-requests-inflight`
multiplies every level's seats with no manifest change (200→1000 took the
bench 600→4000 seats).

**Validated.** Bench A/B: 200→1000 mutating-inflight = p90 1740→1094ms
with zero code change (round 4/5); demand HWM + wait p99 measured from
apiserver APF metrics; sizing re-derivation documented in the manifest.

**Why 🟡:** bench-only manifest today (excluded from release
kustomization); shipping defaults need an upstream sizing decision.

### Task 17: 🟡 +++ (measured: composed A→B p90 1000→301ms, 3.3×) — L1 one-write adoption

**Problem.** Even fully optimized, Ready waited on two serial in-window
writes (sandbox adoption patch → claim status patch); the round-4/5 584ms
residual decomposed to create-ack + watch hops + 2 write RTTs at
~100-150ms each under burst. One of those RTTs was structural, not load.

**Fix (mechanism).** `--one-write-adoption`: the claim status patch
(binding + podIPs + forwarded Ready) becomes the FIRST and only critical
write; the sandbox-side patch (ownership transfer, pool-label removal,
claim-uid label, safe-to-evict strip) is applied asynchronously by a
bounded flush worker (sub-second target) that keeps the optimistic lock as
the cross-process safety net — if it loses, the controller re-verifies and
on a genuine steal clears the stale binding and re-adopts (bounded retries,
loud logs). Claims observe Ready after ONE write RTT; hot-path write
concurrency halves.

**Overload hardening (round 8): reservation semantics + DirectReader
recovery.** Gate-zero leg S exposed the one-write visibility window under
backlog: a popped-but-not-yet-patched sandbox still looks pool-owned in
every cache view, so watch events re-queued it and a second claim
status-bound the same sandbox (3,204 double-binds; ~4.3k losers wedged to
timeout because recovery also read through the lagging cache) — the
sustained-scale recurrence of
[#418](https://github.com/kubernetes-sigs/agent-sandbox/issues/418)'s
double-adoption and
[#478](https://github.com/kubernetes-sigs/agent-sandbox/issues/478)'s
failed-write wedge. Fix: (a) queue pops **reserve** the key — `Add` drops
reserved keys, give-backs go through `Release`, deletes through `Forget`,
and reservations survive terminal adoption outcomes until the sandbox
DELETE event, so no watch event can re-queue an in-transaction or adopted
sandbox; (b) the flusher's post-409 re-verify and `recoverLostAdoption`
read through a **DirectReader** (`mgr.GetAPIReader()`), so recovery
decisions never depend on informer convergence and a genuine loser unbinds
in one RTT; (c) crash-window conflicts reclassify from a direct read (no
409-storm loop); (d) the cold-start guard excludes reserved phantoms.

**Validated.** `onewrite_adoption_test.go` incl. idempotent crash-window
recovery pinned at N=50. Benchmarked in gate-zero leg B (with tasks 10+12):
burst-300 p50 273 / p90 301 / p99 309 / max 312ms, all-ready 0.31s, zero
failures — a 3.3× p90 composed gain over the same cluster's leg A, landing
inside the §1.2 projection (205-345ms). The overload hardening is
**verified by the round-8 sustained rerun (leg S2)**: 300/s × 60s under a
multi-thousand-claim backlog produced **0 double-bound sandboxes in 17,822
bindings and 0 wedged losers** (leg S: 3,204 / ~4.3k), with the smoke floor
intact (21ms p50) and the first contended window at 119ms p50. Regression
tests reproduce both leg-S signatures pre-fix and pass post-fix; `-race`
clean.

**Why 🟡:** sub-second autoscaler-eviction window until the async patch
lands — [#435](https://github.com/kubernetes-sigs/agent-sandbox/issues/435)
(`safe-to-evict=on-completion`) would eliminate it; multi-process steal
window widened (leader election makes it rare, namespace sharding removes
it). Rebase-watch:
[PR #1118](https://github.com/kubernetes-sigs/agent-sandbox/pull/1118).

**Default (post-flip):** `--one-write-adoption=true`; set `=false` to
restore the 2-write transaction exactly.

### Task 18: 🟡 ++ — Collapse the adoption transaction 3 writes → 2

**Problem.** The critical path was claim Update (annotation lock) →
sandbox patch → claim status patch: three serial RTTs, each 100-216ms
under burst vs ~30ms server commits — the transaction shape, not the
server, set the latency.

**Fix (mechanism).** The optimistically-locked sandbox patch **is** the
adoption lock; the annotation moved to a deferred post-Ready flush. Crash
recovery via a claim-UID label index + `IsControlledBy` — which also
closes [#478](https://github.com/kubernetes-sigs/agent-sandbox/issues/478)'s
double-adoption-on-failed-status-write.

**Validated.** Restart/recovery tests at N=50; round-3 A/B carried this
change (p90 2479→1740ms with task 1). Superseded on the hot path by task
17 (which defers the sandbox patch too) but remains the shape of the
legacy `--one-write-adoption=false` path.

**Why 🟡:** annotation may land a beat after Ready; cross-process
double-bind protection now rests on leader election + recovery List.

### Task 19: 🟡 ++ (rate-holding at 500-1000/s, not p90 at 300/s) — Horizontal scale without API changes

**Problem.** One active process must ingest every watch event — a vertical
ceiling as rates rise, regardless of per-claim latency wins.

**Fix (mechanism).** Static namespace sharding via `--watch-namespaces`:
server-side scoped informers + per-shard leader Lease (Lease ID suffixed
with a namespace-list hash, so N instances with disjoint lists elect
independently). Safe because adoption exclusivity rests on the
optimistic-lock patch (task 3), not process-local state, and adoption is
namespace-local.

**Validated.** Unit tests for lease-ID derivation and scoping; example
manifest `k8s/namespace-sharding-example.yaml`. Not yet load-tested
multi-instance (the sustained-rate work it exists for is blocked on the
leg-S findings, task 8).

**Why 🟡:** [PR #1213](https://github.com/kubernetes-sigs/agent-sandbox/pull/1213)
already ships a namespaced mode — this task must adopt its flag surface
(keeping only per-shard Lease + topology + the safety audit) rather than
introduce a competing flag
([#484](https://github.com/kubernetes-sigs/agent-sandbox/issues/484)).

### Task 20: 🟡 + — Serve v1beta1 only; drop v1alpha1 and the conversion webhook

**Problem.** All four CRDs served alpha+beta with webhook conversion,
forcing the webhook server, cert bootstrap, and caBundle patching to exist
even though every in-repo actor speaks v1beta1 (the storage version).

**Fix (mechanism).** Delete both v1alpha1 trees, single-version CRDs with
`conversion: None`; makes `--enable-webhook=false` fully safe. −22,713
lines.

**Validated.** Full test suite green without the webhook server; benchmark
clusters run `--enable-webhook=false` since round 1.

**Why 🟡:** clusters with `v1alpha1` in `status.storedVersions` need
storage migration first; migration periodics from alpha need a decision;
[#751](https://github.com/kubernetes-sigs/agent-sandbox/issues/751)'s
webhook-stamped metric becomes permanently impossible unless SDK-stamped;
conflicts with
[PR #1188](https://github.com/kubernetes-sigs/agent-sandbox/pull/1188)/[PR #1106](https://github.com/kubernetes-sigs/agent-sandbox/pull/1106).

---

## Remaining work (known, scoped, not in this issue)

- **Supply-side sustained demonstration — IN PROGRESS (round 9b).**
  Round-8 leg S2 closed the controller side (zero double-binds/wedges,
  refill unstarved, 89% completion at 300/s on 34 nodes; `RESULTS.md`).
  The 9b run measures the composed 9a items on a supply-adequate cluster.
  Standing paths: (1) node-count sizing (`nodes × per-node-ready-rate ≥ R`;
  ~1.5-2 ready/s per node measured), (2) per-node ready-rate levers
  (node-local I/O
  [PRs #1203-#1208](https://github.com/kubernetes-sigs/agent-sandbox/pull/1203),
  cilium/kubelet/scheduler QPS), (3) L6 recycling (security-gated).
- **Round-9a items — LANDED on the fork, awaiting the 9b measurement:**
  claim-side no-spec adoption (task 12, both halves now merged); create-ack
  riders (S0 decomposed from the leg-B scrape: etcd ~11.6ms + ~10ms
  handler/encode of the 22.7ms server mean, payloads already thin at
  203B/273B — implemented the APF exempt-PL preflight + client-connection
  calibration warning in the harness; remaining S0 headroom rides on CBOR);
  CBOR serving/storage A/B wiring (`TUNE_CBOR=true`, KEP-4222 — novel,
  nobody upstream pursuing it for CR controllers; verify content-type on
  the wire, merge-patch bodies stay JSON); adoption segment histograms
  (clean legs self-decompose); sharded per-namespace claims watch in the
  harness (SUB-FLOOR 5b).
- **Rebase-watch:** [PR #1118](https://github.com/kubernetes-sigs/agent-sandbox/pull/1118)
  (still OPEN as of 2026-07-20; rewrites adoption finalization — re-express
  the stale-pass/bounded-requeue invariants in its structure when it
  merges) and [PR #1213](https://github.com/kubernetes-sigs/agent-sandbox/pull/1213)
  (adopt its namespaced-mode flag for task 19).
- **Speculative pre-binding: demoted to CONDITIONAL** (was "researched,
  deferred"). Decision rule (PATH-TO-100MS §4.4): build only if the
  post-9a supply-adequate sustained p90 measures **>70ms**, or product
  commits to p50 <25ms / burst-p90 <100ms. Otherwise close it — at the
  measured p50 41ms it buys ~20-25ms for a new controller loop + SDK
  acquisition protocol + the #1089 idempotency interaction.
- Router-held first request (3′) — the remaining read-path item (claim
  informer + header path in `sandbox-router`); pair its informer with CBOR.
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

## Observability changes and what they bought

None of the fixes above were found by reading code; each was found by an
instrumentation change that made a specific invisible cost visible. This
section exists because the observability work is upstreamable on its own
merits — and because two of the items are *measurement-bug fixes* without
which the numbers in this issue would be wrong.

- **Per-adoption phase-timing structured log line** (queueLat / pop /
  update / patch / status / in-pass total, per claim, on a dedicated
  **never-sampled** zap core with RFC3339Nano timestamps). This is the
  single highest-yield addition: the round-2 segment decomposition it
  enabled showed creation→winning-pass-entry at p50 1169ms (retry waves +
  doomed stale passes — led to tasks 2/4/5) and in-pass write RTTs running
  3-5× the ~30ms server commit — which indicted the shared HTTP/2
  connection and produced task 1 (the watch-stall finding: informer events
  ~1.0-1.1s late, ~60% of e2e) and later the seat-wall analysis (task 16).
- **Cold-start reason logging.** One label on an existing log line turned
  183 mystery cold-path entries per burst into a single root cause ("warm
  pool queue empty" with 300 adoptable sandboxes present) — directly
  producing task 3's List-fallback guard. Cold starts have been zero in
  every run since; the reason line now proves the negative.
- **REST-client latency histograms** (controller-runtime exports none by
  default). Enabled the client-vs-server RTT split: server-side APF wait
  ~10% + exec ~25% + etcd ~4% vs **~60% client-side in-flight queueing**
  (round 4) — which first exonerated APF as configured, then indicted the
  seat ceiling once insulated (task 16's 272-seat demand HWM), and
  quantified task 1's sharding win.
- **Burst-window CPU/heap profiles** (captured DURING claims-warm, not
  after). Found `persistStampedAnnotations` full-object merge patching at
  **15.8% of controller CPU** and JSON at ~33% (round 2; still 18.3% in
  round 3) — directly producing `internal/rawpatch` (task 6).
- **Measurement-bug fix 1 — CreationTimestamp 1s truncation.**
  `queueLatMs` was derived from the second-truncated CreationTimestamp:
  claims created at t+0.85s inherited ~850ms of phantom queue wait
  (reported p50 1025ms vs **33ms true**). Replaced with a monotonic
  watch-receive→pass-entry measurement; the old value is kept as
  `sinceCreationMs` and documented as truncated. Without this fix, round 3+
  would have chased a queue-latency problem that did not exist
  ([#781](https://github.com/kubernetes-sigs/agent-sandbox/issues/781)).
- **Measurement-bug fix 2 — zap sampler survivor bias.**
  controller-runtime's production zap wraps the core in
  `NewSamplerWithOptions(core, 1s, 100, 100)`: at ~2,100 lines/s burst
  peak, the 101st+ timing line per window was silently dropped — 76/300
  adoptions missing, and the missing cohort's e2e p50 was **1382ms vs
  909ms** for survivors. Every log-derived quantile was biased fast until
  the timing line moved to the never-sampled core.
- **Client-connection calibration** (`--client-connections=N` on the
  stress client). The harness itself shared one HTTP/2 connection (the
  same 100-stream cap as task 1): **~2/3 of reported create-ack was
  harness-side queueing** (ack p50 152ms → 101ms → 49ms as legs
  calibrated; gate-zero runs at 32-48ms with 4 connections). Every
  pre-calibration ack number in older docs overstates server cost ~2-3×.
- **INSTRUMENT_CLUSTER apiserver capture** (opt-in `-v=3` + log download
  in the kops bench script). Provided the server-side half of the RTT
  split and the APF wait/seat metrics behind task 16 — while being exactly
  the kind of tax the clean-run discipline (below) removes from headline
  legs.
- **PSI / runtime-metrics additions** (worker-node pressure-stall capture
  per SCALING-GUIDE idea F; Go `runtime/metrics` in the scrape set per
  GAP-7). Attribution instruments for the sustained-rate walls:
  distinguish disk-stalled from CPU-starved nodes during refill churn, and
  GC assist/pause jitter at sub-100ms targets. These are the standing
  instruments for the leg-S remediation work.
- **Clean-instrumentation discipline (GAP-1).** The instrumentation
  itself was a first-order term: every pre-gate-zero headline ran with
  `--zap-log-level=debug --zap-encoder=json --enable-pprof-debug` (block
  profiling of every blocking event + mutex sampling, process-wide) and
  apiserver `-v=3`. Gate zero's clean legs measured the true smoke floor
  at **14-21ms vs the 31-40ms instrumented** — the assumed "physics floor"
  was ~2× inflated, and the 584ms-era numbers all carry the tax. Standing
  rule: headline legs run stock logging; instrumented legs are for
  attribution only and are labeled as such.

---

*Maintenance: this document is updated at the end of every investigation
round (latest: round-9a absorption, 2026-07-20 — PATH-TO-100MS decomposition
folded in (sustained row, watch-fan-out wall note), task 12 claim half
landed (metadata-only adoption), create-ack riders + TUNE_CBOR wiring +
segment histograms + sharded claims watch in the harness, pre-binding
demoted to conditional pending the 9b measurement). Numbers in the results
table are always the latest verified A/B.*
