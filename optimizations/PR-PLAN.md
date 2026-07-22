# Upstream PR plan (12 PRs) — carved from the perf investigation

Status: wave 1 in flight (2026-07-20); **first merge landed 2026-07-21 —
P3 = [#1245](https://github.com/kubernetes-sigs/agent-sandbox/pull/1245)
merged as upstream `4357fa4`** (see "Post-merge status" below). Source of
truth for what goes upstream, in what order, and what stays on the fork.
Task numbers refer to `UPSTREAM-ISSUE-DRAFT.md`.

Ground rules for every upstream PR:

- Based on `kubernetes-sigs/agent-sandbox` `main` tip at send time; fully
  standalone (compiles + tests green against upstream main alone).
- Flags ship **default OFF** upstream (the fork's default-on flips are a
  fork policy, not an upstream proposal).
- Evidence phrased as "measured in a 300-claim warm-adoption benchmark"
  with the numbers; no references to fork-internal docs/branch mechanics.
- No assumptions from fork-only surgery (e.g. the v1alpha1/webhook removal,
  task 20): webhook blocks in `main.go` stay intact.

## The 12 PRs

| PR | task(s) | title (shape) | status |
|----|---------|---------------|--------|
| P1 | 1 | perf(controller): dedicated watch connection + HTTP/2 API connection sharding (`--separate-watch-connection`, `--api-connections`, default off) | **wave 1** |
| P2 | 3 | fix(sandboxclaim): cold-start guard (indexed List fallback + bounded defer) + adoption optimistic lock | wave 2 |
| P3 | 4 | perf(sandboxclaim): stop fanning pool status churn out to every claim (GenerationChangedPredicate + bound/deleting skip) — links #527 | **MERGED** upstream 2026-07-21 as [`4357fa4`](https://github.com/kubernetes-sigs/agent-sandbox/commit/4357fa4425f5999825c84af7d9d2e949c0b36850) ([#1245](https://github.com/kubernetes-sigs/agent-sandbox/pull/1245)) |
| P4 | 2 + 5 | fix(sandboxclaim): duplicate startup-latency metrics and stale-status overwrites via optimistic-locked status writes (was: stale-pass write suppression) | **filed as [#1256](https://github.com/kubernetes-sigs/agent-sandbox/pull/1256); REWORK SHIPPED 2026-07-22** — v1 memory-guard design abandoned after igooch's review (her self-healing objection empirically confirmed, 0/5 chaos repairs); reworked to stateless optimistic-locked writes, v24 gates all 6 PASS, head `2588c32`. /hold stays pending her re-review |
| P5 | 6 | perf: targeted metadata merge patches (`internal/rawpatch`) + `--disable-claim-events` / `--disable-claim-observability-annotations` (default off) — reconcile with #1087/#1114 | wave 2 |
| P6 | 7 | perf(controller): informer cache diet (strip managedFields, PodCacheTransform, opt-in `--cache-label-selectors`) — links #836, #484 | wave 2 |
| P7 | 8 | perf(warmpool): refill shaping (`--sandbox-warm-pool-replenish-delay`, `--sandbox-warm-pool-max-refill-rate`, default 0) + `--pool-dedicated-connection` (default off) + backlog-aware cold-start polling — supersedes #913, links #1182 | wave 3 (needs P1's transport.go) |
| P8 | 13 | fix(controller): release leader lease on graceful shutdown (`LeaderElectionReleaseOnCancel`) + lease-handover regression test | **wave 1** |
| P9 | 9 | feat(sdk): single-watch `wait_for_claim_ready` + 50ms dev port-forward polling + README latency guidance — links #574, #286 | **wave 1** |
| P10 | 14 | feat(router): namespace/name cache resolution (PodIP header → UID cache → name cache → DNS last) — links #883; complementary to Envoy RFC #850 | **wave 1** |
| P11 | 11 | fix(controller): sandbox status writes via `Status().Patch(MergeFrom)` — coordinate with #882 (adopt its writes-metric idea) | wave 2 |
| P12 | 15 | test/bench: measurement-honesty fixes (never-sampled timing, monotonic queue latency, client-connection calibration) — rebase onto #1148's harness split; file against #403/#624 | wave 3 |

## Send order

1. **Wave 1 (now):** P1, P3, P8, P9, P10 — mutually independent, zero
   overlap with open PR #1118, each either default-off or
   behavior-identical, spread across maintainer areas (controller
   transport, claim watch, manager lifecycle, Python SDK, router).
   *Amendment (2026-07-21):* every PR we OPEN must carry a this-change-alone
   A/B run (tuned 6-node kops cluster, one cluster reused, clean
   instrumentation) — so only P1, P3, P9 are opened in wave 1, each after
   its A/B leg; P8 and P10 are held fully prepared (branches validated +
   pushed, complete bodies in `pr-drafts/P8.md` / `pr-drafts/P10.md` with
   their deferred A/B protocols).
   **FILED (2026-07-21, each with per-PR same-cluster A/B evidence from
   `perf-bench-results/wave-ab-3/`):**
   - P1 → [PR #1240](https://github.com/kubernetes-sigs/agent-sandbox/pull/1240)
     (p90 2684→2175ms −19%, all-ready 3.87→2.90s, claim workqueue
     queue-duration p50 221.6→8.9ms at stock defaults; 400-worker forensic
     2479→1740ms cited)
   - P3 → [PR #1245](https://github.com/kubernetes-sigs/agent-sandbox/pull/1245)
     (claim reconciles −19% at stock 50 workers; at 400/400 workers −43%
     (9,207→5,259), p90 1427→985ms, all-ready 8.39→6.42s; PAIR-P3HC)
     — **MERGED 2026-07-21, upstream sha `4357fa4` (new main tip)**
   - P9 → [PR #1241](https://github.com/kubernetes-sigs/agent-sandbox/pull/1241)
     (SDK wait mean 68.4→54.9ms, p90 83.7→66.0ms, n=50/mode, same cluster)
   A/B mechanics: parallel per-PR cluster pairs (BASE + change leg on the
   same cluster), hybrid bench branches `bench-wave1-{base,p1,p3}` = leg
   code + fork harness overlay; orchestrators v14-v16 archived in
   `perf-bench-scripts/`. Post-filing stress validation (fill +
   throughput-mif100/50 + sustained 40/s) runs as `wave-sv-1`.
2. **Wave 2 (after wave-1 review signals):** P2, P5, P6, P11 — claim/
   sandbox controller semantics; sequence so each rebases trivially.
3. **Wave 3:** P7 (reuses P1's `newIsolatedHTTPClient`; send after P1
   merges), P12 (after #1148 settles).
4. **P4:** parked until #1118 merges (see table).

## Exclusions (stay on the fork / need a decision first)

- **Task 16 (APF insulation manifests)** — bench-only; shipping defaults
  need an upstream seat-sizing decision. Candidate for a docs/discussion
  issue, not a PR.
- **Task 17 (one-write adoption + reservation/DirectReader hardening)** —
  🟡 semantic trade-offs (autoscaler-eviction window, multi-instance steal
  window) + #1118 rebase-watch. Propose via issue/design discussion after
  wave-1/2 credibility is established.
- **Task 18 (3→2 write adoption collapse)** — superseded on the hot path
  by task 17; legacy-path shape only. Folded into the task-17 discussion.
- **Task 19 (namespace sharding)** — must re-scope onto PR #1213's
  namespaced-mode flag surface (keep only per-shard Lease + topology +
  safety audit); also round-10 showed it does not raise the 300/s supply
  ceiling, so pitch as the 500-1000/s watch-ingestion lever only.
- **Task 20 (drop v1alpha1 + conversion webhook)** — needs a storage
  migration story and a maintainer decision (#751); conflicts with open
  #1188/#1106. Issue-first.
- **Task 10 (write-behind coalescing) and task 12 (no-spec adoption)** —
  flag-gated and validated, but they reshape write semantics; queue behind
  wave-2 outcomes rather than pre-assign a slot (candidates to replace P4
  if #1118 stalls).
- Fork default-on flips (`--separate-watch-connection`,
  `--api-connections=4`, `--one-write-adoption`,
  `--sandbox-write-behind-window=250ms`, `--no-spec-adoption`) — fork
  policy; upstream defaults stay legacy until maintainers opt in.

## Post-merge status (2026-07-21, after #1245 landed)

**Merged:** P3 → [#1245](https://github.com/kubernetes-sigs/agent-sandbox/pull/1245)
merged to upstream main as `4357fa4` (the new tip; #363 operator
scaffolding landed just below it as `db7e7a2`).

**All remaining open PRs re-checked against the new tip — every one is
MERGEABLE; none rebased** (no gratuitous rebases; `UNSTABLE` merge-state is
only pending/failing CI checks, not conflicts):
[#1240](https://github.com/kubernetes-sigs/agent-sandbox/pull/1240) (p1
transport sharding), [#1241](https://github.com/kubernetes-sigs/agent-sandbox/pull/1241)
(p9 SDK single-watch), [#1246](https://github.com/kubernetes-sigs/agent-sandbox/pull/1246)
(p10 router name cache), [#1247](https://github.com/kubernetes-sigs/agent-sandbox/pull/1247)
(p11 REST-client latency histograms), [#1248](https://github.com/kubernetes-sigs/agent-sandbox/pull/1248)
(p12 stress sustained-rate), [#1249](https://github.com/kubernetes-sigs/agent-sandbox/pull/1249)
(p8 lease release), [#1250](https://github.com/kubernetes-sigs/agent-sandbox/pull/1250)
(p5 rawpatch + claim write flags — touches `sandboxclaim_controller.go`
like #1245, yet still merges clean), [#1251](https://github.com/kubernetes-sigs/agent-sandbox/pull/1251)
(p2 warmpool refill shaping), [#1252](https://github.com/kubernetes-sigs/agent-sandbox/pull/1252)
(p6 write-behind coalescing), [#1253](https://github.com/kubernetes-sigs/agent-sandbox/pull/1253)
(p7 informer cache diet). Note: the wave-2/3 filings renumbered some
P-slots relative to the table above — the `upstream-pN-*` branch names on
the PRs are authoritative for what each contains.

**P4** (`upstream-p4-claim-write-suppression` = PR
[#1256](https://github.com/kubernetes-sigs/agent-sandbox/pull/1256)) —
**rework SHIPPED 2026-07-22.** History: filed as the v1
`lastWrittenStatusMap` write-suppression design; igooch /hold'd it and her
self-healing objection was empirically confirmed by the 3-arm adjudication
(v1 repaired 0/5 externally wiped statuses and collapsed post-burst with
23k fixed-requeue retries — `ROUND-1256-ADJUDICATION.md`). Reworked to her
suggested stateless design: optimistic lock on the status patch
(`MergeFromWithOptimisticLock`, benign 409 drop, metrics only after an
authoritative write) plus optimistic-locked adoption patch with in-pass
`retry.RetryOnConflict` over `GetAPIReader()` — which also surfaced and
fixed the assignment-flip latent defect (committed assignments are never
abandoned on stale-view patch failures; regression tests pin it). v24
final gates: all 6 PASS (exactly-once #940, burst/sustained parity, 0
byte-identical watch events, 19-77ms chaos self-heal, clean retry
fingerprint). Shipped: rebased onto upstream main `9dcbe62` (one conflict
composed with #1114's one-shot metric annotation), full validation green,
force-pushed → PR head `2588c32`; PR retitled + body replaced (Fixes
#940); igooch reply posted (pull/1256#issuecomment-5051235861); CodeRabbit
and Copilot threads answered and resolved. /hold remains until she
re-reviews.

**A/B relevance after the merge:** #1245's change is now part of BASE for
every future benchmark leg. The already-captured per-PR A/B tables predate
it — that is fine: each table isolated exactly its own change against the
then-current baseline, so their deltas stand. The one fresh pair, P4's
(`bench-wave2-base2` vs `bench-wave2-p4`), ran against post-#1118 upstream
main `16e4aeb` — i.e. its BASE did **not** include #1245 (which merged
after). Delta noted rather than rerun: P4's headline signal (exactly-1:1
startup-latency histogram observations + bounded conflict requeue) is
orthogonal to #1245's pool-watch predicate change, and the P4 code branch
now carries #1245 beneath it post-rebase; only absolute reconcile counts
in that table would shift under a post-#1245 BASE.

## Wave-1 branch names

`upstream-p1-transport-sharding`, `upstream-p3-pool-status-fanout`,
`upstream-p8-lease-release`, `upstream-p9-sdk-single-watch`,
`upstream-p10-router-name-cache` (fork branches; PRs target
`kubernetes-sigs:main`).

**P11 REST-client latency histograms — CLOSED (2026-07-22), superseded by
upstream.** igooch held [#1247](https://github.com/kubernetes-sigs/agent-sandbox/pull/1247):
controller-runtime#3510 (merged 2026-05-22, cherry-picked to `release-0.24`
via c-r#3525) adds the same histograms — identical names/labels — as opt-in
via `metrics.RegisterRESTClientMetrics(...)`. Carrying our local adapter
would panic on duplicate registration post-bump and races c-r's early
`sync.Once` adapter registration for client-go's metrics hook. Agreed and
closed; successor plan: after the next c-r 0.24.x/0.25 dependency bump, send
the one-line enablement PR
(`metrics.RegisterRESTClientMetrics(metrics.MetricRequestLatency,
metrics.MetricRateLimiterLatency)`). Sub-5ms bucket resolution (upstream
classic buckets start at 5ms; our warm hot path is 1–15ms) raised upstream as
[controller-runtime#3559](https://github.com/kubernetes-sigs/controller-runtime/issues/3559)
(bucket overrides at registration; native histograms already cover
native-capable pipelines). The instrumentation itself lives on in
`perf-investigation-master` for our own benchmarking regardless.
