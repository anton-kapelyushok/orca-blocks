#!/usr/bin/env bash
set -euo pipefail

ASSET_DIR=${ASSET_DIR:-firecracker-assets}
ALPINE_VERSION=${ALPINE_VERSION:-3.22.1}
ALPINE_MAJOR_MINOR=${ALPINE_MAJOR_MINOR:-${ALPINE_VERSION%.*}}
ALPINE_ARCH=${ALPINE_ARCH:-x86_64}
ROOTFS_SIZE_MB=${ROOTFS_SIZE_MB:-256}
ROOTFS_NAME=${ROOTFS_NAME:-rootfs.ext4}
FORCE=${FORCE:-false}

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
TARBALL="$ASSET_DIR/alpine-minirootfs-${ALPINE_VERSION}-${ALPINE_ARCH}.tar.gz"
MOUNT_DIR="$ASSET_DIR/mnt-rootfs"

if [[ -e "$ROOTFS_PATH" && "$FORCE" == "true" ]]; then
  rm -f "$ROOTFS_PATH"
elif [[ -e "$ROOTFS_PATH" ]]; then
  echo "refusing to overwrite existing rootfs: $ROOTFS_PATH" >&2
  echo "remove it first, set FORCE=true, or set ROOTFS_NAME to a different filename" >&2
  exit 1
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

log "creating ext4 image at $ROOTFS_PATH"
dd if=/dev/zero of="$ROOTFS_PATH" bs=1M count="$ROOTFS_SIZE_MB" status=progress
mkfs.ext4 -F "$ROOTFS_PATH"

log "mounting rootfs image"
mkdir -p "$MOUNT_DIR"
sudo mount -o loop "$ROOTFS_PATH" "$MOUNT_DIR"

log "extracting Alpine"
sudo tar -xzf "$TARBALL" -C "$MOUNT_DIR"

log "installing Orca guest init"
sudo mkdir -p "$MOUNT_DIR"/{dev,proc,sys,mnt/orca}
sudo tee "$MOUNT_DIR/init" >/dev/null <<'INIT'
#!/bin/sh
set -eu

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

MODE="$(cmdline_value orca.mode || echo smoke)"
PAYLOAD="$(cmdline_value orca.payload || echo hello-from-firecracker)"
DATA_DEV="$(cmdline_value orca.data_dev || echo /dev/vdb)"

log "started mode=$MODE data_dev=$DATA_DEV"

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
    mount "$DATA_DEV" /mnt/orca
    log "proof=$(cat /mnt/orca/proof.txt)"
    umount /mnt/orca
    log "read ok"
    ;;
  *)
    log "unknown mode: $MODE"
    reboot -f
    exit 2
    ;;
esac

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
