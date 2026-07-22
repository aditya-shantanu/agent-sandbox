# Round 1256 — three-arm adjudication of PR #1256 v1 vs the optimistic-lock prototype

Date: 2026-07-22. Purpose: adjudicate, with one controlled A/B/C, (a) what
PR #1256 v1's `lastWrittenStatusMap` guard actually buys and costs on top of
merged #1118, (b) whether the reviewer-suggested optimistic-lock prototype
delivers the same #940/stale-write fixes without the costs, and (c) the
review's two empirical disputes (self-healing regression; byte-identical
patch watch behavior).

**Arms** (identical tuned 12-node kops clusters, e2-standard-16 control
plane, identical harness build):

| arm | tree | sha |
|---|---|---|
| BASE | post-#1118 upstream main | `4357fa4` |
| ARM-1256 | PR #1256 v1 (`upstream-p4-claim-write-suppression`, lastWrittenStatusMap guard) | `160e00f` |
| PROTO | optimistic-lock prototype (`proto-1256-optimistic-status`) | `8b5408f` |

Artifacts:
`gs://kops-state-142966328212/perf-bench-results/round-1256-adjudication/<ARM>/{rep1,rep2s}/`.

---

## Rep 1 — burst-300 + fill + throughput-mif50/100 + sustained 45/s × 60s

Zero failures in every arm and phase.

**Burst-300 (create→Ready):**

| arm | p50 | p90 | all-ready |
|---|---|---|---|
| BASE | 1082ms | 2096ms | 2.44s |
| ARM-1256 | 838ms | 1660ms | 1.91s |
| PROTO | 904ms | 2019ms | 2.65s |

**Sustained 45/s × 60s:**

| arm | p50 | p90 | notes |
|---|---|---|---|
| BASE | 108ms | 236ms | 179 claim workqueue retries |
| ARM-1256 | **727ms** | **4177ms** | p99 6.1s, degrading monotonically through the leg; 46 cold starts; **23,249** claim workqueue retries |
| PROTO | 113ms | 261ms | 357 claim workqueue retries |

**Throughput mif50/mif100 (p50):** BASE 1064/1144ms, ARM-1256 1136/1220ms,
PROTO 890/904ms.

**#940 histogram-sample inflation (observations vs claims):** BASE
4503/2955 = **+52%** (333 obs for 320 claims on the burst label alone);
ARM-1256 ~exact; PROTO exact on every label.

**Byte-identical-patch watch probe:** 0 byte-identical MODIFIED events in
ALL three arms — confirms the apiserver short-circuits a write whose
serialization is byte-equal to the stored object (no resourceVersion bump,
no watch event; k8s.io/apiserver v0.36.2 `pkg/storage/etcd3/store.go:553-576`).
The reviewer was right: #1118's "one idempotent re-patch" degrades to a round
trip, never a watch storm.

**Retry fingerprints (per-claim completion-gap forensics):**

- BASE: conflict-driven, scattered 0-8s (the measured ≈400ms/conflict-cycle
  cost, ROUND2-FINDINGS.md root cause #3 — not rate-limiter backoff).
- ARM-1256: fast fixed-requeue churn — 57/72 gaps <1s with only 19 actual
  conflicts; the 23k retries are the 50ms-requeue machinery polling.
- PROTO: optimistic-409 drops with per-item exponential spacing, 5-11s on
  the residual stragglers.

---

## Rep 2S — sustained-only control + manual chaos (clusters reused; controllers at 11h uptime, 0 restarts)

**ARM-1256's rep1 sustained collapse VANISHES without a preceding burst:**
windows 118/288/109/106/129ms, 0 cold starts. The rep1 collapse is therefore
triggered by preceding-burst debris left in the guard machinery, not by the
sustained load itself (consistent with the earlier SV-P4 leg's clean 192ms,
which also had no preceding burst).

**Manual chaos — external status wipe, 5 targets per arm, all landed:**

| arm | repaired | repair latency |
|---|---|---|
| BASE | 5/5 | 18-52ms |
| PROTO | 5/5 | 144-381ms (optimistic-lock retry path) |
| ARM-1256 | **0/5** | zero restore events within the 115-1445ms observation windows (bounded by dwell deletion); 7 watch events/target vs 8-with-restore in the other arms |

This is the `lastWrittenStatusMap` write-suppression signature: the guard
trusts its memory over the cluster and refuses to repair externally wiped
status — **empirically confirming igooch's self-healing objection to
PR #1256 v1.**

The annotation-removal chaos prediction is UNGRADED: all 5 targets per arm
were NotFound-raced by the 2s claim dwell before grading. Re-run rides the
isolation leg (dedicated long-lived claims).

**#940 independent leg (observations/claims):** BASE 4232/2680 = **1.58×**
(inflation reproducible across reps); ARM-1256 2635/2635 = 1.000; PROTO
2627/2664 = 0.986 (the 37-claim shortfall is claims deleted pre-sample
during backlog, not missed observations).

---

## RESOLVED (2026-07-22, isolation-leg RCA) — the PROTO sustained ramp

> **Root cause found and fixed** (fix local on `proto-1256-optimistic-status`
> @ `ecdba21`, test/lint green; not yet pushed). The amplifier is a
> control-flow hole reached at rate by the prototype:
> `adoptSandboxFromCandidates` treats a `completeAdoption` failure (404 from
> a stale cache view of a deleted candidate — 3,845 PATCH 404s in leg B) as
> "try the next candidate" AFTER the adoption annotation is already
> committed, so the pass overwrites the committed assignment (the claim's
> in-memory resourceVersion is current — the status optimistic lock
> arbitrates CLAIM staleness, not SANDBOX staleness). Watch-stream
> forensics, leg B vs leg A: **340 vs 0** nonempty→different
> assigned-sandbox annotation flips (max 8/claim), **323 status rebinds**
> incl. 21 cold-create overwrites of already-bound claims, **74 Ready
> True→False regressions** — all concentrated in the collapse minute. Each
> flip orphans/steals sandboxes (the unlocked `MergeFrom` adoption patch
> could silently re-transfer ownership) and feeds the upstream sandbox
> controller's status-Update error-retry churn (PUT 409 10,880 / PUT 404
> 8,200 / 18,709 wq retries, ~19 reconciles/sandbox); the write volume grows
> informer lag which produces more stale views — positive feedback, hence
> the monotonic ramp. BASE carries the same upstream fall-through but aborts
> the pass at the first annotation-Update conflict, so it rarely reaches the
> hole; the prototype's (correct) in-pass annotation retry made it reachable
> at rate. Fix (stateless): optimistic lock on the adoption patch +
> `resolveAdoptionCompletion` — authoritative APIReader resolution
> (no-write when linkage already true; one fresh-base re-patch when still
> pool-owned; TERMINAL cleanup of deleted/stolen references, never retried,
> paced by the workqueue rate limiter). Regression tests pin the no-flip,
> idempotent-no-write, and terminal-cleanup shapes. Exonerated with data:
> APIReader GET volume (432 total) and the claim workqueue (1,064 retries).
> Residual open question (minor): what seeds the initial window-0 elevation
> on a second run before any flips occur (17:20 minute has zero anomalies;
> all flips land in 17:21) — the runaway loop itself is closed regardless.

### Original open-item record (superseded by the RCA above)

In rep2s (reused cluster, sustained-only), PROTO ramped p50 637→2089ms from
window 2 onward, p99 12.3s, 2,043 claim retries — beginning BEFORE the +26s
chaos wipes, and the inverse of its clean rep1 sustained leg (113/261ms).

**Status: OPEN — do not cite a mechanism yet.** An isolation leg is running
(fresh cluster + fresh deploy; burst+sustained then sustained-after-idle; no
chaos; plus proper annotation-removal grading on dedicated long-lived
claims). Candidate attributions, deliberately unadjudicated until that leg
reports:

1. stale-reuse conditions on the reused cluster/controller (mirror of the
   ARM-1256 burst-debris finding), vs
2. intrinsic to the prototype under sustained-only load — prime suspect the
   APIReader uncached-GET retry feedback loop (every adoption-annotation 409
   retry issues a quorum read against an already-loaded apiserver).

---

## Interim verdict

- **BASE:** #940 inflation is real and reproducible (1.52-1.58× across two
  independent legs); stale-view retry costs scatter completions over 0-8s.
- **ARM-1256 (v1):** fixes #940 exactly, but fails chaos self-healing 0/5
  (the reviewer's objection, now measured) and collapses under sustained
  load after a burst (727ms p50 / 4.2s p90, 23k retries) from guard debris.
  Not shippable as designed.
- **PROTO:** #940 exact on every label, self-heals 5/5, burst and sustained
  comparable to BASE or better, mif legs best of the three — pending the
  open sustained-only ramp item above. The #1256 rework to this design
  proceeds; final validation gates on the isolation leg.

Ops/RCA notes for this round (adjarm chaos_loop wedge, artifact recovery,
cluster-deletion pitfall): `INFRASTRUCTURE.md`, round-1256 section.
