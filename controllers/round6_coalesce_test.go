// Copyright 2025 The Kubernetes Authors.
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

// Round-6 coalescing tests: write-behind coalescing of the sandbox
// controller's recoverable metadata-only writes, and ownership-derived pod
// hygiene (--no-spec-adoption). See optimizations/ROUND6-COALESCING.md.

import (
	"context"
	"maps"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
	"sigs.k8s.io/agent-sandbox/internal/writebehind"
)

const (
	r6Namespace = "r6-ns"
	r6Sandbox   = "r6-sandbox"
)

func claimOwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "extensions.agents.x-k8s.io/v1beta1",
		Kind:       "SandboxClaim",
		Name:       "r6-claim",
		UID:        "r6-claim-uid",
		Controller: new(true),
	}
}

func poolOwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "extensions.agents.x-k8s.io/v1beta1",
		Kind:       "SandboxWarmPool",
		Name:       "r6-pool",
		UID:        "r6-pool-uid",
		Controller: new(true),
	}
}

// postAdoptionFixture models the state the sandbox controller observes right
// after a stock (spec-rewriting) warm-pool adoption: the sandbox is
// claim-owned and its template no longer carries the safe-to-evict marker,
// while the live pod still does (plus the warm-pool label). The pod metadata
// reconciliation must strip both and update the tracking annotations.
func postAdoptionFixture() (*sandboxv1beta1.Sandbox, *corev1.Pod) {
	hash := NameHash(r6Sandbox)
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       r6Sandbox,
			Namespace:  r6Namespace,
			UID:        sandboxUID,
			Generation: 3,
			Annotations: map[string]string{
				sandboxv1beta1.SandboxPodNameAnnotation: r6Sandbox,
			},
			OwnerReferences: []metav1.OwnerReference{claimOwnerRef()},
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
					// Post-adoption template: safe-to-evict deleted by the
					// claim controller's spec rewrite.
					ObjectMeta: sandboxv1beta1.PodMetadata{},
				},
			},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r6Sandbox,
			Namespace: r6Namespace,
			Labels: map[string]string{
				sandboxLabel:                        hash,
				sandboxv1beta1.SandboxWarmPoolLabel: NameHash("r6-pool"),
			},
			Annotations: map[string]string{
				autoscalerSafeToEvictAnnotation:                        "true",
				sandboxv1beta1.SandboxPropagatedAnnotationsAnnotation: autoscalerSafeToEvictAnnotation,
			},
			OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(r6Sandbox)},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "c"}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIPs:     []corev1.PodIP{{IP: "10.0.0.9"}},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	return sb, pod
}

type r6Counters struct {
	podPatches         int
	sandboxPatches     int
	subResourcePatches int
}

func (w *r6Counters) interceptors() interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			switch obj.(type) {
			case *corev1.Pod:
				w.podPatches++
			case *sandboxv1beta1.Sandbox:
				w.sandboxPatches++
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			w.subResourcePatches++
			return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
	}
}

func newR6Client(counters *r6Counters, objs ...runtime.Object) client.WithWatch {
	return fake.NewClientBuilder().
		WithScheme(Scheme).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}).
		WithIndex(&corev1.Pod{}, podSandboxNameHashIndex, podSandboxNameHashIndexer).
		WithInterceptorFuncs(counters.interceptors()).
		WithRuntimeObjects(objs...).
		Build()
}

func newR6Reconciler(t *testing.T, cl client.Client, window time.Duration, noSpecAdoption bool) *SandboxReconciler {
	t.Helper()
	r := &SandboxReconciler{
		Client:         cl,
		Scheme:         Scheme,
		Tracer:         asmetrics.NewNoOp(),
		ClusterDomain:  "cluster.local",
		NoSpecAdoption: noSpecAdoption,
	}
	if window > 0 {
		f, err := writebehind.New(cl, Scheme, writebehind.Options{Window: window})
		if err != nil {
			t.Fatalf("writebehind.New: %v", err)
		}
		r.WriteBehind = f
	}
	return r
}

func r6Reconcile(t *testing.T, r *SandboxReconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: r6Sandbox, Namespace: r6Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func getR6Pod(t *testing.T, cl client.Client) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: r6Sandbox, Namespace: r6Namespace}, pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	return pod
}

// TestWriteBehindCoalescesAdoptionPodPatch: with write-behind enabled, the
// adoption-path pod metadata reconciliation issues ZERO pod patches during
// the reconcile; N reconciles' worth of pending mutations flush as exactly
// ONE patch; and the flushed pod state is identical to what the synchronous
// (flag-off) path produces.
func TestWriteBehindCoalescesAdoptionPodPatch(t *testing.T) {
	// Reference run: synchronous mode (WriteBehind nil = flag off).
	syncCounters := &r6Counters{}
	sbRef, podRef := postAdoptionFixture()
	syncClient := newR6Client(syncCounters, sbRef, podRef)
	syncR := newR6Reconciler(t, syncClient, 0, false)
	r6Reconcile(t, syncR)
	if syncCounters.podPatches != 1 {
		t.Fatalf("synchronous mode: %d pod patches during reconcile, want 1 (flag-off identity)", syncCounters.podPatches)
	}
	wantPod := getR6Pod(t, syncClient)

	// Write-behind run.
	counters := &r6Counters{}
	sb, pod := postAdoptionFixture()
	cl := newR6Client(counters, sb, pod)
	r := newR6Reconciler(t, cl, time.Hour, false)

	r6Reconcile(t, r)
	if counters.podPatches != 0 {
		t.Fatalf("write-behind mode: %d pod patches during reconcile, want 0 (deferred)", counters.podPatches)
	}
	if got := getR6Pod(t, cl); got.Annotations[autoscalerSafeToEvictAnnotation] != "true" {
		t.Fatal("pod mutated on the server before flush")
	}

	// A second reconcile (level-triggered redelivery) recomputes the same
	// drift and coalesces into the same pending entry — still zero patches.
	r6Reconcile(t, r)
	if counters.podPatches != 0 {
		t.Fatalf("second reconcile issued %d pod patches, want 0 (coalesced)", counters.podPatches)
	}
	if pending := r.WriteBehind.Pending(); pending != 1 {
		t.Fatalf("pending entries = %d, want 1 (per-object coalescing)", pending)
	}

	if err := r.WriteBehind.FlushAll(context.Background()); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	if counters.podPatches != 1 {
		t.Fatalf("after flush: %d pod patches, want exactly 1 for 2 reconciles' mutations", counters.podPatches)
	}

	got := getR6Pod(t, cl)
	if !maps.Equal(got.Labels, wantPod.Labels) {
		t.Errorf("flushed labels diverge from synchronous mode:\n got %v\nwant %v", got.Labels, wantPod.Labels)
	}
	if !maps.Equal(got.Annotations, wantPod.Annotations) {
		t.Errorf("flushed annotations diverge from synchronous mode:\n got %v\nwant %v", got.Annotations, wantPod.Annotations)
	}
	if _, ok := got.Annotations[autoscalerSafeToEvictAnnotation]; ok {
		t.Error("safe-to-evict marker survived the flush")
	}
	if _, ok := got.Labels[sandboxv1beta1.SandboxWarmPoolLabel]; ok {
		t.Error("warm-pool label survived the flush")
	}
}

// TestWriteBehindCrashRecovery: pending mutations lost with the process are
// recomputed by a FRESH reconciler (new process, empty coalescer) from
// informer state alone — the defining safety property of write-behind here.
func TestWriteBehindCrashRecovery(t *testing.T) {
	counters := &r6Counters{}
	sb, pod := postAdoptionFixture()
	cl := newR6Client(counters, sb, pod)

	// Process 1 reconciles but "crashes" before its coalescer flushes.
	r1 := newR6Reconciler(t, cl, time.Hour, false)
	r6Reconcile(t, r1)
	if counters.podPatches != 0 {
		t.Fatalf("pre-crash: %d pod patches, want 0", counters.podPatches)
	}
	r1.WriteBehind = nil // drop the flusher with its pending entry: simulated crash

	// Process 2 (fresh, synchronous for determinism) re-reconciles the same
	// object and must re-detect and re-issue the exact mutation.
	r2 := newR6Reconciler(t, cl, 0, false)
	r6Reconcile(t, r2)
	if counters.podPatches != 1 {
		t.Fatalf("post-crash reconcile issued %d pod patches, want 1", counters.podPatches)
	}
	got := getR6Pod(t, cl)
	if _, ok := got.Annotations[autoscalerSafeToEvictAnnotation]; ok {
		t.Error("safe-to-evict marker not stripped by the recovery reconcile")
	}
	if _, ok := got.Labels[sandboxv1beta1.SandboxWarmPoolLabel]; ok {
		t.Error("warm-pool label not stripped by the recovery reconcile")
	}
}

// TestWriteBehindPodNameAnnotation: the sandbox pod-name annotation is
// write-behind-eligible with the full window; it lands after flush and the
// current pass proceeds on the in-memory copy.
func TestWriteBehindPodNameAnnotation(t *testing.T) {
	counters := &r6Counters{}
	sb, pod := postAdoptionFixture()
	sb.Annotations = nil // cold-style: annotation not yet recorded
	// Make the pod fully converged so the ONLY write left is the annotation.
	pod.Labels = map[string]string{sandboxLabel: NameHash(r6Sandbox)}
	pod.Annotations = nil
	sb.OwnerReferences = nil // standalone sandbox
	cl := newR6Client(counters, sb, pod)
	r := newR6Reconciler(t, cl, time.Hour, false)

	r6Reconcile(t, r)
	if counters.sandboxPatches != 0 {
		t.Fatalf("sandbox (non-status) patches during reconcile: %d, want 0 (deferred)", counters.sandboxPatches)
	}
	if counters.podPatches != 0 {
		t.Fatalf("pod patches during reconcile: %d, want 0 (pod converged)", counters.podPatches)
	}
	if err := r.WriteBehind.FlushAll(context.Background()); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	if counters.sandboxPatches != 1 {
		t.Fatalf("sandbox patches after flush: %d, want 1", counters.sandboxPatches)
	}
	gotSb := &sandboxv1beta1.Sandbox{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: r6Sandbox, Namespace: r6Namespace}, gotSb); err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if gotSb.Annotations[sandboxv1beta1.SandboxPodNameAnnotation] != r6Sandbox {
		t.Errorf("pod-name annotation not flushed: %v", gotSb.Annotations)
	}
}

// noSpecFixture models a METADATA-ONLY adoption (the no-spec-adoption
// protocol): ownership flipped to the claim, top-level labels updated, but
// spec.podTemplate untouched — it still carries safe-to-evict="true".
func noSpecFixture() (*sandboxv1beta1.Sandbox, *corev1.Pod) {
	sb, pod := postAdoptionFixture()
	sb.Spec.PodTemplate.ObjectMeta = sandboxv1beta1.PodMetadata{
		Annotations: map[string]string{autoscalerSafeToEvictAnnotation: "true"},
	}
	return sb, pod
}

// TestNoSpecAdoptionStripsSafeToEvictFromExistingPod: with --no-spec-adoption
// the strip is derived from claim OWNERSHIP, so it fires even though the
// template still carries the marker; with the flag off the template wins
// (stock behavior) and the marker stays.
func TestNoSpecAdoptionStripsSafeToEvictFromExistingPod(t *testing.T) {
	// Flag OFF: template still says "true" → pod keeps the marker.
	offCounters := &r6Counters{}
	sbOff, podOff := noSpecFixture()
	clOff := newR6Client(offCounters, sbOff, podOff)
	r6Reconcile(t, newR6Reconciler(t, clOff, 0, false))
	if got := getR6Pod(t, clOff); got.Annotations[autoscalerSafeToEvictAnnotation] != "true" {
		t.Fatalf("flag off: marker should be kept (template-driven), got %v", got.Annotations)
	}

	// Flag ON: ownership-derived strip.
	onCounters := &r6Counters{}
	sbOn, podOn := noSpecFixture()
	clOn := newR6Client(onCounters, sbOn, podOn)
	r6Reconcile(t, newR6Reconciler(t, clOn, 0, true))
	got := getR6Pod(t, clOn)
	if _, ok := got.Annotations[autoscalerSafeToEvictAnnotation]; ok {
		t.Errorf("flag on: marker not stripped from claim-owned pod: %v", got.Annotations)
	}
	if got.Annotations[sandboxv1beta1.SandboxPropagatedAnnotationsAnnotation] != "" {
		t.Errorf("propagated-annotations tracking not cleared: %q",
			got.Annotations[sandboxv1beta1.SandboxPropagatedAnnotationsAnnotation])
	}

	// Idempotence: a second reconcile must be write-free.
	before := onCounters.podPatches
	r6Reconcile(t, newR6Reconciler(t, clOn, 0, true))
	if onCounters.podPatches != before {
		t.Errorf("second flag-on reconcile issued %d extra pod patches, want 0", onCounters.podPatches-before)
	}
}

// TestNoSpecAdoptionPoolOwnedUnaffected: suppression requires claim
// ownership — pool-owned (idle warm) pods must keep their eviction marker so
// the autoscaler can still scale down idle capacity.
func TestNoSpecAdoptionPoolOwnedUnaffected(t *testing.T) {
	counters := &r6Counters{}
	sb, pod := noSpecFixture()
	sb.OwnerReferences = []metav1.OwnerReference{poolOwnerRef()}
	sb.Labels = map[string]string{sandboxv1beta1.SandboxWarmPoolLabel: NameHash("r6-pool")}
	cl := newR6Client(counters, sb, pod)
	r6Reconcile(t, newR6Reconciler(t, cl, 0, true))
	if got := getR6Pod(t, cl); got.Annotations[autoscalerSafeToEvictAnnotation] != "true" {
		t.Errorf("pool-owned pod lost its eviction marker under --no-spec-adoption: %v", got.Annotations)
	}
}

// TestNoSpecAdoptionNewPodCreation: a claim-owned sandbox whose template
// carries safe-to-evict="true" creates its pod WITHOUT the marker when the
// flag is on (documented divergence: stock keeps it on cold starts).
func TestNoSpecAdoptionNewPodCreation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		flag       bool
		wantMarker bool
	}{
		{name: "flag off keeps template marker", flag: false, wantMarker: true},
		{name: "flag on suppresses marker", flag: true, wantMarker: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counters := &r6Counters{}
			sb, _ := noSpecFixture()
			sb.Annotations = nil // let the controller create the pod fresh
			cl := newR6Client(counters, sb)
			r6Reconcile(t, newR6Reconciler(t, cl, 0, tc.flag))
			got := getR6Pod(t, cl)
			_, hasMarker := got.Annotations[autoscalerSafeToEvictAnnotation]
			if hasMarker != tc.wantMarker {
				t.Errorf("marker present=%v, want %v (annotations: %v)", hasMarker, tc.wantMarker, got.Annotations)
			}
		})
	}
}

// TestNoSpecAdoptionMetadataOnlyAdoptionWriteCount is the write-elimination
// proof for the generation-bump analysis: starting from a CONVERGED
// pool-owned sandbox, a metadata-only ownership flip (what a no-spec claim
// adoption writes) triggers exactly ONE pod patch and ZERO sandbox status
// patches — because metadata changes don't bump metadata.generation, the
// conditions' observedGeneration stays valid and updateStatus short-circuits.
func TestNoSpecAdoptionMetadataOnlyAdoptionWriteCount(t *testing.T) {
	counters := &r6Counters{}
	sb, pod := noSpecFixture()
	// Start as converged pool inventory.
	sb.OwnerReferences = []metav1.OwnerReference{poolOwnerRef()}
	sb.Labels = map[string]string{sandboxv1beta1.SandboxWarmPoolLabel: NameHash("r6-pool")}
	cl := newR6Client(counters, sb, pod)
	r := newR6Reconciler(t, cl, 0, true)

	// Pass 1: converge (writes status once, settles pod tracking metadata).
	r6Reconcile(t, r)
	r6Reconcile(t, r)
	statusWrites, podWrites := counters.subResourcePatches, counters.podPatches
	if statusWrites != 1 {
		t.Fatalf("converged baseline wrote status %d times, want 1", statusWrites)
	}

	// Metadata-only adoption: ownership + top-level labels flip, spec (and
	// therefore generation) untouched — exactly what the claim controller
	// writes under the no-spec-adoption protocol.
	current := &sandboxv1beta1.Sandbox{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: r6Sandbox, Namespace: r6Namespace}, current); err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	genBefore := current.Generation
	current.OwnerReferences = []metav1.OwnerReference{claimOwnerRef()}
	delete(current.Labels, sandboxv1beta1.SandboxWarmPoolLabel)
	current.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = "warm"
	if err := cl.Update(context.Background(), current); err != nil {
		t.Fatalf("metadata-only adoption update: %v", err)
	}
	check := &sandboxv1beta1.Sandbox{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: r6Sandbox, Namespace: r6Namespace}, check); err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if check.Generation != genBefore {
		t.Fatalf("metadata-only update bumped generation %d->%d; fixture invalid", genBefore, check.Generation)
	}

	// Pass 2: the post-adoption reconcile.
	r6Reconcile(t, r)
	if got := counters.podPatches - podWrites; got != 1 {
		t.Errorf("post-adoption reconcile issued %d pod patches, want 1 (strip + warm-pool label prune)", got)
	}
	if got := counters.subResourcePatches - statusWrites; got != 0 {
		t.Errorf("post-adoption reconcile issued %d status patches, want 0 (no generation bump, no observedGeneration refresh)", got)
	}
	got := getR6Pod(t, cl)
	if _, ok := got.Annotations[autoscalerSafeToEvictAnnotation]; ok {
		t.Error("safe-to-evict marker survived metadata-only adoption")
	}
	if _, ok := got.Labels[sandboxv1beta1.SandboxWarmPoolLabel]; ok {
		t.Error("warm-pool label survived metadata-only adoption")
	}
}
