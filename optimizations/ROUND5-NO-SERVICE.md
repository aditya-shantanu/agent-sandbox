# ROUND5 — L3: per-sandbox Service elimination (investigation + implementation)

Status: round-5 deliverable on `perf-investigation-master-no-service`.
Scope: SCALE-ROADMAP item L3 ("Kill per-sandbox Services").

## Headline finding: the benchmark is ALREADY service-free

L3's premise ("every warm-pool sandbox today gets a headless Service …
~20% of total churn writes") does **not** hold for the stress benchmark on
this tree, and the round-3 numbers prove it:

- `spec.service` semantics (`controllers/sandbox_controller.go`,
  `reconcileService`): **nil = do not create** (only preserve pre-existing
  Services), `true` = create, `false` = delete-if-owned. There is no
  defaulting webhook and no CRD default; nothing in the warm-pool or claim
  controllers sets the field.
- The stress SandboxTemplate (`test/stress/phases.go`,
  `buildTemplateObject`) never set `spec.service` → every pool sandbox is
  created service-free.
- **Measured, round-3 leg B** (`gs://kops-state-142966328212/perf-bench-results/round3/B-round3/stress-test/metrics.jsonl.gz`,
  `apiserver_request_total` deltas over the run):

  | resource+verb | delta |
  |---|---|
  | services POST / DELETE | 0 / 1 (namespace cleanup) |
  | endpointslices POST / PUT / DELETE | 0 / 0 / 1 |
  | total mutating requests (all resources) | 9,863 |

  Top writers were sandboxes PATCH (30.3%), sandboxclaims PATCH (10.7%),
  pods PATCH (9.3%), cilium* (~19.5%). Service/EndpointSlice churn: **zero**.

So for the benchmark path there is nothing left to eliminate; the
`optimizations/SCALE-ROADMAP.md` §0 write table's "headless-Service CREATE
(+~2 EndpointSlice writes)" row should be read as *conditional on
`spec.service: true` templates*, and L3's "~20% of churn" as the cost a
production deployment pays for opting in — not a cost the round-3/4 numbers
contain.

## What `service: true` would cost (projection, same artifacts)

Per sandbox lifecycle with `spec.service: true`: 1 Service POST +
1 Service DELETE + ~2-3 EndpointSlice writes (create on sync, update on
pod-ready endpoints, cascade delete) ≈ **4-5 extra writes**, plus
EndpointSlice-controller and kube-proxy/DNS watch fan-out.

- At the 300-burst phase shape (600 sandbox lifecycles per full phase:
  300 pool-fill + 300 refill, all torn down): ~2,400-3,000 extra mutating
  writes on a measured base of 9,863 → **+25-30% total writes**.
- At sustained 1000 claims/s (1 sandbox lifecycle per claim in steady
  state): **+4-5k writes/s** on the ~20-25k/s churn budget → the ~20%
  figure in SCALE-ROADMAP holds *for opted-in deployments*.

Elimination therefore means: keep templates at `service: false`/unset, and
remove the reasons deployments opt in. The only structural reason found is
**router/SDK reachability by DNS** (e.g.
`examples/demo-cilium-egress/manifests/ingress/30-sandboxes-http.yaml`:
"create a headless Service so the router can also reach it by DNS").

## Consumer map (who depends on the per-sandbox Service / ServiceFQDN)

| consumer | mechanism today | service-free path | state |
|---|---|---|---|
| sandbox-router (Go) | headers → PodIP override → UID→PodIP informer cache → **DNS fallback `<id>.<ns>.svc.…`** | name-index cache lookup (ns+ID → PodIP) | **implemented this round** |
| Go SDK (`clients/go`) | sends `X-Sandbox-ID/-Namespace/-Port`, optional `X-Sandbox-Pod-IP` (`SetPodIP`); never sends UID | router name index covers the no-PodIP case; `SetPodIP` already bypasses DNS | works unchanged |
| Python SDK router mode (`clients/python/.../connector.py`, `async_connector.py`) | same headers; injects `X-Sandbox-Pod-IP` when it knows the IP | same as Go SDK | works unchanged |
| Python SDK in-cluster mode (`InClusterConnectionStrategy`) | prefers `status.podIPs` via `get_pod_ip`, DNS URL only as fallback | already podIP-first | works unchanged (DNS fallback dead for service-free sandboxes — acceptable, documented) |
| legacy Python router (`clients/python/.../sandbox-router/sandbox_router.py`) | DNS-only | none | design-only: requires `service: true` or migration to the Go router |
| e2e tests (`test/e2e/*_test.go`) | assert `status.serviceFQDN` for sandboxes | assertions match spec.service semantics already | no change needed |
| `Sandbox.status.serviceFQDN` / `.status.service` | set only when a Service exists | `status.podIPs`/`status.podName` are the service-free equivalents (already populated) | no change needed |
| Ready condition | requires Service **existence** only when requested (`computeReadyCondition` svcRequired) | `service: false`/nil → no Service dependency | no change needed |

## Implemented this round

1. **Router name-index resolution** — makes `spec.service: false`
   sandboxes routable with zero SDK changes:
   - `sandbox-router/cache/cache.go`: `names` index
     (`<ns>/<name>` → UID) maintained alongside the UID map; `GetByName`,
     `InvalidateByName`; UID-guarded removal so a late delete event for a
     replaced Pod cannot evict a same-named successor's entry.
   - `sandbox-router/proxy/resolve.go`: resolution order is now
     PodIP override → UID cache → **name cache** → DNS; new `Source`
     value `cache-name`.
   - `sandbox-router/proxy/proxy.go`: ErrorHandler actively invalidates
     name-resolved entries on dial-class failures (same KEP guarantee as
     the UID path).
   - Unit + integration tests for all of the above; README updated.
2. **Stress template pinned service-free** (`test/stress/phases.go`,
   `buildTemplateObject`): explicit `"service": false` so the benchmark
   remains service-free even if field defaulting ever changes. (This is
   the agreed one-line overlap with the stress-template surface.)

Benchmark readiness: `claims-warm` already runs with service-free pools;
no flag needed. An A/B leg for the projection above is trivial if wanted:
flip the same line to `"service": true` and diff
`apiserver_request_total{resource=~"services|endpointslices"}`.

## Design-only notes (deliberately not implemented)

- **Cluster-wide Service informer in the controller.**
  `SetupWithManager` still does `Owns(&corev1.Service{})`, so the manager
  cache lists/watches every Service in the cluster even when all
  sandboxes are service-free. The fix belongs with R4.2 (cache-level
  label selector `agents.x-k8s.io/sandbox-name-hash exists` +
  transform for Services in `cmd/` cache options) — not done here because
  the `cmd/` main flag/cache surface is owned by the parallel round-5
  agents. `reconcileService`'s unconditional `Get` is a cache read, not
  an API call; it costs nothing once the informer is label-scoped.
- **Router RBAC/deploy defaults.** `--cache-enabled` defaults to `false`;
  service-free deployments must enable it (README now says so). Flipping
  the default is a deploy-manifest decision, out of scope.
- **Legacy Python router** is DNS-only; users on it must keep
  `service: true` or move to the Go router. No code change planned.
- **`status.serviceFQDN` consumers outside this repo**: any user
  automation resolving the FQDN needs `status.podIPs` instead; the field
  is already documented as service-dependent.
