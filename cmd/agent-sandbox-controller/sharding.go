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

package main

// Static namespace-sharded multi-controller mode (SCALE-ROADMAP.md R4.4).
//
// N controller processes, each started with a disjoint --watch-namespaces
// list, partition the cluster BY NAMESPACE:
//
//   - cache.Options.DefaultNamespaces scopes every namespaced informer
//     list/watch SERVER-SIDE to the shard's namespaces, so watch ingestion,
//     JSON decode, cache memory, and workqueue depth all divide by N.
//   - The leader-election Lease ID gains a stable hash of the (normalized)
//     namespace list, so each shard elects its own leader independently and
//     shards can each run active/standby replica pairs for HA.
//
// Correctness does not depend on the partition: adoption exclusivity rests on
// the optimistic-lock sandbox patch (extensions/controllers/
// sandboxclaim_controller.go, completeAdoption), not on the in-memory warm
// queue, and adoption is namespace-local by construction
// (verifySandboxCandidate's ErrCrossNamespaceAdoption guard). Sharding by
// namespace merely keeps each pool's warm-candidate queue on exactly one
// process so shards never race for the same candidates.
//
// Cluster-scoped resources: controller-runtime's multi-namespace cache
// (v0.24.1, pkg/cache/multi_namespace_cache.go) transparently keeps a
// cluster-wide informer for any cluster-scoped object because cache.Options
// is built with a global config (pkg/cache/cache.go newCache path when
// DefaultNamespaces is set), so a cluster-scoped Get/List/Watch through the
// manager client would still work rather than erroring at start. Today no
// controller watches or cache-reads a cluster-scoped resource (all For/Owns/
// Watches types -- Sandbox, SandboxClaim, SandboxWarmPool, SandboxTemplate,
// Pod, Service, NetworkPolicy -- are namespaced; CRD caBundle patching and
// leader election use direct, non-cached clients).

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// parseWatchNamespaces normalizes a comma-separated --watch-namespaces value:
// entries are whitespace-trimmed, empties dropped, duplicates removed, and the
// result sorted so that the same namespace SET always yields the same slice
// (and therefore the same leader-election Lease ID) regardless of flag
// ordering or spacing. Returns nil for an empty/blank value (cluster-wide
// mode, today's behavior).
func parseWatchNamespaces(raw string) []string {
	seen := make(map[string]struct{})
	var namespaces []string
	for _, ns := range strings.Split(raw, ",") {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	return namespaces
}

// shardLeaderElectionID derives a per-shard leader-election Lease ID by
// suffixing baseID with an FNV-1a 64-bit hash of the normalized (sorted,
// deduplicated) namespace list. Different shards therefore hold different
// Leases and elect leaders independently; the same shard always recomputes
// the same ID across restarts. A NUL separator between namespaces prevents
// concatenation ambiguity (["ab","c"] vs ["a","bc"]). The suffix is lowercase
// hex, so the result remains a valid Lease name (RFC 1123 subdomain).
// With no namespaces (cluster-wide mode) baseID is returned unchanged.
func shardLeaderElectionID(baseID string, namespaces []string) string {
	if len(namespaces) == 0 {
		return baseID
	}
	h := fnv.New64a()
	for _, ns := range namespaces {
		_, _ = h.Write([]byte(ns))
		_, _ = h.Write([]byte{0})
	}
	return fmt.Sprintf("%s-shard-%016x", baseID, h.Sum64())
}

// configureNamespaceSharding applies the --watch-namespaces flag value to the
// manager options. With an empty value it is a no-op and returns nil: watches
// stay cluster-wide and the stock Lease ID is kept, exactly today's behavior.
// Otherwise it scopes cache.Options.DefaultNamespaces to the given namespaces
// (server-side watch scoping) and suffixes LeaderElectionID with a stable
// hash of the namespace list, returning the normalized list for logging.
func configureNamespaceSharding(opts *ctrl.Options, rawWatchNamespaces string) []string {
	namespaces := parseWatchNamespaces(rawWatchNamespaces)
	if len(namespaces) == 0 {
		return nil
	}
	defaultNamespaces := make(map[string]cache.Config, len(namespaces))
	for _, ns := range namespaces {
		defaultNamespaces[ns] = cache.Config{}
	}
	opts.Cache.DefaultNamespaces = defaultNamespaces
	opts.LeaderElectionID = shardLeaderElectionID(opts.LeaderElectionID, namespaces)
	return namespaces
}
