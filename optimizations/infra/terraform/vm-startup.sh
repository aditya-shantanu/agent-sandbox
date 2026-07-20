#!/bin/bash
# Startup script for perf-bench-runner: bootstrap once, then launch the
# benchmark orchestrator detached. Idempotent across reboots via marker file.
set -u
MARKER=/root/.bench-started-v11
LOG=/root/startup-bench.log
exec >>"${LOG}" 2>&1

if [ -f "${MARKER}" ]; then
  echo "[$(date -u +%FT%TZ)] marker present, not relaunching"
  exit 0
fi
touch "${MARKER}"
echo "[$(date -u +%FT%TZ)] fetching scripts"
gcloud storage cp 'gs://kops-state-142966328212/perf-bench-scripts/vm-bootstrap.sh' /root/
gcloud storage cp 'gs://kops-state-142966328212/perf-bench-scripts/vm-orchestrate.sh' /root/

echo "[$(date -u +%FT%TZ)] bootstrap"
if ! bash /root/vm-bootstrap.sh; then
  echo "[$(date -u +%FT%TZ)] BOOTSTRAP FAILED"
  gcloud storage cp "${LOG}" gs://kops-state-142966328212/perf-bench-results/round9b/startup-bench.log -q || true
  rm -f "${MARKER}"   # allow retry on next reset
  exit 1
fi

echo "[$(date -u +%FT%TZ)] launching orchestrator"
nohup bash /root/vm-orchestrate.sh > /root/orchestrate.log 2>&1 &
disown
gcloud storage cp "${LOG}" gs://kops-state-142966328212/perf-bench-results/round9b/startup-bench.log -q || true
echo "[$(date -u +%FT%TZ)] launched"
