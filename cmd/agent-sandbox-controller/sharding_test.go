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

import (
	"reflect"
	"regexp"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

const testBaseLeaseID = "a3317529.agent-sandbox.x-k8s.io"

func TestParseWatchNamespaces(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty means cluster-wide", raw: "", want: nil},
		{name: "blank means cluster-wide", raw: "  ", want: nil},
		{name: "only separators means cluster-wide", raw: ", ,,", want: nil},
		{name: "single namespace", raw: "ns-a", want: []string{"ns-a"}},
		{name: "list is sorted", raw: "ns-b,ns-a", want: []string{"ns-a", "ns-b"}},
		{
			name: "whitespace and duplicates are normalized",
			raw:  " ns-b , ns-a ,ns-b,, ",
			want: []string{"ns-a", "ns-b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseWatchNamespaces(tt.raw); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseWatchNamespaces(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestConfigureNamespaceShardingUnsetIsNoOp(t *testing.T) {
	opts := ctrl.Options{LeaderElectionID: testBaseLeaseID}
	for _, raw := range []string{"", "  ", ",,"} {
		got := configureNamespaceSharding(&opts, raw)
		if got != nil {
			t.Errorf("configureNamespaceSharding(%q) = %v, want nil", raw, got)
		}
		if opts.Cache.DefaultNamespaces != nil {
			t.Errorf("configureNamespaceSharding(%q) set Cache.DefaultNamespaces = %v, want nil (cluster-wide)", raw, opts.Cache.DefaultNamespaces)
		}
		if opts.LeaderElectionID != testBaseLeaseID {
			t.Errorf("configureNamespaceSharding(%q) changed LeaderElectionID to %q, want stock %q", raw, opts.LeaderElectionID, testBaseLeaseID)
		}
	}
}

func TestConfigureNamespaceShardingCacheOptions(t *testing.T) {
	opts := ctrl.Options{LeaderElectionID: testBaseLeaseID}
	got := configureNamespaceSharding(&opts, " team-b , team-a ,team-a")

	if want := []string{"team-a", "team-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("configureNamespaceSharding returned %v, want %v", got, want)
	}
	wantCache := map[string]cache.Config{
		"team-a": {},
		"team-b": {},
	}
	if !reflect.DeepEqual(opts.Cache.DefaultNamespaces, wantCache) {
		t.Errorf("Cache.DefaultNamespaces = %#v, want %#v", opts.Cache.DefaultNamespaces, wantCache)
	}
	if opts.LeaderElectionID == testBaseLeaseID {
		t.Errorf("LeaderElectionID was not suffixed for the shard: %q", opts.LeaderElectionID)
	}
}

func TestShardLeaderElectionID(t *testing.T) {
	shardA := shardLeaderElectionID(testBaseLeaseID, []string{"team-a-1", "team-a-2"})
	shardB := shardLeaderElectionID(testBaseLeaseID, []string{"team-b-1", "team-b-2"})

	t.Run("empty list keeps stock ID", func(t *testing.T) {
		if got := shardLeaderElectionID(testBaseLeaseID, nil); got != testBaseLeaseID {
			t.Errorf("got %q, want stock %q", got, testBaseLeaseID)
		}
	})

	t.Run("distinct namespace sets get distinct leases", func(t *testing.T) {
		if shardA == shardB {
			t.Errorf("shard A and shard B derived the same Lease ID %q", shardA)
		}
		if shardA == testBaseLeaseID || shardB == testBaseLeaseID {
			t.Errorf("shard Lease ID collides with the stock cluster-wide ID: a=%q b=%q", shardA, shardB)
		}
	})

	t.Run("stable across restarts and flag formatting", func(t *testing.T) {
		// The full pipeline (parse + hash) must give the same Lease for the
		// same namespace SET however the flag was written.
		if got := shardLeaderElectionID(testBaseLeaseID, parseWatchNamespaces(" team-a-2 ,team-a-1,team-a-1 ")); got != shardA {
			t.Errorf("reordered/whitespaced flag produced %q, want %q", got, shardA)
		}
	})

	t.Run("no concatenation ambiguity", func(t *testing.T) {
		if x, y := shardLeaderElectionID(testBaseLeaseID, []string{"ab", "c"}), shardLeaderElectionID(testBaseLeaseID, []string{"a", "bc"}); x == y {
			t.Errorf("[ab c] and [a bc] hashed to the same Lease ID %q", x)
		}
	})

	t.Run("valid Lease object name", func(t *testing.T) {
		// Lease names must be RFC 1123 subdomains.
		subdomain := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
		for _, id := range []string{shardA, shardB} {
			if !subdomain.MatchString(id) {
				t.Errorf("Lease ID %q is not a valid RFC 1123 subdomain", id)
			}
			if len(id) > 253 {
				t.Errorf("Lease ID %q exceeds 253 characters", id)
			}
		}
	})
}
