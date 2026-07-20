# Community scan — open issues/PRs vs our optimization inventory (2026-07-19)
## Classification table (perf/scale/latency hits)

| # | Title (short) | Class | Takeaway |
|---|---|---|---|
| Issue #418 (critical) | WarmPool adoption race: 2 pods → 1 sandbox | **OVERLAPS T3/T4** | TOCTOU via stale informer cache; our stale-cache re-adoption guard (T3) + optimistic-lock adoption (T4) close exactly this class. Strongest issue to link/close from our branch. |
| Issue #478 | Claim adopts 2 sandboxes when status write fails | **OVERLAPS T4/T6** | The recheck misses adopted sandbox (different name) — our claim-UID label index + `IsControlledBy` recovery (T6) plus optimistic lock (T4) fix this. Note open PR #1129 partially covers it. |
| Issue #527 | Mitigate redundant reconciles (409 storms) | **OVERLAPS T2/T3/T5/T8** | Community's umbrella for exactly our reconcile-storm work; audit trace matches our findings. Link from tasks 2, 3, 5, 8. |
| Issue #1042 / PR #1072 | createSandbox AlreadyExists fatal (cache lag) | **OVERLAPS T3/T4/T5** | PR #1072 applies the same sentinel+50ms-bounded-requeue pattern as our T5; our T3 guard prevents the stale pass entirely. Coordinate, don't duplicate. |
| PR #1118 (lgtm+approved) | Finalize warm-pool adoption in same pass; **removes** `triggeredAdoptions` map, retry sentinel, 50ms requeue from #1108 | **CONFLICTS T3/T5** | Direct design collision: upstream is deleting the very machinery our T3 fingerprints extend and our T5 mirrors. If it merges before we file, tasks 3/5 need rewording and a heavy rebase. Top rebase-watch. |
| Issue #940 / PR #1087 | Startup histogram overcounts under concurrency | **OVERLAPS T3 / mild CONFLICT T7** | Same bug we fixed (911 obs for 300 claims); their fix stamps a persistent annotation = **+1 write/claim**, opposite of our write-reduction direction. Worth a comment on the PR. |
| Issue #1113 / PR #1114 | Metrics re-recorded on resume | COMPLEMENTS (same caveat) | Another annotation-guard write; same trade-off note as #1087. |
| Issue #781 | creation_latency lacks ms precision (CreationTimestamp) | **OVERLAPS T17** | We found and fixed the identical 1s-truncation measurement bug. Link from Task 17. |
| Issue #751 | webhook-first-observed-at never set | **CONFLICTS T1 (context)** | Our T1 removes the webhook entirely — must state in the T1 filing that this metric annotation becomes permanently impossible unless SDK-stamped (as PR #565 does). |
| Issue #484 / PRs #924, #1213 (lgtm) | Namespaced mode via `--namespace`/`WATCH_NAMESPACE` | **OVERLAPS T14** | Community already building the flag half of our static sharding. Our T14 should build on #1213's flag + add per-shard leader Lease and the adoption-exclusivity safety audit, not introduce a competing flag. |
| PR #913 (stale, cla:no) | Warm pool create rate limiting | **OVERLAPS T10** | Same idea as our `--max-refill-rate` token bucket, without the replenish-delay composition. Stale/unsigned — ours supersedes; reference it when filing T10. |
| PR #882 (lgtm, stale) | Sandbox status merge-patch + Pod reconcile predicate | **OVERLAPS T8, COMPLEMENTS T9** | Item 1 *is* our T8. Its per-write metrics (`agent_sandbox_writes_total`) are a good idea we don't have. Rebase-watch on `sandbox_controller.go`. |
| Issue #350 | Sandbox controller: one patch at end of reconcile | **OVERLAPS T7/T8** | Community version of collapsing scattered writes; link from T7/T8. |
| Issue #883 | Router 502/NXDOMAIN for warm-pool sandboxes (no per-sandbox Service) | **OVERLAPS T16** | Exactly the problem our router cache-resolution path solves; issue even proposes resolving from `status.podIPs`. Our branch can close it. Link from T16. |
| Issue #836 | Cilium identity explosion from high-cardinality labels; selector-less Services | **COMPLEMENTS T9/T16, watch T6** | P99 pod start >20s from `sandbox-name-hash`/`claim-uid` *labels*. If labels move to annotations, our T6 claim-UID **label field-index** recovery and T9 `--cache-label-selectors` must adapt. Their selector-less-Service+managed-EndpointSlice idea adds writes — argue for service-free (L3) instead. |
| PR #850 (draft RFC) / #1174 / #1198 | Envoy ext_proc router / sandbox-gateway KEP | **COMPLEMENTS T16** | Community already building informer UID→PodIP service-free routing in the Envoy direction. Don't over-invest in the Python router path; frame T16 as making `spec.service:false` viable for any router. |
| Issue #594 | NetworkPolicy calls in adoption path | COMPLEMENTS (gap in our budget) | Unconditional NP delete/update per claim reconcile sits in the adoption hot path — a write our budget table never counted. Nobody's fixing it; cheap adjacent win. |
| Issue #1059 | CRD field `warmpoolMaxRetries` | **CONFLICTS T5** | Tunable-retry API knob vs our approach (eliminate backoff, bounded requeue makes retry count moot). Also an API change. Push back with T5 data. |
| Issues #764 / PR #1078 (lgtm) | Stale-pod adoption under Recreate | COMPLEMENTS | Correctness check added inside `adoptSandboxFromCandidates` — rebase-watch for T3–T6. |
| PR #683 (lgtm) | Skip warm sandboxes without PodIPs | **mild CONFLICT T4** | On all-candidates-skipped it "falls through to cold creation" — our T4 explicitly forbids cold start while adoptable members exist (List fallback + bounded requeue). Reconcile semantics when both land. |
| PR #1129 (lgtm) | Clear stale assignment refs on lost adoption race | COMPLEMENTS T4 | Assumes adoption races exist (they do until our optimistic lock); harmless after ours, but rebase-watch. |
| Issues #624, #403, #1182 / PRs #661, #1148 (lgtm), #1209, #1028 | Benchmarks: high-throughput, continuous tracking, warmpool capacity cliff, CL2 recipes, modular kOps harness, capacity buffers | **COMPLEMENTS T17** | Community is actively building the measurement layer; our claims-warm/sustained phases + calibration caveats should be filed against #403/#624 and rebased onto #1148's create/run/teardown split. |
| PRs #1203–#1208, #1122 (justinsb, WIP) | Node I/O wall stress series: kubelet pod-start SLI, bpftrace profiler, NVMe/tmpfs/overlayfs-volatile containerd | **COMPLEMENTS T10/T17 + NEW** | Finds the *node-side* churn wall (containerd fdatasync, overlay teardown sync) that our roadmap's etcd/apiserver analysis omitted. ~~Directly explains part of our 70–85/s per-pool refill ceiling.~~ *(Round-9b correction: our refill ceiling was scheduler default-QPS then controller pipelines; nodes idled at 300/s — this series matters at ≥500-1000/s per-node rates, not for the measured ceiling.)* |
| Issue #1214 | Preempt warm pods (resume vs node scale-up) | NEW-ish, COMPLEMENTS | Warm pods as low-PriorityClass preemptible ballast — capacity-side latency lever outside our 17. |
| Issues #245, #686, #1140, #1110/#1076/#1073/#565 (PRs) | Metrics umbrella + stage latencies, warmpool gauge | COMPLEMENTS | The #245 family (memory-flagged priority ref) keeps landing PRs in the claim controller — steady rebase pressure, no design conflict except the annotation-write pattern noted above. |
| Issues #802, #271 / PR #1188 | TTL/failure-state for unbound claims | COMPLEMENTS | Stale-claim reaping matters at 1000/s; also #1188 **adds fields to `extensions/api/v1alpha1`** → direct conflict with T1's v1alpha1 deletion (cla:no though). |
| Issue #435 (critical) | `safe-to-evict=on-completion` for claimed pods | **NEW + touches T-L1** | See new-ideas #4 below — could delete a hot-path write we currently keep synchronous. |
| Issue #512 | GKE scalability test webhook trigger | COMPLEMENTS T17 | Infra only. |

Checked explicitly per your ask: **no one upstream is working on CBOR, batch/multi-object APIs, APF insulation, HTTP/2 connection sharding, or dynamic (label-based) controller sharding.** Tasks 11, 12, 13 have no community counterpart at all — those filings will be entirely novel.

## NEW-IDEAS list (not in our 17 tasks / roadmap)

1. **Node-local I/O wall** (PRs #1206/#1207/#1208/#1203, #1122): containerd bolt/content `fdatasync` (78k calls) + overlayfs teardown sync = 26–36% iowait during churn; `kubelet_pod_start_sli` 7.1s vs 1.5s baseline at mif200. Levers: overlayfs `volatile` mount (kernel 5.10+, "exactly the sandbox lifecycle" contract), `/var/lib/containerd` on local NVMe (c3-…-lssd), tmpfs as upper bound (+18% throughput, 5× pod-start front). This belongs in our L-track: it moves the pod-ready ~4s term in our pool-sizing formula and the refill ceiling.
2. **CNI identity explosion** (#836): high-cardinality labels (`sandbox-name-hash`, `claim-uid`) make Cilium mint one identity per pod → P99 pod-start >20s. We never profiled CNI identity cost; also a compatibility risk for our claim-UID *label* index (T6) and `--cache-label-selectors` (T9).
3. **NetworkPolicy hot-path writes** (#594): unconditional NP delete/update per claim reconcile — uncounted in our 20–25 writes/claim budget.
4. **`safe-to-evict=on-completion`** (#435, CA #9355): if adopted, the annotation can be set once in the pool template and never flipped — potentially deleting the synchronous pod metadata PATCH from our hot path (the write L1 only managed to defer) and de-risking L1's autoscaler-eviction window.
5. **Template→pool watch fan-out cliff** (#1182): 1-template→N-pools re-reconcile storm on template mutation — a second O(N) fan-out; our T2 only killed pool→claims.
6. **Warm-pod preemption ballast** (#1214) and **standby capacity buffers + max-pods 110→160 packing** (#1028): capacity-side latency levers.
7. **Per-write attribution metrics** (`agent_sandbox_writes_total` in PR #882): cheap observability we should adopt in the bench harness.
8. **Idempotent claim names / adopt-on-409 from the SDK** (#1089/#1090): eliminates duplicate-sandbox churn from client retries — a churn reducer we didn't model.

## Community issues to link when filing our tasks

- T2/T3/T5/T7/T8 → **#527**, #940; T3/T4 → **#418**; T4/T6 → **#478**; T4/T5 → #1042 (+PR #1072)
- T8 → PR #882, #350 · T9 → #836, #484 · T10 → PR #913, #1182 · T14 → **#484** (+PRs #924/#1213)
- T15 → #574, #286 (+PR #565) · T16 → **#883**, #836 · T17 → **#403**, #624, #781, #1182
- T1, T11, T12, T13: no existing issue — file fresh (T1 must address #751 and PRs #1188/#1106 touching v1alpha1).

## Rebase-watch (open PRs touching our files, by risk)

1. **#1118** (lgtm+approved) — rewrites the adoption finalization our T3/T5 build on; merge is imminent-class.
2. **#1213** (lgtm) namespaced mode in `main.go` — T14 must compose with it; also #1064 (lgtm+approved, main.go + both controllers).
3. Claim-controller metrics stream: #1073 (lgtm), #1087, #1114, #1076, #565 — constant small conflicts in `sandboxclaim_controller.go`.
4. `sandbox_controller.go`: #882 (lgtm, our T8 twin), #1078 (lgtm), #1080, #1081, #1160, #1192.
5. Harness: #1148 (lgtm) restructures `benchmarks-kops-gcp` (T17 must rebase onto create/run/teardown split); justinsb stack #1204–#1209 all touch the same `run` script.
6. T1 v1alpha1 deletion: #1188 and #1106 add/modify v1alpha1 files.
7. SDK (T15): #1088, #1003, #794, #897 all touch `k8s_helper.py`/`async_k8s_helper.py`.
