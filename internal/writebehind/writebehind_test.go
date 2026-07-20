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

package writebehind

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var podGVK = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}

type patchRecorder struct {
	mu      sync.Mutex
	payload [][]byte
	targets []types.NamespacedName
}

func (p *patchRecorder) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			p.mu.Lock()
			p.payload = append(p.payload, data)
			p.targets = append(p.targets, types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()})
			p.mu.Unlock()
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
}

func (p *patchRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.payload)
}

func testPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: "ns",
		Labels:    map[string]string{"keep": "yes", "drop-me": "x"},
		Annotations: map[string]string{
			"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
		},
	}}
}

func newTestFlusher(t *testing.T, rec *patchRecorder, opts Options, objs ...client.Object) (*Flusher, client.Client) {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...)
	if rec != nil {
		builder = builder.WithInterceptorFuncs(rec.funcs())
	}
	cl := builder.Build()
	f, err := New(cl, clientgoscheme.Scheme, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f, cl
}

// TestCoalescesToOnePatch: N mutations to the same object become exactly one
// merge patch whose payload is the union with last-write-wins per key and
// JSON null for deletions.
func TestCoalescesToOnePatch(t *testing.T) {
	rec := &patchRecorder{}
	pod := testPod("pod-1")
	f, cl := newTestFlusher(t, rec, Options{Window: time.Hour}, pod)
	ctx := context.Background()

	if err := f.Enqueue(ctx, pod, Mutation{SetLabels: map[string]string{"a": "1"}}, 0); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if err := f.Enqueue(ctx, pod, Mutation{SetLabels: map[string]string{"a": "2", "b": "3"}, DeleteLabels: []string{"drop-me"}}, 0); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if err := f.Enqueue(ctx, pod, Mutation{DeleteAnnotations: []string{"cluster-autoscaler.kubernetes.io/safe-to-evict"}}, 0); err != nil {
		t.Fatalf("enqueue 3: %v", err)
	}

	if got := rec.count(); got != 0 {
		t.Fatalf("patches issued before flush: %d, want 0", got)
	}
	if got := f.Pending(); got != 1 {
		t.Fatalf("pending objects = %d, want 1 (coalesced)", got)
	}

	if err := f.FlushAll(ctx); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("patches after flush: %d, want exactly 1", got)
	}

	// Wire payload: nulls present for deletes, last-write-wins for "a".
	var doc map[string]map[string]map[string]*string
	if err := json.Unmarshal(rec.payload[0], &doc); err != nil {
		t.Fatalf("unmarshal payload %s: %v", rec.payload[0], err)
	}
	labels := doc["metadata"]["labels"]
	if labels["a"] == nil || *labels["a"] != "2" {
		t.Errorf("label a: want last-write-wins value 2, got %v", labels["a"])
	}
	if labels["b"] == nil || *labels["b"] != "3" {
		t.Errorf("label b missing: %v", labels["b"])
	}
	if v, ok := labels["drop-me"]; !ok || v != nil {
		t.Errorf("label drop-me: want explicit JSON null, got ok=%v v=%v", ok, v)
	}
	if v, ok := doc["metadata"]["annotations"]["cluster-autoscaler.kubernetes.io/safe-to-evict"]; !ok || v != nil {
		t.Errorf("safe-to-evict: want explicit JSON null, got ok=%v v=%v", ok, v)
	}

	// Server state reflects the coalesced result.
	var got corev1.Pod
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "pod-1"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Labels["a"] != "2" || got.Labels["b"] != "3" || got.Labels["keep"] != "yes" {
		t.Errorf("unexpected labels: %v", got.Labels)
	}
	if _, ok := got.Labels["drop-me"]; ok {
		t.Errorf("drop-me not deleted: %v", got.Labels)
	}
	if _, ok := got.Annotations["cluster-autoscaler.kubernetes.io/safe-to-evict"]; ok {
		t.Errorf("safe-to-evict not deleted: %v", got.Annotations)
	}
}

// TestDeadlineIsMinOfWindowAndMaxDelay pins the flush bound arithmetic:
// effective delay = min(Window, maxDelay), and later enqueues never extend an
// existing entry's deadline.
func TestDeadlineIsMinOfWindowAndMaxDelay(t *testing.T) {
	pod := testPod("pod-1")
	f, _ := newTestFlusher(t, nil, Options{Window: time.Hour}, pod)
	ctx := context.Background()

	// maxDelay below the window tightens the deadline.
	before := time.Now()
	if err := f.Enqueue(ctx, pod, Mutation{SetLabels: map[string]string{"a": "1"}}, time.Second); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	dl, ok := f.deadlineFor(pod, podGVK)
	if !ok {
		t.Fatal("no pending entry")
	}
	if max := before.Add(time.Second + 100*time.Millisecond); dl.After(max) {
		t.Errorf("deadline %v exceeds the 1s maxDelay bound (max %v) despite 1h window", dl, max)
	}

	// A later enqueue with a LOOSER bound must not extend the deadline.
	if err := f.Enqueue(ctx, pod, Mutation{SetLabels: map[string]string{"b": "2"}}, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	dl2, _ := f.deadlineFor(pod, podGVK)
	if dl2.After(dl) {
		t.Errorf("deadline extended by later enqueue: %v -> %v", dl, dl2)
	}

	// A later enqueue with a TIGHTER bound shortens it.
	if err := f.Enqueue(ctx, pod, Mutation{SetLabels: map[string]string{"c": "3"}}, 10*time.Millisecond); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	dl3, _ := f.deadlineFor(pod, podGVK)
	if !dl3.Before(dl2) {
		t.Errorf("tighter maxDelay did not shorten deadline: %v -> %v", dl2, dl3)
	}
}

// TestBackgroundFlushHonorsWindow: with the Runnable started, an enqueued
// mutation lands without any explicit FlushAll, within the window bound.
func TestBackgroundFlushHonorsWindow(t *testing.T) {
	rec := &patchRecorder{}
	pod := testPod("pod-1")
	f, cl := newTestFlusher(t, rec, Options{Window: 30 * time.Millisecond}, pod)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.Start(ctx) }()

	if err := f.Enqueue(ctx, pod, Mutation{SetLabels: map[string]string{"bg": "1"}}, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var got corev1.Pod
		if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "pod-1"}, &got); err == nil && got.Labels["bg"] == "1" {
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("Start returned error: %v", err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background flush did not land within 3s despite a 30ms window")
}

// TestGracefulDrainOnShutdown: canceling the Runnable's context flushes what
// is still pending instead of dropping it.
func TestGracefulDrainOnShutdown(t *testing.T) {
	rec := &patchRecorder{}
	pod := testPod("pod-1")
	f, cl := newTestFlusher(t, rec, Options{Window: time.Hour}, pod)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Start(ctx) }()

	if err := f.Enqueue(ctx, pod, Mutation{SetLabels: map[string]string{"drain": "1"}}, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	var got corev1.Pod
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "pod-1"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Labels["drain"] != "1" {
		t.Errorf("pending mutation not drained on shutdown: %v", got.Labels)
	}
}

// TestBoundedPendingFallsBackToSynchronous: beyond MaxPending distinct
// objects, new mutations are applied inline instead of growing the queue.
func TestBoundedPendingFallsBackToSynchronous(t *testing.T) {
	rec := &patchRecorder{}
	podA, podB := testPod("pod-a"), testPod("pod-b")
	f, cl := newTestFlusher(t, rec, Options{Window: time.Hour, MaxPending: 1}, podA, podB)
	ctx := context.Background()

	if err := f.Enqueue(ctx, podA, Mutation{SetLabels: map[string]string{"x": "1"}}, 0); err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("A should be pending, but %d patches issued", got)
	}

	// B overflows the bound: patched synchronously, A stays pending.
	if err := f.Enqueue(ctx, podB, Mutation{SetLabels: map[string]string{"y": "2"}}, 0); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("overflow enqueue should patch synchronously: %d patches, want 1", got)
	}
	if got := f.Pending(); got != 1 {
		t.Fatalf("pending = %d, want 1 (A only)", got)
	}
	var gotB corev1.Pod
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "pod-b"}, &gotB); err != nil {
		t.Fatalf("get B: %v", err)
	}
	if gotB.Labels["y"] != "2" {
		t.Errorf("synchronous overflow patch not applied: %v", gotB.Labels)
	}

	// Coalescing onto the existing pending object still works at the bound.
	if err := f.Enqueue(ctx, podA, Mutation{SetLabels: map[string]string{"x2": "3"}}, 0); err != nil {
		t.Fatalf("re-enqueue A: %v", err)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("coalescing enqueue must not patch synchronously: %d patches", got)
	}
}

// TestNotFoundIsTerminal: a deleted object's pending mutation is dropped
// without retries.
func TestNotFoundIsTerminal(t *testing.T) {
	attempts := 0
	cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			attempts++
			return k8serrors.NewNotFound(schema.GroupResource{Resource: "pods"}, obj.GetName())
		},
	}).Build()
	f, err := New(cl, clientgoscheme.Scheme, Options{Window: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	pod := testPod("gone")
	if err := f.Enqueue(ctx, pod, Mutation{SetLabels: map[string]string{"a": "1"}}, 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := f.FlushAll(ctx); err != nil {
		t.Fatalf("FlushAll should treat NotFound as terminal success, got %v", err)
	}
	if attempts != 1 {
		t.Errorf("NotFound retried: %d attempts, want 1", attempts)
	}
}

// TestZeroWindowRejected: window 0 means "stay synchronous" — constructing a
// Flusher with it is a programming error.
func TestZeroWindowRejected(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	if _, err := New(cl, clientgoscheme.Scheme, Options{Window: 0}); err == nil {
		t.Fatal("New with zero window should fail")
	}
}
