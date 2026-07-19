variable "project" {
  description = "GCP project for the perf investigation"
  type        = string
  default     = "gke-ai-eco-dev"
}

variable "zone" {
  description = "Zone for the runner VM (keep in the same region as the kops clusters: us-central1)"
  type        = string
  default     = "us-central1-a"
}

variable "runner_name" {
  description = "Name of the benchmark runner VM"
  type        = string
  default     = "perf-bench-runner"
}

variable "runner_machine_type" {
  description = "Machine type; 16 vCPUs keeps qemu-emulated arm64 image builds tolerable"
  type        = string
  default     = "n2-standard-16"
}
