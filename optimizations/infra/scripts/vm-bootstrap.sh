#!/bin/bash
# Bootstrap the perf-bench-runner VM: docker+buildx+qemu, go, gcloud docker auth, repo clones.
set -euxo pipefail

sudo apt-get update -q
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q git docker.io docker-buildx python3-venv jq curl

sudo systemctl enable --now docker

# Go toolchain matching the repo (Dockerfile uses golang:1.26.5)
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q go1.26; then
  curl -fsSL https://go.dev/dl/go1.26.5.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
fi

# qemu binfmt for the arm64 half of the multi-arch image build
sudo docker run --privileged --rm tonistiigi/binfmt --install arm64

# kubectl (not in Ubuntu's default repos; the benchmark script needs it)
if ! command -v kubectl >/dev/null; then
  curl -fsSLo /tmp/kubectl "https://dl.k8s.io/release/v1.35.0/bin/linux/amd64/kubectl"
  sudo install -m 0755 /tmp/kubectl /usr/local/bin/kubectl
fi

# gcloud comes with the GCE Ubuntu image; make sure docker can auth to gcr via the VM service account
sudo gcloud auth configure-docker gcr.io -q

# Clones: one tree per benchmark variant (runs execute in-tree and stomp .build).
# Idempotent: hard-reset existing clones to the latest branch tips.
clone_or_update() { # $1=dir $2=branch
  if [ -d "$1/.git" ]; then
    sudo git -C "$1" fetch -q origin "$2"
    sudo git -C "$1" reset -q --hard "origin/$2"
    sudo git -C "$1" clean -qfdx
  else
    sudo git clone -q https://github.com/aditya-shantanu/agent-sandbox.git "$1"
    sudo git -C "$1" checkout -q -B bench "origin/$2"
  fi
}
clone_or_update /root/repo-baseline perf-baseline-bench
clone_or_update /root/repo-candidate perf-investigation-master

sudo /usr/local/go/bin/go version
sudo docker buildx version
echo "BOOTSTRAP OK"
