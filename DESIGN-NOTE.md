# DESIGN NOTE — RequeueAfter-based write deferral (benchmark variant)

Branch `p6-requeueafter-variant`. This is a **benchmark A/B variant** of the
write-behind coalescing branch (`upstream-p6-write-behind-coalescing`, the
PR #1252 candidate), implementing the reviewer-suggested alternative: instead
of enqueueing recoverable metadata mutations into a background flusher, the
reconciler *skips* the write and returns
`ctrl.Result{RequeueAfter: <remaining window>}`, letting the workqueue itself
provide the deferral and coalescing ("we can also return with RequeueAfter
from the controller and get some of this for free"). It is not a PR.

Both variants gate on the **same flag** (`--sandbox-write-behind-window`,
`0` = fully synchronous stock writes) and defer the **same single write
class** — the sandbox controller's pod label/annotation reconciliation patch
(recoverable: recomputed verbatim from informer state by any later pass),
bounded by `min(window, 1s)` so the safe-to-evict strip cannot lag the
cluster autoscaler. Status writes, ownership transfers, creates and deletes
are never deferred in either variant. **Readiness semantics are identical by
construction:** the deferring pass mutates only its in-memory pod copy, so
status/condition computation in the same pass sees the desired state, and
the status write is never gated on the deferred patch (pinned by
`TestRequeueDeferralCoalescesAdoptionPodPatch`, which asserts the deferring
pass finalizes the same Ready condition as a synchronous run).

## Mechanism (this variant)

- A write site that detects recoverable drift consults a per-request
  deferral clock (`controllers/writebehind_requeue.go`). First detection
  records "first seen" and skips the patch; `Reconcile` returns
  `RequeueAfter = remaining window`. Redeliveries inside the window skip
  again **without re-arming the window** (no starvation under continuous
  events) and shrink the returned `RequeueAfter` toward the original
  deadline. The pass that runs at/after the deadline recomputes the drift
  from informer state and issues the normal, targeted synchronous merge
  patch — the identical bytes the flag-off path would send.
- The only retained in-memory state is a `map[NamespacedName]time.Time`
  ("when was this request's pending deferral first observed"). It holds **no
  mutation payload** — that is always recomputed — so losing it (crash,
  failover) only restarts one sub-second window; it can never lose a write.
  A timestamp is unavoidable here: "flush once the window elapses" needs a
  first-seen clock, the object carries none, and stamping one on the object
  would itself be a write. Entries are dropped whenever a pass has nothing
  pending and on object deletion.

## Mechanical comparison

What `RequeueAfter` gives for free (vs `internal/writebehind.Flusher`):

- **No background machinery.** No flusher goroutine, no manager Runnable, no
  shutdown drain, no per-object in-flight/flushing serialization — the
  workqueue's `AddAfter` timer and per-key dedup replace all of it
  (`RequeueAfter` → `Forget` + `AddAfter`, so deferral never interacts with
  the failure-backoff rate limiter).
- **Natural per-object coalescing.** N watch redeliveries for the object
  within the window collapse in the queue and in the clock; the flush pass
  recomputes the FULL desired state, so N detections still cost exactly one
  patch — same write-count outcome as the flusher for this write class.
- **No pending-mutation store.** The flusher holds merged mutation payloads
  in memory until flush (with drain/crash-loss semantics to reason about);
  here the payload always lives in the cluster state + desired-state
  recomputation.
- **Trivially correct interleavings.** The flusher needed explicit
  serialization so a retry-delayed older payload cannot overwrite a newer
  one; here every write is computed and sent inside a single reconcile pass
  from the freshest cache view.

What it costs:

- **A full reconcile pass per deferred write.** The flush is a complete
  `Reconcile` (cache reads, ownership checks, PVC/Pod/Service reconciliation,
  condition computation, status comparison) instead of the flusher's one
  targeted `PATCH` built from a stored delta. At high rates this converts
  API-write savings into reconcile-CPU/queue-throughput spend — the exact
  trade the A/B should measure.
- **Coarser coalescing granularity.** The flusher coalesces per *object*
  across arbitrarily many enqueue call sites into one merged patch, and can
  batch mutations that arrive from different reconcile passes right up to
  the flush instant. The requeue variant coalesces per *reconcile request*:
  anything not visible in the informer cache at the moment the flush pass
  runs misses the patch and starts a new window (one extra pass + patch),
  and multiple deferrable objects under one request share a single clock
  (bounded by the tightest write-class cap).
- **Requeue jitter under queue depth.** `AddAfter` guarantees *not before*,
  not *at*: under worker saturation or deep queues the flush pass — and with
  it the bounded safe-to-evict strip — lands later than the nominal window.
  The flusher's dedicated goroutine ticks independently of queue depth. The
  1s pod-patch bound is therefore a softer guarantee in this variant.
- **Window restarts on failover.** The flusher loses pending payloads on
  crash too (both variants are level-based-recoverable), but this variant
  also restarts the window on any leader change, adding one window of extra
  deferral latency in that (rare) case.

## A/B expectation

Same benchmark toggles as the PR branch: `--sandbox-write-behind-window=0`
(both variants identical, stock synchronous) vs `=250ms` (each variant's
mechanism active). Compare: pod PATCH counts and RTTs, sandbox-controller
reconcile counts (this variant should show ~+1 pass per adoption), workqueue
depth/latency, claim create→Ready percentiles, and time-from-adoption to
safe-to-evict strip (the bound-honoring check, especially under load).
