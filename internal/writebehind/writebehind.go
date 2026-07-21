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

// Package writebehind implements a small, bounded write-behind coalescer for
// recoverable metadata-only writes (labels/annotations).
//
// Motivation: Kubernetes has no
// multi-object batch or transactional write API — every mutation is one HTTP
// request and one etcd raft commit ("The Kubernetes API verbs get, create,
// update, patch, delete ... support single resources only ... no support for
// submitting multiple resources together in an ordered or unordered list or
// transaction", kubernetes.io/docs/reference/using-api/api-concepts). The
// only client-side lever is therefore to (a) ELIMINATE writes or (b) COALESCE
// multiple pending mutations *to the same object* into one patch and flush it
// outside the latency-critical window. This package implements (b) for writes
// that are safe to lose: every mutation routed here must be recomputable from
// informer state by the owning controller's level-based reconcile, so a crash
// before flush is recovered by the next reconcile re-issuing the mutation.
//
// Safety properties:
//   - Per-object coalescing: N Enqueue calls for the same object merge into
//     ONE JSON merge patch (last write wins per key), flushed once.
//   - Bounded staleness: each entry flushes no later than
//     min(Window, per-call maxDelay) after its FIRST enqueue; later enqueues
//     never extend an entry's deadline (they can only tighten it).
//   - Bounded memory: at most MaxPending distinct objects are held; beyond
//     that, new objects are patched synchronously inline (degrading to the
//     legacy behavior instead of growing without bound).
//   - No optimistic locking: flushed patches are plain merge patches without
//     resourceVersion, so they cannot 409; last-writer-wins is safe for the
//     same reason it is safe on the synchronous path today — the routed
//     writes are recomputed-from-observed-state metadata reconciliation.
//   - Graceful shutdown: on manager stop the Runnable drains all pending
//     entries (bounded by drainTimeout) before returning.
package writebehind

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

const (
	// defaultMaxPending bounds the number of distinct objects with pending
	// mutations. 4096 objects × two small string maps is a few MB worst case.
	defaultMaxPending = 4096
	// drainTimeout bounds the graceful-shutdown flush of pending entries.
	drainTimeout = 5 * time.Second
	// flushRetries and flushRetryBackoff shape the per-entry retry policy for
	// transient flush errors. NotFound is terminal (object deleted — the
	// mutation is moot). Anything still failing after the retries is dropped
	// with an error log: the owning controller's level-based reconcile
	// recomputes and re-issues the mutation on the next object event, exactly
	// as it does after a crash.
	flushRetries      = 3
	flushRetryBackoff = 50 * time.Millisecond
)

// Mutation is a set of metadata-only changes to one object. Set entries win
// over Delete entries for the same key within a single Mutation.
type Mutation struct {
	SetLabels         map[string]string
	DeleteLabels      []string
	SetAnnotations    map[string]string
	DeleteAnnotations []string
}

// IsEmpty reports whether the mutation carries no changes.
func (m Mutation) IsEmpty() bool {
	return len(m.SetLabels) == 0 && len(m.DeleteLabels) == 0 &&
		len(m.SetAnnotations) == 0 && len(m.DeleteAnnotations) == 0
}

// Options configures a Flusher.
type Options struct {
	// Window is the coalescing window: an entry flushes no later than Window
	// after its first enqueue (unless a per-call maxDelay tightens it).
	// Must be > 0 — a zero window means "don't use write-behind at all";
	// callers keep their synchronous path and never construct a Flusher.
	Window time.Duration
	// MaxPending bounds the number of distinct objects held. 0 = default.
	MaxPending int
}

type objKey struct {
	gvk       schema.GroupVersionKind
	namespace string
	name      string
}

// entry accumulates the coalesced mutation for one object. Label/annotation
// values use *string so a nil value marshals to JSON null — the merge-patch
// deletion sentinel (RFC 7386).
type entry struct {
	target      client.Object
	labels      map[string]*string
	annotations map[string]*string
	deadline    time.Time
}

// Flusher coalesces metadata mutations per object and flushes them as single
// merge patches within a bounded window. It implements manager.Runnable and
// LeaderElectionRunnable (flushes only run on the leader, like the reconciles
// that feed it).
type Flusher struct {
	c      client.Client
	scheme *runtime.Scheme
	window time.Duration
	maxPen int

	mu      sync.Mutex
	pending map[objKey]*entry
	wake    chan struct{}
}

// New builds a Flusher. Window must be positive.
func New(c client.Client, scheme *runtime.Scheme, opts Options) (*Flusher, error) {
	if opts.Window <= 0 {
		return nil, fmt.Errorf("writebehind: Window must be > 0 (callers with a zero window keep their synchronous write path and must not construct a Flusher)")
	}
	maxPen := opts.MaxPending
	if maxPen <= 0 {
		maxPen = defaultMaxPending
	}
	return &Flusher{
		c:       c,
		scheme:  scheme,
		window:  opts.Window,
		maxPen:  maxPen,
		pending: make(map[objKey]*entry),
		wake:    make(chan struct{}, 1),
	}, nil
}

// Enqueue records a metadata mutation for obj, coalescing it with any
// mutation already pending for the same object. The coalesced patch flushes
// no later than min(Window, maxDelay) after the object's FIRST pending
// enqueue (maxDelay <= 0 means "no extra bound beyond Window"). If the
// pending set is full, the mutation is applied synchronously inline instead
// (bounded memory; behavior degrades to the legacy synchronous write).
func (f *Flusher) Enqueue(ctx context.Context, obj client.Object, mut Mutation, maxDelay time.Duration) error {
	if mut.IsEmpty() {
		return nil
	}
	gvk, err := apiutil.GVKForObject(obj, f.scheme)
	if err != nil {
		return fmt.Errorf("writebehind: cannot determine GVK: %w", err)
	}
	key := objKey{gvk: gvk, namespace: obj.GetNamespace(), name: obj.GetName()}

	delay := f.window
	if maxDelay > 0 && maxDelay < delay {
		delay = maxDelay
	}
	deadline := time.Now().Add(delay)

	f.mu.Lock()
	e, ok := f.pending[key]
	if !ok {
		if len(f.pending) >= f.maxPen {
			// Bounded queue: apply this object's mutation synchronously.
			f.mu.Unlock()
			target, err := f.newTarget(gvk, key)
			if err != nil {
				return err
			}
			labels, annotations := mutationMaps(mut)
			return f.patch(ctx, target, labels, annotations)
		}
		target, err := f.newTarget(gvk, key)
		if err != nil {
			f.mu.Unlock()
			return err
		}
		e = &entry{target: target, deadline: deadline}
		e.labels, e.annotations = mutationMaps(mut)
		f.pending[key] = e
		f.mu.Unlock()
		f.kick()
		return nil
	}
	// Coalesce into the existing entry; the deadline only ever tightens.
	mergeMutation(e, mut)
	if deadline.Before(e.deadline) {
		e.deadline = deadline
	}
	f.mu.Unlock()
	f.kick()
	return nil
}

// newTarget builds a minimal typed carrier object for the flush Patch call.
func (f *Flusher) newTarget(gvk schema.GroupVersionKind, key objKey) (client.Object, error) {
	ro, err := f.scheme.New(gvk)
	if err != nil {
		return nil, fmt.Errorf("writebehind: scheme.New(%s): %w", gvk, err)
	}
	target, ok := ro.(client.Object)
	if !ok {
		return nil, fmt.Errorf("writebehind: %s is not a client.Object", gvk)
	}
	target.SetNamespace(key.namespace)
	target.SetName(key.name)
	return target, nil
}

func mutationMaps(mut Mutation) (labels, annotations map[string]*string) {
	apply := func(dst map[string]*string, set map[string]string, del []string) map[string]*string {
		if len(set) == 0 && len(del) == 0 {
			return dst
		}
		if dst == nil {
			dst = make(map[string]*string, len(set)+len(del))
		}
		for _, k := range del {
			dst[k] = nil
		}
		for k, v := range set {
			v := v
			dst[k] = &v
		}
		return dst
	}
	labels = apply(nil, mut.SetLabels, mut.DeleteLabels)
	annotations = apply(nil, mut.SetAnnotations, mut.DeleteAnnotations)
	return labels, annotations
}

func mergeMutation(e *entry, mut Mutation) {
	l, a := mutationMaps(mut)
	if l != nil {
		if e.labels == nil {
			e.labels = l
		} else {
			for k, v := range l {
				e.labels[k] = v
			}
		}
	}
	if a != nil {
		if e.annotations == nil {
			e.annotations = a
		} else {
			for k, v := range a {
				e.annotations[k] = v
			}
		}
	}
}

func (f *Flusher) kick() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// Pending returns the number of objects with unflushed mutations.
func (f *Flusher) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

// deadlineFor exposes an entry's flush deadline for white-box tests.
func (f *Flusher) deadlineFor(obj client.Object, gvk schema.GroupVersionKind) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.pending[objKey{gvk: gvk, namespace: obj.GetNamespace(), name: obj.GetName()}]
	if !ok {
		return time.Time{}, false
	}
	return e.deadline, true
}

// NeedLeaderElection makes the Runnable leader-scoped: mutations are only
// produced by leader-scoped reconciles, so flushes follow the same lifecycle.
func (f *Flusher) NeedLeaderElection() bool { return true }

// Start runs the flush loop until ctx is canceled, then drains all pending
// entries. Implements manager.Runnable.
func (f *Flusher) Start(ctx context.Context) error {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		f.mu.Lock()
		var next time.Time
		for _, e := range f.pending {
			if next.IsZero() || e.deadline.Before(next) {
				next = e.deadline
			}
		}
		f.mu.Unlock()

		wait := time.Hour
		if !next.IsZero() {
			wait = time.Until(next)
			if wait < 0 {
				wait = 0
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)

		select {
		case <-ctx.Done():
			// Graceful drain: crash-consistency does not REQUIRE this (the
			// next process's reconciles recompute everything), but flushing
			// cheaply here avoids paying reconcile latency after failover.
			drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
			defer cancel()
			return f.FlushAll(drainCtx)
		case <-f.wake:
		case <-timer.C:
		}
		f.flushDue(ctx, time.Now())
	}
}

// flushDue flushes every entry whose deadline has passed.
func (f *Flusher) flushDue(ctx context.Context, now time.Time) {
	f.mu.Lock()
	var due []*entry
	for k, e := range f.pending {
		if !e.deadline.After(now) {
			due = append(due, e)
			delete(f.pending, k)
		}
	}
	f.mu.Unlock()
	for _, e := range due {
		f.flushEntry(ctx, e)
	}
}

// FlushAll immediately flushes every pending entry. Used on shutdown drain
// and by tests to make coalescing deterministic.
func (f *Flusher) FlushAll(ctx context.Context) error {
	f.mu.Lock()
	due := make([]*entry, 0, len(f.pending))
	for k, e := range f.pending {
		due = append(due, e)
		delete(f.pending, k)
	}
	f.mu.Unlock()
	var firstErr error
	for _, e := range due {
		if err := f.flushEntry(ctx, e); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (f *Flusher) flushEntry(ctx context.Context, e *entry) error {
	err := f.patch(ctx, e.target, e.labels, e.annotations)
	if err != nil {
		ctrl.Log.WithName("writebehind").Error(err,
			"dropping unflushed coalesced patch; the owning controller's next reconcile recomputes it",
			"namespace", e.target.GetNamespace(), "name", e.target.GetName())
	}
	return err
}

// metaPatch is the wire shape of a metadata-only merge patch. *string values
// let JSON null (key deletion, RFC 7386) travel through Marshal.
type metaPatch struct {
	Metadata struct {
		Labels      map[string]*string `json:"labels,omitempty"`
		Annotations map[string]*string `json:"annotations,omitempty"`
	} `json:"metadata"`
}

// patch issues the coalesced merge patch with bounded retries. NotFound is
// terminal success (the object is gone; the mutation is moot).
func (f *Flusher) patch(ctx context.Context, target client.Object, labels, annotations map[string]*string) error {
	var doc metaPatch
	doc.Metadata.Labels = labels
	doc.Metadata.Annotations = annotations
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("writebehind: marshal patch: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < flushRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(flushRetryBackoff << (attempt - 1)):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// Patch mutates the target in place with the server response; use a
		// fresh copy per attempt so a half-populated carrier never leaks
		// state between retries.
		obj := target.DeepCopyObject().(client.Object)
		lastErr = f.c.Patch(ctx, obj, client.RawPatch(types.MergePatchType, data))
		if lastErr == nil || k8serrors.IsNotFound(lastErr) {
			return nil
		}
	}
	return lastErr
}
