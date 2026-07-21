#!/usr/bin/env python3
"""A/B benchmark: SDK SandboxClaim ready-wait latency, legacy vs single-watch.

Measures, against a live cluster, the client-observed time from just before
SandboxClaim creation to the return of the ready wait:

  --mode legacy   create_sandbox_claim -> resolve_sandbox_name ->
                  wait_for_sandbox_ready          (two sequential watches;
                  runs against the upstream-main SDK)
  --mode single   create_sandbox_claim -> wait_for_claim_ready
                  (one watch on the claim; requires the P9 SDK)

Both modes time the identical span (claim create + ready-wait return) so the
numbers are directly comparable. Claims are issued sequentially; between
claims the previous claim is deleted and the warm pool is given time to
report a ready member again, so every iteration is a warm adoption.

Only dependencies: the `kubernetes` package and the `k8s_agent_sandbox` SDK
(whichever tree is pip-installed decides which mode is available).

Usage:
  python3 sdk-wait-ab.py --kubeconfig ~/.kube/config --mode legacy \
      --count 50 --output-json /tmp/legacy.json
"""

import argparse
import json
import logging
import os
import sys
import time
import uuid

# API coordinates (mirrors k8s_agent_sandbox.constants on upstream main).
EXT_GROUP = "extensions.agents.x-k8s.io"
EXT_VERSION = "v1beta1"
TEMPLATE_PLURAL = "sandboxtemplates"
WARMPOOL_PLURAL = "sandboxwarmpools"
CLAIM_PLURAL = "sandboxclaims"

POOL_READY_TIMEOUT_S = 600     # setup: wait up to 10m for the pool to warm up
REFILL_TIMEOUT_S = 30          # between claims: wait for >=1 ready member
PER_CLAIM_TIMEOUT_S = 120

DEFAULT_IMAGE = (
    "us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/"
    "python-runtime-sandbox:latest-main"
)


def parse_args(argv=None):
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--kubeconfig", help="Path to kubeconfig (default: in-cluster, then $KUBECONFIG/~/.kube/config)")
    p.add_argument("--mode", required=True, choices=["legacy", "single"],
                   help="legacy = two-watch path (upstream-main SDK); single = wait_for_claim_ready (P9 SDK)")
    p.add_argument("--count", type=int, default=50, help="Number of sequential claims (default 50)")
    p.add_argument("--namespace", default="sdk-ab")
    p.add_argument("--pool-size", type=int, default=60, help="SandboxWarmPool spec.replicas (default 60)")
    p.add_argument("--template-name", default="sdk-ab-template")
    p.add_argument("--pool-name", default="sdk-ab-pool")
    p.add_argument("--cleanup", choices=["none", "claims", "all"], default="claims",
                   help="claims: delete created claims (default); all: also delete pool, template, namespace")
    p.add_argument("--output-json", help="Write the summary + per-claim samples to this path")
    return p.parse_args(argv)


# ---------------------------------------------------------------------------
# Stats
# ---------------------------------------------------------------------------

def percentile(values, pct):
    """Percentile with linear interpolation between closest ranks.

    `values` need not be sorted; `pct` in [0, 100].
    """
    if not values:
        raise ValueError("percentile() of empty list")
    vals = sorted(values)
    if len(vals) == 1:
        return vals[0]
    rank = (pct / 100.0) * (len(vals) - 1)
    lo = int(rank)
    hi = min(lo + 1, len(vals) - 1)
    frac = rank - lo
    return vals[lo] * (1.0 - frac) + vals[hi] * frac


def summarize(mode, count, samples, failures):
    summary = {
        "mode": mode,
        "count": count,
        "successes": len(samples),
        "failures": failures,
        "per_claim_s": [round(s, 4) for s in samples],
    }
    if samples:
        summary.update({
            "mean_s": round(sum(samples) / len(samples), 4),
            "p50_s": round(percentile(samples, 50), 4),
            "p90_s": round(percentile(samples, 90), 4),
            "max_s": round(max(samples), 4),
        })
    else:
        summary.update({"mean_s": None, "p50_s": None, "p90_s": None, "max_s": None})
    return summary


# ---------------------------------------------------------------------------
# Setup phase (kubernetes client only; idempotent)
# ---------------------------------------------------------------------------

def load_config(kubeconfig):
    from kubernetes import config
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config(config_file=kubeconfig)


def ensure(create_fn, kind, name):
    """Run a create call, tolerating 409 AlreadyExists."""
    from kubernetes.client import ApiException
    try:
        create_fn()
        logging.info("Created %s '%s'", kind, name)
        return True
    except ApiException as e:
        if e.status == 409:
            logging.info("%s '%s' already exists", kind, name)
            return False
        raise


def template_manifest(name, namespace):
    # Modeled on clients/python/agentic-sandbox-client/python-sandbox-template.yaml
    return {
        "apiVersion": f"{EXT_GROUP}/{EXT_VERSION}",
        "kind": "SandboxTemplate",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {
            "podTemplate": {
                "spec": {
                    "containers": [{
                        "name": "python-runtime",
                        "image": DEFAULT_IMAGE,
                        "ports": [{"containerPort": 8888}],
                        "readinessProbe": {
                            "httpGet": {"path": "/", "port": 8888},
                            "initialDelaySeconds": 0,
                            "periodSeconds": 1,
                        },
                        "resources": {
                            "requests": {
                                "cpu": "250m",
                                "memory": "512Mi",
                                "ephemeral-storage": "512Mi",
                            }
                        },
                    }],
                    "restartPolicy": "OnFailure",
                }
            }
        },
    }


def warmpool_manifest(name, namespace, template_name, replicas):
    return {
        "apiVersion": f"{EXT_GROUP}/{EXT_VERSION}",
        "kind": "SandboxWarmPool",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {
            "replicas": replicas,
            "sandboxTemplateRef": {"name": template_name},
        },
    }


def pool_ready_replicas(custom_api, namespace, pool_name):
    pool = custom_api.get_namespaced_custom_object(
        group=EXT_GROUP, version=EXT_VERSION, namespace=namespace,
        plural=WARMPOOL_PLURAL, name=pool_name)
    return int((pool.get("status") or {}).get("readyReplicas") or 0)


def wait_pool_ready(custom_api, namespace, pool_name, want, timeout_s, poll_s=2.0):
    """Poll the pool status until readyReplicas >= want (or timeout)."""
    deadline = time.monotonic() + timeout_s
    last = -1
    while time.monotonic() < deadline:
        ready = pool_ready_replicas(custom_api, namespace, pool_name)
        if ready != last:
            logging.info("WarmPool '%s': readyReplicas=%d (want >=%d)", pool_name, ready, want)
            last = ready
        if ready >= want:
            return True
        time.sleep(poll_s)
    return False


def setup(args):
    from kubernetes import client
    core = client.CoreV1Api()
    custom = client.CustomObjectsApi()

    ensure(lambda: core.create_namespace(
        client.V1Namespace(metadata=client.V1ObjectMeta(name=args.namespace))),
        "Namespace", args.namespace)

    ensure(lambda: custom.create_namespaced_custom_object(
        group=EXT_GROUP, version=EXT_VERSION, namespace=args.namespace,
        plural=TEMPLATE_PLURAL,
        body=template_manifest(args.template_name, args.namespace)),
        "SandboxTemplate", args.template_name)

    created = ensure(lambda: custom.create_namespaced_custom_object(
        group=EXT_GROUP, version=EXT_VERSION, namespace=args.namespace,
        plural=WARMPOOL_PLURAL,
        body=warmpool_manifest(args.pool_name, args.namespace,
                               args.template_name, args.pool_size)),
        "SandboxWarmPool", args.pool_name)
    if not created:
        # Idempotent: make sure an existing pool has the requested size.
        custom.patch_namespaced_custom_object(
            group=EXT_GROUP, version=EXT_VERSION, namespace=args.namespace,
            plural=WARMPOOL_PLURAL, name=args.pool_name,
            body={"spec": {"replicas": args.pool_size}})
        logging.info("Patched WarmPool '%s' spec.replicas=%d", args.pool_name, args.pool_size)

    # Enough ready members to cover the run; pool-size above count is headroom
    # that keeps refills off the critical path.
    want = max(1, min(args.pool_size, args.count))
    logging.info("Waiting up to %ds for %d ready pool members...", POOL_READY_TIMEOUT_S, want)
    if not wait_pool_ready(custom, args.namespace, args.pool_name, want, POOL_READY_TIMEOUT_S):
        raise TimeoutError(
            f"WarmPool '{args.pool_name}' did not reach {want} readyReplicas "
            f"within {POOL_READY_TIMEOUT_S}s")
    return custom


# ---------------------------------------------------------------------------
# Measurement phase (k8s_agent_sandbox public API only)
# ---------------------------------------------------------------------------

def timed_claim(helper, mode, claim_name, pool_name, namespace):
    """One measured iteration: claim create -> ready-wait return, in seconds."""
    t0 = time.monotonic()
    created = helper.create_sandbox_claim(claim_name, pool_name, namespace)

    def remaining():
        return max(1, int(PER_CLAIM_TIMEOUT_S - (time.monotonic() - t0)))

    if mode == "single":
        # P9 SDK: one watch on the claim, anchored at the create response's
        # resourceVersion (upstream-main create_sandbox_claim returns None,
        # but mode single is only run against the P9 SDK anyway).
        rv = None
        if isinstance(created, dict):
            rv = (created.get("metadata") or {}).get("resourceVersion")
        helper.wait_for_claim_ready(claim_name, namespace, remaining(), resource_version=rv)
    else:
        # Upstream-main two-watch path.
        sandbox_id = helper.resolve_sandbox_name(claim_name, namespace, remaining())
        helper.wait_for_sandbox_ready(sandbox_id, namespace, remaining())
    return time.monotonic() - t0


def delete_claim_quiet(helper, claim_name, namespace):
    from kubernetes.client import ApiException
    try:
        helper.delete_sandbox_claim(claim_name, namespace)
    except ApiException as e:
        if e.status != 404:
            logging.warning("Failed to delete claim '%s': %s", claim_name, e)
    except Exception as e:  # noqa: BLE001 - cleanup must not kill the run
        logging.warning("Failed to delete claim '%s': %s", claim_name, e)


def run(args, custom_api):
    from k8s_agent_sandbox.k8s_helper import K8sHelper

    helper = K8sHelper()
    if args.mode == "single" and not hasattr(helper, "wait_for_claim_ready"):
        sys.exit("--mode single requires an SDK with wait_for_claim_ready "
                 "(install the P9 tree); the installed k8s_agent_sandbox lacks it.")

    samples, failures = [], 0
    for i in range(args.count):
        claim_name = f"ab-{args.mode}-{uuid.uuid4().hex[:8]}"
        try:
            elapsed = timed_claim(helper, args.mode, claim_name, args.pool_name, args.namespace)
            samples.append(elapsed)
            logging.info("[%d/%d] %s ready in %.3fs", i + 1, args.count, claim_name, elapsed)
        except KeyboardInterrupt:
            raise
        except Exception as e:  # noqa: BLE001 - count and continue
            failures += 1
            logging.error("[%d/%d] %s FAILED: %s", i + 1, args.count, claim_name, e)
        finally:
            if args.cleanup != "none":
                delete_claim_quiet(helper, claim_name, args.namespace)

        # Keep every iteration a warm adoption: give the pool time to report
        # a ready member again before the next claim.
        if i + 1 < args.count:
            if not wait_pool_ready(custom_api, args.namespace, args.pool_name,
                                   want=1, timeout_s=REFILL_TIMEOUT_S, poll_s=0.5):
                logging.warning("Pool had no ready member after %ds; next claim may be a cold start",
                                REFILL_TIMEOUT_S)
    return samples, failures


def cleanup_all(args):
    from kubernetes import client
    from kubernetes.client import ApiException
    custom = client.CustomObjectsApi()
    core = client.CoreV1Api()
    for plural, name in ((WARMPOOL_PLURAL, args.pool_name),
                         (TEMPLATE_PLURAL, args.template_name)):
        try:
            custom.delete_namespaced_custom_object(
                group=EXT_GROUP, version=EXT_VERSION, namespace=args.namespace,
                plural=plural, name=name)
            logging.info("Deleted %s '%s'", plural, name)
        except ApiException as e:
            if e.status != 404:
                logging.warning("Failed to delete %s '%s': %s", plural, name, e)
    try:
        core.delete_namespace(args.namespace)
        logging.info("Deleted namespace '%s'", args.namespace)
    except ApiException as e:
        if e.status != 404:
            logging.warning("Failed to delete namespace '%s': %s", args.namespace, e)


def main(argv=None):
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    args = parse_args(argv)

    if args.kubeconfig:
        # K8sHelper loads config itself with no arguments; point it (and the
        # setup-phase clients) at the requested kubeconfig via the env var.
        os.environ["KUBECONFIG"] = os.path.abspath(os.path.expanduser(args.kubeconfig))
    load_config(args.kubeconfig)

    custom_api = setup(args)
    try:
        samples, failures = run(args, custom_api)
    finally:
        if args.cleanup == "all":
            cleanup_all(args)

    summary = summarize(args.mode, args.count, samples, failures)

    print()
    print(f"mode={summary['mode']} count={summary['count']} "
          f"successes={summary['successes']} failures={summary['failures']}")
    if summary["mean_s"] is not None:
        print(f"mean={summary['mean_s']:.3f}s p50={summary['p50_s']:.3f}s "
              f"p90={summary['p90_s']:.3f}s max={summary['max_s']:.3f}s")
    if args.output_json:
        with open(args.output_json, "w") as f:
            json.dump(summary, f, indent=2)
        print(f"summary written to {args.output_json}")

    if failures > 0.10 * args.count:
        sys.exit(f"FAIL: {failures}/{args.count} claims failed (>10%)")


if __name__ == "__main__":
    main()
