#!/usr/bin/env bash
set -euo pipefail

ASSET_DIR=${ASSET_DIR:-firecracker-assets}
ALPINE_VERSION=${ALPINE_VERSION:-3.22.1}
ALPINE_MAJOR_MINOR=${ALPINE_MAJOR_MINOR:-${ALPINE_VERSION%.*}}
ALPINE_ARCH=${ALPINE_ARCH:-x86_64}
ROOTFS_SIZE_MB=${ROOTFS_SIZE_MB:-1024}
ROOTFS_NAME=${ROOTFS_NAME:-rootfs.ext4}
BASE_ROOTFS_NAME=${BASE_ROOTFS_NAME:-rootfs-base-${ALPINE_VERSION}-${ALPINE_ARCH}-${ROOTFS_SIZE_MB}m.ext4}
FORCE=${FORCE:-false}
REBUILD_BASE=${REBUILD_BASE:-false}

ROOTFS_URL=${ROOTFS_URL:-https://dl-cdn.alpinelinux.org/alpine/v${ALPINE_MAJOR_MINOR}/releases/${ALPINE_ARCH}/alpine-minirootfs-${ALPINE_VERSION}-${ALPINE_ARCH}.tar.gz}

log() {
  printf '\n==> %s\n' "$*"
}

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 2
  fi
}

need curl
need dd
need mkfs.ext4
need mount
need tar
need umount

mkdir -p "$ASSET_DIR"
ASSET_DIR=$(cd "$ASSET_DIR" && pwd)
ROOTFS_PATH="$ASSET_DIR/$ROOTFS_NAME"
BASE_ROOTFS_PATH="$ASSET_DIR/$BASE_ROOTFS_NAME"
TARBALL="$ASSET_DIR/alpine-minirootfs-${ALPINE_VERSION}-${ALPINE_ARCH}.tar.gz"
MOUNT_DIR="$ASSET_DIR/mnt-rootfs"

if [[ -e "$ROOTFS_PATH" && "$FORCE" == "true" ]]; then
  rm -f "$ROOTFS_PATH"
elif [[ -e "$ROOTFS_PATH" ]]; then
  echo "refusing to overwrite existing rootfs: $ROOTFS_PATH" >&2
  echo "remove it first, set FORCE=true, or set ROOTFS_NAME to a different filename" >&2
  exit 1
fi
if [[ -e "$BASE_ROOTFS_PATH" && "$REBUILD_BASE" == "true" ]]; then
  rm -f "$BASE_ROOTFS_PATH"
fi

cleanup() {
  if mountpoint -q "$MOUNT_DIR" 2>/dev/null; then
    sudo umount "$MOUNT_DIR" || true
  fi
  rmdir "$MOUNT_DIR" 2>/dev/null || true
}
trap cleanup EXIT

log "downloading Alpine minirootfs"
if [[ ! -f "$TARBALL" ]]; then
  curl -fL "$ROOTFS_URL" -o "$TARBALL"
else
  echo "using cached $TARBALL"
fi

copy_image() {
  local src=$1
  local dst=$2
  if cp --reflink=auto "$src" "$dst" 2>/dev/null; then
    return 0
  fi
  cp "$src" "$dst"
}

if [[ ! -f "$BASE_ROOTFS_PATH" ]]; then
  log "creating cached base rootfs at $BASE_ROOTFS_PATH"
  dd if=/dev/zero of="$BASE_ROOTFS_PATH" bs=1M count="$ROOTFS_SIZE_MB" status=progress
  mkfs.ext4 -F "$BASE_ROOTFS_PATH"

  log "mounting base rootfs image"
  mkdir -p "$MOUNT_DIR"
  sudo mount -o loop "$BASE_ROOTFS_PATH" "$MOUNT_DIR"

  log "extracting Alpine"
  sudo tar -xzf "$TARBALL" -C "$MOUNT_DIR"

  log "installing guest packages"
  sudo cp /etc/resolv.conf "$MOUNT_DIR/etc/resolv.conf"
  sudo chroot "$MOUNT_DIR" /sbin/apk add --no-cache ca-certificates docker e2fsprogs iptables

  log "installing offline container image seed"
  sudo mkdir -p "$MOUNT_DIR/opt/orca"
  sudo cp "$TARBALL" "$MOUNT_DIR/opt/orca/alpine-container-rootfs.tar.gz"

  log "writing base guest metadata"
  sudo tee "$MOUNT_DIR/etc/orca-rootfs-base" >/dev/null <<EOF
alpine_version=$ALPINE_VERSION
alpine_arch=$ALPINE_ARCH
rootfs_size_mb=$ROOTFS_SIZE_MB
EOF

  log "unmounting base rootfs image"
  sudo umount "$MOUNT_DIR"
  rmdir "$MOUNT_DIR"
else
  log "using cached base rootfs $BASE_ROOTFS_PATH"
fi

log "creating final rootfs at $ROOTFS_PATH from cached base"
copy_image "$BASE_ROOTFS_PATH" "$ROOTFS_PATH"

log "mounting final rootfs image"
mkdir -p "$MOUNT_DIR"
sudo mount -o loop "$ROOTFS_PATH" "$MOUNT_DIR"

log "installing Orca guest init"
sudo mkdir -p "$MOUNT_DIR"/{dev,proc,sys,mnt/orca}
sudo tee "$MOUNT_DIR/init" >/dev/null <<'INIT'
#!/bin/sh
set -eu
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

log() {
  echo "orca-init: $*" > /dev/console
}

cmdline_value() {
  key="$1"
  for arg in $(cat /proc/cmdline); do
    case "$arg" in
      "$key="*) echo "${arg#*=}"; return 0 ;;
    esac
  done
  return 1
}

mount -t proc proc /proc || true
mount -t sysfs sysfs /sys || true
mount -t devtmpfs devtmpfs /dev || true
mkdir -p /run /tmp /sys/fs/cgroup /var/run /var/lib/docker /mnt/orca
mount -t tmpfs tmpfs /run || true
mount -t tmpfs tmpfs /tmp || true
mount -t cgroup2 none /sys/fs/cgroup || true

MODE="$(cmdline_value orca.mode || echo smoke)"
PAYLOAD="$(cmdline_value orca.payload || echo hello-from-firecracker)"
PAYLOAD_B64="$(cmdline_value orca.payload_b64 || true)"
DATA_DEV="$(cmdline_value orca.data_dev || echo /dev/vdb)"
AFTER_OK="$(cmdline_value orca.after_ok || echo reboot)"
if [ -n "$PAYLOAD_B64" ]; then
  PAYLOAD="$(printf '%s' "$PAYLOAD_B64" | base64 -d)"
fi

log "started mode=$MODE data_dev=$DATA_DEV"

start_dockerd() {
  log "starting dockerd"
  dockerd \
    --host=unix:///var/run/docker.sock \
    --storage-driver=vfs \
    --iptables=false \
    --bridge=none \
    --ip-forward=false \
    --ip-masq=false \
    --userland-proxy=false \
    >/tmp/dockerd.log 2>&1 &
  DOCKERD_PID="$!"
  i=0
  while [ "$i" -lt 60 ]; do
    if docker version >/tmp/docker-version.log 2>&1; then
      log "dockerd ready"
      return 0
    fi
    if ! kill -0 "$DOCKERD_PID" 2>/dev/null; then
      log "dockerd exited early"
      cat /tmp/dockerd.log >/dev/console 2>&1 || true
      return 1
    fi
    i=$((i + 1))
    sleep 0.2
  done
  log "dockerd timed out"
  cat /tmp/dockerd.log >/dev/console 2>&1 || true
  cat /tmp/docker-version.log >/dev/console 2>&1 || true
  return 1
}

load_offline_image() {
  if docker image inspect orca/alpine-local:latest >/dev/null 2>&1; then
    return 0
  fi
  log "loading offline alpine image"
  gzip -dc /opt/orca/alpine-container-rootfs.tar.gz | docker import - orca/alpine-local:latest >/dev/console 2>&1
}

case "$MODE" in
  smoke)
    log "smoke ok"
    ;;
  write)
    log "formatting and mounting $DATA_DEV"
    mkfs.ext4 -F "$DATA_DEV" >/dev/console 2>&1
    mkdir -p /mnt/orca
    mount "$DATA_DEV" /mnt/orca
    printf '%s\n' "$PAYLOAD" > /mnt/orca/proof.txt
    sync
    umount /mnt/orca
    log "write ok"
    ;;
  read)
    mkdir -p /mnt/orca
    mount -t ext4 -o ro,noload "$DATA_DEV" /mnt/orca
    ACTUAL="$(cat /mnt/orca/proof.txt)"
    if [ "$ACTUAL" != "$PAYLOAD" ]; then
      log "proof mismatch expected_len=${#PAYLOAD} actual_len=${#ACTUAL}"
      umount /mnt/orca
      reboot -f
      exit 3
    fi
    log "proof ok"
    umount /mnt/orca
    log "read ok"
    ;;
  docker-smoke)
    start_dockerd
    load_offline_image
    log "formatting and mounting $DATA_DEV"
    mkfs.ext4 -F "$DATA_DEV" >/dev/console 2>&1
    mount "$DATA_DEV" /mnt/orca
    log "running docker container"
    docker run --rm --network=none -e ORCA_PAYLOAD="$PAYLOAD" -v /mnt/orca:/mnt/orca orca/alpine-local:latest \
      /bin/sh -c 'printf "%s\n" "$ORCA_PAYLOAD" > /mnt/orca/proof.txt && echo "container write ok"' >/tmp/docker-run.log 2>&1
    cat /tmp/docker-run.log >/dev/console 2>&1 || true
    log "docker container ok"
    sync
    umount /mnt/orca
    log "docker-smoke ok"
    ;;
  docker-read)
    start_dockerd
    load_offline_image
    log "mounting $DATA_DEV read-only"
    mount -t ext4 -o ro,noload "$DATA_DEV" /mnt/orca
    log "running docker read container"
    docker run --rm --network=none -e ORCA_PAYLOAD="$PAYLOAD" -v /mnt/orca:/mnt/orca:ro orca/alpine-local:latest \
      /bin/sh -c 'actual="$(cat /mnt/orca/proof.txt)"; test "$actual" = "$ORCA_PAYLOAD" && echo "container read ok"' >/tmp/docker-run.log 2>&1
    cat /tmp/docker-run.log >/dev/console 2>&1 || true
    log "docker container ok"
    umount /mnt/orca
    log "docker-read ok"
    ;;
  *)
    log "unknown mode: $MODE"
    reboot -f
    exit 2
    ;;
esac

if [ "$AFTER_OK" = "wait" ]; then
  log "waiting for host"
  while true; do
    sleep 3600
  done
fi

reboot -f
INIT
sudo chmod +x "$MOUNT_DIR/init"
sudo ln -sf /init "$MOUNT_DIR/sbin/init"

log "writing guest metadata"
sudo tee "$MOUNT_DIR/etc/orca-rootfs" >/dev/null <<EOF
alpine_version=$ALPINE_VERSION
alpine_arch=$ALPINE_ARCH
rootfs_size_mb=$ROOTFS_SIZE_MB
EOF

log "unmounting rootfs image"
sudo umount "$MOUNT_DIR"
rmdir "$MOUNT_DIR"
trap - EXIT

log "created $ROOTFS_PATH"
ls -lh "$ROOTFS_PATH"
