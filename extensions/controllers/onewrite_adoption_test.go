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

// Round-6 (L1) one-write adoption test net. Every test here runs with
// --one-write-adoption semantics (OneWriteAdoption=true) except the explicit
// flag-off ordering pin; the rest of the package continues to exercise the
// default 2-write path unmodified.
//
//  1. Hot path: exactly ONE critical write (the claim status patch) before
//     the claim is Ready; the sandbox-side patch is deferred and converges
//     asynchronously (TestOneWriteAdoptionSingleCriticalWriteBeforeReady,
//     ...SyncFallback ordering variant).
//  2. Echo passes during the async window wait instead of burning a second
//     candidate (TestOneWriteAdoptionAsyncWindowWaits...).
//  3. Cross-process safety net: a stolen candidate 409s the async patch; the
//     flusher clears the stale binding and the claim re-adopts
//     (TestOneWriteAdoption409StealRecovery...).
//  4. Benign optimistic-lock conflicts (resourceVersion bumped while still
//     pool-owned) rebuild and retry without touching the claim
//     (TestOneWriteAdoptionBenignConflict...).
//  5. Crash window at N=50 (status committed, sandbox unpatched, process
//     gone): concurrent recovery re-applies the sandbox patch idempotently,
//     zero duplicate sandboxes, zero claim status rewrites, spares unburned
//     (TestOneWriteCrashWindowRecoveryN50, mirroring
//     restart_robustness_test.go).
//  6. Pool audit: during the async window the sandbox still counts as a pool
//     member — no refill over-create and no double-count; the deficit
//     appears only once the async patch lands
//     (TestWarmPoolCountingAcrossOneWriteAsyncWindow).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
)

// owWarmSandbox is rrWarmSandbox plus the fields the one-write path forwards
// into the claim status in the same pass: podIPs and the warm pool's
// safe-to-evict pod-template annotation (whose strip rides the async patch).
func owWarmSandbox(name string) *sandboxv1beta1.Sandbox {
	sb := rrWarmSandbox(name)
	sb.Status.PodIPs = []string{"10.1.0.9"}
	sb.Spec.PodTemplate.ObjectMeta.Annotations = map[string]string{
		autoscalerSafeToEvictAnnotation: "true",
	}
	return sb
}

// opRecorder captures the global order of writes across objects so tests can
// pin "status first, sandbox later".
type opRecorder struct {
	mu  sync.Mutex
	ops []string
}

func (o *opRecorder) add(op string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ops = append(o.ops, op)
}

func (o *opRecorder) sequence() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ops...)
}

func owInterceptors(rec *opRecorder, sandboxCreates *atomic.Int64) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*sandboxv1beta1.Sandbox); ok {
				if sandboxCreates != nil {
					sandboxCreates.Add(1)
				}
				rec.add("sandbox-create")
			}
			return c.Create(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			err := c.Patch(ctx, obj, patch, opts...)
			switch obj.(type) {
			case *sandboxv1beta1.Sandbox:
				if err == nil {
					rec.add("sandbox-patch")
				} else {
					rec.add("sandbox-patch-failed")
				}
			case *extensionsv1beta1.SandboxClaim:
				if err == nil {
					rec.add("claim-meta-patch")
				}
			}
			return err
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			err := c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok && subResourceName == "status" && err == nil {
				rec.add("claim-status-patch")
			}
			return err
		},
	}
}

func owFakeClient(t *testing.T, rec *opRecorder, sandboxCreates *atomic.Int64, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&extensionsv1beta1.SandboxClaim{}).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxClaimUIDLabelIndex, sandboxClaimUIDLabelIndexer).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer).
		WithInterceptorFuncs(owInterceptors(rec, sandboxCreates)).
		Build()
}

func owReconciler(t *testing.T, c client.Client, withFlusher bool, warmKeys ...string) *SandboxClaimReconciler {
	t.Helper()
	q := queue.NewSimpleSandboxQueue()
	for _, name := range warmKeys {
		q.Add(queue.GetNamespacedWarmPoolName(rrNamespace, rrPoolName), queue.SandboxKey{Namespace: rrNamespace, Name: name})
	}
	r := rrReconciler(c, t, q)
	r.OneWriteAdoption = true
	if withFlusher {
		r.adoptionFlusher = newAdoptionFlusher(r)
	}
	return r
}

func owReconcile(t *testing.T, r *SandboxClaimReconciler, claimName string) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: rrNamespace}})
	if err != nil {
		t.Fatalf("reconcile %s: expected nil error, got: %v", claimName, err)
	}
	return res
}

func countOps(seq []string, op string) int {
	n := 0
	for _, s := range seq {
		if s == op {
			n++
		}
	}
	return n
}

// assertSandboxAdoptionConverged checks the full sandbox-side mutation set of
// the (a)synchronously applied adoption patch.
func assertSandboxAdoptionConverged(t *testing.T, c client.Client, sandboxName string, claim *extensionsv1beta1.SandboxClaim) {
	t.Helper()
	sb := &sandboxv1beta1.Sandbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: sandboxName, Namespace: rrNamespace}, sb); err != nil {
		t.Fatalf("failed to get sandbox %s: %v", sandboxName, err)
	}
	if ref := metav1.GetControllerOf(sb); ref == nil || ref.Kind != "SandboxClaim" || ref.UID != claim.UID {
		t.Errorf("sandbox %s: expected controller ref transferred to claim %s, got %+v", sandboxName, claim.Name, ref)
	}
	if _, ok := sb.Labels[warmPoolSandboxLabel]; ok {
		t.Errorf("sandbox %s: warm pool label not removed by the adoption patch", sandboxName)
	}
	if got := sb.Labels[extensionsv1beta1.SandboxIDLabel]; got != string(claim.UID) {
		t.Errorf("sandbox %s: expected claim-uid label %q, got %q", sandboxName, claim.UID, got)
	}
	if got := sb.Labels[sandboxv1beta1.SandboxLaunchTypeLabel]; got != sandboxv1beta1.SandboxLaunchTypeWarm {
		t.Errorf("sandbox %s: expected warm launch type label, got %q", sandboxName, got)
	}
	if val, ok := sb.Spec.PodTemplate.ObjectMeta.Annotations[autoscalerSafeToEvictAnnotation]; ok && val == "true" {
		t.Errorf("sandbox %s: safe-to-evict annotation not stripped from the pod template", sandboxName)
	}
}

// TestOneWriteAdoptionSingleCriticalWriteBeforeReady pins the headline
// invariant: with the flusher wired but not drained, ONE claim status patch
// makes the claim Ready and bound (name + podIPs + forwarded Ready), with
// ZERO sandbox writes issued; draining the flusher then applies exactly one
// sandbox patch carrying the entire mutation set, and a follow-up reconcile
// is write-free.
func TestOneWriteAdoptionSingleCriticalWriteBeforeReady(t *testing.T) {
	claim := rrClaim("ow-claim")
	warm := owWarmSandbox("ow-warm-1")
	rec := &opRecorder{}
	var creates atomic.Int64
	c := owFakeClient(t, rec, &creates, rrTemplate(), rrWarmPool(), claim, warm)
	r := owReconciler(t, c, true, warm.Name)

	owReconcile(t, r, claim.Name)

	// Claim is Ready and fully bound after the single critical write.
	got := &extensionsv1beta1.SandboxClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}, got); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	if got.Status.SandboxStatus.Name != warm.Name {
		t.Errorf("expected claim bound to %s, got %q", warm.Name, got.Status.SandboxStatus.Name)
	}
	if len(got.Status.SandboxStatus.PodIPs) != 1 || got.Status.SandboxStatus.PodIPs[0] != "10.1.0.9" {
		t.Errorf("expected podIPs forwarded in the same status write, got %v", got.Status.SandboxStatus.PodIPs)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True after one write, got %+v", ready)
	}

	// Write ledger before the flusher runs: no sandbox writes at all.
	seq := rec.sequence()
	if n := countOps(seq, "sandbox-patch") + countOps(seq, "sandbox-patch-failed"); n != 0 {
		t.Fatalf("expected ZERO sandbox writes before the async flush, got %d (sequence: %v)", n, seq)
	}
	if n := countOps(seq, "claim-status-patch"); n != 1 {
		t.Errorf("expected exactly ONE claim status patch on the hot path, got %d (sequence: %v)", n, seq)
	}
	if r.adoptionFlusher.pending() != 1 {
		t.Fatalf("expected exactly one queued async patch, got %d", r.adoptionFlusher.pending())
	}

	// The sandbox is still fully pool-owned on the server (the window).
	preFlush := &sandboxv1beta1.Sandbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: warm.Name, Namespace: rrNamespace}, preFlush); err != nil {
		t.Fatalf("failed to get sandbox: %v", err)
	}
	if ref := metav1.GetControllerOf(preFlush); ref == nil || ref.Kind != "SandboxWarmPool" {
		t.Fatalf("expected sandbox to remain pool-owned during the async window, got %+v", ref)
	}

	// Drain: the async patch lands and converges the sandbox side.
	if !r.adoptionFlusher.processNext(context.Background()) {
		t.Fatal("expected a queued flush request to process")
	}
	assertSandboxAdoptionConverged(t, c, warm.Name, claim)
	seq = rec.sequence()
	if n := countOps(seq, "sandbox-patch"); n != 1 {
		t.Errorf("expected exactly ONE sandbox patch after the flush, got %d (sequence: %v)", n, seq)
	}

	// Converged follow-up pass: no writes of any kind.
	before := len(rec.sequence())
	owReconcile(t, r, claim.Name)
	if after := rec.sequence(); len(after) != before {
		t.Errorf("expected converged pass to be write-free, got new ops: %v", after[before:])
	}
	if n := creates.Load(); n != 0 {
		t.Errorf("expected zero sandbox creates, got %d", n)
	}
}

// TestOneWriteAdoptionSyncFallbackKeepsStatusFirstOrdering: without a wired
// flusher the deferred patch is applied synchronously at pass exit — the
// write ORDER must still be status-first (claim status patch strictly before
// the sandbox patch), which is the commit-order inversion the flag is about.
func TestOneWriteAdoptionSyncFallbackKeepsStatusFirstOrdering(t *testing.T) {
	claim := rrClaim("ow-sync-claim")
	warm := owWarmSandbox("ow-sync-warm")
	rec := &opRecorder{}
	c := owFakeClient(t, rec, nil, rrTemplate(), rrWarmPool(), claim, warm)
	r := owReconciler(t, c, false, warm.Name)

	owReconcile(t, r, claim.Name)

	seq := rec.sequence()
	statusIdx, sandboxIdx := -1, -1
	for i, op := range seq {
		if op == "claim-status-patch" && statusIdx == -1 {
			statusIdx = i
		}
		if op == "sandbox-patch" && sandboxIdx == -1 {
			sandboxIdx = i
		}
	}
	if statusIdx == -1 || sandboxIdx == -1 {
		t.Fatalf("expected both a claim status patch and a sandbox patch, got %v", seq)
	}
	if statusIdx > sandboxIdx {
		t.Errorf("one-write ordering violated: sandbox patch at %d preceded claim status patch at %d (%v)", sandboxIdx, statusIdx, seq)
	}
	assertSandboxAdoptionConverged(t, c, warm.Name, claim)
}

// TestTwoWriteDefaultKeepsSandboxFirstOrdering pins the flag-OFF contract:
// the default path still commits the sandbox patch BEFORE the claim status
// patch (the unchanged 2-write transaction), and no async machinery engages.
func TestTwoWriteDefaultKeepsSandboxFirstOrdering(t *testing.T) {
	claim := rrClaim("tw-claim")
	warm := owWarmSandbox("tw-warm")
	rec := &opRecorder{}
	c := owFakeClient(t, rec, nil, rrTemplate(), rrWarmPool(), claim, warm)
	q := queue.NewSimpleSandboxQueue()
	q.Add(queue.GetNamespacedWarmPoolName(rrNamespace, rrPoolName), queue.SandboxKey{Namespace: rrNamespace, Name: warm.Name})
	r := rrReconciler(c, t, q) // OneWriteAdoption stays false

	owReconcile(t, r, claim.Name)

	seq := rec.sequence()
	statusIdx, sandboxIdx := -1, -1
	for i, op := range seq {
		if op == "claim-status-patch" && statusIdx == -1 {
			statusIdx = i
		}
		if op == "sandbox-patch" && sandboxIdx == -1 {
			sandboxIdx = i
		}
	}
	if statusIdx == -1 || sandboxIdx == -1 {
		t.Fatalf("expected both writes on the 2-write path, got %v", seq)
	}
	if sandboxIdx > statusIdx {
		t.Errorf("2-write ordering changed with the flag OFF: claim status patch at %d preceded sandbox patch at %d (%v)", statusIdx, sandboxIdx, seq)
	}
	if r.adoptionFlusher != nil {
		t.Error("expected no adoption flusher with the flag off")
	}
}

// TestOneWriteAdoptionAsyncWindowWaitsInsteadOfBurningASecondCandidate: a
// reconcile pass that lands during the async window (claim bound in status,
// sandbox still pool-owned, patch queued) must wait via the bounded requeue —
// not pop the next candidate, not re-send the patch, not touch the status.
func TestOneWriteAdoptionAsyncWindowWaitsInsteadOfBurningASecondCandidate(t *testing.T) {
	claim := rrClaim("ow-wait-claim")
	warm := owWarmSandbox("ow-wait-warm")
	spare := owWarmSandbox("ow-wait-spare")
	rec := &opRecorder{}
	c := owFakeClient(t, rec, nil, rrTemplate(), rrWarmPool(), claim, warm, spare)
	r := owReconciler(t, c, true, warm.Name, spare.Name)

	owReconcile(t, r, claim.Name)
	opsAfterAdoption := len(rec.sequence())

	// Echo pass inside the window.
	res := owReconcile(t, r, claim.Name)
	if res.RequeueAfter != adoptionCacheLagRequeueDelay {
		t.Errorf("expected bounded requeue of %v during the async window, got %+v", adoptionCacheLagRequeueDelay, res)
	}
	if seq := rec.sequence(); len(seq) != opsAfterAdoption {
		t.Errorf("expected the async-window pass to issue no writes, got: %v", seq[opsAfterAdoption:])
	}

	// The spare candidate was not popped: it must still be adoptable next.
	if r.adoptionFlusher.pending() != 1 {
		t.Fatalf("expected exactly the original queued patch, got %d", r.adoptionFlusher.pending())
	}
	if !r.adoptionFlusher.processNext(context.Background()) {
		t.Fatal("expected to drain the queued patch")
	}

	// Post-window pass converges without further writes beyond none needed.
	owReconcile(t, r, claim.Name)
	got := &extensionsv1beta1.SandboxClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}, got); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	if got.Status.SandboxStatus.Name != warm.Name {
		t.Errorf("expected claim to remain bound to %s, got %q", warm.Name, got.Status.SandboxStatus.Name)
	}
	sb := &sandboxv1beta1.Sandbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: spare.Name, Namespace: rrNamespace}, sb); err != nil {
		t.Fatalf("failed to get spare: %v", err)
	}
	if _, ok := sb.Labels[warmPoolSandboxLabel]; !ok {
		t.Error("spare candidate was burned during the async window")
	}
}

// TestOneWriteAdoption409StealRecoveryRebindsClaim: the cross-process race.
// Another writer takes the candidate during the async window; the deferred
// patch 409s, the flusher verifies the steal, clears the stale binding
// (Ready=False/AdoptionLost) and the next reconcile re-adopts the next
// candidate. The stolen sandbox is left untouched.
func TestOneWriteAdoption409StealRecoveryRebindsClaim(t *testing.T) {
	claim := rrClaim("ow-steal-claim")
	thief := rrClaim("ow-thief-claim")
	warm := owWarmSandbox("ow-steal-warm")
	spare := owWarmSandbox("ow-steal-spare")
	rec := &opRecorder{}
	var creates atomic.Int64
	c := owFakeClient(t, rec, &creates, rrTemplate(), rrWarmPool(), claim, thief, warm, spare)
	r := owReconciler(t, c, true, warm.Name, spare.Name)

	owReconcile(t, r, claim.Name)

	// Steal on the server during the window: another process adopted warm for
	// the thief claim (ownership flip + warm label removal), bumping the RV
	// past the deferred patch's optimistic-lock base.
	stolen := &sandboxv1beta1.Sandbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: warm.Name, Namespace: rrNamespace}, stolen); err != nil {
		t.Fatalf("failed to get sandbox: %v", err)
	}
	delete(stolen.Labels, warmPoolSandboxLabel)
	stolen.Labels[extensionsv1beta1.SandboxIDLabel] = string(thief.UID)
	stolen.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "extensions.agents.x-k8s.io/v1beta1",
		Kind:       "SandboxClaim",
		Name:       thief.Name,
		UID:        thief.UID,
		Controller: ptr.To(true), // nolint:modernize
	}}
	if err := c.Update(context.Background(), stolen); err != nil {
		t.Fatalf("failed to apply the steal: %v", err)
	}
	stolenRV := stolen.ResourceVersion

	// Drain: 409 -> verify -> steal detected -> binding cleared.
	if !r.adoptionFlusher.processNext(context.Background()) {
		t.Fatal("expected a queued flush request")
	}
	cleared := &extensionsv1beta1.SandboxClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}, cleared); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	if cleared.Status.SandboxStatus.Name != "" || cleared.Status.SandboxStatus.PodIPs != nil {
		t.Fatalf("expected the stale binding cleared after the steal, got %q/%v",
			cleared.Status.SandboxStatus.Name, cleared.Status.SandboxStatus.PodIPs)
	}
	ready := meta.FindStatusCondition(cleared.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "AdoptionLost" {
		t.Fatalf("expected Ready=False/AdoptionLost after the steal, got %+v", ready)
	}
	if !strings.Contains(ready.Message, warm.Name) {
		t.Errorf("expected the lost sandbox named in the condition message, got %q", ready.Message)
	}

	// Re-adoption: next pass binds the spare; flush converges it.
	owReconcile(t, r, claim.Name)
	rebound := &extensionsv1beta1.SandboxClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}, rebound); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	if rebound.Status.SandboxStatus.Name != spare.Name {
		t.Fatalf("expected re-adoption of %s, got %q", spare.Name, rebound.Status.SandboxStatus.Name)
	}
	ready = meta.FindStatusCondition(rebound.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready=True after re-adoption, got %+v", ready)
	}
	if !r.adoptionFlusher.processNext(context.Background()) {
		t.Fatal("expected the re-adoption's queued patch")
	}
	assertSandboxAdoptionConverged(t, c, spare.Name, claim)

	// The stolen sandbox was never written by us after the steal.
	final := &sandboxv1beta1.Sandbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: warm.Name, Namespace: rrNamespace}, final); err != nil {
		t.Fatalf("failed to get stolen sandbox: %v", err)
	}
	if final.ResourceVersion != stolenRV {
		t.Errorf("expected the stolen sandbox untouched (RV %s), got RV %s", stolenRV, final.ResourceVersion)
	}
	if ref := metav1.GetControllerOf(final); ref == nil || ref.UID != thief.UID {
		t.Errorf("expected the stolen sandbox to remain the thief's, got %+v", ref)
	}
	if n := creates.Load(); n != 0 {
		t.Errorf("expected zero cold starts across the steal recovery, got %d", n)
	}
}

// TestOneWriteAdoptionBenignConflictRebuildsAndRetries: a resourceVersion
// bump with the sandbox STILL pool-owned (concurrent metadata/status writers,
// or a reservation taken from a lagging cache) must not be treated as a
// steal: the flusher rebuilds the mutation set against the fresh read and
// retries, and the claim binding is never disturbed.
func TestOneWriteAdoptionBenignConflictRebuildsAndRetries(t *testing.T) {
	claim := rrClaim("ow-retry-claim")
	warm := owWarmSandbox("ow-retry-warm")
	rec := &opRecorder{}
	c := owFakeClient(t, rec, nil, rrTemplate(), rrWarmPool(), claim, warm)
	r := owReconciler(t, c, true, warm.Name)

	owReconcile(t, r, claim.Name)

	// Benign RV bump: an unrelated annotation lands on the sandbox.
	bumped := &sandboxv1beta1.Sandbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: warm.Name, Namespace: rrNamespace}, bumped); err != nil {
		t.Fatalf("failed to get sandbox: %v", err)
	}
	if bumped.Annotations == nil {
		bumped.Annotations = map[string]string{}
	}
	bumped.Annotations["test.example.com/unrelated"] = "bump"
	if err := c.Update(context.Background(), bumped); err != nil {
		t.Fatalf("failed to bump sandbox RV: %v", err)
	}

	if !r.adoptionFlusher.processNext(context.Background()) {
		t.Fatal("expected a queued flush request")
	}

	// First attempt must have conflicted, then the rebuilt retry succeeded.
	seq := rec.sequence()
	if n := countOps(seq, "sandbox-patch-failed"); n < 1 {
		t.Errorf("expected at least one conflicted sandbox patch attempt, sequence: %v", seq)
	}
	if n := countOps(seq, "sandbox-patch"); n != 1 {
		t.Errorf("expected exactly one successful sandbox patch, got %d (sequence: %v)", n, seq)
	}
	assertSandboxAdoptionConverged(t, c, warm.Name, claim)

	// The unrelated annotation survived the rebuilt patch.
	final := &sandboxv1beta1.Sandbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: warm.Name, Namespace: rrNamespace}, final); err != nil {
		t.Fatalf("failed to get sandbox: %v", err)
	}
	if final.Annotations["test.example.com/unrelated"] != "bump" {
		t.Error("rebuilt patch clobbered an unrelated concurrent annotation")
	}

	// The claim binding was never disturbed.
	got := &extensionsv1beta1.SandboxClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}, got); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	if got.Status.SandboxStatus.Name != warm.Name {
		t.Errorf("expected binding to survive the benign conflict, got %q", got.Status.SandboxStatus.Name)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready to stay True across the benign conflict, got %+v", ready)
	}
}

// TestOneWriteCrashWindowRecoveryN50: the process died AFTER the status
// commit of 50 one-write adoptions but BEFORE any of their async sandbox
// patches landed — the widest window the inversion introduces. A fresh
// controller (empty maps, rebuilt queue) reconciling all 50 claims
// concurrently must re-apply each sandbox-side patch idempotently:
//   - every claim keeps ITS committed sandbox (zero re-adoptions),
//   - exactly one sandbox patch per claim, zero sandbox creates,
//   - ZERO claim status rewrites (the committed status is already correct),
//   - the spare warm candidates stay unburned (recovery never pops the queue).
func TestOneWriteCrashWindowRecoveryN50(t *testing.T) {
	const numClaims = 50
	const numSpares = 10

	objs := []client.Object{rrTemplate(), rrWarmPool()}
	claims := make([]*extensionsv1beta1.SandboxClaim, 0, numClaims)
	for i := range numClaims {
		cl := rrClaim(fmt.Sprintf("ow-crash-claim-%02d", i))
		sb := owWarmSandbox(fmt.Sprintf("ow-crash-sb-%02d", i))
		// The crash-window shape: claim status committed (bound + podIPs +
		// Ready forwarded verbatim from the sandbox), sandbox fully
		// pool-owned and unpatched.
		cl.Status.SandboxStatus.Name = sb.Name
		cl.Status.SandboxStatus.PodIPs = sb.Status.PodIPs
		cl.Status.Conditions = []metav1.Condition{sb.Status.Conditions[0]}
		claims = append(claims, cl)
		objs = append(objs, cl, sb)
	}
	spares := make([]string, 0, numSpares)
	for i := range numSpares {
		name := fmt.Sprintf("ow-crash-spare-%02d", i)
		spares = append(spares, name)
		objs = append(objs, owWarmSandbox(name))
	}

	rec := &opRecorder{}
	var creates atomic.Int64
	c := owFakeClient(t, rec, &creates, objs...)
	// Restarted process: queue rebuilt from informer replay offers the
	// still-warm window sandboxes AND the spares (all still carry the pool
	// label server-side). Bound claims must never pop any of them.
	keys := make([]string, 0, numClaims+numSpares)
	for i := range numClaims {
		keys = append(keys, fmt.Sprintf("ow-crash-sb-%02d", i))
	}
	keys = append(keys, spares...)
	r := owReconciler(t, c, true, keys...)

	runAll := func(pass int) {
		var wg sync.WaitGroup
		errs := make([]error, numClaims)
		for i, cl := range claims {
			wg.Add(1)
			go func(i int, name string) {
				defer wg.Done()
				_, err := r.Reconcile(context.Background(),
					reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: rrNamespace}})
				errs[i] = err
			}(i, cl.Name)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("pass %d claim %02d: expected nil reconcile error, got: %v", pass, i, err)
			}
		}
	}

	runAll(1) // recovery pass: re-applies the sandbox patches
	runAll(2) // convergence pass: observes ownership, fully idempotent

	seq := rec.sequence()
	if n := countOps(seq, "sandbox-patch"); n != numClaims {
		t.Errorf("expected exactly %d recovery sandbox patches, got %d", numClaims, n)
	}
	if n := countOps(seq, "claim-status-patch"); n != 0 {
		t.Errorf("expected ZERO claim status rewrites during crash-window recovery (status was already committed), got %d", n)
	}
	if n := creates.Load(); n != 0 {
		t.Errorf("expected ZERO sandbox creates (no duplicate cold starts), got %d", n)
	}

	for i := range numClaims {
		claimName := fmt.Sprintf("ow-crash-claim-%02d", i)
		wantSandbox := fmt.Sprintf("ow-crash-sb-%02d", i)
		got := &extensionsv1beta1.SandboxClaim{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: claimName, Namespace: rrNamespace}, got); err != nil {
			t.Fatalf("failed to get %s: %v", claimName, err)
		}
		if got.Status.SandboxStatus.Name != wantSandbox {
			t.Errorf("%s: expected to keep committed binding %s, got %q", claimName, wantSandbox, got.Status.SandboxStatus.Name)
		}
		ready := meta.FindStatusCondition(got.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
		if ready == nil || ready.Status != metav1.ConditionTrue {
			t.Errorf("%s: expected Ready to remain True, got %+v", claimName, ready)
		}
		assertSandboxAdoptionConverged(t, c, wantSandbox, claims[i])
	}

	// Spares stayed in the pool: recovery never burned a fresh candidate.
	for _, name := range spares {
		sb := &sandboxv1beta1.Sandbox{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: rrNamespace}, sb); err != nil {
			t.Fatalf("failed to get spare %s: %v", name, err)
		}
		if _, ok := sb.Labels[warmPoolSandboxLabel]; !ok {
			t.Errorf("spare %s was burned during crash-window recovery", name)
		}
		if ref := metav1.GetControllerOf(sb); ref == nil || ref.Kind != "SandboxWarmPool" {
			t.Errorf("spare %s: expected to remain pool-owned, got %+v", name, ref)
		}
	}
}

// TestWarmPoolCountingAcrossOneWriteAsyncWindow is the reconcilePool audit:
// a status-bound-but-unpatched sandbox still carries the pool label and pool
// controller ref, so during the async window the pool must (a) keep counting
// it as a Ready member and (b) NOT create a replacement (no double-count /
// refill over-create); the deficit becomes visible only once the async patch
// removes the label — at which point exactly one replacement is created,
// same as the 2-write path just shifted by the window.
func TestWarmPoolCountingAcrossOneWriteAsyncWindow(t *testing.T) {
	claim := rrClaim("ow-pool-claim")
	warm := owWarmSandbox("ow-pool-warm")

	var poolCreates, poolDeletes atomic.Int64
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(rrTemplate(), rrWarmPool(), claim, warm).
		WithStatusSubresource(&extensionsv1beta1.SandboxClaim{}, &extensionsv1beta1.SandboxWarmPool{}).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxClaimUIDLabelIndex, sandboxClaimUIDLabelIndexer).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok {
					poolCreates.Add(1)
				}
				return cl.Create(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok {
					poolDeletes.Add(1)
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	claimReconciler := owReconciler(t, c, true, warm.Name)
	poolReconciler := &SandboxWarmPoolReconciler{Client: c, Scheme: newScheme(t), MaxBatchSize: 300}
	poolReq := reconcile.Request{NamespacedName: types.NamespacedName{Name: rrPoolName, Namespace: rrNamespace}}

	// Open the window: claim committed, sandbox unpatched.
	owReconcile(t, claimReconciler, claim.Name)

	// Pool reconcile DURING the window: the member still counts; no churn.
	if _, err := poolReconciler.Reconcile(context.Background(), poolReq); err != nil {
		t.Fatalf("pool reconcile during window: %v", err)
	}
	if n := poolCreates.Load(); n != 0 {
		t.Errorf("pool over-created during the async window: %d replacement(s) for a still-counted member", n)
	}
	if n := poolDeletes.Load(); n != 0 {
		t.Errorf("pool deleted a sandbox during the async window: %d", n)
	}
	pool := &extensionsv1beta1.SandboxWarmPool{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: rrPoolName, Namespace: rrNamespace}, pool); err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}
	if pool.Status.Replicas != 1 || pool.Status.ReadyReplicas != 1 {
		t.Errorf("expected the window member counted once (replicas=1 ready=1), got replicas=%d ready=%d",
			pool.Status.Replicas, pool.Status.ReadyReplicas)
	}

	// Close the window, then the pool sees the deficit exactly once.
	if !claimReconciler.adoptionFlusher.processNext(context.Background()) {
		t.Fatal("expected the queued async patch")
	}
	if _, err := poolReconciler.Reconcile(context.Background(), poolReq); err != nil {
		t.Fatalf("pool reconcile after window: %v", err)
	}
	if n := poolCreates.Load(); n != 1 {
		t.Errorf("expected exactly ONE replacement create after the async patch landed, got %d", n)
	}
	if n := poolDeletes.Load(); n != 0 {
		t.Errorf("expected the adopted sandbox left alone by the pool, got %d delete(s)", n)
	}
}

// TestOneWriteAdoptionReservationBlocksDoubleBind reproduces the leg-S
// (RESULTS.md 2026-07-20) double-bind: with one-write adoption, a popped
// candidate stays pool-owned and adoptable in the informer cache for the
// whole async-patch window, so a watch event observed during the window —
// here the pod-scheduling NodeName status write, the exact re-add trigger in
// sandboxEventHandler.Update — used to re-queue the key, and a SECOND claim
// popped, verified against the same stale cache view, and status-bound the
// same sandbox (3,204 of 5,457 bound sandboxes were double-bound in the
// sustained-300 leg; the losers wedged until their 300s timeout). The pop's
// reservation must make the re-add a no-op: claim B must not bind A's
// candidate, and — because reserved members are also excluded from the
// cold-start guard's adoptable count — must cold-start immediately instead
// of deferring against phantom pool capacity.
func TestOneWriteAdoptionReservationBlocksDoubleBind(t *testing.T) {
	claimA := rrClaim("ow-res-a")
	claimB := rrClaim("ow-res-b")
	warm := owWarmSandbox("ow-res-warm")
	rec := &opRecorder{}
	c := owFakeClient(t, rec, nil, rrTemplate(), rrWarmPool(), claimA, claimB, warm)
	r := owReconciler(t, c, true, warm.Name)

	// Claim A reserves the candidate and commits the status-first bind; the
	// sandbox-side patch stays queued (the async window is open).
	owReconcile(t, r, claimA.Name)
	boundA := &extensionsv1beta1.SandboxClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: claimA.Name, Namespace: rrNamespace}, boundA); err != nil {
		t.Fatalf("failed to get claim A: %v", err)
	}
	if boundA.Status.SandboxStatus.Name != warm.Name {
		t.Fatalf("expected claim A bound to %s, got %q", warm.Name, boundA.Status.SandboxStatus.Name)
	}
	if r.adoptionFlusher.pending() != 1 {
		t.Fatalf("expected the async window open (1 queued patch), got %d", r.adoptionFlusher.pending())
	}

	// Mid-window scheduling event: the sandbox is still pool-owned and
	// adoptable in the cache, and its pod just got a node — exactly the
	// nodeScheduled re-add path in sandboxEventHandler.Update.
	oldView := owWarmSandbox(warm.Name)
	newView := owWarmSandbox(warm.Name)
	newView.Status.NodeName = "node-1"
	h := &sandboxEventHandler{sandboxQueue: r.WarmSandboxQueue}
	h.Update(context.Background(), event.UpdateEvent{ObjectOld: oldView, ObjectNew: newView}, nil)

	// Claim B must NOT receive the reserved candidate; with the pool truly
	// empty for it (the one member is reserved), it cold-starts its own
	// sandbox in the same pass.
	owReconcile(t, r, claimB.Name)
	boundB := &extensionsv1beta1.SandboxClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: claimB.Name, Namespace: rrNamespace}, boundB); err != nil {
		t.Fatalf("failed to get claim B: %v", err)
	}
	if boundB.Status.SandboxStatus.Name == warm.Name {
		t.Fatalf("DOUBLE-BIND: claim B bound to claim A's reserved candidate %s", warm.Name)
	}
	if boundB.Status.SandboxStatus.Name != claimB.Name {
		t.Errorf("expected claim B to cold-start its own sandbox %q (reserved member excluded from the cold-start guard), got %q",
			claimB.Name, boundB.Status.SandboxStatus.Name)
	}

	// Close the window: the candidate converges to claim A alone.
	if !r.adoptionFlusher.processNext(context.Background()) {
		t.Fatal("expected claim A's queued async patch")
	}
	assertSandboxAdoptionConverged(t, c, warm.Name, claimA)

	// A's binding survived untouched.
	finalA := &extensionsv1beta1.SandboxClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: claimA.Name, Namespace: rrNamespace}, finalA); err != nil {
		t.Fatalf("failed to get claim A: %v", err)
	}
	if finalA.Status.SandboxStatus.Name != warm.Name {
		t.Errorf("expected claim A to keep %s, got %q", warm.Name, finalA.Status.SandboxStatus.Name)
	}
}

// TestOneWriteStealRecoveryClearsBindingDespiteStaleClaimCache pins the
// leg-S wedged-loser fix: the flusher's steal recovery must not depend on
// informer convergence. The cached client here serves a permanently PRE-BIND
// view of the claim (modeling the seconds of watch backlog measured in the
// sustained-300 leg); the previous implementation read the claim through the
// cache, concluded its own status write "hadn't appeared yet", burned all
// bounded attempts sleeping, and left the claim advertising a sandbox it
// lost — the ~4,300 claims that sat bound-but-never-Ready until their 300s
// timeout. With direct (non-cached) reads the recovery clears the binding in
// one round regardless of cache lag.
func TestOneWriteStealRecoveryClearsBindingDespiteStaleClaimCache(t *testing.T) {
	claim := rrClaim("ow-lag-claim")
	thief := rrClaim("ow-lag-thief")
	warm := owWarmSandbox("ow-lag-warm")

	base := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(rrTemplate(), rrWarmPool(), claim, thief, warm).
		WithStatusSubresource(&extensionsv1beta1.SandboxClaim{}).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxClaimUIDLabelIndex, sandboxClaimUIDLabelIndexer).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer).
		Build()

	// Freeze the claim's cached view at its pre-bind state: every cached GET
	// of the claim returns this snapshot, no matter what lands on the server.
	staleClaim := &extensionsv1beta1.SandboxClaim{}
	if err := base.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}, staleClaim); err != nil {
		t.Fatalf("failed to snapshot pre-bind claim: %v", err)
	}
	cached := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if target, ok := obj.(*extensionsv1beta1.SandboxClaim); ok && key.Name == claim.Name {
				staleClaim.DeepCopyInto(target)
				return nil
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})

	q := queue.NewSimpleSandboxQueue()
	q.Add(queue.GetNamespacedWarmPoolName(rrNamespace, rrPoolName), queue.SandboxKey{Namespace: rrNamespace, Name: warm.Name})
	r := rrReconciler(cached, t, q)
	r.OneWriteAdoption = true
	r.adoptionFlusher = newAdoptionFlusher(r)
	r.DirectReader = base

	// Bind (status-first commit); async window open.
	owReconcile(t, r, claim.Name)
	bound := &extensionsv1beta1.SandboxClaim{}
	if err := base.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}, bound); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	if bound.Status.SandboxStatus.Name != warm.Name {
		t.Fatalf("expected claim bound to %s, got %q", warm.Name, bound.Status.SandboxStatus.Name)
	}

	// Steal during the window.
	stolen := &sandboxv1beta1.Sandbox{}
	if err := base.Get(context.Background(), types.NamespacedName{Name: warm.Name, Namespace: rrNamespace}, stolen); err != nil {
		t.Fatalf("failed to get sandbox: %v", err)
	}
	delete(stolen.Labels, warmPoolSandboxLabel)
	stolen.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "extensions.agents.x-k8s.io/v1beta1",
		Kind:       "SandboxClaim",
		Name:       thief.Name,
		UID:        thief.UID,
		Controller: ptr.To(true), // nolint:modernize
	}}
	if err := base.Update(context.Background(), stolen); err != nil {
		t.Fatalf("failed to apply the steal: %v", err)
	}

	// Drain the flusher: 409 -> direct re-verify -> steal -> the binding must
	// be cleared on the SERVER even though the cached claim never converges.
	if !r.adoptionFlusher.processNext(context.Background()) {
		t.Fatal("expected a queued flush request")
	}
	cleared := &extensionsv1beta1.SandboxClaim{}
	if err := base.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}, cleared); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	if cleared.Status.SandboxStatus.Name != "" {
		t.Fatalf("wedged loser: stale binding to %q not cleared despite the lagging claim cache", cleared.Status.SandboxStatus.Name)
	}
	ready := meta.FindStatusCondition(cleared.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "AdoptionLost" {
		t.Fatalf("expected Ready=False/AdoptionLost after the steal, got %+v", ready)
	}
}
