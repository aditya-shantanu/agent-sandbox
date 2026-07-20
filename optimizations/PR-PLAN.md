# Upstream PR plan (12 PRs) — carved from the perf investigation

Status: wave 1 in flight (2026-07-20). Source of truth for what goes
upstream, in what order, and what stays on the fork. Task numbers refer to
`UPSTREAM-ISSUE-DRAFT.md`.

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
| P3 | 4 | perf(sandboxclaim): stop fanning pool status churn out to every claim (GenerationChangedPredicate + bound/deleting skip) — links #527 | **wave 1** |
| P4 | 2 + 5 | fix(sandboxclaim): stale-pass write suppression + adoption-conflict bounded requeue | **HELD for #1118** — it rewrites adoption finalization; re-express the invariants (never re-enter adoption from a stale cache view; conflict → 50ms bounded requeue) in its structure once it merges, do not port the code |
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

## Wave-1 branch names

`upstream-p1-transport-sharding`, `upstream-p3-pool-status-fanout`,
`upstream-p8-lease-release`, `upstream-p9-sdk-single-watch`,
`upstream-p10-router-name-cache` (fork branches; PRs target
`kubernetes-sigs:main`).
