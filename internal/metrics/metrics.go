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

// nolint:revive
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/agent-sandbox/internal/version"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	LaunchTypeWarm    = "warm"    // Pod from a SandboxWarmPool
	LaunchTypeCold    = "cold"    // Pod not from a SandboxWarmPool
	LaunchTypeUnknown = "unknown" // Used when Sandbox is nil during failure

	// ObservabilityAnnotation is the annotation key for the time the controller first observed the claim.
	ObservabilityAnnotation = "agents.x-k8s.io/controller-first-observed-at"

	// WebhookAnnotation is the annotation key for the time the webhook first saw the claim.
	WebhookAnnotation = "agents.x-k8s.io/webhook-first-observed-at"
)

var (
	// ClaimStartupLatency measures the time from SandboxClaim creation to SandboxClaim Ready state.
	// Labels:
	// - launch_type: "warm", "cold", "unknown"
	// - sandbox_template: the resolved SandboxTemplateRef used to create the Sandbox.
	// - warmpool_name: the requested warm pool reference name (from SandboxClaim spec.warmPoolRef.name).
	ClaimStartupLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_claim_startup_latency_ms",
			Help: "End-to-end latency from SandboxClaim creation to Sandbox Ready state in milliseconds.",
			// Buckets for latency from 100ms to 4 minutes
			Buckets: []float64{100, 250, 500, 750, 1000, 1250, 1500, 2000, 2500, 5000, 10000, 30000, 60000, 120000, 240000},
		},
		[]string{"launch_type", "sandbox_template", "warmpool_name"},
	)

	// ClaimControllerStartupLatency measures the time from controller first observed timestamp to SandboxClaim Ready state.
	// Labels:
	// - launch_type: "warm", "cold", "unknown"
	// - sandbox_template: the resolved SandboxTemplateRef used to create the Sandbox.
	// - warmpool_name: the requested warm pool reference name (from SandboxClaim spec.warmPoolRef.name).
	ClaimControllerStartupLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_claim_controller_startup_latency_ms",
			Help: "Latency from controller first observed SandboxClaim to Sandbox Ready state in milliseconds.",
			// Buckets for latency from 100ms to 4 minutes
			Buckets: []float64{100, 250, 500, 750, 1000, 1250, 1500, 2000, 2500, 5000, 10000, 30000, 60000, 120000, 240000},
		},
		[]string{"launch_type", "sandbox_template", "warmpool_name"},
	)

	// SandboxCreationLatency measures the time from Sandbox creation to Pod Ready state.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - launch_type: "warm", "cold", "unknown"
	// - sandbox_template: the SandboxTemplateRef.
	SandboxCreationLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "agent_sandbox_creation_latency_ms",
			Help: "Latency from Sandbox creation to Pod Ready state in milliseconds. For warm launches, this measures controller synchronization overhead since the Pod is pre-provisioned.",
			// Buckets for latency from 50ms to 10 minutes
			Buckets: []float64{50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000, 240000, 300000, 600000},
		},
		[]string{"namespace", "launch_type", "sandbox_template"},
	)

	// SandboxClaimCreationTotal calculates the total number of SandboxClaims created.
	// Labels:
	// - namespace: the namespace of the claim
	// - sandbox_template: the SandboxTemplateRef
	// - launch_type: "warm", "cold", "unknown"
	// - warmpool_name: the requested warm pool reference name (from SandboxClaim spec.warmPoolRef.name).
	// - pod_condition: "ready", "not_ready".
	// - created_by: the component that created the claim (e.g. "go-client", "python-client", "controller", "unknown").
	SandboxClaimCreationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agent_sandbox_claim_creation_total",
			Help: "Total number of SandboxClaims created, labeled by namespace, sandbox template, launch type, warmpool name, pod condition, and created_by.",
		},
		[]string{"namespace", "sandbox_template", "launch_type", "warmpool_name", "pod_condition", "created_by"},
	)

	// AgentSandboxesDesc describes the agent_sandboxes metric point-in-time counts.
	// Labels:
	// - namespace: the namespace of the sandbox
	// - ready_condition: "true" | "false"
	// - expired: "true" | "false"
	// - launch_type: "warm" | "cold"
	// - sandbox_template: sandboxTemplateRef.
	// - owned_by: "SandboxClaim" | "SandboxWarmPool" | "None".
	// - created_by: the component that created the sandbox (e.g. "go-client", "python-client", "controller", "unknown").
	AgentSandboxesDesc = prometheus.NewDesc(
		"agent_sandboxes",
		"Monitor the point-in-time number of sandboxes in the cluster.",
		[]string{"namespace", "ready_condition", "expired", "launch_type", "sandbox_template", "owned_by", "created_by"},
		nil,
	)

	// AdoptionSegmentLatency decomposes the warm-adoption hot path into its
	// per-pass segments WITHOUT debug logging (PATH-TO-100MS measurement
	// rider: the gate-zero clean legs had to reconstruct these segments from
	// resourceVersion interleaves because the "adoption timing" line is
	// V(1)-gated; this histogram makes clean legs self-decomposing — the
	// stress harness already scrapes the controller's /metrics).
	//
	// Segments (label "segment", bounded cardinality):
	//   - queue_wait:       controller watch-receive -> winning-pass entry
	//                       (workqueue wait + earlier non-adopting passes;
	//                       monotonic clock, never CreationTimestamp);
	//   - sandbox_patch:    synchronous completeAdoption sandbox patch RTT
	//                       (zero-valued and not recorded under one-write
	//                       adoption, where the patch is deferred);
	//   - status_write:     claim status patch RTT (the critical write);
	//   - annotation_flush: deferred post-status claim annotation patch;
	//   - total:            adoption-pass entry -> summary emission;
	//   - async_queue_wait: one-write flusher enqueue -> worker pickup;
	//   - async_patch:      one-write deferred sandbox patch RTT (success).
	//
	// Buckets give sub-ms resolution at the floor (queue-wait p99 <1ms on a
	// healthy leg) and cover the 60-100ms target regime densely.
	AdoptionSegmentLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "agent_sandbox_claim_adoption_segment_latency_ms",
			Help:    "Warm-adoption hot-path segment latencies in milliseconds, labeled by segment (queue_wait, sandbox_patch, status_write, annotation_flush, total, async_queue_wait, async_patch).",
			Buckets: []float64{0.25, 0.5, 1, 2.5, 5, 10, 15, 25, 35, 50, 75, 100, 150, 250, 500, 1000, 2500},
		},
		[]string{"segment"},
	)

	// SupplySegmentLatency decomposes the warm-pool SUPPLY pipeline — the
	// round-9b binding constraint (~100-150/s aggregate: refill issuance +
	// ready marking, RESULTS.md round-9b ladder item 3) — so sustained legs
	// can attribute a supply ceiling from the standard /metrics scrape,
	// mirroring AdoptionSegmentLatency on the demand side.
	//
	// Segments (label "segment", bounded cardinality):
	//   - pool_member_create:          warm-pool controller replacement
	//                                  sandbox CREATE RTT (per create inside
	//                                  slowStartBatch; queueing behind claim
	//                                  traffic shows up here);
	//   - sandbox_create_to_pod_create: sandbox CreationTimestamp -> pod
	//                                  CREATE issued by the sandbox
	//                                  controller (informer + workqueue +
	//                                  reconcile lag; the "refill issuance"
	//                                  half of the round-9b claim→pod-created
	//                                  41.9s p50). CreationTimestamp is
	//                                  SECOND-truncated, so values carry up
	//                                  to +1s inflation — fine at the
	//                                  multi-second scales this decomposes;
	//   - pod_ready_to_sandbox_ready:  pod PodReady lastTransitionTime ->
	//                                  the sandbox controller pass that first
	//                                  marks the Sandbox Ready (the
	//                                  ready-marking half, p50 50.5s in
	//                                  SUST4; also second-truncated base).
	//
	// Buckets run 1ms..600s: healthy values are ms-scale, and the failure
	// modes being decomposed are tens of seconds to minutes.
	SupplySegmentLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "agent_sandbox_supply_segment_latency_ms",
			Help:    "Warm-pool supply pipeline segment latencies in milliseconds, labeled by segment (pool_member_create, sandbox_create_to_pod_create, pod_ready_to_sandbox_ready). Segments with a *_to_* name are measured from second-truncated API timestamps (up to +1s inflation).",
			Buckets: []float64{1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 25000, 50000, 100000, 200000, 400000, 600000},
		},
		[]string{"segment"},
	)

	buildVersionInfo = version.Get()

	// BuildInfo exposes agent-sandbox-controller build metadata as a constant gauge.
	BuildInfo = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "agent_sandbox_build_info",
			Help: "Agent sandbox controller build metadata exposed as labels with a constant value of 1.",
			ConstLabels: prometheus.Labels{
				"git_version": buildVersionInfo.GitVersion,
				"git_commit":  buildVersionInfo.GitSHA,
				"build_date":  buildVersionInfo.BuildDate,
				"go_version":  buildVersionInfo.GoVersion,
				"compiler":    buildVersionInfo.Compiler,
				"platform":    buildVersionInfo.Platform,
			},
		},
		func() float64 { return 1 },
	)
)

// Init registers custom metrics with the global controller-runtime registry.
func init() {
	metrics.Registry.MustRegister(ClaimStartupLatency)
	metrics.Registry.MustRegister(ClaimControllerStartupLatency)
	metrics.Registry.MustRegister(SandboxCreationLatency)
	metrics.Registry.MustRegister(SandboxClaimCreationTotal)
	metrics.Registry.MustRegister(AdoptionSegmentLatency)
	metrics.Registry.MustRegister(SupplySegmentLatency)
	metrics.Registry.MustRegister(BuildInfo)
}

// RecordAdoptionSegment records one warm-adoption hot-path segment duration.
func RecordAdoptionSegment(segment string, d time.Duration) {
	AdoptionSegmentLatency.WithLabelValues(segment).Observe(float64(d.Nanoseconds()) / 1e6)
}

// RecordSupplySegment records one warm-pool supply pipeline segment duration.
// Negative durations (possible when the base timestamp is second-truncated
// and the segment is sub-second) are clamped to zero rather than dropped, so
// counts still reflect one observation per event.
func RecordSupplySegment(segment string, d time.Duration) {
	if d < 0 {
		d = 0
	}
	SupplySegmentLatency.WithLabelValues(segment).Observe(float64(d.Nanoseconds()) / 1e6)
}

// RecordClaimStartupLatency records the duration since the provided start time.
func RecordClaimStartupLatency(startTime time.Time, launchType, templateName, warmPoolName string) {
	duration := float64(time.Since(startTime).Milliseconds())
	ClaimStartupLatency.WithLabelValues(launchType, templateName, warmPoolName).Observe(duration)
}

// RecordClaimControllerStartupLatency records the duration since the provided controller start time.
func RecordClaimControllerStartupLatency(startTime time.Time, launchType, templateName, warmPoolName string) {
	duration := float64(time.Since(startTime).Milliseconds())
	ClaimControllerStartupLatency.WithLabelValues(launchType, templateName, warmPoolName).Observe(duration)
}

// RecordSandboxCreationLatency records the measured latency duration for a sandbox creation.
func RecordSandboxCreationLatency(duration time.Duration, namespace, launchType, templateName string) {
	SandboxCreationLatency.WithLabelValues(namespace, launchType, templateName).Observe(float64(duration.Milliseconds()))
}

// NormalizeCreatedBy returns the createdBy label normalized to a known allow-list
// (go-client, python-client, controller) or "unknown" for anything else.
func NormalizeCreatedBy(createdBy string) string {
	switch createdBy {
	case "go-client", "python-client", "controller":
		return createdBy
	default:
		return "unknown"
	}
}

// RecordSandboxClaimCreation increments the total count of created sandbox claims.
// The createdBy value is automatically normalized.
func RecordSandboxClaimCreation(namespace, templateName, launchType, warmPoolName, podCondition, createdBy string) {
	SandboxClaimCreationTotal.WithLabelValues(namespace, templateName, launchType, warmPoolName, podCondition, NormalizeCreatedBy(createdBy)).Inc()
}
