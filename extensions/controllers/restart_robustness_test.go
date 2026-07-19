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

// Round-5b restart/failover robustness net.
//
// The perf-investigation branch moved adoption-critical state into process
// memory: triggeredAdoptions / lastWrittenStatuses / observedTimes /
// coldStartDeferrals on the claim controller, the WarmSandboxQueue, and the
// warm pool's replenish-defer baseline. Every test in this file simulates a
// controller restart mid-burst — a FRESH reconciler instance (all in-memory
// state empty) pointed at pre-populated "server" state that a previous
// incarnation left behind — and pins down the recovery invariants:
//
//  1. Claims adopted but status-unwritten (crash inside the 2-write adoption
//     transaction) are recovered via the claim-UID label List, concurrently,
//     with zero duplicate sandboxes (TestRestartMidBurst...N50).
//  2. The warm queue rebuild from informer replay never re-offers
//     already-adopted sandboxes (TestRestartWarmQueueRebuild...), and even a
//     STALE rebuilt queue entry cannot double-adopt because the optimistic
//     lock rejects the stale-RV patch (TestRestartStaleQueueEntry...).
//  3. With lastWrittenStatuses empty, a stale post-restart pass re-sends AT
//     MOST ONE redundant status patch per claim before the guard re-arms
//     (TestRestartEchoBlastRadius...) — the echo-storm blast radius.
//  4. The warm pool's replenish-defer baseline is forfeited for exactly one
//     observation after restart (TestWarmPoolReplenishDeferStateLost...).

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

const (
	rrNamespace    = "default"
	rrPoolName     = "restart-pool"
	rrTemplateName = "restart-template"
	rrPoolUID      = types.UID("restart-pool-uid")
)

func rrTemplate() *extensionsv1beta1.SandboxTemplate {
	return &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: rrTemplateName, Namespace: rrNamespace},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
		}}},
	}
}

func rrWarmPool() *extensionsv1beta1.SandboxWarmPool {
	return &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: rrPoolName, Namespace: rrNamespace, UID: rrPoolUID},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: rrTemplateName}},
	}
}

// rrWarmSandbox builds a pool-owned, Ready, adoptable warm sandbox — the
// server-side shape produced by the warm pool controller.
func rrWarmSandbox(name string) *sandboxv1beta1.Sandbox {
	return &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: rrNamespace,
			UID:       types.UID(name + "-uid"),
			Labels: map[string]string{
				warmPoolSandboxLabel:   sandboxcontrollers.NameHash(rrPoolName),
				sandboxTemplateRefHash: sandboxcontrollers.NameHash(rrTemplateName),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1beta1",
				Kind:       "SandboxWarmPool",
				Name:       rrPoolName,
				UID:        rrPoolUID,
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
		}}},
		Status: sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				Reason:             "Ready",
				Message:            "Sandbox is ready",
				LastTransitionTime: metav1.NewTime(time.Now().Add(-5 * time.Second).Truncate(time.Second)),
			}},
		},
	}
}

// rrAdoptedSandbox builds the server-side shape a sandbox has after write 1
// of the 2-write adoption transaction landed (completeAdoption's
// optimistic-lock patch): warm-pool label gone, claim-UID label and claim
// controller ref stamped, Warm launch type set, pod-template metadata forced
// to exactly what completeAdoption computes for the round's fixtures — so a
// recovery pass that recomputes the merged metadata sees NO drift and stays
// write-free on the sandbox.
func rrAdoptedSandbox(name string, claim *extensionsv1beta1.SandboxClaim) *sandboxv1beta1.Sandbox {
	templateHash := sandboxcontrollers.NameHash(rrTemplateName)
	return &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: rrNamespace,
			UID:       types.UID(name + "-uid"),
			Labels: map[string]string{
				extensionsv1beta1.SandboxIDLabel:      string(claim.UID),
				sandboxTemplateRefHash:                templateHash,
				sandboxv1beta1.SandboxLaunchTypeLabel: sandboxv1beta1.SandboxLaunchTypeWarm,
			},
			Annotations: map[string]string{
				sandboxv1beta1.SandboxPodNameAnnotation: name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1beta1",
				Kind:       "SandboxClaim",
				Name:       claim.Name,
				UID:        claim.UID,
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			ObjectMeta: sandboxv1beta1.PodMetadata{
				Labels: map[string]string{
					extensionsv1beta1.SandboxIDLabel: string(claim.UID),
					sandboxTemplateRefHash:           templateHash,
				},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
		}}},
		Status: sandboxv1beta1.SandboxStatus{
			PodIPs: []string{"10.0.0.1"},
			Conditions: []metav1.Condition{{
				Type:               string(sandboxv1beta1.SandboxConditionReady),
				Status:             metav1.ConditionTrue,
				Reason:             "Ready",
				Message:            "Sandbox is ready",
				LastTransitionTime: metav1.NewTime(time.Now().Add(-5 * time.Second).Truncate(time.Second)),
			}},
		},
	}
}

func rrClaim(name string) *extensionsv1beta1.SandboxClaim {
	return &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: rrNamespace, UID: types.UID(name + "-uid")},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: rrPoolName}},
	}
}

func rrReconciler(c client.Client, t *testing.T, q queue.SandboxQueue) *SandboxClaimReconciler {
	t.Helper()
	return &SandboxClaimReconciler{
		Client:           c,
		Scheme:           newScheme(t),
		Recorder:         events.NewFakeRecorder(256),
		Tracer:           asmetrics.NewNoOp(),
		WarmSandboxQueue: q,
	}
}

// TestRestartMidBurstConcurrentRecoveryOfAdoptedUnwrittenClaimsN50 is the
// question-1(a) regression: the previous leader crashed mid-burst AFTER
// sending the adoption patch (write 1) for 50 claims but BEFORE any of their
// claim status patches (write 2). The restarted controller therefore sees 50
// claims with no bound status, no assigned-sandbox annotation, and empty
// in-memory maps — the binding exists ONLY on the sandboxes, via the
// claim-UID label + controller ref stamped by write 1.
//
// 50 concurrent first passes on the fresh instance must:
//   - recover every claim to ITS OWN pre-adopted sandbox (indexed label List),
//   - issue exactly one status patch + one annotation-flush patch per claim,
//   - create ZERO sandboxes (no duplicate cold starts),
//   - touch ZERO sandboxes (recovery is read-only on the sandbox side),
//   - leave the rebuilt warm queue's spare candidates unburned.
func TestRestartMidBurstConcurrentRecoveryOfAdoptedUnwrittenClaimsN50(t *testing.T) {
	const numClaims = 50
	const numSpares = 10
	scheme := newScheme(t)

	objs := []client.Object{rrTemplate(), rrWarmPool()}
	claims := make([]*extensionsv1beta1.SandboxClaim, 0, numClaims)
	for i := range numClaims {
		cl := rrClaim(fmt.Sprintf("burst-claim-%02d", i))
		claims = append(claims, cl)
		objs = append(objs, cl, rrAdoptedSandbox(fmt.Sprintf("adopted-sb-%02d", i), cl))
	}
	spares := make([]string, 0, numSpares)
	for i := range numSpares {
		name := fmt.Sprintf("spare-warm-sb-%02d", i)
		spares = append(spares, name)
		objs = append(objs, rrWarmSandbox(name))
	}

	var sandboxCreates, sandboxWrites, claimUpdates, claimStatusPatches, claimMetaPatches atomic.Int64
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&extensionsv1beta1.SandboxClaim{}).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxClaimUIDLabelIndex, sandboxClaimUIDLabelIndexer).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok {
					sandboxCreates.Add(1)
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				switch obj.(type) {
				case *extensionsv1beta1.SandboxClaim:
					claimUpdates.Add(1)
				case *sandboxv1beta1.Sandbox:
					sandboxWrites.Add(1)
				}
				return c.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				switch obj.(type) {
				case *sandboxv1beta1.Sandbox:
					sandboxWrites.Add(1)
				case *extensionsv1beta1.SandboxClaim:
					claimMetaPatches.Add(1)
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok && subResourceName == "status" {
					claimStatusPatches.Add(1)
				}
				return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	// Restarted process: the warm queue was rebuilt from the informer replay,
	// so it offers ONLY the still-warm spares (the adopted sandboxes lost
	// their warm-pool label in write 1 and never re-enter the queue — proven
	// separately by TestRestartWarmQueueRebuildFromInformerReplay).
	warmQueue := queue.NewSimpleSandboxQueue()
	for _, name := range spares {
		warmQueue.Add(queue.GetNamespacedWarmPoolName(rrNamespace, rrPoolName), queue.SandboxKey{Namespace: rrNamespace, Name: name})
	}
	reconciler := rrReconciler(fakeClient, t, warmQueue)

	// Fire all 50 first passes concurrently, as the restarted workers would.
	var wg sync.WaitGroup
	errs := make([]error, numClaims)
	for i, cl := range claims {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			_, err := reconciler.Reconcile(context.Background(),
				reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: rrNamespace}})
			errs[i] = err
		}(i, cl.Name)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("claim %02d: expected nil reconcile error, got: %v", i, err)
		}
	}

	// Every claim recovered its own binding, exactly.
	for i := range numClaims {
		claimName := fmt.Sprintf("burst-claim-%02d", i)
		wantSandbox := fmt.Sprintf("adopted-sb-%02d", i)
		got := &extensionsv1beta1.SandboxClaim{}
		if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: claimName, Namespace: rrNamespace}, got); err != nil {
			t.Fatalf("failed to get %s: %v", claimName, err)
		}
		if got.Status.SandboxStatus.Name != wantSandbox {
			t.Errorf("%s: expected recovered binding to %s, got %q", claimName, wantSandbox, got.Status.SandboxStatus.Name)
		}
		ready := meta.FindStatusCondition(got.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
		if ready == nil || ready.Status != metav1.ConditionTrue {
			t.Errorf("%s: expected Ready=True after recovery, got %+v", claimName, ready)
		}
		if got.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation] != wantSandbox {
			t.Errorf("%s: expected assigned-sandbox annotation re-flushed to %s, got %q",
				claimName, wantSandbox, got.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation])
		}
	}

	// Recovery write budget: 1 status patch + 1 annotation flush per claim,
	// nothing else. In particular no duplicate sandboxes and no sandbox writes.
	if n := sandboxCreates.Load(); n != 0 {
		t.Errorf("expected ZERO sandbox creations during concurrent recovery, got %d duplicate cold starts", n)
	}
	if n := sandboxWrites.Load(); n != 0 {
		t.Errorf("expected ZERO sandbox writes during concurrent recovery, got %d", n)
	}
	if n := claimUpdates.Load(); n != 0 {
		t.Errorf("expected ZERO full-object claim Updates during recovery, got %d", n)
	}
	if n := claimStatusPatches.Load(); n != numClaims {
		t.Errorf("expected exactly %d claim status patches (one per recovered claim), got %d", numClaims, n)
	}
	if n := claimMetaPatches.Load(); n != numClaims {
		t.Errorf("expected exactly %d claim annotation-flush patches (one per recovered claim), got %d", numClaims, n)
	}

	// The spare warm candidates were not burned by any recovery pass.
	for _, name := range spares {
		sb := &sandboxv1beta1.Sandbox{}
		if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: rrNamespace}, sb); err != nil {
			t.Fatalf("failed to get spare %s: %v", name, err)
		}
		if _, ok := sb.Labels[warmPoolSandboxLabel]; !ok {
			t.Errorf("spare %s: expected to remain in the warm pool, warm label was removed", name)
		}
		if ref := metav1.GetControllerOf(sb); ref == nil || ref.Kind != "SandboxWarmPool" {
			t.Errorf("spare %s: expected to remain pool-owned, got controller ref %+v", name, ref)
		}
	}

	// Second pass over every claim must be fully idempotent: the first pass
	// repopulated lastWrittenStatuses and persisted the annotations, so a
	// converged re-reconcile issues no writes at all.
	statusBefore, metaBefore := claimStatusPatches.Load(), claimMetaPatches.Load()
	for i := range numClaims {
		name := fmt.Sprintf("burst-claim-%02d", i)
		if _, err := reconciler.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: rrNamespace}}); err != nil {
			t.Fatalf("second pass %s: expected nil error, got: %v", name, err)
		}
	}
	if n := claimStatusPatches.Load(); n != statusBefore {
		t.Errorf("expected ZERO additional status patches on converged second passes, got %d", n-statusBefore)
	}
	if n := claimMetaPatches.Load(); n != metaBefore {
		t.Errorf("expected ZERO additional annotation patches on converged second passes, got %d", n-metaBefore)
	}
	if n := sandboxCreates.Load(); n != 0 {
		t.Errorf("expected ZERO sandbox creations after second passes, got %d", n)
	}
}

// TestRestartWarmQueueRebuildFromInformerReplay is the question-1(b) rebuild
// half: after a restart the WarmSandboxQueue is empty and is rebuilt purely
// from the informer's initial-sync Create replay through sandboxEventHandler.
// The rebuilt queue must contain every adoptable warm member exactly once and
// must NOT re-offer sandboxes the previous incarnation already adopted
// (write 1 removed their warm-pool label and moved their controller ref to
// the claim) nor deleting members. Adoption capacity therefore resumes the
// moment the informer replay finishes — there is no separate warm-up, and the
// rebuild itself introduces no duplicate-adoption source.
func TestRestartWarmQueueRebuildFromInformerReplay(t *testing.T) {
	const numWarm = 30
	const numAdopted = 20
	const numDeleting = 5

	q := queue.NewSimpleSandboxQueue()
	handler := &sandboxEventHandler{sandboxQueue: q}
	ctx := context.Background()

	replay := make([]*sandboxv1beta1.Sandbox, 0, numWarm+numAdopted+numDeleting)
	wantQueued := map[string]bool{}
	for i := range numWarm {
		sb := rrWarmSandbox(fmt.Sprintf("warm-%02d", i))
		wantQueued[sb.Name] = true
		replay = append(replay, sb)
	}
	for i := range numAdopted {
		cl := rrClaim(fmt.Sprintf("owner-claim-%02d", i))
		replay = append(replay, rrAdoptedSandbox(fmt.Sprintf("adopted-%02d", i), cl))
	}
	for i := range numDeleting {
		sb := rrWarmSandbox(fmt.Sprintf("deleting-%02d", i))
		sb.DeletionTimestamp = ptr.To(metav1.NewTime(time.Now()))
		sb.Finalizers = []string{"test.example.com/finalizer"}
		replay = append(replay, sb)
	}

	// Informer initial sync: one Create event per existing object.
	for _, sb := range replay {
		handler.Create(ctx, event.CreateEvent{Object: sb}, nil)
	}

	poolKey := queue.GetNamespacedWarmPoolName(rrNamespace, rrPoolName)
	popped := map[string]bool{}
	for {
		key, ok := q.Get(poolKey)
		if !ok {
			break
		}
		if popped[key.Name] {
			t.Errorf("queue rebuild produced duplicate entry for %s", key.Name)
		}
		popped[key.Name] = true
	}

	if len(popped) != numWarm {
		t.Errorf("expected rebuilt queue to hold exactly the %d adoptable warm members, got %d: %v", numWarm, len(popped), popped)
	}
	for name := range wantQueued {
		if !popped[name] {
			t.Errorf("adoptable warm member %s missing from rebuilt queue", name)
		}
	}
	for name := range popped {
		if !wantQueued[name] {
			t.Errorf("rebuilt queue re-offered non-adoptable sandbox %s (already adopted or deleting) — duplicate-adoption source", name)
		}
	}
}

// TestRestartStaleQueueEntryDoubleAdoptionCaughtByOptimisticLock is the
// question-1(b) race half: the restarted leader rebuilt its queue from a
// LAGGING initial list, so the queue still offers sb-1 even though the OLD
// leader already adopted sb-1 for claim-a and fully finalized claim-a (server
// truth). The new leader's cache also still serves the stale pre-adoption
// view of sb-1. When claim-b pops the stale entry, its optimistic-lock
// adoption patch carries the stale resourceVersion and MUST be rejected with
// a 409 — claim-b then retries with the next candidate in the same pass. The
// window closes with exactly one loser and zero double-adoptions.
func TestRestartStaleQueueEntryDoubleAdoptionCaughtByOptimisticLock(t *testing.T) {
	scheme := newScheme(t)

	claimA := rrClaim("claim-a")
	claimB := rrClaim("claim-b")
	sb1 := rrWarmSandbox("sb-1") // server copy will be flipped to adopted-by-claim-a below
	sb2 := rrWarmSandbox("sb-2")

	var sandboxPatchAttempts, sandboxPatchConflicts atomic.Int64
	staleSb1 := &sandboxv1beta1.Sandbox{}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rrTemplate(), rrWarmPool(), claimA, claimB, sb1, sb2).
		WithStatusSubresource(&extensionsv1beta1.SandboxClaim{}).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxClaimUIDLabelIndex, sandboxClaimUIDLabelIndexer).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				// The restarted leader's informer never saw the old leader's
				// adoption of sb-1: serve the frozen pre-adoption view.
				if sb, ok := obj.(*sandboxv1beta1.Sandbox); ok && key.Name == "sb-1" && staleSb1.Name != "" {
					staleSb1.DeepCopyInto(sb)
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				_, isSandbox := obj.(*sandboxv1beta1.Sandbox)
				if isSandbox {
					sandboxPatchAttempts.Add(1)
				}
				err := c.Patch(ctx, obj, patch, opts...)
				if isSandbox && k8errors.IsConflict(err) {
					sandboxPatchConflicts.Add(1)
				}
				return err
			},
		}).
		Build()

	// Snapshot the pre-adoption server view of sb-1 (what the stale cache
	// serves), THEN apply the old leader's adoption on the server: sb-1 moves
	// to claim-a, its resourceVersion advances past the frozen view.
	preAdoption := &sandboxv1beta1.Sandbox{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "sb-1", Namespace: rrNamespace}, preAdoption); err != nil {
		t.Fatalf("failed to snapshot pre-adoption view of sb-1: %v", err)
	}
	serverSb1 := preAdoption.DeepCopy()
	delete(serverSb1.Labels, warmPoolSandboxLabel)
	serverSb1.Labels[extensionsv1beta1.SandboxIDLabel] = string(claimA.UID)
	serverSb1.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
	// completeAdoption also forces the merged pod-template metadata in the
	// same write; mirror it so claim-a's post-restart pass sees no drift.
	serverSb1.Spec.PodTemplate.ObjectMeta.Labels = map[string]string{
		extensionsv1beta1.SandboxIDLabel: string(claimA.UID),
		sandboxTemplateRefHash:           sandboxcontrollers.NameHash(rrTemplateName),
	}
	serverSb1.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "extensions.agents.x-k8s.io/v1beta1",
		Kind:       "SandboxClaim",
		Name:       claimA.Name,
		UID:        claimA.UID,
		Controller: ptr.To(true), // nolint:modernize
	}}
	if err := fakeClient.Update(context.Background(), serverSb1); err != nil {
		t.Fatalf("failed to apply old leader's adoption of sb-1: %v", err)
	}
	// The old leader also finalized claim-a before crashing.
	boundA := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "claim-a", Namespace: rrNamespace}, boundA); err != nil {
		t.Fatalf("failed to get claim-a: %v", err)
	}
	boundA.Status.SandboxStatus.Name = "sb-1"
	if err := fakeClient.Status().Update(context.Background(), boundA); err != nil {
		t.Fatalf("failed to finalize claim-a status: %v", err)
	}
	// Freeze the stale view only after the server writes landed.
	preAdoption.DeepCopyInto(staleSb1)

	// Restarted leader: fresh in-memory state; the queue rebuilt from the
	// lagging list still offers sb-1 first.
	warmQueue := queue.NewSimpleSandboxQueue()
	poolKey := queue.GetNamespacedWarmPoolName(rrNamespace, rrPoolName)
	warmQueue.Add(poolKey, queue.SandboxKey{Namespace: rrNamespace, Name: "sb-1"})
	warmQueue.Add(poolKey, queue.SandboxKey{Namespace: rrNamespace, Name: "sb-2"})
	reconciler := rrReconciler(fakeClient, t, warmQueue)

	if _, err := reconciler.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: "claim-b", Namespace: rrNamespace}}); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}

	// The stale entry was tried and the optimistic lock rejected it.
	if n := sandboxPatchConflicts.Load(); n < 1 {
		t.Errorf("expected the stale-RV adoption patch of sb-1 to be rejected with a conflict, saw %d conflicts (attempts: %d)",
			n, sandboxPatchAttempts.Load())
	}

	// sb-1 still belongs to claim-a: no silent double-adoption.
	staleSb1.Name = "" // disable the stale-view interceptor
	finalSb1 := &sandboxv1beta1.Sandbox{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "sb-1", Namespace: rrNamespace}, finalSb1); err != nil {
		t.Fatalf("failed to get sb-1: %v", err)
	}
	if ref := metav1.GetControllerOf(finalSb1); ref == nil || ref.Kind != "SandboxClaim" || ref.Name != "claim-a" {
		t.Fatalf("expected sb-1 to remain owned by claim-a after the stale re-adoption attempt, got %+v", ref)
	}

	// claim-b recovered in the same pass with the next candidate; no cold start.
	finalB := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "claim-b", Namespace: rrNamespace}, finalB); err != nil {
		t.Fatalf("failed to get claim-b: %v", err)
	}
	if finalB.Status.SandboxStatus.Name != "sb-2" {
		t.Errorf("expected claim-b to adopt the next candidate sb-2, got %q", finalB.Status.SandboxStatus.Name)
	}
	coldSb := &sandboxv1beta1.Sandbox{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "claim-b", Namespace: rrNamespace}, coldSb); !k8errors.IsNotFound(err) {
		t.Errorf("expected NO cold-start duplicate sandbox for claim-b, get err=%v", err)
	}

	// Bonus restart invariant: the restarted leader re-reconciling the fully
	// written claim-a (converged view, empty maps) is entirely write-free.
	// claim-a's annotations were persisted pre-crash, so re-stamping is a
	// no-op; give it the observability annotation the old leader flushed.
	preWrites := sandboxPatchAttempts.Load()
	withAnnotations := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "claim-a", Namespace: rrNamespace}, withAnnotations); err != nil {
		t.Fatalf("failed to get claim-a: %v", err)
	}
	withAnnotations.Annotations = map[string]string{
		asmetrics.ObservabilityAnnotation:               time.Now().Add(-time.Minute).Format(time.RFC3339Nano),
		extensionsv1beta1.AssignedSandboxNameAnnotation: "sb-1",
	}
	if err := fakeClient.Update(context.Background(), withAnnotations); err != nil {
		t.Fatalf("failed to set claim-a annotations: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: "claim-a", Namespace: rrNamespace}}); err != nil {
		t.Fatalf("claim-a converged pass: expected nil error, got: %v", err)
	}
	if n := sandboxPatchAttempts.Load(); n != preWrites {
		t.Errorf("expected claim-a's converged post-restart pass to be sandbox-write-free, saw %d extra patches", n-preWrites)
	}
	finalA := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "claim-a", Namespace: rrNamespace}, finalA); err != nil {
		t.Fatalf("failed to get claim-a: %v", err)
	}
	if finalA.Status.SandboxStatus.Name != "sb-1" {
		t.Errorf("expected claim-a to stay bound to sb-1, got %q", finalA.Status.SandboxStatus.Name)
	}
}

// TestRestartEchoBlastRadiusSingleRedundantStatusWritePerClaim is the
// question-1(c) regression: lastWrittenStatuses (the echo-storm suppressor)
// is empty after a restart, so a post-restart pass that reads a STALE
// pre-Ready claim view — while the server already carries the Ready status
// the old leader wrote — cannot recognize its recomputed status as already
// persisted. The blast radius must be AT MOST ONE redundant status patch per
// claim: that first patch repopulates the guard, and every further stale pass
// is suppressed again. (Pre-quickwins behavior was ~2 extra writes per claim
// per burst, unbounded by passes; the restart must not reopen that.)
func TestRestartEchoBlastRadiusSingleRedundantStatusWritePerClaim(t *testing.T) {
	scheme := newScheme(t)

	claim := rrClaim("echo-claim")
	adopted := rrAdoptedSandbox("echo-adopted-sb", claim)

	serveStale := false
	staleClaim := &extensionsv1beta1.SandboxClaim{}
	var claimStatusPatches, sandboxCreates atomic.Int64
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rrTemplate(), rrWarmPool(), claim, adopted).
		WithStatusSubresource(&extensionsv1beta1.SandboxClaim{}).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxClaimUIDLabelIndex, sandboxClaimUIDLabelIndexer).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if cl, ok := obj.(*extensionsv1beta1.SandboxClaim); ok && serveStale && key.Name == claim.Name {
					staleClaim.DeepCopyInto(cl)
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok {
					sandboxCreates.Add(1)
				}
				return c.Create(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok && subResourceName == "status" {
					claimStatusPatches.Add(1)
				}
				return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	// Freeze the pre-adoption claim view (no status, no annotations) — this
	// is what a lagging watch keeps serving right after the restart.
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}, staleClaim); err != nil {
		t.Fatalf("failed to snapshot stale claim view: %v", err)
	}

	// Old leader's finalization is on the server: bound + Ready, condition
	// forwarded verbatim from the sandbox (as the adoption pass does).
	serverClaim := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}, serverClaim); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	serverClaim.Status.SandboxStatus.Name = adopted.Name
	serverClaim.Status.SandboxStatus.PodIPs = adopted.Status.PodIPs
	serverClaim.Status.Conditions = []metav1.Condition{adopted.Status.Conditions[0]}
	if err := fakeClient.Status().Update(context.Background(), serverClaim); err != nil {
		t.Fatalf("failed to write old leader's status: %v", err)
	}

	// Restarted controller, empty maps, stale claim view, converged sandbox view.
	reconciler := rrReconciler(fakeClient, t, queue.NewSimpleSandboxQueue())
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}}
	serveStale = true

	// Pass 1: the one echo the restart cannot avoid. The claim-UID label List
	// recovers the binding, the recomputed status equals what the old leader
	// persisted, but with lastWrittenStatuses empty the patch goes out.
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("pass 1: expected nil error, got: %v", err)
	}
	if n := claimStatusPatches.Load(); n != 1 {
		t.Fatalf("pass 1: expected exactly ONE redundant status patch (the restart echo), got %d", n)
	}

	// Passes 2-4: the guard has re-armed off pass 1's write; the storm stays dead.
	for pass := 2; pass <= 4; pass++ {
		if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("pass %d: expected nil error, got: %v", pass, err)
		}
		if n := claimStatusPatches.Load(); n != 1 {
			t.Fatalf("pass %d: echo suppression did not re-arm after the first post-restart write: %d status patches total", pass, n)
		}
	}

	// No duplicate sandbox appeared, and the server status is untouched.
	if n := sandboxCreates.Load(); n != 0 {
		t.Errorf("expected ZERO sandbox creations across post-restart echo passes, got %d", n)
	}
	serveStale = false
	final := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), req.NamespacedName, final); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}
	if final.Status.SandboxStatus.Name != adopted.Name {
		t.Errorf("expected claim to remain bound to %s, got %q", adopted.Name, final.Status.SandboxStatus.Name)
	}
	ready := meta.FindStatusCondition(final.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("expected claim to remain Ready, got %+v", ready)
	}
}

// TestWarmPoolReplenishDeferStateLostOnRestart documents the replenish-defer
// baseline's restart semantics (question 1, warm pool leg). The
// per-pool baseline lives only in memory, so the FIRST observation after a
// restart cannot detect an in-flight adoption burst:
//
//   - restart mid-burst forfeits the hold for exactly one observation — the
//     deficit refills immediately, competing with the burst tail for API
//     budget (bounded regression: one refill batch of the old behavior);
//   - equally, replacements created by the previous incarnation that the
//     fresh informer has not served yet are NOT shielded by the
//     noteReplenishCreates baseline anymore — a stale low count refills
//     immediately, so duplicate creates are possible once (self-corrected
//     later by excess deletion, but write-amplifying at burst scale);
//   - from the SECOND observation on, any further drop re-arms the hold, so
//     the defer machinery self-heals after one pass.
func TestWarmPoolReplenishDeferStateLostOnRestart(t *testing.T) {
	const delay = 20 * time.Second
	t0 := time.Now()
	poolKey := types.NamespacedName{Namespace: rrNamespace, Name: rrPoolName}

	type observation struct {
		name        string
		current     int32
		desired     int32
		at          time.Time
		notedBefore int32 // noteReplenishCreates before this observation
		wantHold    bool
	}

	tests := []struct {
		name string
		obs  []observation
	}{
		{
			name: "restart mid-burst: first observation refills immediately despite deficit",
			obs: []observation{
				// Old leader deferred at 220/300 and crashed. New leader's
				// first pass sees the same deficit but has no baseline: the
				// hold is forfeited and the refill fires into the burst tail.
				{name: "first post-restart pass", current: 220, desired: 300, at: t0, wantHold: false},
				// The burst is still draining: the next drop re-arms the hold.
				{name: "second pass, further drop", current: 180, desired: 300, at: t0.Add(time.Second), wantHold: true},
				// Still inside the re-armed window.
				{name: "third pass, no further drop", current: 180, desired: 300, at: t0.Add(2 * time.Second), wantHold: true},
				// Window elapsed with no further drops: refill proceeds.
				{name: "after the window", current: 180, desired: 300, at: t0.Add(25 * time.Second), wantHold: false},
			},
		},
		{
			name: "restart forgets noteReplenishCreates: stale low count refills immediately (duplicate-create exposure)",
			obs: []observation{
				// In-process protection (for contrast): after creating 80
				// replacements the baseline is raised, so a stale re-read of
				// the SAME low count registers as a drop and defers instead
				// of double-creating.
				{name: "baseline pass", current: 220, desired: 300, at: t0, wantHold: false},
				{name: "stale re-read shielded in-process", current: 220, desired: 300, at: t0.Add(time.Second), notedBefore: 80, wantHold: true},
			},
		},
		{
			name: "restart with full pool: no hold, no spurious defer",
			obs: []observation{
				{name: "first pass, pool full", current: 300, desired: 300, at: t0, wantHold: false},
				{name: "drop after baseline", current: 250, desired: 300, at: t0.Add(time.Second), wantHold: true},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh reconciler per case = fresh restart (empty replenishState).
			r := &SandboxWarmPoolReconciler{ReplenishDelay: delay}
			for _, ob := range tc.obs {
				if ob.notedBefore > 0 {
					r.noteReplenishCreates(poolKey, ob.notedBefore)
				}
				hold := r.observeMembersForReplenish(poolKey, ob.current, ob.desired, ob.at)
				if got := hold > 0; got != ob.wantHold {
					t.Errorf("%s: observe(current=%d desired=%d) hold=%v (%v), want hold=%v",
						ob.name, ob.current, ob.desired, got, hold, ob.wantHold)
				}
			}
		})
	}

	t.Run("second restart simulation: duplicate-create exposure is real once", func(t *testing.T) {
		// Old leader: observed 220/300, created 80 replacements (baseline
		// raised to 300), then crashed. New leader's informer list is stale
		// and still reports 220. With the baseline gone the new leader sees
		// "first observation, deficit 80" and refills immediately — up to 80
		// duplicate creates that the excess-deletion path must later reap.
		oldLeader := &SandboxWarmPoolReconciler{ReplenishDelay: delay}
		if hold := oldLeader.observeMembersForReplenish(poolKey, 220, 300, t0); hold != 0 {
			t.Fatalf("old leader first observation: expected immediate refill, got hold %v", hold)
		}
		oldLeader.noteReplenishCreates(poolKey, 80)

		newLeader := &SandboxWarmPoolReconciler{ReplenishDelay: delay}
		if hold := newLeader.observeMembersForReplenish(poolKey, 220, 300, t0.Add(2*time.Second)); hold != 0 {
			t.Errorf("documented exposure changed: new leader with stale count deferred (hold %v) — if the baseline is now persisted/recovered, update this test and the round-5b findings", hold)
		}
	})
}
