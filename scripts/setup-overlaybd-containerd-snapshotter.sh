#!/usr/bin/env bash
set -euo pipefail

# Install OverlayBD as a containerd snapshotter without changing Docker's
# storage driver. This keeps Docker builds on normal overlayfs while allowing
# ctr to convert and run OverlayBD images.

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "run as root" >&2
  exit 1
fi

OVERLAYBD_VERSION="${OVERLAYBD_VERSION:-v1.0.17}"
OVERLAYBD_DEB="${OVERLAYBD_DEB:-overlaybd-1.0.17-20260605.afa06c7.ubuntu1.22.04.x86_64.deb}"
SNAPSHOTTER_VERSION="${SNAPSHOTTER_VERSION:-v1.4.3}"
SNAPSHOTTER_DEB="${SNAPSHOTTER_DEB:-overlaybd-snapshotter_1.4.3-20260330130113.43c3295_amd64.deb}"
WORK_DIR="${WORK_DIR:-/root/orca-vm-setup/overlaybd-containerd}"
REGISTRY_ADDR="${REGISTRY_ADDR:-127.0.0.1:5000}"
OVERLAYBD_RW_MODE="${OVERLAYBD_RW_MODE:-dev}"
SETUP_MYSQL="${SETUP_MYSQL:-1}"
MYSQL_DATABASE="${MYSQL_DATABASE:-overlaybd}"
MYSQL_USER="${MYSQL_USER:-overlaybd}"
MYSQL_PASSWORD="${MYSQL_PASSWORD:-overlaybd}"

log() {
  printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"
}

wait_for_apt() {
  while fuser /var/lib/apt/lists/lock /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock >/dev/null 2>&1; do
    log "waiting for apt/dpkg lock"
    sleep 3
  done
}

install_packages() {
  wait_for_apt
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl jq python3
}

setup_mysql_schema() {
  if [[ "$SETUP_MYSQL" != "1" ]]; then
    return
  fi

  log "installing/configuring MySQL metadata DB for OverlayBD conversion"
  wait_for_apt
  DEBIAN_FRONTEND=noninteractive apt-get install -y mysql-server
  systemctl enable --now mysql
  mysql <<SQL
CREATE DATABASE IF NOT EXISTS \`${MYSQL_DATABASE}\`;
CREATE USER IF NOT EXISTS '${MYSQL_USER}'@'127.0.0.1' IDENTIFIED BY '${MYSQL_PASSWORD}';
CREATE USER IF NOT EXISTS '${MYSQL_USER}'@'localhost' IDENTIFIED BY '${MYSQL_PASSWORD}';
GRANT ALL PRIVILEGES ON \`${MYSQL_DATABASE}\`.* TO '${MYSQL_USER}'@'127.0.0.1';
GRANT ALL PRIVILEGES ON \`${MYSQL_DATABASE}\`.* TO '${MYSQL_USER}'@'localhost';
USE \`${MYSQL_DATABASE}\`;
CREATE TABLE IF NOT EXISTS overlaybd_layers (
  host VARCHAR(255) NOT NULL,
  repo VARCHAR(255) NOT NULL,
  chain_id VARCHAR(255) NOT NULL,
  data_digest VARCHAR(255) NOT NULL,
  data_size BIGINT NOT NULL,
  PRIMARY KEY (host, repo, chain_id),
  KEY index_registry_chainId (host, chain_id) USING BTREE
) DEFAULT CHARSET=utf8;
CREATE TABLE IF NOT EXISTS overlaybd_manifests (
  host VARCHAR(255) NOT NULL,
  repo VARCHAR(255) NOT NULL,
  src_digest VARCHAR(255) NOT NULL,
  out_digest VARCHAR(255) NOT NULL,
  data_size BIGINT NOT NULL,
  mediatype VARCHAR(255) NOT NULL,
  PRIMARY KEY (host, repo, src_digest, mediatype),
  KEY index_registry_src_digest (host, src_digest, mediatype) USING BTREE
) DEFAULT CHARSET=utf8;
SQL
}

install_overlaybd() {
  mkdir -p "$WORK_DIR"
  cd "$WORK_DIR"
  if ! dpkg -s overlaybd >/dev/null 2>&1; then
    log "downloading OverlayBD ${OVERLAYBD_VERSION}"
    curl -fL -o overlaybd.deb "https://github.com/containerd/overlaybd/releases/download/${OVERLAYBD_VERSION}/${OVERLAYBD_DEB}"
    wait_for_apt
    DEBIAN_FRONTEND=noninteractive apt-get install -y ./overlaybd.deb
  fi
  if ! dpkg -s overlaybd-snapshotter >/dev/null 2>&1; then
    log "downloading overlaybd-snapshotter ${SNAPSHOTTER_VERSION}"
    curl -fL -o overlaybd-snapshotter.deb "https://github.com/containerd/accelerated-container-image/releases/download/${SNAPSHOTTER_VERSION}/${SNAPSHOTTER_DEB}"
    wait_for_apt
    DEBIAN_FRONTEND=noninteractive apt-get install -y ./overlaybd-snapshotter.deb
  fi
  modprobe target_core_user
}

write_overlaybd_config() {
  mkdir -p /etc/overlaybd-snapshotter /etc/containerd
  cat >/etc/overlaybd-snapshotter/config.json <<JSON
{
  "root": "/var/lib/containerd/io.containerd.snapshotter.v1.overlaybd",
  "address": "/run/overlaybd-snapshotter/overlaybd.sock",
  "rwMode": "${OVERLAYBD_RW_MODE}",
  "verbose": "info",
  "logReportCaller": false,
  "autoRemoveDev": true,
  "mirrorRegistry": [
    {
      "host": "${REGISTRY_ADDR}",
      "insecure": true
    }
  ]
}
JSON
}

configure_containerd_proxy() {
  if [[ ! -s /etc/containerd/config.toml ]]; then
    containerd config default >/etc/containerd/config.toml
  fi
  python3 - <<'PY'
from pathlib import Path

p = Path("/etc/containerd/config.toml")
s = p.read_text()
block = '  [proxy_plugins.overlaybd]\n    type = "snapshot"\n    address = "/run/overlaybd-snapshotter/overlaybd.sock"\n'
if "[proxy_plugins.overlaybd]" not in s:
    marker = "[proxy_plugins]\n"
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
  systemctl restart overlaybd-tcmu
  systemctl restart overlaybd-snapshotter
  systemctl restart containerd
  systemctl restart docker || true
}

verify() {
  systemctl is-active overlaybd-tcmu overlaybd-snapshotter containerd
  if [[ "$SETUP_MYSQL" == "1" ]]; then
    systemctl is-active mysql
    mysql -N -e "SELECT COUNT(*) FROM ${MYSQL_DATABASE}.overlaybd_layers; SELECT COUNT(*) FROM ${MYSQL_DATABASE}.overlaybd_manifests;" >/dev/null
    echo "MYSQL_OVERLAYBD_OK ${MYSQL_USER}:***@tcp(127.0.0.1:3306)/${MYSQL_DATABASE}"
  fi
  ctr plugins ls | grep -F 'io.containerd.snapshotter.v1' | grep -F overlaybd
  /opt/overlaybd/snapshotter/ctr version | sed -n '1,3p'
}

main() {
  log "installing prerequisites"
  install_packages
  setup_mysql_schema
  log "installing OverlayBD packages"
  install_overlaybd
  log "writing OverlayBD config"
  write_overlaybd_config
  log "configuring containerd proxy plugin"
  configure_containerd_proxy
  log "starting services"
  start_services
  log "verification"
  verify
  log "done"
}

main "$@"
