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

// Write-behind coalescing tests: opt-in coalescing of the sandbox
// controller's recoverable metadata-only writes (--sandbox-write-behind-window),
// including the flag-off (window=0) synchronous identity, per-object patch
// coalescing, and level-based crash recovery.

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
	wbNamespace = "wb-ns"
	wbSandbox   = "wb-sandbox"
	// autoscalerSafeToEvictAnnotation marks a pod as evictable by the cluster
	// autoscaler. The warm pool stamps it "true" on pool-owned pods so idle
	// warm capacity can be scaled down; once a pod backs a claimed sandbox it
	// must NOT carry the marker (an eviction would kill an in-use sandbox).
	autoscalerSafeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"
)

func claimOwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "extensions.agents.x-k8s.io/v1beta1",
		Kind:       "SandboxClaim",
		Name:       "wb-claim",
		UID:        "wb-claim-uid",
		Controller: new(true),
	}
}

// postAdoptionFixture models the state the sandbox controller observes right
// after a warm-pool adoption: the sandbox is claim-owned and its template no
// longer carries the safe-to-evict marker (deleted by the claim controller's
// spec rewrite), while the live pod still does (plus the warm-pool label).
// The pod metadata reconciliation must strip both and update the tracking
// annotations.
func postAdoptionFixture() (*sandboxv1beta1.Sandbox, *corev1.Pod) {
	hash := NameHash(wbSandbox)
	sb := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:       wbSandbox,
			Namespace:  wbNamespace,
			UID:        sandboxUID,
			Generation: 3,
			Annotations: map[string]string{
				sandboxv1beta1.SandboxPodNameAnnotation: wbSandbox,
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
			Name:      wbSandbox,
			Namespace: wbNamespace,
			Labels: map[string]string{
				sandboxLabel:                        hash,
				sandboxv1beta1.SandboxWarmPoolLabel: NameHash("wb-pool"),
			},
			Annotations: map[string]string{
				autoscalerSafeToEvictAnnotation:                       "true",
				sandboxv1beta1.SandboxPropagatedAnnotationsAnnotation: autoscalerSafeToEvictAnnotation,
			},
			OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(wbSandbox)},
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

type wbCounters struct {
	podPatches         int
	sandboxPatches     int
	subResourcePatches int
}

func (w *wbCounters) interceptors() interceptor.Funcs {
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

func newWbClient(counters *wbCounters, objs ...runtime.Object) client.WithWatch {
	return fake.NewClientBuilder().
		WithScheme(Scheme).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}).
		WithIndex(&corev1.Pod{}, podSandboxNameHashIndex, podSandboxNameHashIndexer).
		WithInterceptorFuncs(counters.interceptors()).
		WithRuntimeObjects(objs...).
		Build()
}

// newWbReconciler builds a SandboxReconciler; window == 0 models the default
// flag-off configuration (no Flusher constructed, WriteBehind nil), exactly
// as cmd/agent-sandbox-controller wires it.
func newWbReconciler(t *testing.T, cl client.Client, window time.Duration) *SandboxReconciler {
	t.Helper()
	r := &SandboxReconciler{
		Client:        cl,
		Scheme:        Scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
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

func wbReconcile(t *testing.T, r *SandboxReconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: wbSandbox, Namespace: wbNamespace},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func getWbPod(t *testing.T, cl client.Client) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: wbSandbox, Namespace: wbNamespace}, pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	return pod
}

// TestWriteBehindDisabledIsSynchronous pins the flag-off identity: with
// window=0 no Flusher exists (WriteBehind nil) and the reconcile runs the
// stock synchronous path — the pod metadata patch is issued inline, exactly
// once, during the reconcile, and the pod converges before Reconcile returns.
func TestWriteBehindDisabledIsSynchronous(t *testing.T) {
	counters := &wbCounters{}
	sb, pod := postAdoptionFixture()
	cl := newWbClient(counters, sb, pod)
	r := newWbReconciler(t, cl, 0)
	if r.WriteBehind != nil {
		t.Fatal("window=0 must not construct a Flusher")
	}

	wbReconcile(t, r)
	if counters.podPatches != 1 {
		t.Fatalf("synchronous mode: %d pod patches during reconcile, want exactly 1", counters.podPatches)
	}
	got := getWbPod(t, cl)
	if _, ok := got.Annotations[autoscalerSafeToEvictAnnotation]; ok {
		t.Error("safe-to-evict marker not stripped synchronously during reconcile")
	}
	if _, ok := got.Labels[sandboxv1beta1.SandboxWarmPoolLabel]; ok {
		t.Error("warm-pool label not stripped synchronously during reconcile")
	}

	// Idempotence: a second reconcile is pod-write-free.
	wbReconcile(t, r)
	if counters.podPatches != 1 {
		t.Fatalf("second synchronous reconcile issued %d extra pod patches, want 0", counters.podPatches-1)
	}
}

// TestWriteBehindCoalescesAdoptionPodPatch: with write-behind enabled, the
// adoption-path pod metadata reconciliation issues ZERO pod patches during
// the reconcile; N reconciles' worth of pending mutations flush as exactly
// ONE patch; and the flushed pod state is identical to what the synchronous
// (flag-off) path produces.
func TestWriteBehindCoalescesAdoptionPodPatch(t *testing.T) {
	// Reference run: synchronous mode (WriteBehind nil = flag off).
	syncCounters := &wbCounters{}
	sbRef, podRef := postAdoptionFixture()
	syncClient := newWbClient(syncCounters, sbRef, podRef)
	syncR := newWbReconciler(t, syncClient, 0)
	wbReconcile(t, syncR)
	if syncCounters.podPatches != 1 {
		t.Fatalf("synchronous mode: %d pod patches during reconcile, want 1 (flag-off identity)", syncCounters.podPatches)
	}
	wantPod := getWbPod(t, syncClient)

	// Write-behind run.
	counters := &wbCounters{}
	sb, pod := postAdoptionFixture()
	cl := newWbClient(counters, sb, pod)
	r := newWbReconciler(t, cl, time.Hour)

	wbReconcile(t, r)
	if counters.podPatches != 0 {
		t.Fatalf("write-behind mode: %d pod patches during reconcile, want 0 (deferred)", counters.podPatches)
	}
	if got := getWbPod(t, cl); got.Annotations[autoscalerSafeToEvictAnnotation] != "true" {
		t.Fatal("pod mutated on the server before flush")
	}

	// A second reconcile (level-triggered redelivery) recomputes the same
	// drift and coalesces into the same pending entry — still zero patches.
	wbReconcile(t, r)
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

	got := getWbPod(t, cl)
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
	counters := &wbCounters{}
	sb, pod := postAdoptionFixture()
	cl := newWbClient(counters, sb, pod)

	// Process 1 reconciles but "crashes" before its coalescer flushes.
	r1 := newWbReconciler(t, cl, time.Hour)
	wbReconcile(t, r1)
	if counters.podPatches != 0 {
		t.Fatalf("pre-crash: %d pod patches, want 0", counters.podPatches)
	}
	r1.WriteBehind = nil // drop the flusher with its pending entry: simulated crash

	// Process 2 (fresh, synchronous for determinism) re-reconciles the same
	// object and must re-detect and re-issue the exact mutation.
	r2 := newWbReconciler(t, cl, 0)
	wbReconcile(t, r2)
	if counters.podPatches != 1 {
		t.Fatalf("post-crash reconcile issued %d pod patches, want 1", counters.podPatches)
	}
	got := getWbPod(t, cl)
	if _, ok := got.Annotations[autoscalerSafeToEvictAnnotation]; ok {
		t.Error("safe-to-evict marker not stripped by the recovery reconcile")
	}
	if _, ok := got.Labels[sandboxv1beta1.SandboxWarmPoolLabel]; ok {
		t.Error("warm-pool label not stripped by the recovery reconcile")
	}
}
