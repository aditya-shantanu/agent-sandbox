# SCALE-ROADMAP — from 300-burst to sustained 500→1000 SandboxClaims/s

Status: research deliverable, round-3 tree (`perf-investigation-master` @ e77ad2e).
Constraint set: no breaking CRD changes; everything else on the table. Target:
sustained 500 then 1000 claims/s against warm pools, create→Ready p90 in the
low hundreds of ms, and optimizations that hold as scale rises.

## 0. The write budget: what 1000 claims/s actually costs

Per warm-adopted claim, the round-3 tree performs (system-wide, steady state):

| write | who | ref |
|---|---|---|
| claim CREATE | client | — |
| sandbox adoption PATCH (optimistic lock = the adoption lock) | claim ctrl | `extensions/controllers/sandboxclaim_controller.go:1529` |
| claim status PATCH | claim ctrl | `sandboxclaim_controller.go:955` |
| deferred annotation flush PATCH (observability + assigned-sandbox) | claim ctrl | `sandboxclaim_controller.go:571-580,683-690` |
| pod metadata PATCH (strip safe-to-evict, claim-uid label; deliberately synchronous) | sandbox ctrl | `controllers/sandbox_controller.go:906-927` |
| sandbox status PATCH (observedGeneration echo of the adoption bump) | sandbox ctrl | `sandbox_controller.go:459-494` |
| **refill:** sandbox CREATE + pod CREATE + headless-Service CREATE | warmpool + sandbox ctrl | `sandboxwarmpool_controller.go:565-575`, `sandbox_controller.go:594-622,1007-1020` |
| **refill downstream:** scheduler binding, ~2-4 kubelet pod-status writes, ~2 EndpointSlice writes (headless Service has a selector), ~2 sandbox status writes to Ready | kube components | — |
| **teardown (steady state):** claim/sandbox/pod/service DELETEs + GC/kubelet updates | mixed | — |

Totals: **~3 claim-controller writes + ~2 sandbox-controller writes on the hot
path (~5)**, but **~20-25 cluster-wide writes per claim** once refill churn and
teardown are included. At 1000 claims/s that is **~20-25k writes/s and ~1000
pods/s + 1000 Services/s of churn** — an order of magnitude beyond what any
single etcd/apiserver/scheduler stack sustains. Conclusion up front: *the hot
path can be made to fit; the churn cannot, unless it is reduced per claim.*
That asymmetry drives the ranking below.

## 1. Ceiling analysis (evidence-based)

### 1.1 apiserver + etcd

- **Protobuf for CRs: confirmed unavailable.** CRs are served and stored as
  JSON only; `application/vnd.kubernetes.protobuf` is only for built-in types.
  The sanctioned replacement is **CBOR (KEP-4222)**: alpha since k8s 1.32
  behind `CBORServingAndStorage` (apiserver) + client-go `ClientsAllowCBOR`,
  benchmarked at up to ~8x faster CR encode / ~2x decode; **beta retargeted to
  1.37** — usable today only on self-managed clusters (our kops benchmark can
  enable it; GKE cannot yet). Our tree is on client-go v0.36.2 /
  controller-runtime v0.24.1, so the client side is code-ready.
- **etcd raw ceiling:** ~30k tiny writes/s benchmark-best; the `etcdctl check
  perf` "large cluster" bar is 8k writes/s; production reports (Zendesk)
  ~6.6k/s. Kubernetes-object writes are KB-sized with txn overhead —
  realistic sustained budget on serious NVMe is **low thousands to ~8k
  writes/s per etcd cluster**. Hot path at 1000 claims/s (~5k writes/s) is
  *borderline feasible*; hot path + churn (~20k/s) is **not** on one etcd.
- **Write-path scaling levers that need no code:** (a)
  `--etcd-servers-overrides` to split *events* — and, crucially, it also
  accepts any group/resource, so `/events`, `pods`, and
  `extensions.agents.x-k8s.io` resources can each land on **separate etcd
  clusters** (kops supports the events split natively; arbitrary overrides via
  cluster spec). This multiplies the write ceiling by the number of etcd
  backends. (b) etcd on dedicated NVMe (fdatasync latency IS commit latency),
  `--etcd-compaction-interval` (apiserver, default 5m) shortened under high
  churn so compactions stay small, scheduled defrag off-peak. (c)
  `--max-mutating-requests-inflight` default **200** (400 read) — at 1000/s
  with ~30ms commits we need ≥600 mutating seats for our traffic alone; raise
  to 1000/2000-class values and re-derive the APF nominal shares in
  `k8s/apf-insulation.yaml` (they are fractions of the seat total, so raising
  seats without revisiting shares silently shrinks our insulation percentage).
- **Multiple apiservers scale CPU, not commits.** All writes still serialize
  through the etcd raft leader; extra apiservers buy JSON
  serialize/deserialize CPU and watch fan-out capacity (each CR watch event is
  JSON-encoded per watcher). Measured 2.35/8 cores at 300-burst (~90
  claims/s effective) → **~25 cores at 1000/s: 2-3 apiservers on 16-32 core
  nodes**, with controller shards pinned to different apiservers (per-shard
  `--kubeconfig` host or LB) to also multiply the per-connection
  HTTP/2 stream budget (`cmd/agent-sandbox-controller/transport.go:35-41`).
- **SSA vs merge patch:** stay on merge patches for the hot path. SSA adds
  managedFields recomputation on every write and bloats every object copy the
  informers then decode; the tree already uses merge patches everywhere that
  matters (`sandboxclaim_controller.go:1529,955`, `sandbox_controller.go:487`).
  Corollary: strip managedFields in *our* caches (see 1.2) since other
  writers (kubelet uses SSA for pod status) inflate objects we watch.
- **Watch cache:** on by default for CRs and dynamically sized; the risk at
  10x is not the cache but **per-watcher event encoding** — every additional
  full-resource watcher of `sandboxes` costs the apiserver another JSON encode
  stream at ~5k events/s. Keep the number of full watches minimal and
  namespace/label-scope every shard's watch (server-side filtering in the
  watch cache cuts bytes and encode work).

### 1.2 Controller process (CPU, connections, JSON)

- 45% of one core at 300-burst, **~33% of that JSON**, and
  `persistStampedAnnotations` alone 15.8% (ROUND2-FINDINGS "Leg-B
  attribution"). Root cause of the latter is mechanical: it deep-copies the
  entire claim and diffs two full objects to emit a two-key annotation patch
  (`sandboxclaim_controller.go:683-690`). Same pattern in
  `initializeSandboxLaunchTypeLabel` (:2316-2329) and the pod-name annotation
  patch (`sandbox_controller.go:847-855`).
- **The pod and Service informers are cluster-wide and unfiltered.**
  `Owns(&corev1.Pod{}, builder.WithPredicates(labelSelectorPredicate))`
  (`sandbox_controller.go:1419-1424`) filters *events after decode*; the cache
  still lists/watches **every pod and Service in the cluster** and JSON-decodes
  every kubelet status update. At 1000 pods/s churn this alone is a
  multi-core JSON tax. All pods/Services we act on carry `sandboxLabel`
  (creation paths stamp it; `reconcilePod` looks pods up only through the
  `podSandboxNameHashIndex` label index, `sandbox_controller.go:766`), so a
  **cache-level label selector** is behavior-preserving.
- Pod field usage is narrow: `pod.Status.*`, `pod.Spec.NodeName`, metadata
  (`sandbox_controller.go:251-252,355-377`). A cache transform can drop
  `spec.containers/volumes/tolerations` + managedFields — the merge patches
  we send are diffs of *our own mutations* against the cached base, so
  stripped fields never appear in a patch.
- **HTTP/2 stream ceiling:** solved directionally in round 3
  (`--api-connections`, `--separate-watch-connection`,
  `transport.go:58-176`); at 10x the *watch ingestion* side becomes the next
  serialization point — one decode goroutine per resource watch. Sharding
  (1.3) splits this naturally.

### 1.3 Controller horizontal scale — the single-active-process wall

Everything stateful in the claim controller is process-local: the warm
candidate queue (`extensions/controllers/queue/simple_sandbox_queue.go`), and
the `observedTimes` / `triggeredAdoptions` / `lastWrittenStatuses` /
`coldStartDeferrals` maps (`sandboxclaim_controller.go:354-358`). The critical
observation: **adoption exclusivity does NOT depend on the single process.**
The optimistic-lock sandbox patch is the real lock (:1522-1529) — two
processes racing for one candidate resolve as a 409, which every caller
already treats as "candidate lost, try next" (:1367-1376). The in-memory queue
is a performance hint; the crash-recovery path (claim-UID label index,
:70,:2244-2257) and the stale-pass guards (:2192-2198) are all keyed by data
readable from the API. So multi-instance operation is *safe today*; the only
thing lost by naive N-way active-active is efficiency (systematic candidate
collisions). That is fixed by **partitioning, not new locks**:

- **Namespaces are the perfect shard key.** Adoption is namespace-local by
  construction (`ErrCrossNamespaceAdoption`, :2706-2709), queue keys are
  `namespace/pool` (:1190), all indexes are namespaced, and a claim's entire
  object graph (pool → sandboxes → pods → Services) lives in its namespace. N
  processes each configured with `cache.Options.DefaultNamespaces` (or a
  namespace-list flag) partition watches **server-side**, split the JSON
  decode load, keep per-shard warm queues exclusive by construction, and need
  zero API changes. Leader election per shard (distinct
  `LeaderElectionID`s) gives HA.
- **For a single hot namespace**, the proven pattern is
  [timebertt/kubernetes-controller-sharding](https://github.com/timebertt/kubernetes-controller-sharding)
  (KubeCon EU 2025): per-shard Leases + a sharder that stamps a shard label at
  admission, shards watch with a label selector. It is controller-runtime
  native and requires labels only (no CRD change). Pool-affinity hashing
  (shard = hash(namespace/poolName)) is the right assignment function for us:
  it keeps each pool's queue on exactly one shard, so warm-queue exclusivity
  and the replenish-delay bookkeeping
  (`sandboxwarmpool_controller.go:78-146`) survive sharding unchanged.

### 1.4 The machinery nobody tunes: pod churn at 1000/s

Sustained 1000 claims/s ⇒ 1000 pod creates/s + 1000 deletes/s forever:
- kube-scheduler: ~100/s is the SIG-scalability planning number; up to
  ~1000/s in isolation with raised `--kube-api-qps/burst` (default 50/100 —
  must be raised or the scheduler is the refill ceiling all by itself).
- kubelet/CNI: cilium endpoint-create limit already raised to 100/s
  *cluster-wide setting per node script* (INFRASTRUCTURE.md); at 1000 pods/s
  across ~500+ nodes the per-node rate is fine, but cilium's own k8s client
  QPS (50/100) needs raising.
- kube-controller-manager: EndpointSlice + GC controllers run at default
  QPS (`--kube-api-qps` default 20!) — they will fall behind by design;
  every headless Service create/delete drags EndpointSlice writes with it.
- Capacity: 1000/s × (pod-ready ~4s + sandbox lifetime) concurrent pods. At
  even 60s mean claim lifetime that is 60k+ live pods — 500-600 nodes.

**Implication:** at 1000/s the dominant, non-negotiable cost is not our
controller — it is per-claim child-object churn. The highest-leverage
architectural lever available without CRD breaks is **removing the per-sandbox
Service** (it is already optional: `spec.service` false skips creation,
`sandbox_controller.go:594-626`; the router can target `status.podIPs`/
`status.podName` instead of DNS) and, more aggressively, **decoupling claim
rate from pod churn rate** (pool as shock absorber + bounded-rate refill,
or scrub-and-reuse recycling where the security model allows it).

**Empirically measured — CORRECTED in round 9b (supersedes the round-8
reading below):** round-8 leg S2 (34 nodes) measured ~47/s ready (65.7
best-60s) and attributed it to a per-node churn wall (`nodes ×
per-node-ready-rate ≥ R`, ~1.5-2 ready/s/node ⇒ 150-200 nodes for 300/s).
Round-9b SUST3 falsified that: the SAME ~47/s (75.7 best-60s) on **150
nodes / 16,500 slots**. The real ladder, measured leg by leg
(`RESULTS.md` round-9b):

1. **kube-scheduler client QPS (default 50) was the 47-50/s wall** — this
   section's own "must be raised or the scheduler is the refill ceiling
   all by itself" warning, hidden because kops v1.35 silently ignores
   `cluster.spec.kubeScheduler.kubeAPIQPS` (flag-tagged field; only
   `kubeScheduler.qps/burst` reach the generated
   KubeSchedulerConfiguration.clientConnection). Every "tuned" bench
   cluster ran an untuned scheduler until the round-9b run-script fix.
   With qps=800 applied (SUST4): binds 104-222/s, pod-create→scheduled
   p50 183s → **0.82s**.
2. **Controller supply pipelines ≈ 100-150/s aggregate** (SUST4's binding
   constraint): refill issuance (claim→pod-created p50 41.9s under
   backlog) + sandbox-Ready marking (pod-Ready→sandbox-Ready p50 50.5s at
   ~13k live pods). Ready steady 78.6/s / best-60s 109.5/s. Lever:
   sharding (R4.4/L2) + pipeline profiling — not nodes.
3. **etcd default quota-backend-bytes (2GiB) is a hard DURATION wall:**
   ~3.2MB/s main-DB revision growth at 300/s churn ⇒ NOSPACE (writes
   rejected cluster-wide) in ~10 min; ~30 min even at 100/s. Mandatory for
   any sustained run: ETCD_QUOTA_BACKEND_BYTES ≥ 8GiB +
   `--etcd-compaction-interval=2m` (the R4.5 items, never yet applied),
   plus the L4 pods/CR-group etcd split at higher rates.
4. Nodes/slots: exonerated at 300/s — 150 e2-standard-8 workers idled
   (scheduled→running p50 1.0-2.3s; ≥4.4 starts/s/node demonstrated).
   Corrected slot sizing once walls 1-3 fall: pool + R × ~10s residence
   (300/s ⇒ ~6,000 slots ⇒ **~60-80 nodes, not 150-200**).

**Supply-path costs (measured 2026-07-20, us-central1):** 150×
e2-standard-8 + n2-standard-16 CP + 100GB pd-ssd ≈ $45/h; a full
bring-up→measure→teardown leg = 33-37 min ≈ **$25-28**. Standing churn
cluster for 300/s after walls 2-3 fall: ~60-80 nodes ≈ $12-16k/mo.
Quota reality in the bench project: N2_CPUS is ~exhausted (7,708/8,000 —
the hidden 34-worker cap of rounds ≤8); node-scale legs use e2-standard-8
(CPUS quota) and 100GB pd-ssd (SSD_TOTAL_GB headroom ~21TB). L6 recycling
comparison: removes pod churn entirely (~6 writes/claim, no binds, ~zero
etcd revision growth) — the only path where sustained rate decouples from
walls 2-3; cluster size becomes R × mean sandbox lifetime.

## 2. Ranked roadmap

Legend: ceiling raised = which wall it moves and roughly how far; effort
S/M/L; risk = correctness/operational risk.

### Round 4 — implementable now

**R4.1 — Kill the JSON tax in our own patches: raw targeted patches.**
Replace DeepCopy+MergeFrom with `client.RawPatch(types.MergePatchType,
{"metadata":{"annotations":{...}}})` in `persistStampedAnnotations`
(`sandboxclaim_controller.go:683-690`), `initializeSandboxLaunchTypeLabel`
(:2316-2329), and the pod-name annotation patch
(`sandbox_controller.go:847-855`). Mechanism: stop serializing two full
objects to diff a 2-key map. Ceiling: −15%+ controller CPU immediately
(measured attribution), rising with scale. Effort S. Risk: low —
identical bytes on the wire; unit-test patch payloads byte-for-byte.

**R4.2 — Cache label selectors + transforms (pods, Services) and strip
managedFields everywhere.** `cache.Options.ByObject[&corev1.Pod{}] = {Label:
sandboxLabel exists, Transform: strip spec except nodeName + strip
managedFields}`, same selector for Services; `DefaultTransform =
TransformStripManagedFields` for all our CRs. Mechanism: informer decode work
and cache memory become O(our pods), not O(cluster pods), and objects shrink.
Ceiling: this is what keeps ONE shard viable at 100-200 claims/s while the
cluster churns 1000 pods/s around it; biggest single controller-CPU lever.
Effort S-M (main.go cache options + verify `reconcilePod`/`updatePodMetadata`
never read stripped fields — verified reads today are status/metadata/nodeName,
`sandbox_controller.go:251-252,355-377`). Risk: medium-low; the adoption path
for unowned adoptable pods requires the tracking or adoptable label — pods
lacking `sandboxLabel` were already invisible to `reconcilePod`'s indexed List
(:766), so behavior is preserved; needs an envtest asserting merge patches
from transformed cache objects don't erase stripped fields (they can't —
merge base is our own DeepCopy — but prove it).

**R4.3 — Cut claim-side writes 3→2 (and 2→2 cheaper): make the annotation
flush optional and rate-decoupled.** Add `--disable-claim-observability-annotations`
(or route flushes through a background low-QPS jittered flusher). The flush
is post-Ready and never gates the transition (:568-580, comment at :541-545);
crash recovery uses the claim-UID label List, not the annotation (:1358-1366).
Mechanism: −1 write/claim (−1000 writes/s at target), and removes the
last non-critical write from burst windows. Keep the `observedTimes`-based
metric: it is in-memory. Ceiling: ~⅓ of claim-controller write volume. Effort
S. Risk: low — losing a debugging breadcrumb; the assigned-sandbox hint can
be folded into... nothing else needed, recovery is label-based.

**R4.4 — Shard-by-namespace multi-process mode (static).** Flag
`--watch-namespaces=<list>`; deploy N controller replicas each owning a
namespace subset, each leader-elected in its own Lease
(`LeaderElectionID` suffix). No code touches adoption logic: per-shard warm
queues are exclusive because pools and claims are co-namespaced (guard at
:2706-2709). The benchmark gains a multi-namespace claims-warm phase (spread
1000 claims/s across 8 namespaces × 8 shards). Mechanism: divides watch
ingestion, JSON decode, write concurrency, and workqueue depth by N.
Ceiling: near-linear controller scale to the apiserver's limit; 8 shards ×
~150/s ≥ 1000/s. Effort M (flag + manifests + bench phase). Risk: low-medium;
static assignment means a hot namespace still binds to one shard (accepted
until L2); mixed-shard restart storms need the existing stale-pass guards —
which are per-process and namespace-partitioned, so they keep working.

**R4.5 — Control-plane scaling of the benchmark environment (validates
everything above at 500/s).** kops: control-plane `n2-standard-32`;
`--max-mutating-requests-inflight=1000 --max-requests-inflight=3000` (and
recompute APF shares in `k8s/apf-insulation.yaml`); events on a separate etcd
(`--etcd-servers-overrides=/events#...`); etcd on local NVMe;
`--etcd-compaction-interval=2m`; scheduler `--kube-api-qps=800 --kube-api-burst=1600`
(on kops: MUST be expressed as `kubeScheduler.qps/burst` — the
`kubeAPIQPS` spec key is silently inert, the round-9b discovery in §1.4);
KCM `--kube-api-qps=200` (EndpointSlice/GC keep-up); cilium k8s client QPS up.
Mechanism: removes the ceilings that would otherwise mis-attribute round-4
regressions to our code. Ceiling: apiserver seat ceiling ~5x; etcd hot-path
headroom ~2x. Effort M (scripts only, `optimizations/infra` + kops run
script). Risk: low; each knob independently revertible; A/B on one cluster
per the reuse workflow.

**R4.6 — Warm-pool refill shaping for sustained load.** Today refill is
burst-shaped: `--sandbox-warm-pool-replenish-delay` defers, then
`slowStartBatch` fires the whole deficit (`sandboxwarmpool_controller.go:302-331,
803-836`). For sustained arrivals add `--sandbox-warm-pool-max-refill-rate`
(token bucket inside the batch loop) + per-pool worker parallelism guidance
(multiple pools scale trivially; one pool serializes on its single reconcile).
Size guidance to document: pool ≥ arrival_rate × (refill_p99 + hold), i.e.
1000/s × ~10s ⇒ ~10k warm replicas as shock absorber; several pools of
1-3k backed by the same template beat one 10k pool (spreads the pool-status
single-writer, the queue mutex, and shard affinity). Mechanism: refill stops
competing with adoption at every timescale, not just bursts. Effort M. Risk:
low; under-provisioning bounded by rate knob = explicit SLO tradeoff.

**R4.7 — Pipeline the two hot-path writes per claim.** The status patch
cannot start before the adoption patch returns *in the current call shape*,
but the two are independent server-side objects: issue `completeAdoption` and
the status patch concurrently once the adoption patch has been *accepted
locally* is unsafe — instead, the cheap version: issue the status patch
immediately after the adoption patch's response headers (no body decode
needed; client-go decodes fully — so instead skip decoding cost by using
`client.Patch` with `client.MergeFromWithOptimisticLock` as-is but move the
`getTemplate`+`mergedMeta` compute (:1473-1520) *ahead* of candidate pop so
the pop→patch gap shrinks). Mechanism: shaves compute out of the
candidate-held window that round 2 showed causes queue exhaustion. Ceiling:
tail robustness at high concurrency more than median. Effort S. Risk: low.

### Longer-term architecture (post-round-4)

**L1 — One-write adoption: claim-status-first commit with shard-exclusive
pools (target p90 = 1 write RTT + watch delivery).** With pool-affinity
sharding (R4.4/L2) a pool's candidates are assigned by exactly one process
again — restoring the invariant the in-memory queue had pre-sharding, but now
*by partition rather than by accident*. That makes it safe to invert the
transaction: reserve candidate in-memory → write **claim status** (the
authoritative binding per `getOrCreateSandbox`'s status-first lookup, :1974) →
async, coalesced sandbox patch + pod patch. Crash window (status written,
sandbox unpatched) is recovered by the existing status-name lookup which
tolerates a still-warm-pool-owned sandbox and completes adoption idempotently
(:2030-2089). Result: critical path = 1 write + Ready forwarding already done
in-pass. The safe-to-evict strip stays synchronous-ish via a bounded (<1s)
async flush; the autoscaler window is documented risk. Ceiling: p50/p90
approach the 62-79ms smoke floor under load; halves hot-path write
concurrency demand. Effort L. Risk: medium-high (autoscaler eviction window,
discovery-label lag; needs the pod-patch deferral rationale at
`sandbox_controller.go:906-921` explicitly revisited) — prototype behind a flag.

**L2 — Dynamic sharding (single hot namespace, elastic shards).** Adopt the
timebertt sharder pattern: shard Leases + admission-stamped shard label +
label-selector watches, assignment = hash(namespace/poolName) so pool
affinity holds. Claims get the label from the sharder webhook (adds one
admission hop; measure — CL2 GKE reference had a webhook in path at p90
245ms) or from a client convention (label pre-stamped by the SDK; zero hop).
Ceiling: removes the "one namespace = one shard" limit of R4.4; N elastic.
Effort L. Risk: medium; well-trodden external project, but a new component.

**L3 — Kill per-sandbox Services.** Default templates to `service: false` at
scale; router resolves via `status.podIPs`/`status.podName` (already in
status) or a single shared headless Service per template with pod hostnames.
Mechanism: −1 create, −1 delete, −~2 EndpointSlice writes per claim ⇒ ~20% of
total churn writes; also removes the cluster-wide Service informer's growth.
Ceiling: biggest single etcd-write reduction available without touching pods.
Effort M (router + template defaults; no CRD change — field exists). Risk:
medium (DNS consumers; SandboxServiceReady condition semantics).

**L4 — etcd/apiserver horizontal write scaling.** 2-3 apiservers; controller
shards pinned per-apiserver; `--etcd-servers-overrides` to place `/events`,
`pods`, and `extensions.agents.x-k8s.io` + `agents.x-k8s.io` groups on
separate etcd clusters (3-4 write domains ⇒ ~3-4x commit ceiling). Ceiling:
raises the 20k-writes/s wall to ~sustainable if L3 lands too. Effort M-L
(infra only). Risk: medium operationally (more etcds to run); zero code.

**L5 — CBOR serving+storage for our CR groups (k8s ≥1.37 / self-managed
now).** Enable `CBORServingAndStorage` + client-go `ClientsAllowCBOR` on the
benchmark cluster and measure: expected ~8x CR encode / ~2x decode CPU cut on
both apiserver and controller — directly attacks the 33%-JSON controller CPU
and the apiserver's per-watcher encode at 5-15k events/s. Ceiling: CPU walls
roughly double their claims/s. Effort S (flags) once platform supports it;
gate on beta for production. Risk: alpha feature today — bench-only until 1.37.

**L6 — Churn decoupling / recycling (research).** If the security model ever
permits same-template scrub-and-reuse (fresh MAC/netns not required, or
node-local scrubber), returning expired sandboxes to the pool converts
20-25 writes/claim into ~6 and removes pod churn from the steady state
entirely — it is the only path where 1000/s does not imply a 500-node
control-plane-bound pod pipeline. Requires no CRD change (ownership flip back
to pool + label restore is the adoption patch inverted). Risk: high
(isolation guarantees) — needs an explicit security decision before any
engineering.

### Explicitly confirmed dead ends

- **Protobuf for CRs** — not available, by design; CBOR (L5) is the successor.
- **Server-side batch/multi-object writes** — no such API; per-object writes
  are irreducible, so "batching" must mean *eliminating* writes (R4.3, L1, L3)
  or *spreading* them (background flusher, refill rate shaping).
- **Raising `--http2-max-streams-per-connection` client-side** — server-side
  flag only; transport sharding (merged, `transport.go`) is the correct
  workaround; more apiservers multiply it further.
- **SSA for hot-path writes** — strictly worse CPU/size than merge patch here.

## 3. Suggested measurement additions

1. Sustained-rate phase for the stress tool (`claims-warm-sustained`:
   arrivals at R/s for T minutes, Poisson jitter) — the current
   simultaneous-burst phase cannot validate "holds as scale rises".
2. Per-shard scrape aggregation + a shard-imbalance metric (max/mean queue
   depth) once R4.4 lands.
3. etcd-side dashboards in the kops instrumentation:
   `etcd_disk_backend_commit_duration_seconds`, `etcd_server_proposals_pending`,
   apiserver `apiserver_flowcontrol_*` per PL (already documented in
   ANALYSIS.md), plus `scheduler_pending_pods` and EndpointSlice controller
   workqueue depth — the three places the 1000/s refill pipeline will
   visibly drown first.

---

