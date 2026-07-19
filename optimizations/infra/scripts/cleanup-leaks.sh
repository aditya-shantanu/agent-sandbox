#!/bin/bash
# Sweep for leaked benchmark resources: kops clusters (sandbox-YYYYMMDD-HHMMSS)
# whose runs died without cleanup, and optionally the runner VM.
#
# Usage: cleanup-leaks.sh [--delete] [--delete-vm]
#   default    list only
#   --delete   delete every leaked benchmark cluster found
#   --delete-vm also delete the perf-bench-runner VM
set -euo pipefail

PROJECT="${PROJECT:-gke-ai-eco-dev}"
export KOPS_STATE_STORE="${KOPS_STATE_STORE:-gs://kops-state-142966328212}"

echo "== kops state store clusters =="
CLUSTERS=$(gcloud storage ls "${KOPS_STATE_STORE}/" 2>/dev/null | grep -oE 'sandbox-[0-9]{8}-[0-9]{6}' || true)
echo "${CLUSTERS:-<none>}"

echo "== live instances matching sandbox-* =="
gcloud compute instances list --project="${PROJECT}" \
  --filter="name~'sandbox-2026'" --format="table(name,zone,status)" || true

echo "== networks matching sandbox-* (leak indicator even when instances are gone) =="
gcloud compute networks list --project="${PROJECT}" \
  --filter="name~'sandbox-2026'" --format="value(name)" || true

if [[ "${1:-}" == "--delete" || "${2:-}" == "--delete" ]]; then
  if ! command -v kops >/dev/null; then
    GOBIN=/tmp/kops-bin go install k8s.io/kops/cmd/kops@v1.35.0
    export PATH=/tmp/kops-bin:$PATH
  fi
  for c in ${CLUSTERS}; do
    echo "Deleting cluster ${c}..."
    kops delete cluster --name "${c}" --yes || echo "WARN: delete failed for ${c}"
  done
fi

if [[ "${1:-}" == "--delete-vm" || "${2:-}" == "--delete-vm" ]]; then
  gcloud compute instances delete perf-bench-runner --project="${PROJECT}" --zone=us-central1-a --quiet
fi
