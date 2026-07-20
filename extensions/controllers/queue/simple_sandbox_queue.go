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

package queue

import (
	"sync"
)

// SandboxKey uniquely identifies a sandbox in the queue.
type SandboxKey struct {
	Namespace string
	Name      string
	NodeName  string
}

// SandboxQueue defines the interface for managing a thread-safe,
// highly concurrent queue of adoptable warm pool sandboxes.
//
// RESERVATION SEMANTICS (round-8 double-bind fix): popping a key (Get /
// GetWithStrategy) also RESERVES it — until the reservation is cleared, Add
// is a no-op for that sandbox. This closes the re-add leak that produced
// double-bound sandboxes at sustained rate: with one-write adoption the
// popped candidate stays pool-owned and adoptable in the informer cache for
// the whole async-patch window, so watch events observed during the window
// (most commonly the pod-scheduling NodeName status write re-triggering
// sandboxEventHandler.Update) re-Added the key and a second claim popped and
// status-bound the same sandbox (leg S 2026-07-20: 3,204 of 5,457 bound
// sandboxes were bound by 2+ claims). A reservation is cleared only by:
//   - Release: the holder gives the candidate back (verify failed
//     cross-namespace, adoption attempt failed, flush attempts exhausted with
//     the sandbox still pool-owned) — clears the reservation AND re-adds;
//   - Forget: the sandbox is confirmed gone or no longer pool material
//     (ghost pop, sandbox DELETE event) — clears without re-adding, so a
//     later legitimate adoptable transition can re-add it.
//
// Terminal adoption outcomes (async patch landed, steal by another owner)
// deliberately KEEP the reservation: the sandbox has permanently left the
// pool, and holding the reservation until its DELETE event Forgets it makes
// late stale watch events (delivered from pre-adoption object states)
// harmless. Reservations are process-local, like the queue itself.
type SandboxQueue interface {
	Add(namespacedWarmPoolName string, item SandboxKey)
	Get(namespacedWarmPoolName string) (SandboxKey, bool)
	GetWithStrategy(namespacedWarmPoolName string, pick func([]SandboxKey) (SandboxKey, bool)) (SandboxKey, bool)
	RemoveQueue(namespacedWarmPoolName string)
	RemoveItem(namespacedWarmPoolName string, item SandboxKey)
	// Release clears the item's reservation and returns it to the queue.
	Release(namespacedWarmPoolName string, item SandboxKey)
	// Forget clears any reservation for the sandbox without re-queueing it.
	Forget(namespace, name string)
	// IsReserved reports whether the sandbox is currently reserved (popped by
	// an adoption transaction whose outcome is not yet terminal).
	IsReserved(namespace, name string) bool
}

// SimpleSandboxQueue implements SandboxQueue using simple synchronized slices.
type SimpleSandboxQueue struct {
	// queues is a thread-safe dictionary from warm pool name to a synchronizedQueue
	queues sync.Map

	// reservedMu guards reserved.
	reservedMu sync.Mutex
	// reserved holds "namespace/name" keys popped from any pool queue whose
	// adoption transaction has not reached a terminal state. Reserved keys
	// cannot re-enter any queue via Add. See the interface comment.
	reserved map[string]struct{}
}

// NewSimpleSandboxQueue initializes a new SimpleSandboxQueue.
func NewSimpleSandboxQueue() *SimpleSandboxQueue {
	return &SimpleSandboxQueue{reserved: make(map[string]struct{})}
}

func reservationID(namespace, name string) string { return namespace + "/" + name }

func (s *SimpleSandboxQueue) reserve(item SandboxKey) {
	s.reservedMu.Lock()
	defer s.reservedMu.Unlock()
	s.reserved[reservationID(item.Namespace, item.Name)] = struct{}{}
}

func (s *SimpleSandboxQueue) unreserve(namespace, name string) {
	s.reservedMu.Lock()
	defer s.reservedMu.Unlock()
	delete(s.reserved, reservationID(namespace, name))
}

// IsReserved reports whether the sandbox is currently reserved.
func (s *SimpleSandboxQueue) IsReserved(namespace, name string) bool {
	s.reservedMu.Lock()
	defer s.reservedMu.Unlock()
	_, ok := s.reserved[reservationID(namespace, name)]
	return ok
}

// Add pushes an item to the specific warm pool's queue. Reserved items are
// silently dropped: they are in an in-flight adoption transaction and must
// not be handed to a second claim (the double-bind fix).
func (s *SimpleSandboxQueue) Add(namespacedWarmPoolName string, item SandboxKey) {
	if s.IsReserved(item.Namespace, item.Name) {
		return
	}
	q, _ := s.queues.LoadOrStore(namespacedWarmPoolName, newSynchronizedQueue())
	q.(*synchronizedQueue).Push(item)
}

// Release clears the item's reservation and returns it to the queue. Callers
// use it on every intentional give-back of a popped candidate.
func (s *SimpleSandboxQueue) Release(namespacedWarmPoolName string, item SandboxKey) {
	s.unreserve(item.Namespace, item.Name)
	s.Add(namespacedWarmPoolName, item)
}

// Forget clears any reservation for the sandbox without re-queueing it.
// Called on sandbox deletion (any owner) and on ghost pops.
func (s *SimpleSandboxQueue) Forget(namespace, name string) {
	s.unreserve(namespace, name)
}

// Get pops an item from the specific warm pool's queue and reserves it.
func (s *SimpleSandboxQueue) Get(namespacedWarmPoolName string) (SandboxKey, bool) {
	q, ok := s.queues.Load(namespacedWarmPoolName)
	if !ok {
		return SandboxKey{}, false
	}
	item, ok := q.(*synchronizedQueue).Pop()
	if ok {
		s.reserve(item)
	}
	return item, ok
}

// GetWithStrategy pops an item from the specific warm pool's queue using a
// custom strategy, and reserves it.
func (s *SimpleSandboxQueue) GetWithStrategy(namespacedWarmPoolName string, pick func([]SandboxKey) (SandboxKey, bool)) (SandboxKey, bool) {
	q, ok := s.queues.Load(namespacedWarmPoolName)
	if !ok {
		return SandboxKey{}, false
	}
	item, ok := q.(*synchronizedQueue).PopWithStrategy(pick)
	if ok {
		s.reserve(item)
	}
	return item, ok
}

// RemoveItem deletes a specific sandbox from a warm pool's queue and clears
// any reservation (the sandbox is gone; a same-name recreation is a new
// object and must be addable).
func (s *SimpleSandboxQueue) RemoveItem(namespacedWarmPoolName string, item SandboxKey) {
	s.unreserve(item.Namespace, item.Name)
	if q, ok := s.queues.Load(namespacedWarmPoolName); ok {
		sq := q.(*synchronizedQueue)
		sq.Remove(item)
	}
}

// Remove scans the slice and deletes the item to prevent Ghost Pods.
func (q *synchronizedQueue) Remove(key SandboxKey) {
	q.mu.Lock()
	defer q.mu.Unlock()

	uniqueID := key.Namespace + "/" + key.Name
	if _, exists := q.set[uniqueID]; !exists {
		return
	}

	delete(q.set, uniqueID)

	for i, k := range q.items {
		if k.Namespace == key.Namespace && k.Name == key.Name {
			// Shift left and clear the tail slot so removed keys don't linger.
			// Same pattern as Pop()
			last := len(q.items) - 1
			copy(q.items[i:], q.items[i+1:])
			q.items[last] = SandboxKey{}
			q.items = q.items[:last]
			break
		}
	}
}

// We should remove the queue from the sync.Map when the corresponding
// SandboxWarmPool is deleted to prevent memory leaks.
type synchronizedQueue struct {
	mu    sync.Mutex
	items []SandboxKey
	set   map[string]struct{} // Used for O(1) deduplication by namespace/name
}

func newSynchronizedQueue() *synchronizedQueue {
	return &synchronizedQueue{
		items: make([]SandboxKey, 0),
		set:   make(map[string]struct{}),
	}
}

// Push adds an item to the queue if it isn't already present.
func (q *synchronizedQueue) Push(key SandboxKey) {
	q.mu.Lock()
	defer q.mu.Unlock()
	uniqueID := key.Namespace + "/" + key.Name
	if _, exists := q.set[uniqueID]; !exists {
		q.set[uniqueID] = struct{}{}
		q.items = append(q.items, key)
	} else {
		// Key already exists. Always update the NodeName to reflect latest placement state.
		for i := range q.items {
			if q.items[i].Namespace == key.Namespace && q.items[i].Name == key.Name {
				q.items[i].NodeName = key.NodeName
				break
			}
		}
	}
}

// Pop removes and returns the first item from the queue.
func (q *synchronizedQueue) Pop() (SandboxKey, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) == 0 {
		return SandboxKey{}, false
	}

	// Grab the first item
	item := q.items[0]

	// This removes the pointer references so the Garbage Collector
	// can free the strings in memory!
	q.items[0] = SandboxKey{}

	// Remove it from slice and set
	q.items = q.items[1:]
	delete(q.set, item.Namespace+"/"+item.Name)

	return item, true
}

// PopWithStrategy applies the strategy function to pick an item from the queue,
// removes it thread-safely, and returns it.
func (q *synchronizedQueue) PopWithStrategy(pick func([]SandboxKey) (SandboxKey, bool)) (SandboxKey, bool) {
	for {
		q.mu.Lock()
		if len(q.items) == 0 {
			q.mu.Unlock()
			return SandboxKey{}, false
		}

		// Snapshot the queue items
		snapshot := make([]SandboxKey, len(q.items))
		copy(snapshot, q.items)
		q.mu.Unlock()

		key, ok := pick(snapshot)
		if !ok {
			return SandboxKey{}, false
		}

		q.mu.Lock()
		uniqueID := key.Namespace + "/" + key.Name
		// Verify the key is still present in the queue
		if _, exists := q.set[uniqueID]; !exists {
			// The picked key was concurrently popped by another goroutine.
			// Unlock and retry snapshot and pick.
			q.mu.Unlock()
			continue
		}

		// Find the picked key in q.items and remove it
		for i, k := range q.items {
			if k.Namespace == key.Namespace && k.Name == key.Name {
				// Shift left and clear the tail slot
				last := len(q.items) - 1
				copy(q.items[i:], q.items[i+1:])
				q.items[last] = SandboxKey{}
				q.items = q.items[:last]
				break
			}
		}
		delete(q.set, uniqueID)
		q.mu.Unlock()

		return key, true
	}
}

// RemoveQueue completely deletes a warm pool's queue from the sync.Map
// to prevent memory leaks when SandboxTemplates or WarmPools are deleted.
// Reservations are deliberately left intact: they belong to in-flight
// adoption transactions, not to the pool's queue.
func (s *SimpleSandboxQueue) RemoveQueue(namespacedWarmPoolName string) {
	s.queues.Delete(namespacedWarmPoolName)
}

// GetNamespacedWarmPoolName forms the namespace-aware index value to use as a key to a SimpleSandboxQueue type.
func GetNamespacedWarmPoolName(namespace, warmPoolName string) string {
	return namespace + "/" + warmPoolName
}
