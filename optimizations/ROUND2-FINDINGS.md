# Round 2 Findings — decomposing the ~1.5s p90 warm adoption

Sources: (a) code deep-dive of `perf-investigation-master` (2026-07-19), (b)
forensics on candidate1's run artifacts — controller debug log, client watch
stream (µs clocks), Prometheus scrapes. Line refs are to the
perf-investigation-master tree at the time of analysis.

## Measured decomposition of the p90 (candidate1, 294 warm claims)

Segments from the client watch stream (every write is visible as an event):

| segment | p10 | p50 | p90 | p99 |
|---|---|---|---|---|
| A: claim create → adoption claim-Update visible | 198ms | 534ms | **1349ms** | 1541ms |
| B: claim-Update → sandbox adoption patch | 21ms | 107ms | 356ms | 466ms |
| C: sandbox patch → claim Ready visible | 18ms | 101ms | 184ms | 350ms |

The p90 lives in segment A: slow claims sit with ZERO writes for ~1s, then
complete in retry waves at ~0.8/1.2/1.6s. Fast claims (n=33, <400ms) do one
clean pass: adoption Update ~79ms after creation, Ready ~180ms later.

**Ruled out with data:** workqueue wait (mean 26.7ms, depth 0 at every
scrape), APF (mean wait 2.3ms, all PLs idle), apiserver latency (PATCH
23-37ms, POST ~50ms, never saturated), controller CPU (0.78 of 8 cores),
client-side throttling (zero "Waited for" messages; per-GVK token buckets
never exhausted at qps=1000/burst=2000), EventRecorder blocking (async,
drop-on-full — verified in client-go source), zap log volume (sampled),
priority-queue contention (buffered adds), warm-queue mutex (µs).

## Root cause #1: stale-cache re-adoption storm (dominant)

Measured: **4,357 claim reconciles for 300 claims (14.5×)**; ~547 adoption
claim-Updates competed for 300 candidate keys → **PUT 409 rate 45%** (245
conflicts vs 302 successes); **161 claims' status flapped
Ready→AdoptionConflict→Ready after readiness**; **144 cold-path sandbox
creations, 138 of them duplicates** for claims that were already warm-bound
(real pods created and thrown away; POST 409 ×77 from repeat creates).
Roughly half the burst's write load was waste, and it inflated segment A for
everyone.

Mechanism: a reconcile pass that reads a STALE pre-adoption claim from the
informer cache falls straight through the name-lookup path into
`adoptSandboxFromCandidates` and, on empty queue, into the cold path. The
`triggeredAdoptions` in-memory dedup map is only consulted on the
annotation-present branch (~:1696), not before adoption entry (~:1927).
Echo events from the adoption pass's own 3 writes (plus the sandbox
controller's 2) keep re-enqueueing the claim while the cache converges.

## Root cause #2: cold starts (p99/max tail)

Exactly 6 claims cold-bound in candidate1 (4.4-6.7s e2e), all within one
~10ms window of in-memory candidate-queue exhaustion at burst+1.55s:
- Failed adoption attempts HOLD their popped key for the entire ~300-500ms
  attempt before the deferred re-Add; doomed stale adopters (root cause #1)
  checked out keys too — momentarily draining all 300.
- There is NO List fallback: empty queue → cold start in the same pass
  (`getOrCreateSandbox` ~:1779-1788), and the cold binding is STICKY
  (status.sandbox.name set → later passes take the name-lookup fast path and
  never reconsider the pool).
- The adoption sandbox patch has NO optimistic lock (~:1187) — two claims can
  silently steal the same sandbox; the loser burns its candidate.
- Replenish-delay (20s) worked as designed: 54 deferrals, zero mid-burst
  refill; it was NOT a cause.

## Root cause #3: serial write chain + retry cycle cost

Happy pass = 3 serial write RTTs (claim Update → sandbox patch → status
patch) + duplicated work: `getTemplate` is called twice per pass and
`mergedMeta` recomputed/DeepEqualed right after `completeAdoption` forced an
exact match. Clean pass ≈300ms; each conflict/retry cycle ≈400ms → the
0.8/1.2/1.6s waves. Write collapse options (design discussion): make the
sandbox patch the adoption lock and drop the claim annotation Update (3→2,
recovery via `SandboxIDLabel` cache List), or reorder so status lands before
the annotation write.

## Open hypothesis: in-flight concurrency ceiling ~100-110

Effective concurrent API requests plateaued at ~100-110 despite 800
configured workers, with workers idle, queue near-empty, APF idle, CPU low —
consistent with the single HTTP/2 connection's ~100 concurrent-stream cap to
the apiserver. Untested; separate experiment (transport sharding /
MaxConcurrentStreams / HTTP/1.1) if write-reduction doesn't moot it.

## Fixes in flight (branch `perf-investigation-master-round2-quickwins`)

1. Stale-pass guard: consult the adoption/last-written fingerprint BEFORE
   entering adoption or cold path; stale passes perform zero writes.
2. Echo suppression: in-memory last-written-status fingerprint per claim UID;
   skip status patch + metric re-record when semantically unchanged (also
   fixes the 911-observations-for-300-claims histogram pollution).
3. Cold-start guard: labeled cache List for adoptable members before
   cold-starting; bounded 50ms requeue if any exist (attempt-capped).
4. Instant key return on failed adoption attempts (don't hold keys for the
   full attempt RTT).
5. Optimistic lock on the adoption sandbox patch (double-adoption → 409 +
   re-adopt, not silent steal).
6. Remove duplicate getTemplate/mergedMeta recompute on the adopted path.

Instrumentation (merged, 0497bba): per-adoption phase-timing debug line
(queueLat/pop/update/patch/status/total ms), cold-start reason line,
rest_client latency histograms (controller-runtime exports none by default),
controller CPU/heap profiles captured DURING the claims-warm burst,
INSTRUMENT_CLUSTER=true apiserver verbosity+log capture in the kops script.

## Measurement pitfalls recorded for posterity

- Controller log timestamps are 1s resolution and zap sampling truncates hot
  messages (caps at 100/window) — use the client watch stream or Prometheus
  counters for rates; logs only for path identification.
- The cumulative `claim_controller_startup_latency_ms` histogram re-records
  on stale passes — unusable for absolute quantiles until fix #2 lands.
- Warm-adopted claims' Ready condition carries the SANDBOX's original
  lastTransitionTime (pre-dates the claim) — server-side condition timestamps
  cannot measure adoption latency.

## Leg-B attribution forensics (round-3 input, 2026-07-19)

Verdict on the remaining 1490ms p50: **(i) ~75-80% fixable in agent-sandbox**
— watch-ingestion stall behind our own writes on the single shared HTTP/2
connection (~1.0-1.1s p50, ~60% of e2e; controller informer events arrive
seconds after an independent client watch got them in 20-305ms), plus
client-side in-flight write queueing (134ms of each 194ms write RTT = 69%;
APF 12%, server exec 19%); **(ii) ~15-20% control plane under our own load**
(APF wait mean 23ms/write p99 434ms in shared workload-low — addressed by the
merged APF insulation); **(iii) ~5-10% irreducible** (smoke run floor:
79ms p50). CPU: 45% of one core; `persistStampedAnnotations` full-object
merge patch = 15.8% of controller CPU (fix candidate); ~33% of CPU is JSON.
Waste: 3677 post-bind requeues, 1629 teardown 404-PATCHes, 89 409-POSTs.
Instrumentation bugs found: queueLatMs used CreationTimestamp (1s truncation
→ ~900ms phantom; true creation→winning-pass p50 1081ms), and 40/300 fastest
adoptions emit no timing line. Round-3 merge priority: watch/write transport
separation > write collapse (merged) > APF (merged).
