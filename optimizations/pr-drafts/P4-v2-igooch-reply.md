# PR #1256 — reply to igooch's review (POSTED 2026-07-22)

Posted verbatim as https://github.com/kubernetes-sigs/agent-sandbox/pull/1256#issuecomment-5051235861
after the rework was force-pushed (PR head `2588c32`).

---

Thanks for the thorough review, @igooch — you were right, and the PR is now
reworked to the design you suggested.

**On the core objection: agreed.** The `lastWrittenStatusMap` guard in the
first revision reintroduced exactly the class of per-claim in-memory tracking
that #1118 deliberately removed, just under a different name. Guarding writes
on what this process remembers rather than on what the cluster says is the
pattern #1118 got rid of, and it was a mistake to bring it back. The rework
drops the map, the stale-view sentinel, and the fixed 50ms requeue entirely,
and instead puts an optimistic lock on the claim status patch
(`MergeFromWithOptimisticLock`) so the server arbitrates staleness: a 409 on
the status patch can only mean the pass computed status from a cache view
older than a write this controller already committed, so it is dropped as
benign and the watch event from the committed write drives the next pass.
The startup-latency metrics record only after an authoritative status write,
which closes the #940 double-count durably — including across restarts, since
there is no process-local state to lose. A 409 on the adoption-annotation
update is retried in-pass against a fresh API-server read
(`retry.RetryOnConflict` over `mgr.GetAPIReader()`) instead of failing the
pass and burning the popped candidate.

**On self-healing: you predicted the regression correctly, and our chaos
check confirms it.** With the v1 memory guard, a pass that trusted its
remembered status over the cluster would refuse to repair externally wiped or
mangled status. We added a chaos check that clears a bound claim's status
mid-run: across runs, 20 externally wiped claim statuses with zero controller
restarts — current main repairs in 18-52ms, the rework in 19-77ms, and the v1
memory-guard revision repaired 0 of 5 (never, absent a restart). Your
prediction is empirically confirmed, and it is why the memory-guard design
was abandoned. The reworked design has no memory to trust, so repair is just
the normal level-triggered pass.

**Extending your optimistic-lock direction to the adoption patch surfaced a
further latent defect, now fixed.** Once the adoption-annotation write
carried an optimistic lock, a failure mode became visible that the v1
memory-guard would have masked: when `completeAdoption` failed on a
stale-view patch, the controller could abandon an assignment that had in
fact already been committed, overwrite it, and re-bind the claim to a
different candidate — an assignment-flip loop across candidates. The rework
never abandons a committed adoption assignment on a stale-view patch
failure (a 409 there triggers an in-pass retry against a fresh API-server
read instead), and regression tests pin that behavior.

**A correction to our own PR body.** The v1 description attributed the
0.8/1.2/1.6s retry waves in the burst benchmark to exponential backoff steps.
That was wrong — our own round-2 forensics had already measured the real
mechanism: a clean adoption pass costs about 300ms and each conflict/retry
cycle adds about 400ms of doomed write plus re-pass work, and it is that
per-cycle cost, stacked once or twice across the synchronized herd, that
produces the wave spacing. We have corrected this in our notes and the new PR
body doesn't repeat it.

**One small factual point in the same area, offered with citations because we
had it wrong first.** On controller-runtime v0.24.1 the priority queue is the
default (`ptr.Deref(options.UsePriorityQueue, true)`,
`pkg/controller/controller.go:252-266`), and on that path the default rate
limiter is a per-item exponential with a 5ms base only — the 10qps/100burst
`BucketRateLimiter` is constructed only on the legacy-queue branch. So the
first several failure retries are delayed 5-80ms, and the ~400ms wave spacing
comes from the conflict-cycle cost above, not from the bucket limiter. This
doesn't change your conclusion at all — the backoff machinery is not where
the latency lives either way — it only pins the mechanism.

**What the rework still adds on top of #1118** (all three reproduced on
current post-#1118 main, same harness): (1) the #940 duplicate
startup-latency observations — on main the histograms over-record
reproducibly, 4,503 samples for 2,955 claims (+52%) in one run and 4,232 for
2,680 (1.58x) in an independent run (333 samples for the 320-claim burst
label); with the rework, 3,022 samples for 3,022 claims — exactly-once,
exact on every label; (2) stale pre-adoption views re-entering adoption and
issuing doomed 409 writes, which with a momentarily drained warm queue can
duplicate a cold-create POST for an already-bound claim — now prevented, not
just retried; (3) an unlocked stale status patch overwriting a newer bound
status (a visible Ready flap) — now rejected server-side. Performance is
parity, as intended: burst-300 create→Ready p50/p90/p99 1097/2394/2571ms
(all-300 in 2.63s) vs base 1082/2096/2401ms (2.44s) within cross-cluster
noise, and sustained 45/s p50/p90 108/221ms vs base 108/236ms, flat across a
second back-to-back run — versus the v1 revision's 23k-retry churn under the
same load. 0 failed claims in every phase of every run.

**And you were right about byte-identical patches.** The apiserver
short-circuits a write whose serialization is byte-equal to the stored object
before it reaches etcd (k8s.io/apiserver v0.36.2,
`pkg/storage/etcd3/store.go:553-576`) — no resourceVersion bump, no watch
event. So #1118's "one idempotent re-patch" really does degrade to a round
trip, not a watch storm. Our watch probe confirms it: 0 byte-identical
MODIFIED events (normalized minus resourceVersion/managedFields) across all
arms of every run. The measurable wins are in the non-identical cases (the
doomed adoption writes, the stale overwrites, and the metric double-counts),
and the new PR body scopes its claims accordingly.

We'll keep the /hold in place until you've had a chance to look at the
rework — no rush, and thanks again for steering this to a better design.
