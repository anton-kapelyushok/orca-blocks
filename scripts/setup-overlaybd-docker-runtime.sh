#!/usr/bin/env bash
set -euo pipefail

# Configure OverlayBD the way accelerated-container-image/docs/DOCKER.md
# describes: Docker uses the containerd image store and the overlaybd
# snapshotter as its storage driver.

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "run as root" >&2
  exit 1
fi

OVERLAYBD_VERSION="${OVERLAYBD_VERSION:-v1.0.17}"
OVERLAYBD_DEB="${OVERLAYBD_DEB:-overlaybd-1.0.17-20260605.afa06c7.ubuntu1.22.04.x86_64.deb}"
SNAPSHOTTER_VERSION="${SNAPSHOTTER_VERSION:-v1.4.3}"
SNAPSHOTTER_DEB="${SNAPSHOTTER_DEB:-overlaybd-snapshotter_1.4.3-20260330130113.43c3295_amd64.deb}"
WORK_DIR="${WORK_DIR:-/root/orca-blocks/.tmp/overlaybd-docker-install}"
OVERLAYBD_RW_MODE="${OVERLAYBD_RW_MODE:-dev}"

log() {
  printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"
}

wait_for_apt() {
  while fuser /var/lib/apt/lists/lock /var/lib/dpkg/lock-frontend >/dev/null 2>&1; do
    sleep 3
  done
}

install_docker() {
  wait_for_apt
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl gnupg jq python3
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  . /etc/os-release
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" >/etc/apt/sources.list.d/docker.list
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  systemctl enable --now containerd docker
}

install_overlaybd() {
  mkdir -p "$WORK_DIR"
  cd "$WORK_DIR"
  curl -fL -o overlaybd.deb "https://github.com/containerd/overlaybd/releases/download/${OVERLAYBD_VERSION}/${OVERLAYBD_DEB}"
  curl -fL -o overlaybd-snapshotter.deb "https://github.com/containerd/accelerated-container-image/releases/download/${SNAPSHOTTER_VERSION}/${SNAPSHOTTER_DEB}"
  DEBIAN_FRONTEND=noninteractive apt-get install -y ./overlaybd.deb ./overlaybd-snapshotter.deb
  modprobe target_core_user
}

write_configs() {
  mkdir -p /etc/docker /etc/containerd

  cat >/etc/overlaybd-snapshotter/config.json <<'JSON'
{
    "root": "/var/lib/containerd/io.containerd.snapshotter.v1.overlaybd",
    "address": "/run/overlaybd-snapshotter/overlaybd.sock",
    "runtimeType": "docker",
    "rwMode": "${OVERLAYBD_RW_MODE}",
    "verbose": "info",
    "logReportCaller": false,
    "autoRemoveDev": true,
    "mirrorRegistry": []
}
JSON

  cat >/etc/docker/daemon.json <<'JSON'
{
    "features": {
        "containerd-snapshotter": true
    },
    "storage-driver": "overlaybd"
}
JSON

  containerd config default >/etc/containerd/config.toml
  python3 - <<'PY'
from pathlib import Path

p = Path("/etc/containerd/config.toml")
s = p.read_text()
if "[proxy_plugins.overlaybd]" not in s:
    marker = "[proxy_plugins]\n"
    block = '  [proxy_plugins.overlaybd]\n    type = "snapshot"\n    address = "/run/overlaybd-snapshotter/overlaybd.sock"\n'
    if marker in s:
        s = s.replace(marker, marker + block, 1)
    else:
        s = s.rstrip() + "\n[proxy_plugins]\n" + block
p.write_text(s)
PY
}

start_services() {
  systemctl enable /opt/overlaybd/overlaybd-tcmu.service
  systemctl enable /opt/overlaybd/snapshotter/overlaybd-snapshotter.service
  systemctl start overlaybd-tcmu overlaybd-snapshotter
  systemctl restart containerd
  systemctl restart docker
}

verify() {
  systemctl is-active overlaybd-tcmu overlaybd-snapshotter containerd docker
  ctr plugins ls | grep overlaybd
  docker info --format 'Driver={{.Driver}} DriverStatus={{json .DriverStatus}}'
}

log "installing Docker"
install_docker
log "installing OverlayBD"
install_overlaybd
log "writing Docker runtime config"
write_configs
log "starting services"
start_services
log "verification"
verify
