# Benchmark runner VM for the sandbox-claim latency investigation.
#
# The VM runs the entire benchmark pipeline in-cloud (image builds, kops
# cluster lifecycle, stress client, artifact upload) so runs survive operator
# laptops going offline and client-observed latencies have no WAN component.
#
# The ephemeral kops benchmark clusters are NOT managed here — kops owns their
# lifecycle (created/deleted per run, or kept via KEEP_CLUSTER=true; see
# ../scripts/ and optimizations/INFRASTRUCTURE.md).
#
# To adopt the already-running VM instead of creating a new one:
#   terraform import google_compute_instance.perf_bench_runner \
#     projects/gke-ai-eco-dev/zones/us-central1-a/instances/perf-bench-runner

terraform {
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 5.0"
    }
  }
}

provider "google" {
  project = var.project
  zone    = var.zone
}

resource "google_compute_instance" "perf_bench_runner" {
  name         = var.runner_name
  machine_type = var.runner_machine_type
  zone         = var.zone

  boot_disk {
    initialize_params {
      image = "ubuntu-os-cloud/ubuntu-2404-lts-amd64"
      size  = 100
    }
  }

  network_interface {
    network = "default"
    access_config {} # ephemeral external IP (image pulls, go module downloads)
  }

  # Default compute service account with cloud-platform scope: needed for
  # gcr.io pushes, the kops state bucket, cluster API calls, and artifact
  # uploads. Matches what the kops clusters themselves use.
  service_account {
    scopes = ["https://www.googleapis.com/auth/cloud-platform"]
  }

  # Bootstraps docker/buildx/qemu/go/kubectl, clones the benchmark branches,
  # and launches the orchestrator. Idempotent across reboots via a marker
  # file; bump the marker name in the script to force a relaunch on reset.
  metadata_startup_script = file("${path.module}/vm-startup.sh")

  labels = {
    purpose = "agent-sandbox-perf-bench"
  }
}

output "runner_instance" {
  value = "${google_compute_instance.perf_bench_runner.name} (${google_compute_instance.perf_bench_runner.zone})"
}
