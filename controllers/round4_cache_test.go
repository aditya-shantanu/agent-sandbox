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
	"strings"
	"testing"

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
)

func fullPodFixture() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: "ns",
			Labels:    map[string]string{sandboxLabel: "hash-1"},
			Annotations: map[string]string{
				"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager: "kubelet", Operation: metav1.ManagedFieldsOperationApply,
			}},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-7",
			Containers: []corev1.Container{{
				Name: "c", Image: "debian:latest",
				Env: []corev1.EnvVar{{Name: "A", Value: "B"}},
			}},
			InitContainers: []corev1.Container{{Name: "init", Image: "busybox"}},
			Volumes:        []corev1.Volume{{Name: "v"}},
			Tolerations:    []corev1.Toleration{{Key: "k"}},
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIP:  "10.0.0.1",
			PodIPs: []corev1.PodIP{{IP: "10.0.0.1"}},
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
			}},
		},
	}
}

// TestPodCacheTransform verifies exactly what the informer transform keeps
// and drops: everything the controllers read survives (metadata, status,
// spec.nodeName); managedFields and the bulky spec payload do not.
func TestPodCacheTransform(t *testing.T) {
	out, err := PodCacheTransform(fullPodFixture())
	if err != nil {
		t.Fatalf("PodCacheTransform error: %v", err)
	}
	pod, ok := out.(*corev1.Pod)
	if !ok {
		t.Fatalf("transform returned %T, want *corev1.Pod", out)
	}

	// Dropped.
	if pod.ManagedFields != nil {
		t.Error("managedFields not stripped")
	}
	if len(pod.Spec.Containers) != 0 || len(pod.Spec.InitContainers) != 0 ||
		len(pod.Spec.Volumes) != 0 || len(pod.Spec.Tolerations) != 0 {
		t.Errorf("pod spec not stripped: %+v", pod.Spec)
	}

	// Kept: the fields the controllers actually read.
	if pod.Spec.NodeName != "node-7" {
		t.Errorf("spec.nodeName lost: %q", pod.Spec.NodeName)
	}
	if pod.Labels[sandboxLabel] != "hash-1" {
		t.Error("labels lost")
	}
	if pod.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"] != "true" {
		t.Error("annotations lost")
	}
	if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP != "10.0.0.1" || len(pod.Status.Conditions) != 1 {
		t.Errorf("status lost: %+v", pod.Status)
	}

	// Non-pod objects pass through untouched.
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "s"}}
	got, err := PodCacheTransform(svc)
	if err != nil {
		t.Fatalf("transform of non-pod errored: %v", err)
	}
	if got != any(svc) {
		t.Error("non-pod object was not passed through unchanged")
	}
}

// TestPodCacheTransformMergePatchUnaffected proves the R4.2 safety claim:
// merge patches computed against a transform-stripped cache pod are
// byte-identical to those computed against the full pod, because the patch is
// a diff of OUR OWN metadata mutations against a DeepCopy of the same cached
// base — stripped fields appear on neither side, so they can neither leak
// into nor be deleted by the patch.
func TestPodCacheTransformMergePatchUnaffected(t *testing.T) {
	// Adoption-style mutation: add claim-uid label, drop the safe-to-evict
	// annotation (exactly what updatePodMetadata does on adoption).
	mutate := func(p *corev1.Pod) {
		p.Labels["agents.x-k8s.io/claim-uid"] = "uid-1"
		delete(p.Annotations, "cluster-autoscaler.kubernetes.io/safe-to-evict")
	}

	// Patch from the FULL pod (pre-R4.2 behavior).
	full := fullPodFixture()
	fullBase := client.MergeFrom(full.DeepCopy())
	mutate(full)
	fullData, err := fullBase.Data(full)
	if err != nil {
		t.Fatalf("full-pod patch data: %v", err)
	}

	// Patch from the TRANSFORMED pod (post-R4.2 cache behavior).
	strippedAny, err := PodCacheTransform(fullPodFixture())
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	stripped := strippedAny.(*corev1.Pod)
	strippedBase := client.MergeFrom(stripped.DeepCopy())
	mutate(stripped)
	strippedData, err := strippedBase.Data(stripped)
	if err != nil {
		t.Fatalf("stripped-pod patch data: %v", err)
	}

	if string(fullData) != string(strippedData) {
		t.Errorf("merge patch changed by cache transform:\n full:     %s\n stripped: %s", fullData, strippedData)
	}
	if strings.Contains(string(strippedData), `"spec"`) {
		t.Errorf("metadata-only mutation leaked a spec key into the patch: %s", strippedData)
	}
	if strings.Contains(string(strippedData), "managedFields") {
		t.Errorf("patch touches managedFields: %s", strippedData)
	}
}

// TestPodNameAnnotationRawPayload pins the exact bytes of the sandbox
// pod-name annotation patch after the round-4 raw-patch rewrite, via a real
// reconcilePod pass against an owned pod.
func TestPodNameAnnotationRawPayload(t *testing.T) {
	sandboxName := "sandbox-name"
	sandboxNs := "sandbox-ns"
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxName,
			Namespace: sandboxNs,
			UID:       sandboxUID,
		},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{PodTemplate: sandboxv1beta1.PodTemplate{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
			}},
			OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
		},
	}
	nameHash := NameHash(sandboxName)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sandboxName,
			Namespace:       sandboxNs,
			ResourceVersion: "1",
			Labels:          map[string]string{sandboxLabel: nameHash},
			OwnerReferences: []metav1.OwnerReference{sandboxControllerRef(sandboxName)},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
	}

	var sandboxPatches [][]byte
	fakeClient := fake.NewClientBuilder().
		WithScheme(Scheme).
		WithStatusSubresource(&sandboxv1beta1.Sandbox{}).
		WithIndex(&corev1.Pod{}, podSandboxNameHashIndex, podSandboxNameHashIndexer).
		WithRuntimeObjects([]runtime.Object{sandbox, pod}...).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok {
					data, err := patch.Data(obj)
					if err != nil {
						return err
					}
					if patch.Type() != types.MergePatchType {
						t.Errorf("sandbox patch type = %v, want merge", patch.Type())
					}
					sandboxPatches = append(sandboxPatches, data)
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := SandboxReconciler{
		Client:        fakeClient,
		Scheme:        Scheme,
		Tracer:        asmetrics.NewNoOp(),
		ClusterDomain: "cluster.local",
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: sandboxName, Namespace: sandboxNs},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	want := `{"metadata":{"annotations":{"` + sandboxv1beta1.SandboxPodNameAnnotation + `":"` + sandboxName + `"}}}`
	found := false
	for _, p := range sandboxPatches {
		if string(p) == want {
			found = true
		}
	}
	if !found {
		t.Errorf("no sandbox patch matched the expected byte-exact pod-name annotation payload.\nwant: %s\ngot:  %s",
			want, sandboxPatches)
	}

	// And it must be persisted.
	got := &sandboxv1beta1.Sandbox{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: sandboxName, Namespace: sandboxNs}, got); err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.Annotations[sandboxv1beta1.SandboxPodNameAnnotation] != sandboxName {
		t.Errorf("pod-name annotation not persisted: %v", got.Annotations)
	}
}
