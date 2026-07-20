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

// Claim-side no-spec adoption test net (ROUND6-COALESCING.md §3.3, the half
// completing the --no-spec-adoption protocol; the sandbox-controller half is
// pinned in controllers/round6_coalesce_test.go):
//
//  1. With NoSpecAdoption on and no claim additionalPodMetadata the adoption
//     patch is METADATA-ONLY: spec.podTemplate byte-identical before/after,
//     metadata.generation unchanged, ownership/labels/annotations fully
//     transferred (TestNoSpecAdoptionMetadataOnlyPatch).
//  2. Steady-state passes after a metadata-only adoption are write-free —
//     the drift check compares modulo system-reserved keys, so the legacy
//     identity-label injection is not re-applied through the back door
//     (TestNoSpecAdoptionSteadyStatePassWriteFree).
//  3. Genuine user-visible template drift still triggers the spec rewrite
//     under the flag (TestNoSpecAdoptionDriftCheckStillCatchesUserDrift).
//  4. Claims WITH additionalPodMetadata keep the legacy full spec rewrite
//     byte-for-byte even with the flag on — the KEP-0174 contract
//     (TestNoSpecAdoptionKeepsSpecRewriteForAdditionalPodMetadata).
//  5. The one-write async flush and the crash-window recovery reuse
//     applyAdoptionMutations, so both stay metadata-only under the flag
//     (TestNoSpecAdoptionOneWriteAsyncPatchIsMetadataOnly,
//     TestNoSpecAdoptionStatusFirstCrashRecoveryIsMetadataOnly).

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	sandboxcontrollers "sigs.k8s.io/agent-sandbox/controllers"
	extensionsv1beta1 "sigs.k8s.io/agent-sandbox/extensions/api/v1beta1"
	"sigs.k8s.io/agent-sandbox/extensions/controllers/queue"
)

// nsWarmSandbox is a pool-member shape faithful to buildSandboxCR: the pool
// stamps bookkeeping labels into BOTH top-level and pod-template metadata and
// the safe-to-evict marker into the template annotations. These are exactly
// the keys a metadata-only adoption leaves behind in the spec.
func nsWarmSandbox(name string) *sandboxv1beta1.Sandbox {
	sb := rrWarmSandbox(name)
	sb.Generation = 1
	sb.Labels[sandboxv1beta1.DeprecatedSandboxPodTemplateHashLabel] = "pod-template-hash"
	sb.Labels[sandboxv1beta1.SandboxTemplateHashLabel] = "blueprint-hash"
	sb.Spec.PodTemplate.ObjectMeta.Labels = map[string]string{
		warmPoolSandboxLabel:   sandboxcontrollers.NameHash(rrPoolName),
		sandboxTemplateRefHash: sandboxcontrollers.NameHash(rrTemplateName),
		sandboxv1beta1.DeprecatedSandboxPodTemplateHashLabel: "pod-template-hash",
		sandboxv1beta1.SandboxTemplateHashLabel:              "blueprint-hash",
	}
	sb.Spec.PodTemplate.ObjectMeta.Annotations = map[string]string{
		autoscalerSafeToEvictAnnotation: "true",
	}
	sb.Status.PodIPs = []string{"10.1.0.9"}
	return sb
}

// nsCounters tallies sandbox writes by kind so tests can pin exact ledgers.
type nsCounters struct {
	sandboxPatches       int
	sandboxStatusPatches int
}

func nsFakeClient(t *testing.T, counters *nsCounters, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objs...).
		// Sandbox registered as a status-subresource object so the fake
		// client models generation semantics: spec changes bump
		// metadata.generation, metadata-only changes do not.
		WithStatusSubresource(&extensionsv1beta1.SandboxClaim{}, &sandboxv1beta1.Sandbox{}).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxClaimUIDLabelIndex, sandboxClaimUIDLabelIndexer).
		WithIndex(&sandboxv1beta1.Sandbox{}, sandboxWarmPoolLabelIndex, sandboxWarmPoolLabelIndexer).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				err := c.Patch(ctx, obj, patch, opts...)
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok && err == nil && counters != nil {
					counters.sandboxPatches++
				}
				return err
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				err := c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
				if _, ok := obj.(*sandboxv1beta1.Sandbox); ok && subResourceName == "status" && err == nil && counters != nil {
					counters.sandboxStatusPatches++
				}
				return err
			},
		}).
		Build()
}

func nsReconciler(t *testing.T, c client.Client, warmKeys ...string) *SandboxClaimReconciler {
	t.Helper()
	q := queue.NewSimpleSandboxQueue()
	for _, name := range warmKeys {
		q.Add(queue.GetNamespacedWarmPoolName(rrNamespace, rrPoolName), queue.SandboxKey{Namespace: rrNamespace, Name: name})
	}
	r := rrReconciler(c, t, q)
	r.NoSpecAdoption = true
	return r
}

func nsReconcile(t *testing.T, r *SandboxClaimReconciler, claimName string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: rrNamespace}}); err != nil {
		t.Fatalf("reconcile %s: expected nil error, got: %v", claimName, err)
	}
}

func nsGetSandbox(t *testing.T, c client.Client, name string) *sandboxv1beta1.Sandbox {
	t.Helper()
	sb := &sandboxv1beta1.Sandbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: rrNamespace}, sb); err != nil {
		t.Fatalf("get sandbox %s: %v", name, err)
	}
	return sb
}

// assertMetadataAdoptionConverged checks the metadata half of the adoption
// transfer (the part BOTH modes must produce identically).
func assertMetadataAdoptionConverged(t *testing.T, sb *sandboxv1beta1.Sandbox, claim *extensionsv1beta1.SandboxClaim) {
	t.Helper()
	if ref := metav1.GetControllerOf(sb); ref == nil || ref.Kind != "SandboxClaim" || ref.UID != claim.UID {
		t.Errorf("sandbox %s: expected controller ref transferred to claim %s, got %+v", sb.Name, claim.Name, ref)
	}
	if _, ok := sb.Labels[warmPoolSandboxLabel]; ok {
		t.Errorf("sandbox %s: warm pool label not removed", sb.Name)
	}
	if got := sb.Labels[extensionsv1beta1.SandboxIDLabel]; got != string(claim.UID) {
		t.Errorf("sandbox %s: expected top-level claim-uid label %q, got %q", sb.Name, claim.UID, got)
	}
	if got := sb.Labels[sandboxv1beta1.SandboxLaunchTypeLabel]; got != sandboxv1beta1.SandboxLaunchTypeWarm {
		t.Errorf("sandbox %s: expected warm launch type label, got %q", sb.Name, got)
	}
	if got := sb.Labels[sandboxTemplateRefHash]; got != sandboxcontrollers.NameHash(rrTemplateName) {
		t.Errorf("sandbox %s: expected top-level template-ref-hash kept, got %q", sb.Name, got)
	}
	if got := sb.Annotations[sandboxv1beta1.SandboxPodNameAnnotation]; got != sb.Name {
		t.Errorf("sandbox %s: expected pod-name annotation %q, got %q", sb.Name, sb.Name, got)
	}
}

// TestNoSpecAdoptionMetadataOnlyPatch pins the headline invariant of the
// claim-side half: adopting a warm sandbox for a claim without
// additionalPodMetadata leaves spec.podTemplate BYTE-IDENTICAL and does not
// bump metadata.generation — so the sandbox controller's observedGeneration
// stays valid and the adoption produces zero sandbox status writes.
func TestNoSpecAdoptionMetadataOnlyPatch(t *testing.T) {
	claim := rrClaim("ns-claim")
	warm := nsWarmSandbox("ns-warm-1")
	specBefore := warm.Spec.DeepCopy()
	genBefore := warm.Generation
	counters := &nsCounters{}
	c := nsFakeClient(t, counters, rrTemplate(), rrWarmPool(), claim, warm)
	r := nsReconciler(t, c, warm.Name)

	nsReconcile(t, r, claim.Name)

	got := &extensionsv1beta1.SandboxClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}, got); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if got.Status.SandboxStatus.Name != warm.Name {
		t.Fatalf("expected claim bound to %s, got %q", warm.Name, got.Status.SandboxStatus.Name)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, string(sandboxv1beta1.SandboxConditionReady))
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True, got %+v", ready)
	}

	sb := nsGetSandbox(t, c, warm.Name)
	assertMetadataAdoptionConverged(t, sb, claim)
	if !equality.Semantic.DeepEqual(specBefore, &sb.Spec) {
		t.Errorf("metadata-only adoption modified the spec:\n before: %+v\n after:  %+v", specBefore.PodTemplate.ObjectMeta, sb.Spec.PodTemplate.ObjectMeta)
	}
	// NOTE: the fake client does not model generation semantics (see
	// controller-runtime fake doc.go), so the spec byte-identity check above
	// is the load-bearing assertion: a patch that carries no "spec" key
	// cannot bump metadata.generation on a real apiserver.
	if sb.Generation != genBefore {
		t.Errorf("metadata-only adoption bumped generation %d->%d", genBefore, sb.Generation)
	}
	// The pool's safe-to-evict marker deliberately survives in the SPEC; the
	// sandbox controller's ownership-derived hygiene keeps it off the Pod.
	if got := sb.Spec.PodTemplate.ObjectMeta.Annotations[autoscalerSafeToEvictAnnotation]; got != "true" {
		t.Errorf("expected template safe-to-evict annotation untouched (=true), got %q", got)
	}
	if counters.sandboxStatusPatches != 0 {
		t.Errorf("adoption issued %d sandbox status patches, want 0", counters.sandboxStatusPatches)
	}
}

// TestNoSpecAdoptionSteadyStatePassWriteFree: passes AFTER a metadata-only
// adoption must not re-add the legacy spec metadata (identity labels,
// stripped markers) through reconcileActive's drift check — that would
// reintroduce the generation bump through the back door on the very next
// event for the claim.
func TestNoSpecAdoptionSteadyStatePassWriteFree(t *testing.T) {
	claim := rrClaim("ns-steady-claim")
	warm := nsWarmSandbox("ns-warm-2")
	counters := &nsCounters{}
	c := nsFakeClient(t, counters, rrTemplate(), rrWarmPool(), claim, warm)
	r := nsReconciler(t, c, warm.Name)

	nsReconcile(t, r, claim.Name) // adoption pass
	adopted := nsGetSandbox(t, c, warm.Name)
	genAfterAdoption := adopted.Generation
	patchesAfterAdoption := counters.sandboxPatches

	// Steady-state passes: the sandbox is found (not adopted this pass), so
	// reconcileActive runs the full template/mergedMeta drift comparison.
	nsReconcile(t, r, claim.Name)
	nsReconcile(t, r, claim.Name)

	if counters.sandboxPatches != patchesAfterAdoption {
		t.Errorf("steady-state passes issued %d extra sandbox patches, want 0 (back-door spec rewrite)",
			counters.sandboxPatches-patchesAfterAdoption)
	}
	final := nsGetSandbox(t, c, warm.Name)
	if final.Generation != genAfterAdoption {
		t.Errorf("steady-state pass bumped generation %d->%d", genAfterAdoption, final.Generation)
	}
	if _, ok := final.Spec.PodTemplate.ObjectMeta.Labels[extensionsv1beta1.SandboxIDLabel]; ok {
		t.Error("steady-state pass re-injected the claim-uid label into the pod template")
	}
}

// TestNoSpecAdoptionDriftCheckStillCatchesUserDrift: the modulo-system-keys
// comparison must NOT swallow genuine user-visible template drift — a
// template change in a non-system key still forces the spec sync (and with
// it, legitimately, a generation bump).
func TestNoSpecAdoptionDriftCheckStillCatchesUserDrift(t *testing.T) {
	claim := rrClaim("ns-drift-claim")
	warm := nsWarmSandbox("ns-warm-3")
	template := rrTemplate()
	template.Spec.PodTemplate.ObjectMeta.Labels = map[string]string{"team": "search"}
	counters := &nsCounters{}
	c := nsFakeClient(t, counters, template, rrWarmPool(), claim, warm)
	r := nsReconciler(t, c, warm.Name)

	nsReconcile(t, r, claim.Name) // metadata-only adoption (spec untouched)
	nsReconcile(t, r, claim.Name) // steady state: user label drift detected

	final := nsGetSandbox(t, c, warm.Name)
	if got := final.Spec.PodTemplate.ObjectMeta.Labels["team"]; got != "search" {
		t.Errorf("expected user template label synced to the pod template, got %q", got)
	}
}

// TestNoSpecAdoptionKeepsSpecRewriteForAdditionalPodMetadata: the KEP-0174
// contract — a claim that carries additionalPodMetadata keeps the full
// legacy spec rewrite even with the flag on: merged metadata forced into the
// template (with identity labels), safe-to-evict stripped, generation bumped.
func TestNoSpecAdoptionKeepsSpecRewriteForAdditionalPodMetadata(t *testing.T) {
	claim := rrClaim("ns-kep174-claim")
	claim.Spec.AdditionalPodMetadata = sandboxv1beta1.PodMetadata{
		Labels: map[string]string{"sandbox.users.io/tenant": "blue"},
	}
	warm := nsWarmSandbox("ns-warm-4")
	specBefore := warm.Spec.DeepCopy()
	counters := &nsCounters{}
	c := nsFakeClient(t, counters, rrTemplate(), rrWarmPool(), claim, warm)
	r := nsReconciler(t, c, warm.Name)

	nsReconcile(t, r, claim.Name)

	sb := nsGetSandbox(t, c, warm.Name)
	assertMetadataAdoptionConverged(t, sb, claim)
	tplMeta := sb.Spec.PodTemplate.ObjectMeta
	if got := tplMeta.Labels["sandbox.users.io/tenant"]; got != "blue" {
		t.Errorf("expected additionalPodMetadata label in the pod template, got %q", got)
	}
	if got := tplMeta.Labels[extensionsv1beta1.SandboxIDLabel]; got != string(claim.UID) {
		t.Errorf("expected legacy claim-uid template label, got %q", got)
	}
	if val, ok := tplMeta.Annotations[autoscalerSafeToEvictAnnotation]; ok && val == "true" {
		t.Error("expected safe-to-evict stripped by the legacy spec rewrite")
	}
	// The spec must have been rewritten (the fake client does not model
	// generation semantics, so the spec diff is the observable here).
	if equality.Semantic.DeepEqual(specBefore, &sb.Spec) {
		t.Error("expected the KEP-0174 path to rewrite spec.podTemplate (legacy behavior)")
	}
}

// TestNoSpecAdoptionOneWriteAsyncPatchIsMetadataOnly: the one-write flusher
// defers the SAME mutation set applyAdoptionMutations builds, so under the
// flag the async sandbox patch must also be metadata-only.
func TestNoSpecAdoptionOneWriteAsyncPatchIsMetadataOnly(t *testing.T) {
	claim := rrClaim("ns-ow-claim")
	warm := nsWarmSandbox("ns-warm-5")
	specBefore := warm.Spec.DeepCopy()
	genBefore := warm.Generation
	counters := &nsCounters{}
	c := nsFakeClient(t, counters, rrTemplate(), rrWarmPool(), claim, warm)
	r := nsReconciler(t, c, warm.Name)
	r.OneWriteAdoption = true
	r.adoptionFlusher = newAdoptionFlusher(r)

	nsReconcile(t, r, claim.Name)
	if r.adoptionFlusher.pending() != 1 {
		t.Fatalf("expected one queued async patch, got %d", r.adoptionFlusher.pending())
	}
	if !r.adoptionFlusher.processNext(context.Background()) {
		t.Fatal("expected a queued flush request to process")
	}

	sb := nsGetSandbox(t, c, warm.Name)
	assertMetadataAdoptionConverged(t, sb, claim)
	if !equality.Semantic.DeepEqual(specBefore, &sb.Spec) {
		t.Errorf("async metadata-only adoption modified the spec:\n before: %+v\n after:  %+v", specBefore.PodTemplate.ObjectMeta, sb.Spec.PodTemplate.ObjectMeta)
	}
	if sb.Generation != genBefore {
		t.Errorf("async metadata-only adoption bumped generation %d->%d", genBefore, sb.Generation)
	}
}

// TestNoSpecAdoptionStatusFirstCrashRecoveryIsMetadataOnly: the crash window
// of one-write adoption (claim status committed, sandbox patch lost with the
// process) recovers through recoverStatusFirstBinding -> completeAdoption;
// under the flag that recovery patch stays metadata-only too, and the
// recovery decision itself reads only metadata (ownerRef + top-level
// labels), so it is mode-agnostic.
func TestNoSpecAdoptionStatusFirstCrashRecoveryIsMetadataOnly(t *testing.T) {
	claim := rrClaim("ns-crash-claim")
	warm := nsWarmSandbox("ns-warm-6")
	specBefore := warm.Spec.DeepCopy()
	genBefore := warm.Generation
	// Server state after the crash: claim status bound + Ready, sandbox
	// still fully pool-owned (the async patch never landed).
	claim.Status = extensionsv1beta1.SandboxClaimStatus{
		SandboxStatus: extensionsv1beta1.SandboxStatus{Name: warm.Name, PodIPs: []string{"10.1.0.9"}},
		Conditions: []metav1.Condition{{
			Type:               string(sandboxv1beta1.SandboxConditionReady),
			Status:             metav1.ConditionTrue,
			Reason:             "Ready",
			Message:            "Sandbox is ready",
			LastTransitionTime: metav1.Now(),
		}},
	}
	counters := &nsCounters{}
	c := nsFakeClient(t, counters, rrTemplate(), rrWarmPool(), claim, warm)
	// Fresh reconciler, empty in-memory state (post-restart), empty queue.
	// The status-first crash window only exists under one-write adoption.
	r := nsReconciler(t, c)
	r.OneWriteAdoption = true

	nsReconcile(t, r, claim.Name)

	sb := nsGetSandbox(t, c, warm.Name)
	assertMetadataAdoptionConverged(t, sb, claim)
	if !equality.Semantic.DeepEqual(specBefore, &sb.Spec) {
		t.Errorf("crash-recovery adoption modified the spec:\n before: %+v\n after:  %+v", specBefore.PodTemplate.ObjectMeta, sb.Spec.PodTemplate.ObjectMeta)
	}
	if sb.Generation != genBefore {
		t.Errorf("crash-recovery adoption bumped generation %d->%d", genBefore, sb.Generation)
	}
	bound := &extensionsv1beta1.SandboxClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: claim.Name, Namespace: rrNamespace}, bound); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if bound.Status.SandboxStatus.Name != warm.Name {
		t.Errorf("expected binding preserved through recovery, got %q", bound.Status.SandboxStatus.Name)
	}
}

// TestPodTemplateMetaEqualModuloSystemKeys pins the comparison semantics the
// steady-state drift check relies on.
func TestPodTemplateMetaEqualModuloSystemKeys(t *testing.T) {
	tests := []struct {
		name string
		a, b sandboxv1beta1.PodMetadata
		want bool
	}{
		{
			name: "system label residue ignored both directions",
			a: sandboxv1beta1.PodMetadata{Labels: map[string]string{
				extensionsv1beta1.SandboxIDLabel: "uid-1",
				"app":                            "x",
			}},
			b: sandboxv1beta1.PodMetadata{Labels: map[string]string{
				warmPoolSandboxLabel: "pool-hash",
				"app":                "x",
			}},
			want: true,
		},
		{
			name: "safe-to-evict true ignored",
			a:    sandboxv1beta1.PodMetadata{},
			b: sandboxv1beta1.PodMetadata{Annotations: map[string]string{
				autoscalerSafeToEvictAnnotation: "true",
			}},
			want: true,
		},
		{
			name: "explicit safe-to-evict false is compared",
			a: sandboxv1beta1.PodMetadata{Annotations: map[string]string{
				autoscalerSafeToEvictAnnotation: "false",
			}},
			b:    sandboxv1beta1.PodMetadata{},
			want: false,
		},
		{
			name: "user label drift detected",
			a:    sandboxv1beta1.PodMetadata{Labels: map[string]string{"team": "a"}},
			b:    sandboxv1beta1.PodMetadata{Labels: map[string]string{"team": "b"}},
			want: false,
		},
		{
			name: "user annotation drift detected",
			a:    sandboxv1beta1.PodMetadata{Annotations: map[string]string{"note": "a"}},
			b:    sandboxv1beta1.PodMetadata{},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := podTemplateMetaEqualModuloSystemKeys(&tc.a, &tc.b); got != tc.want {
				t.Errorf("podTemplateMetaEqualModuloSystemKeys() = %v, want %v", got, tc.want)
			}
		})
	}
}
