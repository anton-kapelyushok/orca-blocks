#!/usr/bin/env bash
set -euo pipefail

# Reproducible bootstrap for the Docker/Sysbox benchmark VMs.
#
# Target: fresh Ubuntu 22.04/24.04 VM, run as root.
# Installs:
#   - Docker Engine from Docker's apt repository
#   - Sysbox CE runtime
#   - optional CNI bridge used by ctr + sysbox-runc benchmarks
#   - optional native host registry on 127.0.0.1:5000
#   - optional stargz-snapshotter tools used by the benchmark scripts
#
# This intentionally does not switch Docker to the OverlayBD storage driver.
# OverlayBD Docker-mode setup is kept in setup-overlaybd-docker-runtime.sh.

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "run as root" >&2
  exit 1
fi

INSTALL_DOCKER="${INSTALL_DOCKER:-1}"
INSTALL_SYSBOX="${INSTALL_SYSBOX:-1}"
INSTALL_CNI="${INSTALL_CNI:-1}"
INSTALL_NATIVE_REGISTRY="${INSTALL_NATIVE_REGISTRY:-1}"
INSTALL_STARGZ_TOOLS="${INSTALL_STARGZ_TOOLS:-1}"
ENABLE_CONTAINERD_IMAGE_STORE="${ENABLE_CONTAINERD_IMAGE_STORE:-1}"
DOCKER_PACKAGE_VERSION="${DOCKER_PACKAGE_VERSION:-}"
DOCKER_MAJOR_MAX="${DOCKER_MAJOR_MAX:-28}"
APT_MARK_HOLD_DOCKER="${APT_MARK_HOLD_DOCKER:-1}"

SYSBOX_VERSION="${SYSBOX_VERSION:-0.7.0}"
SYSBOX_AMD64_SHA256="${SYSBOX_AMD64_SHA256:-eeff273671467b8fa351ab3d40709759462dc03d9f7b50a1b207b37982ce40a9}"
STARGZ_VERSION="${STARGZ_VERSION:-v0.18.2}"

REGISTRY_ADDR="${REGISTRY_ADDR:-127.0.0.1:5000}"
REGISTRY_STORAGE_ROOT="${REGISTRY_STORAGE_ROOT:-/var/lib/docker-registry}"
CNI_BRIDGE_NAME="${CNI_BRIDGE_NAME:-orca-bridge}"
CNI_BRIDGE_IFACE="${CNI_BRIDGE_IFACE:-cni0}"
CNI_SUBNET="${CNI_SUBNET:-10.88.0.0/16}"
DOCKER_BIP="${DOCKER_BIP:-172.20.0.1/16}"
DOCKER_DEFAULT_POOL_BASE="${DOCKER_DEFAULT_POOL_BASE:-172.25.0.0/16}"
DOCKER_DEFAULT_POOL_SIZE="${DOCKER_DEFAULT_POOL_SIZE:-24}"
DOCKER_STORAGE_DRIVER="${DOCKER_STORAGE_DRIVER:-}"
STARGZ_TOOLS_DIR="${STARGZ_TOOLS_DIR:-/opt/orca/stargz-tools}"
WORK_DIR="${WORK_DIR:-/root/orca-vm-setup}"

log() {
  printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"
}

wait_for_apt() {
  while fuser /var/lib/apt/lists/lock /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock >/dev/null 2>&1; do
    log "waiting for apt/dpkg lock"
    sleep 3
  done
}

install_base_packages() {
  wait_for_apt
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y \
    ca-certificates curl gnupg jq python3 tar
}

install_docker() {
  if [[ "$INSTALL_DOCKER" != "1" ]]; then
    return
  fi

  if ! command -v docker >/dev/null || ! command -v dockerd >/dev/null; then
    log "installing Docker Engine"
    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
    chmod a+r /etc/apt/keyrings/docker.asc
    . /etc/os-release
    docker_codename="${DOCKER_APT_CODENAME:-${VERSION_CODENAME}}"
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${docker_codename} stable" >/etc/apt/sources.list.d/docker.list
    wait_for_apt
    apt-get update -qq
  fi

  if [[ -z "$DOCKER_PACKAGE_VERSION" ]]; then
    DOCKER_PACKAGE_VERSION="$(apt-cache madison docker-ce |
      awk -v max_major="$DOCKER_MAJOR_MAX" '
        {
          version=$3
          split(version, parts, ":")
          semver=parts[length(parts)]
          split(semver, nums, ".")
          major=nums[1] + 0
          if (major <= max_major && selected == "") {
            selected = version
          }
        }
        END {
          if (selected != "") {
            print selected
          }
        }')"
    if [[ -z "$DOCKER_PACKAGE_VERSION" ]]; then
      echo "could not find docker-ce package with major <= ${DOCKER_MAJOR_MAX}" >&2
      echo "Set DOCKER_PACKAGE_VERSION explicitly, or set DOCKER_MAJOR_MAX to a supported major." >&2
      apt-cache madison docker-ce | sed -n '1,20p' >&2
      exit 1
    fi
  fi

  log "installing Docker packages version ${DOCKER_PACKAGE_VERSION}"
  wait_for_apt
  DEBIAN_FRONTEND=noninteractive apt-get install -y --allow-downgrades \
    "docker-ce=${DOCKER_PACKAGE_VERSION}" \
    "docker-ce-cli=${DOCKER_PACKAGE_VERSION}" \
    "docker-ce-rootless-extras=${DOCKER_PACKAGE_VERSION}" \
    containerd.io docker-buildx-plugin docker-compose-plugin

  if [[ "$APT_MARK_HOLD_DOCKER" == "1" ]]; then
    apt-mark hold docker-ce docker-ce-cli docker-ce-rootless-extras containerd.io
  fi

  systemctl enable --now containerd docker
}

sysbox_arch() {
  case "$(dpkg --print-architecture)" in
    amd64) echo "amd64" ;;
    arm64) echo "arm64" ;;
    *) echo "unsupported architecture: $(dpkg --print-architecture)" >&2; exit 1 ;;
  esac
}

install_sysbox() {
  if [[ "$INSTALL_SYSBOX" != "1" ]]; then
    return
  fi

  if command -v sysbox-runc >/dev/null; then
    log "Sysbox already installed"
  else
    local arch deb url
    arch="$(sysbox_arch)"
    deb="sysbox-ce_${SYSBOX_VERSION}.linux_${arch}.deb"
    url="https://github.com/nestybox/sysbox/releases/download/v${SYSBOX_VERSION}/${deb}"
    mkdir -p "$WORK_DIR"
    log "downloading Sysbox ${SYSBOX_VERSION}"
    curl -fL -o "$WORK_DIR/$deb" "$url"
    if [[ "$arch" == "amd64" && -n "$SYSBOX_AMD64_SHA256" ]]; then
      echo "${SYSBOX_AMD64_SHA256}  $WORK_DIR/$deb" | sha256sum -c -
    fi
    wait_for_apt
    DEBIAN_FRONTEND=noninteractive apt-get install -y "$WORK_DIR/$deb"
  fi

  systemctl enable --now sysbox sysbox-fs sysbox-mgr
}

install_cni() {
  if [[ "$INSTALL_CNI" != "1" ]]; then
    return
  fi

  log "installing CNI plugins for ctr + sysbox-runc"
  wait_for_apt
  DEBIAN_FRONTEND=noninteractive apt-get install -y containernetworking-plugins

  mkdir -p /opt/cni /etc/cni/net.d
  if [[ ! -e /opt/cni/bin && -d /usr/lib/cni ]]; then
    ln -s /usr/lib/cni /opt/cni/bin
  elif [[ -d /usr/lib/cni && ! -x /opt/cni/bin/bridge ]]; then
    rm -rf /opt/cni/bin
    ln -s /usr/lib/cni /opt/cni/bin
  fi

  cat >/etc/cni/net.d/10-orca-bridge.conf <<EOF
{
  "cniVersion": "0.4.0",
  "name": "${CNI_BRIDGE_NAME}",
  "type": "bridge",
  "bridge": "${CNI_BRIDGE_IFACE}",
  "isGateway": true,
  "ipMasq": true,
  "hairpinMode": true,
  "ipam": {
    "type": "host-local",
    "subnet": "${CNI_SUBNET}",
    "routes": [
      { "dst": "0.0.0.0/0" }
    ]
  }
}
EOF

  sysctl -w net.ipv4.ip_forward=1 >/dev/null
}

configure_docker_daemon() {
  mkdir -p /etc/docker
  [[ -f /etc/docker/daemon.json ]] || echo '{}' >/etc/docker/daemon.json

  log "configuring Docker daemon"
  ENABLE_CONTAINERD_IMAGE_STORE="$ENABLE_CONTAINERD_IMAGE_STORE" \
  REGISTRY_ADDR="$REGISTRY_ADDR" \
  DOCKER_BIP="$DOCKER_BIP" \
  DOCKER_DEFAULT_POOL_BASE="$DOCKER_DEFAULT_POOL_BASE" \
  DOCKER_DEFAULT_POOL_SIZE="$DOCKER_DEFAULT_POOL_SIZE" \
  DOCKER_STORAGE_DRIVER="$DOCKER_STORAGE_DRIVER" \
  python3 - <<'PY'
import json
import os
from pathlib import Path

p = Path("/etc/docker/daemon.json")
try:
    data = json.loads(p.read_text() or "{}")
except json.JSONDecodeError as exc:
    raise SystemExit(f"/etc/docker/daemon.json is not valid JSON: {exc}")

data.setdefault("runtimes", {})
data["runtimes"]["sysbox-runc"] = {"path": "/usr/bin/sysbox-runc"}

if os.environ["ENABLE_CONTAINERD_IMAGE_STORE"] == "1":
    data.setdefault("features", {})
    data["features"]["containerd-snapshotter"] = True

registry = os.environ["REGISTRY_ADDR"]
registries = data.setdefault("insecure-registries", [])
if registry and registry not in registries:
    registries.append(registry)

bip = os.environ["DOCKER_BIP"]
if bip:
    data["bip"] = bip

pool_base = os.environ["DOCKER_DEFAULT_POOL_BASE"]
pool_size = int(os.environ["DOCKER_DEFAULT_POOL_SIZE"])
if pool_base:
    data["default-address-pools"] = [{"base": pool_base, "size": pool_size}]

storage_driver = os.environ["DOCKER_STORAGE_DRIVER"]
if storage_driver:
    data["storage-driver"] = storage_driver

p.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")
PY

  systemctl restart docker
}

install_native_registry() {
  if [[ "$INSTALL_NATIVE_REGISTRY" != "1" ]]; then
    return
  fi

  log "installing native docker-registry service"
  wait_for_apt
  DEBIAN_FRONTEND=noninteractive apt-get install -y docker-registry

  mkdir -p "$(dirname /etc/docker/registry/config.yml)" "$REGISTRY_STORAGE_ROOT"
  cat >/etc/docker/registry/config.yml <<EOF
version: 0.1
log:
  fields:
    service: registry
storage:
  cache:
    blobdescriptor: inmemory
  filesystem:
    rootdirectory: ${REGISTRY_STORAGE_ROOT}
http:
  addr: ${REGISTRY_ADDR}
  headers:
    X-Content-Type-Options: [nosniff]
EOF

  systemctl enable --now docker-registry
  systemctl restart docker-registry
}

install_stargz_tools() {
  if [[ "$INSTALL_STARGZ_TOOLS" != "1" ]]; then
    return
  fi

  if [[ "$(dpkg --print-architecture)" != "amd64" ]]; then
    echo "stargz tool bootstrap currently expects amd64; set INSTALL_STARGZ_TOOLS=0 for this architecture" >&2
    exit 1
  fi

  mkdir -p "$STARGZ_TOOLS_DIR" "$WORK_DIR"
  if [[ -x "$STARGZ_TOOLS_DIR/ctr-remote" && -x "$STARGZ_TOOLS_DIR/containerd-stargz-grpc" ]]; then
    log "stargz tools already installed"
    return
  fi

  local archive url
  archive="$WORK_DIR/stargz-snapshotter-${STARGZ_VERSION}-linux-amd64.tar.gz"
  url="https://github.com/containerd/stargz-snapshotter/releases/download/${STARGZ_VERSION}/stargz-snapshotter-${STARGZ_VERSION}-linux-amd64.tar.gz"
  log "downloading stargz tools ${STARGZ_VERSION}"
  curl -fL -o "$archive" "$url"
  tar -xzf "$archive" -C "$STARGZ_TOOLS_DIR" ctr-remote containerd-stargz-grpc
  chmod +x "$STARGZ_TOOLS_DIR/ctr-remote" "$STARGZ_TOOLS_DIR/containerd-stargz-grpc"
}

verify() {
  log "verification"
  systemctl is-active containerd docker
  if [[ "$INSTALL_SYSBOX" == "1" ]]; then
    systemctl is-active sysbox sysbox-fs sysbox-mgr
    sysbox-runc --version | sed -n '1,3p'
  fi
  if [[ "$INSTALL_CNI" == "1" ]]; then
    test -x /opt/cni/bin/bridge
    test -f /etc/cni/net.d/10-orca-bridge.conf
    echo "CNI_OK ${CNI_BRIDGE_NAME} ${CNI_SUBNET}"
  fi
  docker info --format 'Driver={{.Driver}} Runtimes={{json .Runtimes}} DriverStatus={{json .DriverStatus}}'
  docker run --rm hello-world >/dev/null
  if [[ "$INSTALL_SYSBOX" == "1" ]]; then
    docker run --rm --runtime=sysbox-runc alpine:3.22 sh -lc 'echo SYSBOX_OK'
  fi
  if [[ "$INSTALL_NATIVE_REGISTRY" == "1" ]]; then
    curl -fsS "http://${REGISTRY_ADDR}/v2/" >/dev/null
    echo "REGISTRY_OK ${REGISTRY_ADDR}"
  fi
  if [[ "$INSTALL_STARGZ_TOOLS" == "1" ]]; then
    "$STARGZ_TOOLS_DIR/ctr-remote" --version | sed -n '1p'
    "$STARGZ_TOOLS_DIR/containerd-stargz-grpc" --version | sed -n '1p'
  fi
}

main() {
  log "installing base packages"
  install_base_packages
  install_docker
  install_sysbox
  install_cni
  configure_docker_daemon
  install_native_registry
  install_stargz_tools
  verify
  log "done"
}

main "$@"
