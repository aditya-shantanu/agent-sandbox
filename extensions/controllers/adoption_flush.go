// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"fmt"
	"sync"
	"time"

	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	uberzap "go.uber.org/zap"
	v1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
)

// One-write adoption (--one-write-adoption): the async sandbox-patch flusher.
//
// TRANSACTION SHAPE. The classic (2-write) adoption transaction is
//
//	write 1: sandbox patch  (ownership transfer, optimistic lock = the adoption lock)
//	write 2: claim status   (binding + podIPs + forwarded Ready)
//
// so a claim becomes Ready only after TWO serial write RTTs. One-write
// adoption inverts it: the candidate is reserved from the in-memory warm
// queue (single-process exclusivity: each key is handed to exactly one
// claim), the claim STATUS is written first — the single critical write, and
// the commit point clients observe — and the sandbox-side patch is applied
// asynchronously by this flusher, bounded well under a second in the absence
// of API-server pathology.
//
// WHY A DEDICATED SMALL WORKER POOL, NOT PER-CLAIM GOROUTINES: a 300-claim
// burst would otherwise fan out 300 goroutines racing the claim workers for
// the same connection pool; a fixed handful of workers drains a bounded
// channel instead, keeping the async write concurrency a small constant and
// the window length observable (queueWaitMs in the timing line).
//
// CORRECTNESS INVENTORY (each item has a pinned test):
//
//   - Cross-process safety net: the async patch keeps the optimistic lock.
//     If ANOTHER writer took the sandbox during the window, the patch 409s;
//     the flusher re-verifies against a fresh read and either retries (benign
//     resourceVersion bump — e.g. a sandbox status write — with the sandbox
//     still pool-owned) or, on a genuine steal, clears the claim's stale
//     binding (status re-write) so the claim re-adopts. Bounded retries, loud
//     logging. NOTE: multi-process operation over the same namespaces
//     (no leader election, overlapping --watch-namespaces shards) widens this
//     window from "leader-failover only" to "every adoption"; leader election
//     makes steals rare, namespace sharding removes them by construction.
//
//   - Crash window (status written, sandbox unpatched, process died): the
//     status-name fast path in getOrCreateSandbox tolerates a still-pool-owned
//     sandbox and completes the adoption idempotently on the next pass
//     (recoverStatusFirstBinding).
//
//   - Pool interference during the window: the sandbox still carries the
//     warm-pool label and pool controller ref, so reconcilePool counts it as
//     a member — the pool does NOT double-count or over-refill (refill starts
//     one window later, when the async patch removes the label; there is no
//     transient over-create because the member count is a consistent snapshot
//     per reconcile). The residual exposure is the pool DELETING the sandbox
//     during the window (excess-deletion when over target, stuck-unready
//     reaping, stale-template recreate): the async patch then observes
//     NotFound and the lost-binding recovery rebinds the claim.
//
//   - Autoscaler exposure: the warm pool stamps its pods with
//     cluster-autoscaler.kubernetes.io/safe-to-evict=true; the strip is part
//     of the async sandbox patch and of the sandbox controller's subsequent
//     pod metadata sync (see the "must remain synchronous" rationale in
//     controllers/sandbox_controller.go). Under one-write adoption the live
//     pod therefore stays evict-safe for the async window plus one sandbox
//     reconcile — a sub-second exposure in which a scale-down could evict a
//     just-claimed pod. Documented trade-off of the flag.
type adoptionFlusher struct {
	reconciler *SandboxClaimReconciler
	requests   chan *adoptionFlushRequest
}

const (
	// adoptionFlushWorkers is the fixed number of goroutines draining the
	// flush queue. The async patch is a single merge patch per adoption; a
	// handful of workers sustains hundreds of adoptions/s while keeping the
	// added write concurrency a small constant.
	adoptionFlushWorkers = 4
	// adoptionFlushQueueCapacity bounds the backlog. At 300-burst scale the
	// queue never holds more than the burst size; if it ever fills, enqueue
	// degrades to processing synchronously (never drops: the sandbox patch is
	// the durable, crash-recoverable record of the adoption).
	adoptionFlushQueueCapacity = 4096
	// adoptionFlushMaxAttempts bounds patch attempts per request, including
	// rebuild-and-retry rounds after benign optimistic-lock conflicts.
	adoptionFlushMaxAttempts = 5
	// adoptionFlushRetryDelay paces retries; combined with the attempt cap it
	// keeps the worst-case async window (attempts x (RTT + delay)) inside the
	// sub-second bound the flag documents.
	adoptionFlushRetryDelay = 100 * time.Millisecond
	// adoptionFlushClearAttempts bounds the lost-binding status clear. It is
	// more patient than the patch loop because its only enemy is our own
	// informer lag (the fresh read must first observe the status this
	// controller just wrote).
	adoptionFlushClearAttempts = 10
)

// adoptionFlushRequest is one deferred sandbox-side adoption patch. mutated
// and original are the prepared in-memory pair (post- and pre-mutation) whose
// diff is the same optimistic-lock merge patch completeAdoption would have
// sent synchronously; claim is a deep copy taken at reservation time, used to
// rebuild the mutation set against a fresh sandbox read after a benign
// conflict and to identify the claim during lost-binding recovery.
type adoptionFlushRequest struct {
	claim      *extensionsv1beta1.SandboxClaim
	mutated    *v1beta1.Sandbox
	original   *v1beta1.Sandbox
	queueKey   queue.SandboxKey
	poolQueue  string
	enqueuedAt time.Time
}

func newAdoptionFlusher(r *SandboxClaimReconciler) *adoptionFlusher {
	return &adoptionFlusher{
		reconciler: r,
		requests:   make(chan *adoptionFlushRequest, adoptionFlushQueueCapacity),
	}
}

// enqueue hands a prepared sandbox patch to the workers. It never blocks and
// never drops: with the channel full (extreme backlog) the patch is applied
// synchronously, degrading that adoption to the 2-write latency profile
// instead of leaving an unbounded async window.
func (f *adoptionFlusher) enqueue(req *adoptionFlushRequest) {
	select {
	case f.requests <- req:
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		log.FromContext(ctx).Info("Adoption flush queue full; applying sandbox patch synchronously",
			"claim", req.claim.Name, "namespace", req.claim.Namespace, "sandbox", req.mutated.Name)
		f.reconciler.processAdoptionFlush(ctx, req)
	}
}

// processNext synchronously processes at most one queued request. Test hook:
// lets tests drain the queue deterministically without running Start.
func (f *adoptionFlusher) processNext(ctx context.Context) bool {
	select {
	case req := <-f.requests:
		f.reconciler.processAdoptionFlush(ctx, req)
		return true
	default:
		return false
	}
}

// pending reports the current backlog (test/observability hook).
func (f *adoptionFlusher) pending() int { return len(f.requests) }

// Start implements manager.Runnable: it drains the flush queue until the
// manager stops, then drains any leftover requests with a short grace budget
// so a graceful shutdown does not widen the crash window unnecessarily.
func (f *adoptionFlusher) Start(ctx context.Context) error {
	var wg sync.WaitGroup
	for range adoptionFlushWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					for {
						select {
						case req := <-f.requests:
							drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							f.reconciler.processAdoptionFlush(drainCtx, req)
							cancel()
						default:
							return
						}
					}
				case req := <-f.requests:
					f.reconciler.processAdoptionFlush(ctx, req)
				}
			}
		}()
	}
	wg.Wait()
	return nil
}

// NeedLeaderElection gates the workers on leadership: the flush queue is fed
// exclusively by this process's reconciles, and a non-leader must not write.
func (f *adoptionFlusher) NeedLeaderElection() bool { return true }

// enqueueAdoptionFlush routes a deferred adoption patch to the flusher, or —
// when no flusher is wired (tests constructing the reconciler directly) —
// applies it synchronously so correctness never depends on the worker.
func (r *SandboxClaimReconciler) enqueueAdoptionFlush(req *adoptionFlushRequest) {
	req.enqueuedAt = time.Now()
	if r.adoptionFlusher == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r.processAdoptionFlush(ctx, req)
		return
	}
	r.adoptionFlusher.enqueue(req)
}

// sleepCtx sleeps for d unless ctx ends first; reports whether the full sleep
// completed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// processAdoptionFlush applies the deferred sandbox-side adoption patch. See
// the file-header comment for the full transaction and race analysis.
func (r *SandboxClaimReconciler) processAdoptionFlush(ctx context.Context, req *adoptionFlushRequest) {
	logger := log.FromContext(ctx).WithValues(
		"claim", req.claim.Name, "namespace", req.claim.Namespace, "sandbox", req.mutated.Name)
	start := time.Now()

	mutated, original := req.mutated, req.original
	for attempt := 1; attempt <= adoptionFlushMaxAttempts; attempt++ {
		err := r.Patch(ctx, mutated, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
		if err == nil {
			// The window is closed: ownership, labels and the safe-to-evict
			// strip are on the server; the sandbox controller's next reconcile
			// propagates them to the live pod.
			if logger.V(1).Enabled() {
				adoptionTimingLog.Info("adoption async patch",
					uberzap.String("claim", req.claim.Namespace+"/"+req.claim.Name),
					uberzap.String("sandbox", mutated.Name),
					uberzap.Float64("asyncPatchMs", durationMs(time.Since(start))),
					uberzap.Float64("queueWaitMs", durationMs(start.Sub(req.enqueuedAt))),
					uberzap.Int("attempts", attempt),
				)
			}
			return
		}

		if !k8errors.IsConflict(err) && !k8errors.IsNotFound(err) {
			// Transient API error: plain bounded retry with the same payload.
			logger.Error(err, "Async adoption sandbox patch failed; retrying", "attempt", attempt)
			if attempt < adoptionFlushMaxAttempts && !sleepCtx(ctx, adoptionFlushRetryDelay) {
				return
			}
			continue
		}

		// Optimistic-lock conflict or object gone: another writer touched the
		// sandbox since it was reserved. Re-verify before deciding.
		fresh := &v1beta1.Sandbox{}
		getErr := r.Get(ctx, client.ObjectKey{Namespace: req.mutated.Namespace, Name: req.mutated.Name}, fresh)
		switch {
		case k8errors.IsNotFound(getErr):
			// Deleted during the window (pool excess-deletion / stale-template
			// reap / external delete): the reservation is unrecoverable.
			r.recoverLostAdoption(ctx, req, "sandbox deleted before the async adoption patch landed")
			return
		case getErr != nil:
			logger.Error(getErr, "Async adoption patch conflicted and re-read failed; retrying", "attempt", attempt)
			if attempt < adoptionFlushMaxAttempts && !sleepCtx(ctx, adoptionFlushRetryDelay) {
				return
			}
			continue
		}

		if metav1.IsControlledBy(fresh, req.claim) {
			// Idempotent success: the crash-window recovery in
			// getOrCreateSandbox (or a competing flush) applied the patch first.
			logger.V(1).Info("Async adoption patch already applied by another path; nothing to do")
			return
		}
		if ref := metav1.GetControllerOf(fresh); ref == nil || ref.Kind != "SandboxWarmPool" {
			// Genuine steal: a racing writer (another process adopting for a
			// different claim, or an external ownership change) won the
			// optimistic lock. The claim status advertises a sandbox it does
			// not own — clear the binding so the claim re-adopts.
			owner := "none"
			if ref != nil {
				owner = fmt.Sprintf("%s %s/%s", ref.Kind, req.mutated.Namespace, ref.Name)
			}
			r.recoverLostAdoption(ctx, req, "sandbox was taken by another owner during the async window: "+owner)
			return
		}
		if verr := verifySandboxCandidate(fresh, req.claim); verr != nil {
			// Still pool-owned but no longer adoptable (e.g. being deleted).
			r.recoverLostAdoption(ctx, req, "sandbox became non-adoptable during the async window: "+verr.Error())
			return
		}

		// Benign conflict: the sandbox is still pool-owned and adoptable — the
		// resourceVersion moved under us (a sandbox status write, or the
		// reserved copy came from a lagging cache). Rebuild the mutation set on
		// the fresh read and retry.
		logger.V(1).Info("Async adoption patch hit a benign conflict; rebuilding against fresh sandbox", "attempt", attempt)
		original = fresh.DeepCopy()
		mutated = fresh
		if merr := r.applyAdoptionMutations(ctx, req.claim, mutated); merr != nil {
			logger.Error(merr, "Failed to rebuild adoption mutations after conflict; abandoning reservation")
			r.recoverLostAdoption(ctx, req, "failed to rebuild adoption mutations after conflict: "+merr.Error())
			return
		}
		if attempt < adoptionFlushMaxAttempts && !sleepCtx(ctx, adoptionFlushRetryDelay) {
			return
		}
	}

	// Attempts exhausted with the sandbox (last observed) still pool-owned:
	// give the candidate back to the queue — it was never patched, so it is
	// still a valid pool member — and clear the claim binding for re-adoption.
	logger.Error(nil, "Async adoption sandbox patch attempts exhausted; returning candidate to the warm queue and clearing the claim binding",
		"attempts", adoptionFlushMaxAttempts)
	r.WarmSandboxQueue.Add(req.poolQueue, req.queueKey)
	r.recoverLostAdoption(ctx, req, "async adoption patch attempts exhausted")
}

// recoverLostAdoption is the loud, bounded recovery for a one-write adoption
// whose sandbox-side patch can never land: the claim status was already
// committed (possibly Ready) pointing at a sandbox this claim will not own.
// It clears the stale binding with an optimistic-locked status re-write so
// the claim's next reconcile re-enters adoption with the next candidate.
func (r *SandboxClaimReconciler) recoverLostAdoption(ctx context.Context, req *adoptionFlushRequest, reason string) {
	sandboxName := req.mutated.Name
	key := types.NamespacedName{Name: req.claim.Name, Namespace: req.claim.Namespace}
	logger := log.FromContext(ctx).WithValues("claim", key.String(), "sandbox", sandboxName, "reason", reason)
	logger.Error(nil, "ONE-WRITE ADOPTION LOST: claim status was committed but the sandbox-side patch lost the race; clearing the stale binding so the claim re-adopts")

	// Drop the in-process pending-adoption record FIRST so the reconcile pass
	// triggered by the status clear does not keep waiting on a patch that will
	// never land (recoverStatusFirstBinding / the annotation path check it).
	if prev, ok := r.triggeredAdoptions.Load(key); ok && prev.uid == req.claim.UID && prev.sandbox == sandboxName {
		r.triggeredAdoptions.Delete(key)
	}

	for attempt := 1; attempt <= adoptionFlushClearAttempts; attempt++ {
		claim := &extensionsv1beta1.SandboxClaim{}
		if err := r.Get(ctx, key, claim); err != nil {
			if k8errors.IsNotFound(err) {
				return // claim gone; nothing to clear
			}
			logger.Error(err, "Failed to read claim for lost-adoption recovery; retrying", "attempt", attempt)
			if !sleepCtx(ctx, adoptionFlushRetryDelay) {
				return
			}
			continue
		}
		if claim.UID != req.claim.UID {
			return // deleted and recreated under the same name
		}
		if claim.Status.SandboxStatus.Name != sandboxName {
			// Either the binding already moved on (a reconcile pass observed
			// the steal and re-adopted — done), or the cached view predates the
			// status write this controller itself made. Only the latter needs
			// patience: our own last-written record tells the two apart.
			if entry, ok := r.lastWrittenStatuses.Load(key); ok && entry.uid == claim.UID &&
				entry.status.SandboxStatus.Name == sandboxName {
				logger.V(1).Info("Lost-adoption recovery waiting for own status write to appear in the cache", "attempt", attempt)
				if !sleepCtx(ctx, adoptionFlushRetryDelay) {
					return
				}
				continue
			}
			logger.Info("Lost-adoption recovery found the claim already rebound or cleared; nothing to do",
				"boundSandbox", claim.Status.SandboxStatus.Name)
			return
		}

		orig := claim.DeepCopy()
		claim.Status.SandboxStatus.Name = ""
		claim.Status.SandboxStatus.PodIPs = nil
		meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               string(v1beta1.SandboxConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             "AdoptionLost",
			Message:            fmt.Sprintf("warm sandbox %q was lost after the status-first commit (%s); re-adopting", sandboxName, reason),
			ObservedGeneration: claim.Generation,
		})
		// Optimistic lock: if a concurrent reconcile rebinds the claim between
		// our read and this write, the clear MUST lose (a plain merge patch
		// would silently wipe the new, valid binding).
		if err := r.Status().Patch(ctx, claim, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
			logger.Error(err, "Failed to clear lost adoption binding; retrying", "attempt", attempt)
			if !sleepCtx(ctx, adoptionFlushRetryDelay) {
				return
			}
			continue
		}
		r.lastWrittenStatuses.Store(key, lastWrittenStatusEntry{uid: claim.UID, status: claim.Status.DeepCopy()})
		logger.Info("Cleared lost adoption binding; claim will re-adopt on its next reconcile")
		return
	}
	logger.Error(nil, "Failed to clear lost adoption binding after bounded retries; the claim may keep advertising a sandbox it does not own until its next reconcile pass observes the foreign owner and re-adopts")
}
