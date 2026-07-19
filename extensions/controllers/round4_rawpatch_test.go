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

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

// TestPersistStampedAnnotationsRawPayload pins the exact bytes the round-4
// raw-patch rewrite of persistStampedAnnotations puts on the wire, and proves
// they are identical to what the historical DeepCopy+MergeFrom pattern
// computed for the same mutation.
func TestPersistStampedAnnotationsRawPayload(t *testing.T) {
	scheme := newScheme(t)
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid-123",
			Annotations: map[string]string{
				"pre-existing/anno": "kept",
			},
		},
	}

	var captured []byte
	var capturedType types.PatchType
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(claim.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				data, err := patch.Data(obj)
				if err != nil {
					return err
				}
				captured = data
				capturedType = patch.Type()
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &SandboxClaimReconciler{Client: fakeClient, Scheme: scheme}
	stamped := map[string]string{
		asmetrics.ObservabilityAnnotation: "2026-07-19T15:27:56.85Z",
		"b-second/key":                    "v2",
	}
	live := claim.DeepCopy()
	if err := r.persistStampedAnnotations(context.Background(), live, stamped); err != nil {
		t.Fatalf("persistStampedAnnotations failed: %v", err)
	}

	if capturedType != types.MergePatchType {
		t.Fatalf("patch type = %v, want %v", capturedType, types.MergePatchType)
	}

	// Byte-exact expectation: only the stamped keys, in sorted key order
	// ("agents.x-k8s.io/..." sorts before "b-second/key"), nothing else.
	want := `{"metadata":{"annotations":{"` + asmetrics.ObservabilityAnnotation +
		`":"2026-07-19T15:27:56.85Z","b-second/key":"v2"}}}`
	if string(captured) != want {
		t.Errorf("payload mismatch:\n got: %s\nwant: %s", captured, want)
	}

	// Equivalence with the legacy pattern (DeepCopy base, delete stamped
	// keys, restore, MergeFrom): identical bytes on the wire.
	legacyClaim := claim.DeepCopy()
	restoreStampedAnnotations(legacyClaim, stamped)
	base := legacyClaim.DeepCopy()
	for key := range stamped {
		delete(base.Annotations, key)
	}
	legacyData, err := client.MergeFrom(base).Data(legacyClaim)
	if err != nil {
		t.Fatalf("legacy MergeFrom Data() failed: %v", err)
	}
	if string(captured) != string(legacyData) {
		t.Errorf("raw payload differs from legacy MergeFrom payload:\n raw:    %s\n legacy: %s", captured, legacyData)
	}

	// The annotations must actually be persisted (and pre-existing ones kept).
	got := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-claim", Namespace: "default"}, got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	for k, v := range stamped {
		if got.Annotations[k] != v {
			t.Errorf("annotation %q = %q, want %q", k, got.Annotations[k], v)
		}
	}
	if got.Annotations["pre-existing/anno"] != "kept" {
		t.Errorf("pre-existing annotation lost: %v", got.Annotations)
	}
}

// TestInitializeSandboxLaunchTypeLabelRawPayload pins the exact single-label
// merge-patch payload and its equivalence with the legacy MergeFrom bytes.
func TestInitializeSandboxLaunchTypeLabelRawPayload(t *testing.T) {
	scheme := newScheme(t)
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-sb",
			Namespace: "default",
			UID:       "warm-sb-uid",
			Labels:    map[string]string{"existing": "label"},
		},
	}

	var captured []byte
	var capturedType types.PatchType
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sandbox.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				data, err := patch.Data(obj)
				if err != nil {
					return err
				}
				captured = data
				capturedType = patch.Type()
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &SandboxClaimReconciler{Client: fakeClient, Scheme: scheme}
	live := sandbox.DeepCopy()
	if err := r.initializeSandboxLaunchTypeLabel(context.Background(), live, sandboxv1beta1.SandboxLaunchTypeWarm); err != nil {
		t.Fatalf("initializeSandboxLaunchTypeLabel failed: %v", err)
	}

	if capturedType != types.MergePatchType {
		t.Fatalf("patch type = %v, want %v", capturedType, types.MergePatchType)
	}
	want := `{"metadata":{"labels":{"` + sandboxv1beta1.SandboxLaunchTypeLabel + `":"` + sandboxv1beta1.SandboxLaunchTypeWarm + `"}}}`
	if string(captured) != want {
		t.Errorf("payload mismatch:\n got: %s\nwant: %s", captured, want)
	}

	// Legacy equivalence.
	legacy := sandbox.DeepCopy()
	base := legacy.DeepCopy()
	legacy.Labels[sandboxv1beta1.SandboxLaunchTypeLabel] = sandboxv1beta1.SandboxLaunchTypeWarm
	legacyData, err := client.MergeFrom(base).Data(legacy)
	if err != nil {
		t.Fatalf("legacy MergeFrom Data() failed: %v", err)
	}
	if string(captured) != string(legacyData) {
		t.Errorf("raw payload differs from legacy MergeFrom payload:\n raw:    %s\n legacy: %s", captured, legacyData)
	}

	// Idempotence short-circuit: a sandbox that already has the label makes
	// no API call at all.
	captured = nil
	if err := r.initializeSandboxLaunchTypeLabel(context.Background(), live, sandboxv1beta1.SandboxLaunchTypeWarm); err != nil {
		t.Fatalf("second initializeSandboxLaunchTypeLabel failed: %v", err)
	}
	if captured != nil {
		t.Errorf("expected no patch when label already present, got %s", captured)
	}
}

// TestDisableObservabilityAnnotationsSkipsFlush verifies the
// --disable-claim-observability-annotations behavior: a warm adoption
// completes with ZERO non-status claim writes (the deferred annotation flush
// is skipped entirely), and no observability/assigned-sandbox annotations are
// persisted.
func TestDisableObservabilityAnnotationsSkipsFlush(t *testing.T) {
	scheme := newScheme(t)
	claim := &extensionsv1beta1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default", UID: "claim-uid-123"},
		Spec:       extensionsv1beta1.SandboxClaimSpec{WarmPoolRef: extensionsv1beta1.SandboxWarmPoolRef{Name: "test-pool"}},
	}
	template := &extensionsv1beta1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1beta1.SandboxTemplateSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
		}}},
	}
	warmPool := &extensionsv1beta1.SandboxWarmPool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool", Namespace: "default", UID: "warmpool-uid-123"},
		Spec:       extensionsv1beta1.SandboxWarmPoolSpec{TemplateRef: extensionsv1beta1.SandboxTemplateRef{Name: "test-template"}},
	}
	warmSandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warm-sb",
			Namespace: "default",
			UID:       "warm-sb-uid",
			Labels: map[string]string{
				warmPoolSandboxLabel:   sandboxcontrollers.NameHash("test-pool"),
				sandboxTemplateRefHash: sandboxcontrollers.NameHash("test-template"),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "extensions.agents.x-k8s.io/v1beta1",
				Kind:       "SandboxWarmPool",
				Name:       "test-pool",
				UID:        "warmpool-uid-123",
				Controller: ptr.To(true), // nolint:modernize
			}},
		},
		Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}}}}},
		Status: sandboxv1beta1.SandboxStatus{
			Conditions: []metav1.Condition{{
				Type: string(sandboxv1beta1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Ready",
			}},
		},
	}

	claimMetaPatches := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, warmPool, claim, warmSandbox).
		WithStatusSubresource(&extensionsv1beta1.SandboxClaim{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*extensionsv1beta1.SandboxClaim); ok {
					claimMetaPatches++
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	warmSandboxQueue := queue.NewSimpleSandboxQueue()
	warmSandboxQueue.Add(
		queue.GetNamespacedWarmPoolName("default", "test-pool"),
		queue.SandboxKey{Namespace: "default", Name: "warm-sb"},
	)

	reconciler := &SandboxClaimReconciler{
		Client:                          fakeClient,
		Scheme:                          scheme,
		Recorder:                        events.NewFakeRecorder(10),
		Tracer:                          asmetrics.NewNoOp(),
		WarmSandboxQueue:                warmSandboxQueue,
		DisableObservabilityAnnotations: true,
	}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-claim", Namespace: "default"}}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	if claimMetaPatches != 0 {
		t.Errorf("expected 0 non-status claim patches with the flag enabled, got %d", claimMetaPatches)
	}

	updatedClaim := &extensionsv1beta1.SandboxClaim{}
	if err := fakeClient.Get(context.Background(), req.NamespacedName, updatedClaim); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if v := updatedClaim.Annotations[asmetrics.ObservabilityAnnotation]; v != "" {
		t.Errorf("observability annotation should not be persisted, got %q", v)
	}
	if v := updatedClaim.Annotations[extensionsv1beta1.AssignedSandboxNameAnnotation]; v != "" {
		t.Errorf("assigned-sandbox annotation should not be persisted, got %q", v)
	}
	// The adoption itself must still have completed: status bound to warm-sb.
	if updatedClaim.Status.SandboxStatus.Name != "warm-sb" {
		t.Errorf("claim status not bound to warm sandbox: %+v", updatedClaim.Status.SandboxStatus)
	}
}
