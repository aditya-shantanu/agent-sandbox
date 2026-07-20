# ROUND6-COALESCING — can the control plane stop being the bottleneck?

Status: round-6b research + implementation deliverable, branch
`perf-investigation-master-coalesce` (base: `perf-investigation-master` @
81cc536). Driving question: *"can we coalesce control-plane operations / do
local ops with eventual bulk update so the control plane is never the
bottleneck?"* Constraint: no breaking CRD changes. Companion branch (other
agent, not touched here): `perf-investigation-master-l1` — one-write adoption
in the claim controller; this round's surface is the **sandbox controller**
plus analysis.

## 1. The batching verdict: there is no multi-object write API — final

Claimed in SCALE-ROADMAP's dead-end list; now verified against sources so it
never needs re-litigating:

- **The API contract itself rules it out.** Kubernetes API concepts, verbatim:
  the write verbs (`create`, `update`, `patch`, `delete`) "support single
  resources only" and have "no support for submitting multiple resources
  together in an ordered or unordered list or transaction"
  ([kubernetes.io/docs/reference/using-api/api-concepts](https://kubernetes.io/docs/reference/using-api/api-concepts/)).
  The long-standing upstream ask is
  [kubernetes/kubernetes#81694](https://github.com/kubernetes/kubernetes/issues/81694)
  — closed as a support question; no KEP ever picked it up.
- **KEP scan through 1.35 (and the current 1.36 cycle, checked July 2026):**
  no batch/transactional write KEP exists. Everything adjacent is read-side or
  encoding-side: KEP-3157/3161 (watch-list / streaming lists — reads),
  KEP-4222 (CBOR — encoding), KEP-5598 (opportunistic *scheduler* batching —
  internal queue, no API change). **ApplySets (KEP-3659)** are the closest-
  sounding feature and are explicitly not it: a kubectl-side grouping/pruning
  *convention* (parent object + member labels); every member is still applied
  and deleted with its own request.
- **The floor per write is structural.** apiserver storage commits every
  object write as a single-key guarded etcd transaction:
  `staging/src/k8s.io/apiserver/pkg/storage/etcd3/store.go` (`GuaranteedUpdate`
  / `Create`) issues `txn.If(compare ModRevision(key)).Then(OpPut(key, data))`
  — one HTTP request, one etcd raft proposal, one quorum fdatasync, one watch
  fan-out, per object. etcd's own `Txn` can batch up to `--max-txn-ops`
  (default 128) keys atomically in one raft commit, but **no Kubernetes API
  verb exposes it** for user objects. Even `deletecollection`, the only
  multi-object mutating verb, is implemented in
  `registry/generic/registry/store.go` as a worker pool issuing per-object
  deletes — N commits, no atomicity.
- **Aggregated apiservers** could serve a custom "batch bind" endpoint, but
  our types are CRDs served by apiextensions-apiserver; moving them into a
  custom apiserver is a compatibility break (out of bounds), and a custom
  storage layer would have to re-model objects into shared keys to share raft
  commits — a de facto CRD break.

**Conclusion (unchanged, now cited):** "batching" against the Kubernetes API
can only mean (a) *eliminating* writes, (b) *coalescing multiple mutations to
the SAME object* into one patch, or (c) *moving writes out of the latency
window*. This round implements (b)+(c) for the sandbox controller's
recoverable writes and designs (a) for the largest remaining structural write
(the generation-bump status echo).

## 2. The per-claim write ledger (current tree, warm adoption steady state)

Classification: **CRITICAL** = client-visible, must stay synchronous;
**DEFER** = write-behind-safe (recomputable from informer state after a
crash); **ELIM** = eliminable outright.

| # | write | who | ref | class | disposition |
|---|---|---|---|---|---|
| 1 | SandboxClaim CREATE | client | — | CRITICAL | is the request |
| 2 | Sandbox adoption PATCH (optimistic lock: ownerRef + claim-uid label + spec.podTemplate rewrite) | claim ctrl | `sandboxclaim_controller.go:1581` | CRITICAL | it IS the adoption lock; L1 branch restructures it (out of scope here) |
| 3 | SandboxClaim status PATCH (bind + Ready forward) | claim ctrl | `:955` | CRITICAL | the SDK watches claim status; this is the "ready" signal |
| 4 | SandboxClaim annotation flush PATCH | claim ctrl | `:571-580,683-690` | ELIM | already optional (`--disable-claim-observability-annotations`, round 4); post-Ready, recovery is label-index-based |
| 5 | "SandboxAdopted" Event POST | claim ctrl | `:1453` | ELIM | already optional (`--disable-claim-events`) |
| 6 | Pod metadata PATCH (strip `safe-to-evict`, prune warm-pool label, tracking annotations) | sandbox ctrl | `sandbox_controller.go` reconcileExistingPod | **DEFER (≤1s bound)** | **coalesced this round** via write-behind; not eliminable — the strip must reach the live Pod |
| 7 | Sandbox status PATCH (observedGeneration echo of the adoption generation bump) | sandbox ctrl | `updateStatus` + kstatus comment at `computeReadyCondition` | CRITICAL *given the bump* | **ELIM at the source**: remove the generation bump (no-spec adoption, §3) and the write never happens — `updateStatus`'s DeepEqual short-circuits |
| 8 | Sandbox pod-name annotation PATCH | sandbox ctrl | `ensurePodNameAnnotation` | DEFER | cold path only (warm adoptions pre-set it in `completeAdoption:1512`); **coalesced this round**, full window |

Not per-claim but per-claim-amortized (out of scope, from SCALE-ROADMAP): the
refill pipeline (sandbox CREATE + pod CREATE + optional Service + scheduler
binding + kubelet status writes + EndpointSlice churn ≈ 15-20 writes/claim)
and teardown. Those are attacked by L3 (service-free), refill shaping, and
recycling — not by coalescing, because they are all *different objects*.

One documentation correction found while auditing #6: the old comment claimed
the pod patch propagates the `agents.x-k8s.io/claim-uid` label to the Pod.
It never did — `claim-uid` is a system-reserved key that `isSystemLabel`
blocks from template→pod propagation; claim discovery uses the *Sandbox's*
top-level label index (`sandboxClaimUIDLabelIndex`). Pods do not carry
claim-uid on any path. The comment is fixed in this branch; the only
autoscaler-relevant payload of the adoption-path pod patch is the
safe-to-evict strip + warm-pool label prune + tracking-annotation updates.

## 3. Generation-bump elimination (SCALE-ROADMAP #9) — the structural item

### 3.1 Why adoption costs a Sandbox write it doesn't need

`completeAdoption` force-rewrites `spec.podTemplate.ObjectMeta`
(`sandboxclaim_controller.go:1562`) and deletes the template's
`safe-to-evict` annotation (`:1496-1498`). Any non-metadata change to a CR
with the status subresource increments `metadata.generation` —
apiextensions-apiserver `customresource/strategy.go PrepareForUpdate`:
status is copied from old (subresource carve-out), then "except for the
changes to `metadata`, any other changes cause the generation to increment."
The Sandbox CRD has `subresources.status`
(`k8s/crds/agents.x-k8s.io_sandboxes.yaml:4026`). The bump forces exactly one
sandbox status write per adoption: `computeReadyCondition` stamps
`ObservedGeneration = sandbox.Generation` and the kstatus rationale
(documented on that function) makes the refresh mandatory *while the
generation changes* — generation-matching waiters would otherwise see the
Sandbox as permanently stale, and the claim controller forwards the condition
verbatim.

**Corollary:** labels, annotations, and ownerReferences are `metadata` — a
**metadata-only adoption patch bumps nothing**. No bump ⇒ observedGeneration
stays valid ⇒ `updateStatus`'s `reflect.DeepEqual` short-circuit produces
**zero status writes** for the adoption reconcile, with kstatus semantics
fully intact (there is no new generation to observe). Verified empirically in
`TestNoSpecAdoptionMetadataOnlyAdoptionWriteCount`.

### 3.2 What the spec rewrite actually buys, item by item

| spec.podTemplate mutation in `completeAdoption` | who consumes it | metadata-only replacement |
|---|---|---|
| delete `safe-to-evict: "true"` | sandbox ctrl propagation/pruning → live Pod | **ownership-derived**: sandbox is claim-controlled (ownerRef = metadata, stamped by the same optimistic-lock patch) ⇒ treat template `safe-to-evict:"true"` as absent — implemented this round (§3.3) |
| `claim-uid` into template labels | *nothing pod-side* (system key, never propagated); crash-recovery + platform discovery use the **top-level** label index | already covered: `completeAdoption:1521` stamps the top-level label (metadata) |
| `template-ref-hash` into template labels | pod gets it from **top-level** labels via `computeExtensionPodLabels` | already metadata-driven |
| `created-by` into template labels | pod propagation reads **top-level** `sandbox.Labels[CreatedByLabel]` | already metadata-driven |
| `claim.Spec.AdditionalPodMetadata` merge | KEP-0174 contract: claim → sandbox `spec.podTemplate.metadata` → pod | **not replaceable** without violating KEP-0174 — keep the spec write for claims that use it |

KEP-0174 reconciliation (`docs/keps/174-metadata-propagation/README.md`):
the KEP's warm-pool scenario B prescribes the spec injection *as the
mechanism for `additionalPodMetadata`*. The no-spec fast path therefore
applies only when `additionalPodMetadata` is empty (the benchmark case and
the common API case); non-empty claims keep today's spec rewrite, generation
bump, and status echo. This keeps the KEP contract byte-for-byte for every
user of the feature, and `Sandbox.spec.podTemplate.metadata` remains the
declared source of truth whenever it is actually personalized.

### 3.3 Split of work (respecting file ownership)

**Implemented this round (sandbox controller, flag `--no-spec-adoption`,
default off):** ownership-derived pod hygiene, the enabling half. When a
Sandbox is controlled by a SandboxClaim, `safe-to-evict:"true"` in the
template is treated as absent: never propagated to the Pod (existing-pod and
new-pod paths) and stripped from a Pod carrying it via the normal
pruning/tracking machinery (`suppressSafeToEvictForClaim`,
`controllers/sandbox_controller.go`). It mirrors `completeAdoption`'s exact
rule ("delete when the value is `true`; explicit overrides like `false` are
kept"), so with the flag ON the Pod's terminal state is identical whether or
not the claim controller rewrote the spec — the two halves are independently
deployable. Documented behavioral delta: claim-owned *cold-start* pods whose
template explicitly sets `safe-to-evict:"true"` also lose the marker
(strictly safer — a claim-backed pod is in-use by definition — and it makes
warm and cold pods uniform).

**Design-only here (claim controller — owned by the L1 branch/agent):**
1. In `completeAdoption`: when `additionalPodMetadata` is empty and the
   template matches, skip `:1496-1498` and `:1522`/`:1562-1571` — the patch
   becomes labels + annotations + ownerRef only (all already computed).
   The optimistic lock, `claim-uid` top-level label, launch-type label,
   pod-name annotation, and trace-context propagation are all metadata and
   stay exactly as-is. This composes with L1's deferred sandbox patch: the
   deferred patch simply becomes metadata-only and cheaper to coalesce.
2. In `reconcileActive` (`:822` drift check): under the flag, compare
   `mergedMeta` against the template *modulo the identity keys* (claim-uid,
   template-ref-hash, created-by) when `additionalPodMetadata` is empty —
   otherwise the next steady-state pass would re-add the spec metadata and
   re-introduce the bump through the back door.
3. Crash-window audit: recovery (`getOrCreateSandbox` status-first lookup +
   claim-UID top-level label index `:70,:2244-2257`) reads only metadata —
   unaffected. The `IsControlledBy` check is ownerRef — unaffected.

Net effect once both halves are on: **−1 write per adoption system-wide**
(ledger #7), and −1 sandbox reconcile's worth of work (the adoption MODIFIED
event still arrives, but the pass is read-only: DeepEqual status
short-circuit + the pod patch it already had to do).

## 4. Implemented: write-behind coalescing for recoverable writes

New package `internal/writebehind` + sandbox-controller wiring, flag
`--sandbox-write-behind-window` (duration, **default 0 = fully synchronous,
stock behavior on the same code paths**).

Mechanism — `writebehind.Flusher`, a manager Runnable (leader-scoped):
- **Per-object coalescing:** mutations are keyed by (GVK, namespace, name);
  N enqueues merge into ONE `{"metadata":{"labels":{…},"annotations":{…}}}`
  JSON merge patch — sets plus RFC-7386 `null` deletes, last-write-wins per
  key. No optimistic lock ⇒ cannot 409 (same last-writer-wins argument as
  the synchronous path: every routed write is recomputed-from-observed-state
  metadata reconciliation).
- **Bounded staleness:** an entry flushes no later than
  `min(window, per-call maxDelay)` after its FIRST enqueue; later enqueues
  can only tighten the deadline. The pod metadata patch always passes
  `maxDelay = 1s` (`podMetadataFlushBound`), honoring the safe-to-evict
  rationale: the strip that protects an adopted pod from cluster-autoscaler
  eviction lands within <1s regardless of the configured window (autoscaler
  scan intervals are ≥10s). The sandbox pod-name annotation uses the full
  window.
- **Bounded memory:** at most 4096 distinct objects pending; overflow
  degrades to an inline synchronous patch for the new object (never
  unbounded growth, never dropped work).
- **Crash recovery = the normal reconcile.** Pending entries are process
  memory. On crash, the replacement leader's level-based reconcile re-reads
  informer state, re-detects the identical drift, and re-issues the mutation
  (pinned by `TestWriteBehindCrashRecovery`). On *graceful* shutdown the
  Runnable drains all pending entries (≤5s) so failover doesn't even pay the
  re-detect.
- **Scope discipline:** only ledger #6 and #8 are routed. Status writes,
  creates/deletes, ownership transfers (`SetControllerReference` on unowned
  adoptable pods), and the suspended-path annotation clear stay synchronous.

What coalescing does and does not buy: on the unique-claim hot path each pod
is patched once either way — the win is **moving the write out of the burst
window** (it no longer competes for APF seats with adoption/status patches
during the latency-critical seconds; at 300-burst that window carried ~846
writes/s against a ~600-900/s seat-limited service rate, ROUND4-FINDINGS) —
plus genuine N→1 coalescing wherever the same object is mutated repeatedly
(claim `additionalPodMetadata` update storms, template label churn, reconcile
redeliveries, the echo-relapse cohort after failover).

### Validation

`go build ./... && go vet ./...` clean; `go test ./controllers/...
./extensions/... ./internal/...` fully green. New tests:

- `internal/writebehind`: N mutations → exactly 1 patch with correct
  merged payload incl. JSON-null deletes (`TestCoalescesToOnePatch`);
  deadline = min(window, maxDelay), never extended by later enqueues
  (`TestDeadlineIsMinOfWindowAndMaxDelay`); background flush lands within the
  window (`TestBackgroundFlushHonorsWindow`); graceful drain on shutdown;
  bounded-queue synchronous fallback; NotFound terminal without retries;
  zero-window construction rejected.
- `controllers`: zero pod patches during reconcile + 2 reconciles → 1
  flushed patch, byte-state-identical to synchronous mode
  (`TestWriteBehindCoalescesAdoptionPodPatch`, which also pins flag-off
  identity: nil flusher ⇒ 1 synchronous patch, unchanged legacy path);
  fresh-process crash recovery re-issues the mutation
  (`TestWriteBehindCrashRecovery`); pod-name annotation deferral
  (`TestWriteBehindPodNameAnnotation`); `--no-spec-adoption` strip on
  existing pods with flag-off contrast + idempotence, pool-owned pods
  unaffected, new-pod suppression, and the §3 write-count proof
  (metadata-only adoption ⇒ 1 pod patch, 0 status patches,
  generation unchanged).

## 5. Flags and deployment

| flag | default | effect |
|---|---|---|
| `--sandbox-write-behind-window=250ms` (bench suggestion) | `0` (= synchronous, stock) | coalesce pod-metadata + pod-name-annotation writes; pod patches capped at 1s deferral regardless |
| `--no-spec-adoption` | `false` | ownership-derived safe-to-evict hygiene (sandbox-side half); safe standalone; prerequisite for the claim-side metadata-only adoption |

Both compose with every existing round-4/5 flag. Suggested next A/B (after
the L1 branch merges): candidate adds `--sandbox-write-behind-window=250ms
--no-spec-adoption` (+ the claim-side no-spec flag when it exists).

## 6. Projected controller writes per warm claim

Hot-path writes only (client CREATE included; refill/teardown churn excluded
— see SCALE-ROADMAP §0 for the ~15-20 writes/claim churn budget those add):

| configuration | writes/claim | of which in the latency-critical window | writes/s at 300 / 500 / 1000 rps (hot path) |
|---|---|---|---|
| round-3 baseline | 6 (create, adoption, claim status, flush, pod patch, sbx status) | 6 | 1.8k / 3.0k / 6.0k |
| current tree, bench flags (flush+events off) | 5 | 5 | 1.5k / 2.5k / 5.0k |
| + write-behind (this round) | 5 | **3** (create, adoption, claim status) | 1.5k / 2.5k / 5.0k total; 0.9k / 1.5k / 3.0k in-window |
| + no-spec adoption (both halves) | **4** | 3 | 1.2k / 2.0k / 4.0k |
| + L1 one-write adoption (companion branch; adoption patch leaves the critical path and coalesces) | 4 | **2** (create, claim status) | 1.2k / 2.0k / 4.0k total; 0.6k / 1.0k / 2.0k in-window |

At 1000 rps the fully-composed hot path demands ~2k synchronous writes/s —
comfortably inside a tuned single control plane (R4.5: 4000 APF seats,
etcd ~8k writes/s budget) — leaving the refill/teardown churn as the sole
remaining control-plane wall, exactly as SCALE-ROADMAP §0 predicted. The
answer to the driving question is therefore: **yes for everything we
control — via elimination + per-object coalescing + window-eviction, never
via server-side batching (which does not exist) — and the residual
bottleneck is child-object churn, owned by L3/refill-shaping/recycling, not
by the adoption path.**

## 7. Explicitly out of scope / follow-ups

- Claim-side no-spec fast path + `reconcileActive` drift-check relaxation:
  L1 agent's file (`sandboxclaim_controller.go`), design in §3.3.
- Routing the claim controller's deferred annotation flush through the same
  Flusher (it has its own deferral today; unification is cosmetic).
- Events cannot be coalesced client-side beyond what `--disable-claim-events`
  already does (the API's event series/aggregation only dedupes *identical*
  events).
- Watch-delivery and create-ack latency: untouched by write coalescing;
  see ROUND4-FINDINGS §3 for why they bound p90 next.
