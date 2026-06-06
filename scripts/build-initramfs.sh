#!/usr/bin/env bash
set -euo pipefail

ASSET_DIR=${ASSET_DIR:-firecracker-assets}
INITRAMFS_NAME=${INITRAMFS_NAME:-initramfs.cpio.gz}
FORCE=${FORCE:-false}

log() {
  printf '\n==> %s\n' "$*"
}

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 2
  fi
}

need cpio
need gzip
need mkdir
need rm

BUSYBOX=${BUSYBOX:-$(command -v busybox || true)}
if [[ -z "$BUSYBOX" ]]; then
  echo "missing required command: busybox" >&2
  exit 2
fi

mkdir -p "$ASSET_DIR"
ASSET_DIR=$(cd "$ASSET_DIR" && pwd)
INITRAMFS_PATH="$ASSET_DIR/$INITRAMFS_NAME"

if [[ -e "$INITRAMFS_PATH" && "$FORCE" == "true" ]]; then
  rm -f "$INITRAMFS_PATH"
elif [[ -e "$INITRAMFS_PATH" ]]; then
  echo "refusing to overwrite existing initramfs: $INITRAMFS_PATH" >&2
  echo "remove it first, set FORCE=true, or set INITRAMFS_NAME to a different filename" >&2
  exit 1
fi

WORK_DIR=$(mktemp -d)
cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

log "creating initramfs tree"
mkdir -p "$WORK_DIR"/{bin,dev,proc,sys,mnt/orca}
cp "$BUSYBOX" "$WORK_DIR/bin/busybox"
chmod +x "$WORK_DIR/bin/busybox"

for applet in sh mount umount mkdir cat printf sync reboot sleep base64 mke2fs; do
  ln -s busybox "$WORK_DIR/bin/$applet"
done

cat >"$WORK_DIR/init" <<'INIT'
#!/bin/sh
set -eu

PATH=/bin

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
PAYLOAD_B64="$(cmdline_value orca.payload_b64 || true)"
DATA_DEV="$(cmdline_value orca.data_dev || echo /dev/vda)"
AFTER_OK="$(cmdline_value orca.after_ok || echo reboot)"
if [ -n "$PAYLOAD_B64" ]; then
  PAYLOAD="$(printf '%s' "$PAYLOAD_B64" | base64 -d)"
fi

log "started mode=$MODE data_dev=$DATA_DEV"

case "$MODE" in
  smoke)
    log "smoke ok"
    ;;
  write)
    log "formatting and mounting $DATA_DEV"
    mke2fs -F "$DATA_DEV" >/dev/console 2>&1
    mkdir -p /mnt/orca
    mount "$DATA_DEV" /mnt/orca
    printf '%s\n' "$PAYLOAD" > /mnt/orca/proof.txt
    sync
    umount /mnt/orca
    log "write ok"
    ;;
  read)
    mkdir -p /mnt/orca
    mount -o ro "$DATA_DEV" /mnt/orca
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
chmod +x "$WORK_DIR/init"

log "packing $INITRAMFS_PATH"
(
  cd "$WORK_DIR"
  find . -print0 | cpio --null -ov --format=newc 2>/dev/null | gzip -9 >"$INITRAMFS_PATH"
)

log "created $INITRAMFS_PATH"
ls -lh "$INITRAMFS_PATH"
