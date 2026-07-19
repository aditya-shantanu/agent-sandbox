#!/bin/bash
# Launch (or relaunch) the benchmark queue on the perf-bench-runner VM without
# SSH: upload the current scripts to GCS, install the startup script as
# instance metadata, and reset the VM. The startup script bootstraps the VM
# and starts the orchestrator detached; progress streams to GCS.
#
# Usage: launch-runner.sh [--create]
#   --create   create the VM first (otherwise it must already exist; or use
#              the terraform in ../terraform instead)
#
# Monitor:  gcloud storage ls gs://kops-state-142966328212/perf-bench-results/latest/
#           (heartbeat-*.log refresh every 3 min; RESULTS.txt per finished run)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT="${PROJECT:-gke-ai-eco-dev}"
ZONE="${ZONE:-us-central1-a}"
VM="${VM:-perf-bench-runner}"
SCRIPTS_GCS="gs://kops-state-142966328212/perf-bench-scripts"

if [[ "${1:-}" == "--create" ]]; then
  gcloud compute instances create "${VM}" \
    --project="${PROJECT}" --zone="${ZONE}" \
    --machine-type=n2-standard-16 \
    --image-family=ubuntu-2404-lts-amd64 --image-project=ubuntu-os-cloud \
    --boot-disk-size=100GB \
    --scopes=cloud-platform \
    --labels=purpose=agent-sandbox-perf-bench
fi

echo "Uploading VM scripts to ${SCRIPTS_GCS}..."
gcloud storage cp "${SCRIPT_DIR}/vm-bootstrap.sh" "${SCRIPT_DIR}/vm-orchestrate.sh" "${SCRIPTS_GCS}/"

echo "Installing startup script + resetting ${VM}..."
gcloud compute instances add-metadata "${VM}" --project="${PROJECT}" --zone="${ZONE}" \
  --metadata-from-file startup-script="${SCRIPT_DIR}/../terraform/vm-startup.sh"
gcloud compute instances reset "${VM}" --project="${PROJECT}" --zone="${ZONE}"

echo "Launched. NOTE: the startup script is idempotent via a marker file"
echo "(/root/.bench-started-v2). To force a re-run on an already-used VM, bump"
echo "the marker name in vm-startup.sh before re-running this script."
