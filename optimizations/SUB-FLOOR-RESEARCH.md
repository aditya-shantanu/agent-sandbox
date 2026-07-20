# SUB-FLOOR-RESEARCH — can client-perceived ready-latency beat the 2-commit + 2-hop floor without breaking the CRD contract?

Status: research deliverable for `optimizations/SUB-FLOOR-RESEARCH.md` (not written to tree — read-only checkout). Base: `perf-investigation-master` tree at `/private/tmp/bench-candidate-tree`. Inputs: ROUND7-PLAN §1.3/§6/§7 (floor + stopping rule), GAP-AUDIT (GAP-1 instrumentation tax, GAP-4 pool-delete race, GAP-5 SDK watch shape), SCALE-ROADMAP (L1 design), `extensions/controllers/adoption_flush.go` (L1 transaction + steal recovery), `extensions/controllers/sandboxclaim_controller.go` (status-first commit, claim-UID label recovery), `clients/python/agentic-sandbox-client/k8s_agent_sandbox/k8s_helper.py` (SDK wait path), `sandbox-router/` (pod-informer cache, `X-Sandbox-*` headers, tokenreview authz), COMMUNITY-SCAN (#1214, #1089, #883, #850/#1174/#1198).

**Contract held constant throughout:** the SandboxClaim CRD schema is unchanged; claim `status.sandbox.{name,podIPs}` + the forwarded Ready condition are still written and watchable for every bound claim. The question is only whether the *client's perceived* create→usable latency must include waiting on that write.

---

## 0. Reframing the floor

ROUND7 §1.3's floor — ≈35ms p50 / 60-80ms p90 sustained — is the cost of the **transaction shape "request, then bind, then observe"**: commit the claim (raft #1), hop to controller, commit the binding (raft #2), hop to client. Two decompositions of that shape exist, and they bound everything below:

- **Hop shaving** (options 2, 3, 4, 5b): keep both commits in the perceived path, remove one or both watch deliveries and the watch-*init* cost. Limit: **2 commits + 0-1 hops ≈ 25-35ms p50** — reaches the floor, never meaningfully beats it. (The measured floor may itself be lower than 31-40ms once GAP-1's instrumentation tax is removed — which shrinks these options' headroom further.)
- **Commit prepayment** (option 1, 5a): move both binding commits *ahead of demand*, so the client's critical path is a single CAS write. Limit: **1 commit + 0 hops ≈ 12-25ms p50**. This is the only class that is genuinely sub-floor, and it works precisely because it changes *when* the contract's writes happen, not *whether* they happen.
- **Not waiting for durability** (optimistic ready in any costume): rejected in ROUND7 §6 and re-rejected below each time it reappears — it always reappears wearing a different hat.

---

## 1. Option 1 — Speculative pre-binding (claim-by-label acquisition)

**Mechanism.** A pre-binder loop (natural home: the warm-pool controller or a sibling) maintains, per pool, a small buffer of *vanilla* SandboxClaims it created itself — already adopted, `status.sandbox.name`/`podIPs` populated, Ready=True, carrying an idle marker label (e.g. `extensions.agents.x-k8s.io/prebound-idle: "true"`). Client "create" becomes **acquire**: pick an idle claim (from a session-level informer or one label-selector LIST), then issue a JSON merge patch that (a) includes `metadata.resourceVersion` in the body — the standard optimistic-lock precondition, the same trick `MergeFromWithOptimisticLock` uses on the adoption patch at `sandboxclaim_controller.go:1529` — (b) removes the idle label, (c) stamps consumer identity + a client request-id annotation. HTTP 200 = the binding is *already durable* and now exclusively yours; the 200 itself is the ready signal. No rename exists in Kubernetes (names are immutable, there is no rename verb, and "delete+recreate" forfeits the binding) — so the pattern is **label-CAS, not rename**, and the SDK must treat the claim name as an output, not an input.

**Latency math.** Warm client (informer running): pick ≈ 0ms amortized + 1 mutating request = 1 raft commit + request processing ≈ **12-25ms p50 / 25-45ms p90 sustained**. Cold client: + one cached LIST (~10-15ms) → still ≈ the current floor on the first acquire, sub-floor thereafter. Zero watch hops in the perceived path. Under contention (two clients CAS the same claim) the loser 409s and retries the next candidate — randomize the pick (power-of-two-choices over the idle set) or the whole herd serializes on the lexicographically-first idle claim and p90 degrades toward N×RTT.

**Consistency analysis.**
- The CAS is a real lock: exactly one acquirer wins per claim; no torn state is possible because the binding pre-exists the acquisition.
- *Stale-Ready hazard:* the sandbox can die while idle (eviction, node loss). The claim controller keeps forwarding the sandbox Ready condition, so the idle claim flips Ready=False within watch lag; the picker filters Ready=True, but a client can still win a claim whose sandbox died inside the lag window. Mandatory fallback: first-request failure (or a post-acquire freshness check) → release/delete + fall back to the normal create path. This failure mode is *visible and recoverable*, unlike optimistic-ready's.
- *GAP-4 interplay — actually favorable:* pre-bound sandboxes were adopted out of the warm pool minutes ago; pool scale-down/stale-member deletion (the delete-race in `sandboxwarmpool_controller.go:698-703`) no longer targets them. Acquisition happens entirely outside the adoption race window; L1's steal-recovery machinery is not in the client's path at all.
- *Semantics that do change:* (i) client-chosen claim names are gone — #1089 idempotent-create must be re-expressed as "request-id annotation + did-I-already-acquire LIST", which is weaker; (ii) `creationTimestamp`, creator identity, and audit attribution belong to the pre-binder's SA, not the user — consumer identity lives in labels/annotations; (iii) per-claim spec customization is unavailable: `env` and `volumeClaimTemplates` force cold starts by design, `additionalPodMetadata` and `lifecycle` are baked at pre-create. `lifecycle.shutdownTime` can be patched *after* acquisition (off the critical path); the rest cannot. **Pre-binding only serves the vanilla-claim shape** — which is the entire hot path in the bench, but must be measured for production mix.
- *Capacity:* the pre-binder consumes sandboxes from the existing warm pool — no extra pods. The buffer of idle *claims* only needs to cover acquire-rate × pre-bind-refill-latency, and refill = one ordinary claim create (≈35-100ms), so a buffer of `rate × ~0.5s` suffices (150 idle claims at 300/s). Cheap.

**Comparison to #1214 / warm-pool semantics.** #1214 (preemptible warm-pod ballast) is capacity-side and orthogonal — it makes *cold* capacity appear faster; pre-binding makes *warm* capacity attach faster. Conceptually, pre-binding is the warm-pool idea applied one layer up: the pool today amortizes pod creation ahead of demand; this amortizes *adoption* ahead of demand, and relocates the pool's own optimistic-lock trick (the adoption patch) into the acquisition patch. It is L1's logical conclusion: L1 got the transaction to one in-window write; this moves that one write before the window opens, leaving the client a single CAS.

**CRD-compat:** no schema change, no new API. Idle marker is a label; the claim status contract is fulfilled (written earlier, still watchable). It is an additive *usage pattern* + one controller loop + one SDK method. **Effort: M-L** (pre-binder loop with idle-set health monitoring and recycling; SDK acquire + fallback; TTL patch-after-acquire; bench phase). **Verdict: FEASIBLE — the only genuinely sub-floor option. Prototype it.**

---

## 2. Option 2 — Out-of-band ready channel (SSE/gRPC broker, subscribe-before-create)

**Mechanism.** SDK subscribes to a broker (or the router) keyed by claim name *before* POSTing the claim; the controller publishes Ready when it knows it. The status write continues unchanged for the record.

**Latency math — the push point is everything.**
- *Push after the status PATCH response* (the only durable-safe point): the perceived path is still S0 + S1 + S2 + S3 + broker-RTT — you removed only S4 (watch hop, 4-8ms floor / 20-45ms burst) and the client's watch-init. Best case ≈ **2 commits + 1 hop ≈ 27-38ms p50**. Not sub-floor; a broker bought ~5-10ms over a well-shaped watch (option 5b).
- *Push at in-memory reservation, before the commit:* this is client-side optimistic ready with extra steps — ROUND7 §6 rejected it and the L1 machinery shows exactly why it's worse than it looks: under L1, if the async sandbox patch 409s on a **genuine steal**, the flusher *clears the claim's binding and the claim re-adopts a different sandbox* (`adoption_flush.go` correctness inventory). A client that acted on the pushed identity may have already executed side-effectful work inside a sandbox that is subsequently rebound to a different claim — residue from client A visible to client B. That is an isolation violation, not a latency bug. Making it safe requires taint-tracking (any sandbox that served pre-durable traffic must be destroyed on binding failure, never returned or rebound) — a new protocol spanning router, controller, and pool.

**Hazards regardless of push point:** broker HA (a down broker adds a timeout+fallback path to every create), authn/authz must mirror claims RBAC (who may subscribe to claim X — the router's tokenreview machinery could be reused, but it's real work), at-most-once delivery forces a fallback watch anyway, so the SDK carries both paths forever.

**Effort: L. Verdict: REJECT.** Durable-push saves single-digit ms over option 5b at the cost of a stateful component; pre-commit push is the rejected optimistic-ready contract with an isolation failure mode.

---

## 3. Option 3 — Read-path shortcut: wait on the sandbox instead of the claim

**As stated — REJECT, and under L1 it is strictly *slower*.** Two independent defects:
1. *Identity delivery is impossible via the create response.* The binding is decided by the controller after the claim commits; the POST response cannot contain the sandbox name. A "preferred sandbox" hint annotation isn't binding until the controller's optimistic lock wins — the hint can be stolen, so the client would wait on a sandbox it may never own.
2. *Ordering under L1:* the sandbox-side patch (ownership labels) is now the *async* half — it lands after the claim status write, via the flusher. Watching the sandbox for the claim-UID label waits on the deferred write; watching the claim waits on the critical one. Under the classic 2-write shape this option would have saved one commit; L1 already banked that saving and inverted the order.

Nothing on the client needs the claim status before the first request *except the sandbox identity itself* — which leads to the reframing that IS worth doing:

**3′ — Router-held first request (latency masking, not floor beating).** Add a SandboxClaim informer to the sandbox-router (today it has only the pod informer, `sandbox-router/cache/cache.go`) and accept an `X-Claim-Name`/`X-Claim-Namespace` header alternative to `X-Sandbox-Id` (`sandbox-router/proxy/headers.go:27-31`). The SDK then POSTs the claim and *immediately* dials the router with its first real request; the router parks it until its claim informer shows bound+Ready, then resolves pod IP and proxies. Perceived time-to-first-byte ≈ S0+S1+S2+S3+(router informer hop, shared) — the client-side watch (and GAP-5's per-claim no-RV watch with its quorum-read init, `k8s_helper.py:139-147`) disappears from the path entirely, and connection setup overlaps the bind. Consistency: unchanged — the router acts only on committed status via its informer; the request is not forwarded until Ready, so nothing is replayed or revoked. Failure mode: bind never happens → router returns 504 + Retry-After after a bounded hold; body buffering must be capped. This composes with community #883/#850/#1174/#1198 (the router/gateway direction upstream is already building informer-based resolution). **Effort: S-M. Saves 10-40ms perceived (watch-init + S4) — reaches the floor, cannot pass it. Verdict: DO IT as hygiene regardless of option 1's fate.**

---

## 4. Option 4 — Aggregated API / custom long-poll endpoint (`GET /bind?claim=x`)

**Mechanism.** Controller (leader) answers a long-poll from its in-memory state the instant the status PATCH response returns — the earliest durable-safe moment any component knows the binding.

**Latency:** 2 commits + 0 watch hops + aggregation proxy hop ≈ **25-35ms p50** — a few ms better than 3′ (leader memory beats informer delivery by one hop), still not sub-floor.

**Why it loses anyway:** (a) *HA/leader-affinity* — an APIService routes through a Service to a random replica; only the leader has the memory; standby replicas must either proxy to the leader (hop back) or serve from informers (= option 3′ with more moving parts). (b) *Operational blast radius* — an unavailable APIService degrades API discovery cluster-wide; that is a heavy price for a wait endpoint. (c) *Auth* is solvable (delegated authn + SubjectAccessReview on `sandboxclaims/get`) but is exactly the machinery the router already runs. (d) CRD-compat is technically fine (additive aggregated group), but this adds a second API surface for information the CRD already carries — the least "boring" option on the list. **Effort: M-L. Verdict: REJECT — dominated by 3′ on every axis except ~5ms.**

---

## 5. Other ideas in class (each with its failure mode)

- **5a. Client-side adoption** (SDK does the sandbox CAS itself, creates the claim async "for the record"): 1 commit ≈ 15-25ms — sub-floor, but requires granting every client `patch` on Sandboxes (today the SDK needs only claims — this collapses the tenant/controller trust boundary); a crash between CAS and claim-create leaks an owned-by-nobody sandbox that no existing recovery path reclaims (claim-UID label points at a claim that never existed — `findAdoptedSandboxByClaimUID` recovery presumes a claim); and the record-write inversion breaks audit ("claim CREATE is the request of record"). **REJECT** — it is option 1 with the CAS moved to the wrong side of the trust boundary.
- **5b. Watch-before-create / session-scoped watch** (SDK opens the claim watch — with `resourceVersion="0"` or from the create response's RV — *before/at* POST; multi-claim clients share one stream): removes watch-init (quorum read + TLS + stream setup) from every perceived wait. Reaches the floor; already GAP-5/item-9 scope. **DO IT — table stakes, effort S.**
- **5c. Ready-response coupling** (controller writes status with a deterministic "bind epoch" the client polls with a single GET after a fixed delay): a GET is a commit-free read but polling either wastes RTTs or adds delay quantization; strictly worse than 5b. REJECT.
- **5d. Any variant that surfaces Ready before the status PATCH response returns** — including "the router peeks at the controller's reservation" — is optimistic-ready and inherits §2's isolation failure. Standing rejection.

---

## 6. Verdict table

| # | option | perceived latency achievable (p50 sustained) | sub-floor? | consistency risk | CRD-compat | effort | verdict |
|---|---|---|---|---|---|---|---|
| 1 | Speculative pre-binding (label-CAS acquire) | **12-25ms** (1 commit, 0 hops) | **yes** | low-med: CAS is a real lock; stale-Ready window needs first-request fallback; names/audit/spec-customization semantics shift | clean — no schema change; status still written+watchable (earlier) | M-L | **prototype** |
| 2 | Out-of-band ready channel | 27-38ms durable-push; 20-30ms pre-commit push | no / only unsafely | med (durable) / **severe** (pre-commit: revocation + sandbox-residue isolation violation via L1 steal-clear) | clean | L | reject |
| 3 | Wait on sandbox instead of claim (as stated) | worse than today under L1 (waits on the async half) | no | n/a — identity can't be delivered at create | clean | — | reject |
| 3′ | Router-held first request (claim-header resolution) | 30-40ms TTFB; removes client watch entirely | no (masks) | none — router acts on committed status | clean; composes with #883/#850 | **S-M** | **do it** |
| 4 | Aggregated API long-poll from leader memory | 25-35ms | no | low, but leader-affinity + APIService availability = operational hazard | technically clean, second API surface | M-L | reject (dominated by 3′) |
| 5a | Client-side sandbox CAS | 15-25ms | yes | high: RBAC collapse + unrecoverable leak window | clean schema, broken trust posture | M | reject |
| 5b | Watch-before-create / shared session watch | reaches floor (~30-40ms) | no | none | clean | S | do it (GAP-5/item 9) |

## 7. Recommended experiment

**One bench phase, three legs, one cluster** (after gate zero + GAP-1 clean instrumentation, since the floor itself is suspect):
1. Baseline: composed-tree create→Ready, SDK watch as-is.
2. **5b+3′ leg:** session watch from create-RV + router claim-header hold → measures how much of the gap to the floor is read-path waste (prediction: perceived p90 converges to ≈S0+S1+S2+S3).
3. **Pre-binding leg:** minimal pre-binder (bench-side loop is fine — create N vanilla claims/pool, maintain idle label) + SDK acquire = cached label LIST + resourceVersion-preconditioned merge patch with randomized candidate pick + fallback-to-create. Measure acquire p50/p90 at sustained 300/s, 409-retry rate, fallback rate, and stale-Ready incidence. **Success bar: p90 ≤ 40ms with <1% fallback and zero acquired-but-dead sandboxes reaching first-request.**

## 8. Not worth it if…

Not worth building if the GAP-1 clean-instrumentation gate-zero run shows the composed tree already sustaining p90 ≤ ~60-70ms — pre-binding then buys ≤30-40ms in exchange for a new controller loop, an acquisition protocol in every SDK, the loss of client-chosen claim names (weakening #1089 idempotency), and a vanilla-claims-only fast path; and it is not worth building *at all* if the production claim mix is majority non-vanilla (`env`/`volumeClaimTemplates`/`additionalPodMetadata`), because those claims can never ride it. The 3′+5b read-path work is worth doing under all outcomes.

---

**Key files referenced:** `/private/tmp/bench-candidate-tree/optimizations/ROUND7-PLAN.md` (§1.3 floor, §6 rejections, §7 stopping rule), `/private/tmp/bench-candidate-tree/optimizations/GAP-AUDIT.md` (GAP-1/4/5), `/private/tmp/bench-candidate-tree/extensions/controllers/adoption_flush.go` (L1 transaction, steal-clear recovery — the load-bearing hazard for option 2), `/private/tmp/bench-candidate-tree/extensions/controllers/sandboxclaim_controller.go` (status-first commit :395-471, optimistic-lock adoption, claim-UID recovery :2300-2335), `/private/tmp/bench-candidate-tree/extensions/api/v1beta1/sandboxclaim_types.go` (spec fields that gate pre-binding eligibility), `/private/tmp/bench-candidate-tree/clients/python/agentic-sandbox-client/k8s_agent_sandbox/k8s_helper.py` (per-claim no-RV watch :139-147), `/private/tmp/bench-candidate-tree/sandbox-router/proxy/headers.go` + `cache/cache.go` (router has pod informer + authz but no claim awareness today — the 3′ delta).

**Bottom line:** the floor is a property of "bind after request." Only pre-binding (option 1) changes that shape and goes genuinely sub-floor (~15-25ms, one CAS commit, no watch hops) while keeping the CRD contract byte-identical; options 2/4 buy single-digit ms unsafely or expensively; option 3 as stated is worse than today under L1, but its router-held reframing plus watch hygiene (5b) are cheap, safe, and worth shipping regardless.
