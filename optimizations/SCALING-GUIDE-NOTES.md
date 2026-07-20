# SCALING-GUIDE-NOTES — mining `scaling-guide.pdf` (GKE Agent Sandbox best-practices, 15 pp.) against the perf-investigation bench

Source: `/Users/adityashantanu/repos/agent-sandbox/scaling-guide.pdf` ("Best Practices for Scaling Agentic Workloads on GKE Agent Sandbox", GKE Standard mode, p. 1). Compared against `perf-investigation-master` at `/tmp/bench-candidate-tree`: `test/benchmarks/scenarios/benchmarks-kops-gcp/run`, `k8s/apf-insulation.yaml`, `optimizations/{ROUND7-PLAN,SCALE-ROADMAP,RESULTS}.md`, and the controller flag defaults in `cmd/agent-sandbox-controller/main.go:129-145`.

**Scope caveat, up front:** the guide's node-sizing and storage numbers are derived from **gVisor/runsc** overhead — syscall translation thread pools (p. 4), shim startup I/O (pp. 3-4). Our kops bench runs plain runc pods with no RuntimeClass, so those rationales transfer only partially (disk IOPS and kubelet-starvation pressure transfer; syscall-translation CPU does not). The guide also never mentions APF, CBOR, transport sharding, watch-path costs, or write-count reduction — it independently confirms ROUND7 §2.3's novelty claim for those three contributions.

---

## 1. Setting-by-setting inventory: PDF recommendation vs our bench

Legend: **Δ** = delta worth testing; ✓ = already at/beyond the PDF value; ✗ = we deliberately diverge (see §3); n/a = doesn't apply to the bench.

### 1.1 Controller flags (PDF p. 7 table, restated p. 11 manifest)

| flag | PDF default | PDF high-scale | Our binary default (`main.go:129-145`) | Our bench value (run script `CONTROLLER_ARGS` / RESULTS header) | verdict |
|---|---|---|---|---|---|
| `--kube-api-qps` | -1 | **-1** ("disables client-side rate limiting entirely — already optimal", p. 7) | -1 | **1000** (burst 2000) | **Δ — our bench args RE-ENABLE client throttling that the default disables.** The recommended `CONTROLLER_ARGS` (`run:150-153`, RESULTS header) predate the -1 default. At sustained 300-1000/s × ~2-5 writes/claim + informer relists, a 1000-QPS token bucket is a real cork. Drop both flags. |
| `--kube-api-burst` | 10 | 10 ("consulted only when QPS > 0", p. 7) | 10 | 2000 | Δ (moot once qps=-1) |
| `--sandbox-concurrent-workers` | 1 | **1000** (p. 7) | 1 | 400 | Δ — A/B 400 vs 1000 at sustained ≥500/s only (see §3.3) |
| `--sandbox-claim-concurrent-workers` | 50 | **1000** (p. 7) | 50 | 400 | Δ — same A/B |
| `--sandbox-warm-pool-concurrent-workers` | 1 | **1000** (p. 7) | 1 | **1** (deliberate, `run:154-156`) | ✗ — contradicts our burst finding (§3.1); test = pool-count, not 1000, in the multi-pool sustained phase |
| `--sandbox-template-concurrent-workers` | 1 | **1000** (p. 7) | 1 | 1 (default) | Δ only for multi-template runs; single-template bench: no-op. Pairs with issue #1182 (template→pools fan-out, ROUND7 item 8e). |
| `--sandbox-warm-pool-max-batch-size` | 300 | **1000** (p. 7) | 300 | 300 (default) | Δ for pools >300; ROUND7/R4.6 goes the *opposite* way (rate-limit refill, not bigger batches) — test only with `--sandbox-warm-pool-max-refill-rate` shaping in place |
| controller `replicas` | — | **2** with `--leader-elect` (p. 11) | 1 | 1 | n/a for latency (standby replica; failover already ≤2s per ROUND7 §6) |
| SDK client pool (`connection_pool_maxsize`) | — | **1000** (p. 7) | — | stress client: `--client-connections`/transport sharding | ✓ mechanism-wise; fold the urllib3 number into ROUND7 item 9's SDK guidance (§2, idea I) |

### 1.2 Node & storage (PDF pp. 3-5, 8-9)

| setting | PDF value | Our bench | verdict |
|---|---|---|---|
| Worker VM family/size | C4/C4d or N2/N2d, **≥16 vCPU**, 1:4 CPU:mem; avoid E2/N1 (p. 4); `c4-standard-16` in the recipe (p. 9) | `n2-standard-8` workers (`run:272`) | **Δ** — but the PDF's rationale is runsc thread pools (pp. 3-4), absent on our bench. Still worth one leg at the churn phases: fewer/larger nodes concentrate cilium-agent + kubelet work per node. Watch the pod-slot budget: 110 pods/node regardless, so 3×n2-standard-16 has *fewer* slots than 6×n2-standard-8. |
| CP size | not separately specified | `n2-standard-16` on tuned runs (`CP_MACHINE_TYPE`, `run:186`; RESULTS round-4/5) | ✓ ahead of the guide (it defers CP tuning to Google Support, p. 6) |
| Boot disk | **pd-ssd or hyperdisk-balanced, ≥200 GB** — pd-balanced <100 GB caps at 3,000 IOPS → gVisor shim freezes/CRI timeouts at 2,000-pod bursts (pp. 4-5, 8-9, 14) | ~~unset~~ landed as TUNE_NODES (pd-ssd, 200GB default / 100GB at node scale) since gate zero | ✓ done — *round-9b note: the refill wall turned out to be scheduler QPS then controller pipelines, NOT node I/O (nodes exonerated at 300/s: scheduled→running p50 1.0-2.3s on both 6k- and 3k-IOPS disks); this row remains correct hygiene, not a ceiling-mover at ≤300/s* |
| Local NVMe SSD for ephemeral | `--ephemeral-storage-local-ssd count=2` for 300+ simultaneous pod startups/node; bulk pod startup **136s → 51s (2.6×)** (pp. 5, 9) | not used | ✓ covered by ROUND7 item 8a (justinsb PRs #1203-#1208, NVMe ephemeral) — PDF's 2.6× number is a useful independent anchor for the expected win |
| Kubelet system reserves | **2 vCPU + 8 GiB** static reserve on high-density nodes (p. 5) | kops defaults, untouched | **Δ** — see §2 idea B |
| KubeletConfig recipe | `cpuManagerPolicy: static`, `cpuCFSQuota: true` @100ms, `podPidsLimit: 4096`, `evictionSoft memoryAvailable: 8Gi`/1m grace, `evictionMaxPodGracePeriodSeconds: 30` (p. 9) | kops defaults | Δ, selectively — `podPidsLimit`+evictionSoft yes; `cpuManagerPolicy: static` questionable for us (§3.4) |
| NodeSwap | enable on GKE 1.34+, taint `gke-swap=enabled:NoSchedule` (p. 5) | off | n/a for the latency bench (no OOM signal in any round); density lever for production docs only |
| Pod density profile | 0.5 vCPU/512 MiB, 50-100+ sandboxes/node (p. 2) | fill 10/node + claims-warm pools | context only |

### 1.3 Control plane & etcd (PDF pp. 3, 5-6, 14-15)

| setting | PDF value | Our bench | verdict |
|---|---|---|---|
| API server inflight/APF | **not covered** — "API Server Timeout… contact Google Cloud Support" (p. 6) | TUNE_CONTROL_PLANE: `maxRequestsInflight=3000`, `maxMutatingRequestsInflight=1000` (`run:254-255`); APF insulation with dedicated PLs, share math in `k8s/apf-insulation.yaml:44-62` | ✓ far ahead; nothing to import |
| Scheduler | "High-Throughput Scheduler" named, zero numbers (p. 3) | ~~`kubeAPIQPS=800/1600`~~ **round-9b correction: the `kubeScheduler.kubeAPIQPS` key is silently INERT on kops v1.35** (flag-tagged field never mapped into clientConnection) — every "tuned" cluster through 9b-SUST3 ran the scheduler at default 50 QPS (= the measured 47-50 binds/s supply wall). Fixed in the run script via `kubeScheduler.qps=800`/`burst=1600` | **✓ ahead only since the 9b fix** |
| KCM | not covered | `kubeAPIQPS=200/400` (`run:258-259`) | ✓ ahead |
| **Event TTL** | **`event_ttl_sec` = 900 (15m)** vs 1h default; excessive events from churn are the root cause of etcd capacity exhaustion (pp. 14-15) | default 1h | **Δ — genuinely new, see §2 idea A** |
| etcd storage quota | default 6 GB/12 GB quota (p. 3); keep **active pod data ≤ 10 GiB**, **≤ 300k pods**, **≤ 10 KB per pod spec** (pp. 5, 15) | untracked | Δ (monitoring + spec-size audit; §2 ideas C/D) |
| Telemetry watch scoping | field-selector (`spec.nodeName=$NODE`) or namespace-scope monitoring-agent pod watches (pp. 6, 12-13) | n/a — stress tool scrapes `/metrics` directly, no watch-based telemetry on the bench | n/a for bench; one line for production guidance (§2 idea E). Also validates ROUND7 item 5(a) (minimize watcher count) from the other direction. |

### 1.4 Cilium / Dataplane V2 (PDF pp. 3-4, 6, 10, 14)

| setting | PDF value | Our bench | verdict |
|---|---|---|---|
| Identity label exclusion | `label-clean-patterns: "agent-id=* session-id=* timestamp=* uuid=*"` in cilium-config (p. 6); detection one-liner via `kubectl get ciliumidentities` (p. 10); patch syntax `labels: "!timestamp"` + restart (p. 10) | not applied | ✓ already planned as ROUND7 item 8d (issue #836, exclude `agents.x-k8s.io/*`) — **PDF contributes the concrete config-key syntax and the detection command**; fold both into 8d's execution |
| endpoint-create rate limit | **RATE_VALUE 4-8/s, BURST=1.5×rate, `auto-adjust:false`** (p. 14) | **100/s, burst 100, parallel-requests 32, auto-adjust false** (`run:313-314`) | ✗ **contradicts our measurement** — §3.2 |
| cilium k8s-client QPS | **not mentioned at all** | `k8s-client-qps=50, burst=100` (`run:313-314`) — measured as the next cork after the limiter (~0.4s/POST, ~1.4s/DELETE throttle at default 5/10; `run:308-312`) | ✓ ahead; PDF has a gap here |

### 1.5 Monitoring (PDF p. 13)

| setting | PDF value | Our bench | verdict |
|---|---|---|---|
| PSI (Pressure Stall Information) | track node memory/disk `stall_seconds_total`, alert at **>5% over 3 min** (p. 13) | not collected | Δ — §2 idea F; direct instrumentation for the refill/churn wall |

---

## 2. Ideas the guide has that ROUND7-PLAN does not already cover

Strictly cross-checked against ROUND7 §4 items 1-9 and the §6 dead-end list. Most of the guide is *behind* our plan (concurrency flags ≈ our round-1 work; local SSD/cilium identity ≈ item 8; client pooling ≈ items 3b/9). What is genuinely not in the plan:

**A. Cluster event-TTL reduction (`eventTTL: 15m`) — PDF pp. 14-15.**
Not in any ROUND7 item or dead-end. Our `--disable-claim-events` flag (round 2) silences only *our controller's* events; at sustained churn the kubelet/scheduler/GC event stream per pod create+delete remains, at 1h retention (PDF p. 15 names default 1-hour Event TTL as the root cause of control-plane DB exhaustion). Applies to the **refill/churn pipeline**: at 1000 pods/s, events are a large share of etcd storage growth and background write/compaction load on the `etcd-events` cluster, and long sustained runs (the exact phase gate zero introduces) accumulate for an hour. Expected impact: negligible on burst-300 p90; protects sustained-rate runs from a mid-run etcd-events degradation and shrinks compaction pauses. Effort: **S** — one kops `--set` at creation. Belongs beside TUNE_CONTROL_PLANE.

**B. Kubelet static system reservations + soft eviction (PDF pp. 5, 9).**
ROUND7 item 8a covers containerd/fdatasync/NVMe; nothing in the plan protects **kubelet/containerd/cilium-agent CPU+memory from the pods themselves** during refill storms (PDF's mechanism: burst pods starve system daemons on high-density nodes, p. 5). Applies to the refill/churn pipeline: pod-start p99 stability at 50-110 pods/node, i.e. the same wall as 8a but from the scheduling side. Expected impact: none at burst-300 (10 fill pods/node); tail-stabilizer for throughput-mif and 1000/s phases. Effort: **S** (kops `cluster.spec.kubelet` fields at creation). Take `systemReserved` + `podPidsLimit` + `evictionSoft`; skip `cpuManagerPolicy: static` (§3.4).

**C. Explicit boot-disk sizing for worker nodes (PDF pp. 4-5, 14).**
Item 8a jumps straight to NVMe; the guide's cheaper first step — **pd-ssd ≥200 GB boot disk** to escape the small-disk 3,000-IOPS cap — is not in the plan, and our run script provisions kops GCE default volumes (unset, `run:262-283`). Applies to the refill ceiling (70-85/s per pool): if the current node boot disks are small/pd-balanced-class, part of that ceiling may be bought back for the cost of two kops flags, and it de-noises the NVMe A/B (otherwise the NVMe leg conflates "local SSD" with "not IOPS-starved"). Expected impact: on refill/churn throughput only. Effort: **S**. *(Round-10 status: landed as TUNE_NODES since gate zero; round-9b then showed the ≤300/s refill ceiling was scheduler-QPS/controller-pipeline-bound, not disk-bound — keep for hygiene, expect nothing from it below ~500/s.)*

**D. Pod-spec size budget (≤10 KB/pod, ≤10 GiB active, ≤300k pods — PDF pp. 5, 15).**
No ROUND7 item audits our object sizes (the plan audits write *counts*, ROUND6 ledger). A one-off audit of the chrome-sandbox pod spec + Sandbox/SandboxClaim encoded sizes belongs in gate zero: object bytes multiply through every per-watcher encode (ROUND7 item 5a) and every etcd write. Expected impact: informational unless we're fat; if the pod spec is >10 KB, trimming it is a multiplicative win across S1/S4 encode and etcd. Effort: **S** (audit only).

**E. Telemetry watch-scoping guidance (PDF pp. 6, 12-13).**
n/a to the bench itself (no watch-based telemetry), but it is a **production co-tenant hazard for our latency SLO**: a global pod-watch from a monitoring agent during churn adds per-watcher encode load on the exact apiserver serving S1/S4. One paragraph in the item-9 client/ops guidance doc: "scope monitoring pod watches by `spec.nodeName` field selector or namespace". Effort: **S** (docs).

**F. PSI-based node stall monitoring (PDF p. 13, 5%/3-min threshold).**
The plan's measurement additions (SCALE-ROADMAP §3) cover etcd/APF/scheduler dashboards but nothing node-side beyond kubelet metrics. PSI (`/proc/pressure/*` via node metrics) is the correct leading indicator for the 8a I/O wall — it distinguishes "disk-stalled" from "CPU-starved" per node during refill, which is exactly the attribution question 8a asks. Effort: **S** (add to the stress tool's scrape set on workers).

**G. (Enrichment, not new)** Cilium identity work is already item 8d, but adopt the PDF's concrete mechanics: `label-clean-patterns` config key (p. 6), the `ciliumidentities` cardinality-detection pipeline (p. 10), the operator-managed-identities caveat requiring a CP re-roll (p. 10), and the `anetd` restart note (p. 10 — GKE name; on kops it's the `cilium` DaemonSet, which our script already handles via pod deletion, `run:315-320`).

**H. (Enrichment)** Python SDK `connection_pool_maxsize` (p. 7) → fold into item 9's SDK guidance next to the jitter formula and #1089 idempotent-create.

Explicitly *not* new despite appearances: 16-vCPU node sizing (≈ our CP work + gVisor-specific), local SSD (item 8a), cilium rate limits (done, better than PDF — §3.2), controller concurrency (round 1 + §3.3), etcd quota pressure (items 8b/SCALE-ROADMAP compaction), scheduler throughput (TUNE_CONTROL_PLANE).

---

## 3. Where the guide contradicts our measured findings

**3.1 `--sandbox-warm-pool-concurrent-workers=1000` (PDF p. 7) vs our measured burst configuration.**
We run warm-pool workers = **1** during claim bursts, deliberately: replenishment creates competing with adoption writes was a measured failure mode — it is half the reason `k8s/apf-insulation.yaml` exists ("the controller's OWN bulk traffic — warm-pool replenishment creates… shares the same seats as the latency-critical adoption writes (self-interference)", apf-insulation.yaml:10-13), plus `--sandbox-warm-pool-replenish-delay=20s` on every candidate run (RESULTS header). The PDF's blanket 1000 optimizes pool *warm-up* throughput and ignores the burst-window contention we measured. Correct synthesis: workers ≈ pool count for multi-pool sustained phases (per the comment at `extensions/controllers/sandboxwarmpool_controller.go:110`), never 1000, and only with APF bulk insulation + refill-rate shaping (R4.6) in place.

**3.2 Cilium endpoint-create rate limit 4-8/s (PDF p. 14) vs our measured 100/s.**
The PDF recommends `rate-limit:4-8/s, rate-burst:1.5×` as the *fix* for `putEndpointIdTooManyRequests`. Our data says the limiter itself, not the datapath, is the pod-launch bottleneck: run 2077526390265090048 measured **20.8s mean limiter wait vs 1.6s processing** at throughput-mif200, with actual endpoint work ~100ms; we run **100/s, burst 100, parallel-requests 32, auto-adjust:false** (`run:294-314`) and it holds. At 4-8/s per node, a 110-pod node fill takes 14-27 minutes of limiter wait alone. The PDF's number is a WAI-protective floor for clusters where cilium-agent is the victim; for a churn-optimized platform it would recreate the exact pathology we removed. Also: the PDF never mentions cilium-agent's own k8s client QPS (default 5/10), which we measured as the next cork (~0.4s/POST, ~1.4s/DELETE of client-side throttle; `run:308-312`) — anyone applying only the PDF's fix hits our second wall immediately.

**3.3 "More workers = more throughput" framing (PDF pp. 4, 7) vs the measured seat wall.**
The PDF's causal story is "single-threaded queues serialize claims → set 1000 workers" (p. 4). Round 2 measured the opposite regime past ~100 in-flight: with 400 workers, write RTTs inflated 3-5× over the ~30ms server commit (update 110 / patch 216 / status 179ms p50, RESULTS Leg B) — the constraint was HTTP/2 stream limits + APF seats, fixed by transport sharding + TUNE_CONTROL_PLANE, not worker count. 1000 workers without those would deepen the in-flight queue and worsen p90. Test 400→1000 only at sustained ≥500/s on the tuned CP, expecting flat-to-modest gains.

**3.4 `cpuManagerPolicy: static` (PDF p. 9) vs the PDF's own workload profile.**
Static CPU manager gives exclusive cores only to Guaranteed pods with integer CPU requests; the PDF's own profile is 0.5 vCPU sandboxes (p. 2), which get nothing from it while the shared pool shrinks as any integer-request pod lands. For our bench pods this is at best a no-op and at worst added tail jitter. Adopt the rest of the p. 9 KubeletConfig without this key.

**3.5 (Consistency check, not contradiction)** PDF p. 7 marks `--kube-api-qps=-1` as "already optimal" — it is *our own bench args* that contradict this, still pinning 1000/2000 (`run:150`, RESULTS header). The PDF is right; fix our side (§4).

---

## 4. Ready-to-apply settings diff (first-wave deltas for the bench)

```bash
# ==== scaling-guide.pdf deltas — first test wave ====
# (A) Controller: stop re-enabling client-side throttling (PDF p.7; binary default is already -1).
#     Drop --kube-api-qps/--kube-api-burst from CONTROLLER_ARGS entirely:
CONTROLLER_ARGS="--leader-elect=true --enable-pprof-debug --zap-log-level=debug \
  --zap-encoder=json \
  --sandbox-concurrent-workers=400 --sandbox-claim-concurrent-workers=400 \
  --sandbox-warm-pool-concurrent-workers=1 --enable-webhook=false"
#     Sustained-phase A/B leg only (after gate zero, tuned CP):
#       --sandbox-concurrent-workers=1000 --sandbox-claim-concurrent-workers=1000
#     Multi-pool sustained leg: --sandbox-warm-pool-concurrent-workers=<pool count>  # NOT 1000

# (B) kops creation flags (TUNE_CONTROL_PLANE companion; creation-time only, like the rest):
TUNE_KOPS_FLAGS+=(
  # Event TTL 1h -> 15m: bounds etcd-events growth during sustained churn (PDF pp.14-15)
  "--set" "cluster.spec.kubeAPIServer.eventTTL=15m0s"
  # Kubelet system reserves + PID/eviction hygiene for refill storms (PDF pp.5,9)
  # (deliberately WITHOUT cpuManagerPolicy=static — see notes §3.4)
  "--set" "cluster.spec.kubelet.systemReserved.cpu=2000m"
  "--set" "cluster.spec.kubelet.systemReserved.memory=8Gi"
  "--set" "cluster.spec.kubelet.podPidsLimit=4096"
)
# Worker boot disks: pd-ssd, 200GB (PDF pp.4-5,8-9; kops leaves GCE defaults today):
#   add to `kops create cluster`:   --node-volume-size=200
#   then before `kops update cluster --yes`, set the node IG volume type:
#     kops get ig --name "${CLUSTER_NAME}" -o yaml \
#       | sed 's/^\(  rootVolumeSize:.*\)/\1\n  rootVolumeType: pd-ssd/' … (or kops edit ig)
#   [verify exact IG field: spec.rootVolumeType on GCE]

# (C) Cilium: NO change to endpoint-create limits — keep 100/s (PDF's 4-8/s would recreate
#     the measured limiter bottleneck, run 2077526390265090048). When running ROUND7 item 8d,
#     use the PDF's mechanism (p.6/p.10) against our identity-heavy labels:
#     kubectl patch cm -n kube-system cilium-config --type merge \
#       --patch '{"data":{"label-clean-patterns":"agents.x-k8s.io/* extensions.agents.x-k8s.io/*"}}'
#     detection: kubectl get ciliumidentities -ojsonpath='{.items[*].security-labels}' | … (p.10)
#     (verify NetworkPolicy semantics that reference these labels first — ROUND7 8d caveat)

# (D) Measurement: add worker-node PSI capture (/proc/pressure via node metrics) to the
#     stress scrape set; flag legs where memory/disk 'some' stall >5% over 3min (PDF p.13).
```

Ordering within the existing plan: (A) rides along with gate zero (it's a client-side-of-controller correctness fix, zero risk); (B) joins the next fresh-cluster creation alongside TUNE_CONTROL_PLANE; the boot-disk change lands **before** the item-8a NVMe A/B so the NVMe leg measures NVMe, not IOPS starvation; (C)/(D) fold into items 8d and the measurement additions respectively.

### Bottom line
The guide is directionally aligned but generally *behind* this investigation (its concurrency advice ≈ our round 1; it has no answer for APF, write-count reduction, CBOR, or transport limits, and its cilium rate-limit number is contradicted by our data). Its real value to us: **four cheap, untested infra knobs** — event TTL 15m, explicit pd-ssd/200GB worker boot disks, kubelet system reserves, PSI monitoring — all aimed at the refill/churn pipeline (the 500→1000/s track), none at burst-300 p90; plus one embarrassing catch: our recommended `CONTROLLER_ARGS` still re-enable client-side rate limiting that the controller's own default (-1) disables.

---

