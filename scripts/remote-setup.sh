#!/usr/bin/env bash
set -euo pipefail

log() {
  printf '\n==> %s\n' "$*"
}

if ! command -v apt-get >/dev/null 2>&1; then
  echo "remote-setup currently supports Debian/Ubuntu hosts with apt-get" >&2
  exit 2
fi

log "checking KVM visibility"
if [[ ! -e /dev/kvm ]]; then
  echo "/dev/kvm is missing. Enable nested virtualization before running Firecracker." >&2
  exit 1
fi
ls -l /dev/kvm
virt_flags=$(grep -Ewc 'vmx|svm' /proc/cpuinfo || true)
echo "virtualization flags: $virt_flags"
if [[ "$virt_flags" == "0" ]]; then
  echo "vmx/svm CPU flags are not visible in the VM." >&2
  exit 1
fi

log "installing development packages"
sudo apt-get update
compose_package="docker-compose-plugin"
if ! apt-cache show "$compose_package" >/dev/null 2>&1; then
  compose_package="docker-compose-v2"
fi
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
  ca-certificates \
  cpu-checker \
  curl \
  "$compose_package" \
  docker.io \
  git \
  golang \
  jq \
  make \
  rsync

log "enabling docker"
sudo systemctl enable --now docker

log "adding $USER to docker and kvm groups"
sudo usermod -aG docker,kvm "$USER"

log "checking KVM"
if command -v kvm-ok >/dev/null 2>&1; then
  kvm-ok || true
fi

log "configuring NBD devices"
printf 'nbd\n' | sudo tee /etc/modules-load.d/orca-nbd.conf >/dev/null
printf 'options nbd nbds_max=16 max_part=8\n' | sudo tee /etc/modprobe.d/orca-nbd.conf >/dev/null
sudo modprobe nbd nbds_max=16 max_part=8 || sudo modprobe nbd max_part=8
ls -l /dev/nbd* | head

log "checking docker"
if docker version >/dev/null 2>&1; then
  docker version
else
  sudo docker version
  echo
  echo "Docker works via sudo. Log out and back in, or run 'newgrp docker', to use Docker without sudo."
fi

log "checking container access to /dev/kvm"
if docker run --rm --device /dev/kvm ubuntu:24.04 sh -lc 'ls -l /dev/kvm' >/dev/null 2>&1; then
  docker run --rm --device /dev/kvm ubuntu:24.04 sh -lc 'ls -l /dev/kvm'
else
  sudo docker run --rm --device /dev/kvm ubuntu:24.04 sh -lc 'ls -l /dev/kvm'
  echo
  echo "Container KVM access works via sudo. Re-login/newgrp docker for non-sudo Docker."
fi

log "remote host is ready"
