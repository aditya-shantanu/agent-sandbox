# PATH-TO-100MS — decomposing the 301ms burst p90, the additive plan for both regimes, and the sub-floor verdict update

Status: deep-research deliverable (2026-07-20), branch `perf-investigation-master`,
after the gate-zero and round-8 verdicts (RESULTS.md). Inputs: gate-zero leg-B
artifacts (`gs://kops-state-142966328212/perf-bench-results/gate0/B-clean-composed/`
— summary.json, watch.jsonl.gz, sandboxes.jsonl, metrics.jsonl.gz incl. the
kube-apiserver scrape, controller.log), round-8 `S2-sustained300/sandboxes.jsonl`,
ROUND7-PLAN, GAP-AUDIT, SUB-FLOOR-RESEARCH, and code verification on this tree.

Measured state this document starts from (all clean instrumentation):

| regime | p50 | p90 | p99 | note |
|---|---|---|---|---|
| smoke (20 claims, uncontended) | 19-22ms | 21-22ms | — | min 13.8ms; ack p50 8-10ms |
| burst-300 all-at-once | 273ms | **301ms** | 309ms | all-ready 0.31s; zero failures |
| sustained-300 (first-window headline) | 119ms | (176s — pool-exhaustion artifact) | — | see §3.1 for the honest supply-present quantiles |

---

## 1. QUESTION 1a — decomposition of the burst-300 p90 (301ms)

### 1.1 Method and a measurement caveat

The clean legs emit **no** "adoption timing" lines: the line is never-*sampled*
(dedicated zap core, `adoption_timing_log.go`) but still gated on `V(1)`
(`sandboxclaim_controller.go:540` `!logger.V(1).Enabled() → return`), and GAP-1
clean legs run without debug. Decomposition therefore comes from four
independent artifact sources, cross-checked:

1. **Per-claim client records** (`sandboxes.jsonl`): createCalled /
   createReturned / sandboxReady per claim.
2. **The client watch stream** (`watch.jsonl.gz`): receipt time + resourceVersion
   of every claim ADDED and Ready-MODIFIED event, plus the sandboxes stream.
3. **apiserver-side histograms** (metrics.jsonl.gz has a kube-apiserver scrape
   at 06:41:30 and 06:41:44 — the burst is exactly the delta): request
   durations per verb/resource, APF wait, watch-cache counters.
4. **Controller workqueue/rest_client histograms** (same delta).

Key reconstruction trick: etcd resourceVersions are a single global commit
sequence, so interleaving the claims stream with the *sandboxes* stream bounds
when each claim-status write actually **committed**, independently of when the
claims stream *delivered* it. (Example: claim-status commit at RV 18841 is
bracketed by sandbox events at RV 18795/18870 received at 74/107ms — so it
committed by ≤~107ms — yet the claims stream delivered it at 271ms.)

### 1.2 The decomposition table

Per-claim segments, burst-300 leg B (300/300, n=300):

| segment | p50 | p90 | basis |
|---|---|---|---|
| **S0** create → ack | 32ms | 50ms | client records; server-side POST sandboxclaims mean 22.7ms, 100% ≤50ms; all 300 creates **committed by ≤58ms** (acks returned) |
| **S1** create-commit → controller watch-in | ~30ms | ~165ms | client's ADDED receipt (66/216ms after create) minus ack, used as controller proxy — validated below |
| **S2** workqueue wait + pass compute | ~0.06ms | <1ms | workqueue delta: 3,302 items, queue-duration sum 0.19s, p99 <1ms |
| **S3** watch-in → claim-status commit (the one L1 write) | 25ms | 37ms | commit-time estimate (RV interleave) minus ADDED receipt — a tight 25/37ms across all ranks; server-side PATCH claims/status mean 15.8ms, 100% ≤100ms |
| **S4** status-commit → client Ready receipt | ≥143ms | ≥202ms | Ready receipt minus commit-time estimate (upper bound on commit ⇒ lower bound on S4) |
| **E2E** | 273ms | 301ms | min 45ms — the one claim that dodged both queues rode ack 15 + hop + patch + hop ≈ the floor |

Cross-checks: APF wait mean **0.10-0.13ms** on every priority level (not a
factor); status-commit estimates spread 53→266ms (p50 99 / p90 245); ready
receipts 45→312ms; corr(ready-rank, e2e) = **0.767** (round-4: 0.961) — rank
explains ~59% of variance now, so pipeline position still dominates but no
longer explains almost everything; the residual is where in the two watch
queues a claim happened to sit.

### 1.3 What the table actually says: the wall moved from writes to watch fan-out

The two big terms — S1 and S4 — are the **same mechanism traversed twice**:
the claims watch-event fan-out path (etcd → watch cache → per-watcher
dispatch) delivers events at **~0.7-0.9ms/event (~1,350 events/s) per
stream**. Evidence:

- All 300 creates committed by ≤58ms (~6,000/s — creation is NOT the wall),
  yet ADDED events dripped into the client stream at a steady ~0.74ms/event
  through 234ms.
- The controller's informer was paced **identically**: status commits track
  `client-ADDED-receipt + 25ms` with only ±12ms spread across all 300 ranks —
  two independent watcher processes (Go typed informer; Go dynamic client)
  received each event on the same schedule. The pacing is server-side (or at
  minimum common-path), not a client artifact.
- The e2e "flood" shape (only ~30 ready before 248ms, then 270 in a 65ms
  burst) is in-RV-order stream delivery: the 300 ADDED events (RV 18378-18703)
  all precede almost all Ready MODs (RV ≥18713), so on a single shared claims
  stream every Ready delivery queues behind the full ADDED backlog.
- Per-watcher JSON encode alone cannot explain it: claim events average
  **1,014 bytes** (sandbox events 3,544B, paced ~1.4-1.9ms/event — roughly
  per-byte-consistent). ~0.7ms/event for 1KB is dispatch-path overhead, not
  raw encode. Watchers on sandboxclaims: 3 (controller, bench client, KCM GC)
  — `apiserver_watch_events_total` delta 3,138 = 3 × 1,046 storage events.

What is exonerated, with numbers: **writes** (POST 22.7ms mean, PATCH 15.8ms
mean, 100% ≤50/100ms — the L1 single write is behaving exactly as designed),
**APF** (0.1ms waits), **workqueue** (<1ms), **etcd** (~900 burst-window
commits absorbed without visible strain), **conflicts** (the 32 "ONE-WRITE
ADOPTION LOST" recoveries in the log predate the round-8 reservation fix,
which is now merged; round-8 S2 measured 0 at 60× the scale).

One production nuance: **S4's 143-202ms is partly a bench-shape artifact.** A
production SDK opens a per-claim watch (field-selector, 1-2 events total) — no
backlog, so its S4 ≈ watch-init + one hop. The bench's single shared stream
pays the whole herd's backlog twice. The *controller's* S1, however, is real
in every deployment: its informer is always a shared stream. S1 is the
irreducibly-shared half of the wall; S4 is shared only for shared observers
(the bench client today; the sandbox-router informer if 3′ is built).

---

## 2. QUESTION 1a (continued) — additive plan: burst-300 all-at-once to <100ms p90

### 2.1 The arithmetic, re-run with measured numbers

ROUND7 §1.2 called burst-300 <100ms "arithmetic, not code" against an assumed
drain of 350-500/s. The measured composed drain is **~1,120/s** (300 ready in
0.268s), so the rank bound is now p90 ≈ 0.9·300/1120 ≈ **240ms** + floor ≈
what we measure (301). The prompt's model holds: **rank-domination still
binds, at ≈270/1120 ≈ 240ms.**

For p90 <100ms as a pure drain-rate statement: p90 ≈ floor(≈30-45ms under
burst) + 270/D ⇒ **D ≥ ~4,000-5,000 claims/s** (the ~3,000/s guess was
directionally right; the measured burst floor eats more of the budget). But
the claims stream carries **2 events per claim** (ADDED + Ready MOD), so the
per-watcher fan-out stage would need **~8,000-10,000 events/s ≈ 0.1ms/event —
7-9× today's 1,350/s.** No single lever gets there; the plan below removes
the backlogs structurally instead of racing them.

Which walls would a 3-5k/s drain hit? — checked against leg B:

| wall | measured | verdict at 3-5k/s |
|---|---|---|
| create-ack (300 POSTs) | committed ≤58ms ≈ 6k/s; server POST 22.7ms mean | **not a wall**; but ack p90 50ms already spends half the 100ms budget for late-acked claims — item 3 must hold it ≤50ms |
| apiserver seats / APF | ~120 concurrent seats used; APF wait 0.1ms | not a wall (tuned CP + PL insulation) |
| etcd commits | ~900 commits in the burst window, no strain | not a wall at 300-burst; thin margin at 1000/s sustained → multi-etcd (ROUND7 item 8b) is insurance, not prerequisite |
| watch fan-out | **1,350 ev/s/stream** | **THE wall — both S1 and S4** |

### 2.2 The additive plan (burst regime)

| # | lever | mechanism | expected effect on burst p90 | basis |
|---|---|---|---|---|
| B1 | **Controller sharding** (ROUND7 item 6, composed with #1213) | 8 namespace shards ⇒ each controller informer sees ≤38 ADDEDs ⇒ S1 backlog 38 × 0.7ms ≈ **27ms** (vs ~165) | **−120 to −140ms** | measured 0.7ms/event pace; backlog is linear in per-stream event count |
| B2 | **Per-claim (or per-shard) client watches** | production SDK shape: 1-event watcher ⇒ no S4 backlog ⇒ S4 → ~5-10ms hop; bench must add a per-claim-watcher mode (GAP-5 leg) or shard its stream per namespace to observe it | **−130 to −190ms** on the shared-stream number (this is partly reclassifying a bench artifact, partly real for any shared observer) | S4 lag is backlog order, not per-event cost, for a 1-event watcher; watch-init is already fixed (watch-from-RV merged) |
| B3 | **CBOR serving/storage** (item 4) | cuts per-event encode/decode on whatever stream remains shared, plus S0/S3 body encode | **−10 to −30ms** composite; alone it is ~2-4× on the encode *share* of 0.7ms/event ⇒ lands ~150-200ms p90 — insufficient without B1/B2 | KEP-4222 benchmarks; claim events are 1KB so encode is not the dominant per-event term — do not oversell |
| B4 | **Create-ack reduction** (item 3) | POST admission/validation trim + client connections; server mean 22.7 → ~12-15ms | **−10 to −20ms** on the p90 claim's S0 | smoke ack p50 8-10ms shows the floor; the delta is burst seat-share |
| B5 | **Fan-out stage disambiguation experiment** (new, S) | one leg: second in-cluster watcher + apiserver CPU profile during a burst — pins the 0.7ms/event to per-watcher dispatch vs cacher ingestion. **This gates B1's S1 saving**: if ingestion (single per-resource cacher goroutine) dominates, namespace-sharded *watchers* don't split it — only CBOR-storage decode, KEP-5866, or multi-apiserver do | decision data | the leg-B artifacts cannot separate the two; both hypotheses fit the identical two-watcher pacing |

**Post-plan burst arithmetic:** ack 40-50 + S1 15-27 (sharded, CBOR) + S3
20-30 + S4 5-10 ≈ **80-115ms p90 — achievable-borderline**, and only with B1 +
B2 together; neither alone gets under ~150ms. The honest position stays the
ROUND7 one — the <100ms target is *stated* against sustained-300, and
burst-300 <100ms is a stretch goal that now has a concrete, measured path
instead of an impossibility proof. The change since ROUND7: drain is 1,120/s
not 350-500/s, and the binding wall is watch fan-out, not writes — L1 already
killed the write wall.

---

## 3. QUESTION 1b — sustained-300 with adequate supply

### 3.1 The honest first-window quantiles (round-8 S2, per-claim records)

The verdict table's "[0-10s) p50 119ms / p90 176.2s" mixes two populations:
claims that arrived while pool supply existed, and claims that arrived after
the ~5s drain. Recomputed from `S2-sustained300/sandboxes.jsonl` (17,822
records) by arrival time:

| arrival window | n | e2e p50 | e2e p90 | e2e p99 | ack p50/p90 |
|---|---|---|---|---|---|
| [0,2s) | 608 | **30.3ms** | **59.9ms** | 122.5ms | 11 / 22ms |
| [0,3s) | 916 | 38.1ms | **96.2ms** | 131.6ms | 14 / 34ms |
| [2,4s) | 607 | 46.6ms | **105.2ms** | 136.8ms | 17 / 42ms |
| [4,6s) | 581 | 135.7ms | 61s (pool cliff) | — | 15 / 40ms |
| warm-hit cohort, first 10s (e2e <2s, n=1,517) | | **41ms** | **95ms** | 134ms | — |

**The claim path at a true 300/s arrival rate, with supply, measures p50
41ms / p90 95-105ms — the <100ms p90 target is already being touched**, and
the p50 sits almost exactly on ROUND7 §1.3's predicted sustained floor
(≈35ms). The drift 60→96→105ms across the first three windows is rising
contention (refill churn + backlog onset), which is what the remaining items
buy margin against. Everything after ~5s is queueing behind pod supply — a
different problem with a different unit (pods/s, not ms).

### 3.2 Which ROUND7 items close the gap below 100 — per-item budgets

Open-state verification on this tree: `--no-spec-adoption` (default-on) is the
**sandbox-side half only**; the claim-side metadata-only adoption patch
(ROUND7 item 2, ROUND6 §3.3 design) is **not implemented** (no trace in
`sandboxclaim_controller.go`). CBOR, create-ack work, and sharding are all
still open.

| item | expected on sustained p90 (95-105 → target) | basis |
|---|---|---|
| item 3 create-ack | **−10 to −15ms** (first-window ack p90 34-42ms → ~20ms) | smoke ack p50 8-10ms; POST server mean 22.7ms is admission+encode, trimmable; ack is additive in every claim's path |
| item 2 claim-side no-spec | **−5 to −15ms** | removes the sandbox status echo write + one sandbox reconcile per adoption — less competing traffic in the same cacher/etcd/seat pool at 300 adoptions/s (ROUND7's estimate, unchanged — the mechanism was verified in leg B: 835 sandbox PATCHes + 1,537 sandbox status PATCHes rode along in the burst window) |
| item 4 CBOR | **−5 to −10ms** | at 300/s the claims stream (~600-750 ev/s) runs at ρ≈0.5 of the 1,350 ev/s fan-out capacity — queueing is modest, so CBOR buys less here than at burst; S0/S3 encode share is the real sustained win |
| sharding (item 6) | ~0 at 300/s | fan-out unsaturated; this is the 500-1000/s rate-holding item |
| GAP-12 payload trim | reprioritize **down** | measured: claim events are already only ~1KB; the trim targets are sandbox events (3.5KB) — indirect benefit only |

**Projection: sustained-300 p90 ≈ 60-80ms with items 2+3+4 landed** — under
the target with margin, and consistent with ROUND7's §1.2 prediction
(90-140 → items → 65-80) now that the instrumentation tax is gone.

### 3.3 The actual blocker for a 60-second <100ms demonstration: supply

At the measured contended refill (~26-46 creates/s/pool, creates outrunning
Ready 3-4×, node pipeline ~50-65 ready/s on 34 nodes), a 60s × 300/s phase
starves after ~5s regardless of claim-path latency: deficit ≈ (300 − ~60)/s ×
55s ≈ 13k pods — no pool size on this cluster covers it. To *demonstrate*
sustained-<100ms-p90 over a full window, pick one:

1. **Rate within supply:** ramp legs at 100-150/s (within the measured ready
   throughput) — expected p90 well under 100ms for the whole window; this is
   the cheapest true "sustained <100ms" result and validates §3.2's items.
2. **Deeper shock absorber for exactly 60s:** pool ≈ deficit ≈ 13-14k warm
   pods — beyond the 3,668-pod envelope; not feasible at 34 nodes.
3. **More nodes:** ~150-200 nodes (RESULTS round-8 verdict path 1) makes
   300/s indefinitely supply-feasible.
4. **L6 recycling** — the only path where sustained rate decouples from pod
   churn; still gated on the security memo.

---

## 4. QUESTION 2 — SUB-FLOOR-RESEARCH re-validated

### 4.1 Assumption audit against the new data

| SUB-FLOOR assumption | then | now | effect |
|---|---|---|---|
| floor | 31-40ms (instrumented smoke) | **14-23ms clean** (leg-B smoke min 13.8 / p50 18.7 / p90 20.9; leg-S 13/22; round-8 21/22/23) | pre-binding headroom **grew**: option 1's 12-25ms acquire estimate now sits at ~the floor itself, and the gap it can close vs the composed path widened at p50 |
| composed sustained p90 (the "not worth it if ≤60-70ms" gate) | unmeasured | **95-105ms with supply** (§3.1) | the gate does **not** fire today — pre-binding is still in play; but §3.2 projects 60-80ms post-items, which straddles the gate |
| composed sustained p50 | predicted 65-95 | **41ms measured** | pre-binding's p50 saving shrank: 41 → ~15-20ms acquire = **−20 to −25ms**, not the −50 to −70 the doc's era implied |
| L1 as flag/prototype | assumed | **default-on** (main.go:180, gate-zero validated) | option 3's rejection is now permanent for every deployment (the sandbox-side patch is the async half by default); option 2's durable-push point unchanged |
| adoption race machinery | pre-round-8 | **reservation + DirectReader merged** (`650bc0e`) | see §4.2 — pre-binder gets cheaper |

New fact the original doc could not know: the burst wall is **watch fan-out
pacing** (§1.3). This cuts both ways for the sub-floor options:

- It *strengthens* pre-binding's core claim: the acquire path (1 CAS, 0 watch
  hops) bypasses **both** paced fan-out traversals, so under burst its
  advantage is far larger than the floor math suggested (it dodges the ~240ms
  rank term entirely, not just ~20ms of commits).
- It *weakens* 3′ (router-held first request) at burst: the router's claim
  informer is a shared watcher — it inherits the same S1-style pacing the
  controller has, so at burst the router masks the client's watch but still
  waits on its own paced stream. At sustained ≤300/s (stream ρ≈0.5) 3′ works
  as designed. 3′ remains worth building; CBOR/BS-side fan-out work now also
  benefits it directly.

### 4.2 Does round-8 make pre-binding easier? Yes — materially.

The reservation machinery (`extensions/controllers/queue/simple_sandbox_queue.go`:
pop-reserves the key, `Release`/`Forget` lifecycle, reservations survive until
the sandbox DELETE event) is exactly the "no two actors select the same
candidate" primitive a pre-binder loop needs to keep its idle buffer from
racing live claim traffic — previously the pre-binder had to invent this. And
`DirectReader` (`sandboxclaim_controller.go:446-465`, `mgr.GetAPIReader()`)
is the acquire-verify/recovery read pattern the SDK fallback path needs
(fresh-read, never informer-lagged). The wedged-loser fix also removes the
worst stale-Ready hazard class (bindings pointing at dead sandboxes now clear
in one RTT). Net: option 1's effort drops **M-L → M**; its consistency-risk
row improves (the CAS-loser and stale-binding recovery paths are now
production code, verified at 17,822-binding scale with zero double-binds).

### 4.3 Updated verdict table

| # | option | perceived latency (p50 sustained) | vs measured composed (41 / 95-105) | sub-floor? | status / verdict |
|---|---|---|---|---|---|
| 1 | Speculative pre-binding (label-CAS acquire) | 12-25ms | −20/−25ms p50; −55/−75ms p90; dodges the burst rank term entirely | **yes** | **defer to a decision point** (§4.4) — effort now M; do NOT build yet |
| 2 | Out-of-band ready channel | 27-38ms durable / unsafe pre-commit | dominated | no | reject (unchanged; reservation reduces steal odds but the commit-crash window stands — pre-commit push is still optimistic-ready) |
| 3 | Wait on sandbox instead of claim | worse than today | — | no | reject, now **permanently** (L1 default-on: sandbox patch is the async half everywhere) |
| 3′ | Router-held first request | ~35-45ms TTFB sustained; at burst inherits the shared-watcher pacing wall | masks client watch-init + S4 | no (masks) | **still open** (verified: `sandbox-router/` has no SandboxClaim informer or claim-header path) — build; pair with CBOR for its informer |
| 4 | Aggregated API long-poll | 25-35ms | dominated by 3′ | no | reject (unchanged) |
| 5a | Client-side sandbox CAS | 15-25ms | — | yes | reject (trust-boundary, unchanged) |
| 5b | Watch-before-create / session watch | reaches floor | −10-40ms of watch-init already banked | no | **half done**: watch-from-explicit-RV merged (SDK `k8s_helper.py:154`, bench GAP-5 fix in 2b60967); the **shared session-level watch for multi-claim clients is still open** — and §1.3 adds a caveat: a session-shared stream re-creates the S4 backlog under burst, so implement it with per-namespace sharding or a bounded fan-in, not one global stream |

### 4.4 Pre-binding: build now, after ROUND7 items, or never?

**After the ROUND7 items — with an explicit decision point, not now.** Three
reasons, all from this round's data:

1. **The sustained bottleneck is supply, and pre-binding adds zero supply.**
   Idle pre-bound claims consume the same warm pool; under the measured refill
   ceiling an idle-claim buffer just drains ~0.5s earlier. Building it now
   optimizes the 5 seconds before the cliff.
2. **The gate is about to be re-tested for free.** Items 2+3+4 project
   sustained p90 60-80ms (§3.2). If the re-measured number lands ≤70ms, the
   original "not worth it" clause fires and pre-binding buys ~40-50ms p90 for
   a new controller loop + SDK acquisition protocol + the #1089 idempotency
   regression — a bad trade unless product explicitly wants p50 <25ms.
3. **If it is built, it is cheaper and safer than when SUB-FLOOR was written**
   (§4.2), and the burst analysis gives it a second justification (rank-term
   bypass) that should be weighed *then* — specifically if burst-shape
   arrivals with <100ms p90 ever become a hard requirement, pre-binding is a
   much shorter path than B1+B2+B3 combined.

Decision rule to record: **after items 2/3/4 land and a supply-adequate
sustained-300 leg is measured — build pre-binding iff p90 > 70ms, or product
commits to a p50 <25ms / burst-p90 <100ms requirement. Otherwise close it.**
The 3′ + 5b(session, sharded) work proceeds regardless, unchanged from
SUB-FLOOR's "do it" verdict.

---

## 5. Recommended execution order

1. **Supply-side sustain work first** (round-8 verdict paths: ramp benches at
   100→200→300/s, refill/node levers, node-count sizing; L6 memo in flight) —
   it gates any honest 60-second <100ms demonstration (§3.3) and every
   sustained re-measurement below.
2. **Item 2 (claim-side no-spec)** — still unimplemented; S-M; sequenced after
   the #1118 rebase check exactly as ROUND7 §2 requires.
3. **Item 3 (create-ack)** + **5b completion (session watch, sharded)** +
   **3′ (router claim informer + held first request)** — three S efforts,
   independent, parallelizable.
4. **B5 fan-out disambiguation leg + item 4 (CBOR A/B)** on one fresh cluster
   — B5's answer decides how much of B1's S1 saving is real before item 6's
   manifests are built.
5. **Item 6 (sharding at rate)** — the burst-S1 killer and the 500-1000/s
   rate-holder.
6. **Re-run the sustained gate with adequate supply; apply the ROUND7 §7
   stopping rule** — updated for the clean floor: stop latency work at
   sustained p90 ≤100ms with S0 ≤35ms; and the "revised-target" clause moves
   from <60-70ms to **<45-50ms p90** (2× the clean 14-23ms floor) as the line
   below which only pre-binding or contract changes can go.
7. **Pre-binding decision point** per §4.4.

Measurement riders (cheap, do with whatever runs next): (a) expose queueLat /
patch-RTT / watch-in-lag as **histogram metrics** so clean legs decompose
without V(1) logs (this round had to reconstruct S1/S3/S4 from RV interleaves —
it worked, but it shouldn't be necessary); (b) add a per-claim-watcher bench
mode (GAP-5 leg) so B2's effect is observable; (c) keep the apiserver scrape
in every leg — the 2-snapshot delta was decisive here.

---

## 6. Bottom line

- The 301ms burst p90 is: **ack 32-50ms + controller watch-in up to ~165ms +
  one 25-37ms adoption write + client watch-out ≥143-202ms** — the writes are
  solved (L1 verified: 15.8ms mean server-side, zero conflicts); both big
  terms are the same ~0.7-0.9ms/event watch fan-out wall traversed twice, and
  rank still dominates (corr 0.767, drain 1,120/s ⇒ ~240ms rank term).
- Burst-300 <100ms is no longer arithmetically impossible — it needs
  ~4-5k/s drain ⇒ ~8-10k ev/s fan-out, reachable only via sharding + per-claim
  /sharded client watches + CBOR together, landing ~80-115ms: borderline, a
  stretch goal, correctly not the stated target.
- Sustained-300 with supply already measures **p50 41 / p90 95-105ms**; items
  2+3+4 project **60-80ms** — the target falls on the claim path; what's left
  of "<100ms at 300/s for 60s" is pods-per-second supply, not milliseconds.
- SUB-FLOOR stands with updated numbers: pre-binding is the only sub-floor
  option, now cheaper to build (round-8 reservation + DirectReader) but less
  necessary (p50 41ms) and useless against the supply wall — decide after the
  ROUND7 items; 3′ and the session-watch half of 5b are still open and still
  unconditionally worth doing.
