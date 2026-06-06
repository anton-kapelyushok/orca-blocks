#!/usr/bin/env bash
set -euo pipefail

ASSET_DIR=${ASSET_DIR:-firecracker-assets}
MODE=${MODE:-smoke}
PAYLOAD=${PAYLOAD:-hello-from-firecracker}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-20}
FIRECRACKER_BOOT_MODE=${FIRECRACKER_BOOT_MODE:-initramfs}

log() {
  printf '\n==> %s\n' "$*"
}

ASSET_DIR=$(cd "$ASSET_DIR" && pwd)
FIRECRACKER_BIN=${FIRECRACKER_BIN:-$ASSET_DIR/firecracker}
KERNEL_IMAGE=${KERNEL_IMAGE:-$ASSET_DIR/vmlinux}
ROOTFS_IMAGE=${ROOTFS_IMAGE:-$ASSET_DIR/rootfs.ext4}
INITRD_IMAGE=${INITRD_IMAGE:-$ASSET_DIR/initramfs.cpio.gz}

required=("$FIRECRACKER_BIN" "$KERNEL_IMAGE")
case "$FIRECRACKER_BOOT_MODE" in
  rootfs) required+=("$ROOTFS_IMAGE") ;;
  initramfs) required+=("$INITRD_IMAGE") ;;
  *)
    echo "unsupported FIRECRACKER_BOOT_MODE=$FIRECRACKER_BOOT_MODE" >&2
    exit 2
    ;;
esac

for path in "${required[@]}"; do
  if [[ ! -e "$path" ]]; then
    echo "missing required Firecracker asset: $path" >&2
    echo "run make firecracker-assets and make firecracker-initramfs/rootfs first" >&2
    exit 2
  fi
done

if [[ ! -e /dev/kvm ]]; then
  echo "/dev/kvm is missing; Firecracker needs Linux KVM" >&2
  exit 1
fi

WORK_DIR=$(mktemp -d)
cleanup() {
  if [[ -n "${FC_PID:-}" ]] && kill -0 "$FC_PID" 2>/dev/null; then
    kill "$FC_PID" 2>/dev/null || true
    wait "$FC_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

SOCKET="$WORK_DIR/firecracker.sock"
CONFIG="$WORK_DIR/firecracker.json"
SERIAL_LOG="$WORK_DIR/serial.log"
FC_LOG="$WORK_DIR/firecracker.log"

BOOT_ARGS="console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/init orca.mode=${MODE} orca.payload=${PAYLOAD}"
DRIVES_JSON="[]"
INITRD_JSON=""
if [[ "$FIRECRACKER_BOOT_MODE" == "rootfs" ]]; then
  BOOT_ARGS="root=/dev/vda rw $BOOT_ARGS"
  DRIVES_JSON='[
    {
      "drive_id": "rootfs",
      "path_on_host": "'"$ROOTFS_IMAGE"'",
      "is_root_device": true,
      "is_read_only": false
    }
  ]'
else
  INITRD_JSON=', "initrd_path": "'"$INITRD_IMAGE"'"'
fi

cat >"$CONFIG" <<EOF
{
  "boot-source": {
    "kernel_image_path": "$KERNEL_IMAGE",
    "boot_args": "$BOOT_ARGS"$INITRD_JSON
  },
  "drives": $DRIVES_JSON,
  "machine-config": {
    "vcpu_count": 1,
    "mem_size_mib": 128,
    "track_dirty_pages": false
  },
  "logger": {
    "log_path": "$FC_LOG",
    "level": "Info",
    "show_level": true,
    "show_log_origin": true
  }
}
EOF

log "booting Firecracker $FIRECRACKER_BOOT_MODE smoke test"
"$FIRECRACKER_BIN" --api-sock "$SOCKET" --config-file "$CONFIG" >"$SERIAL_LOG" 2>>"$FC_LOG" &
FC_PID=$!

deadline=$((SECONDS + TIMEOUT_SECONDS))
while kill -0 "$FC_PID" 2>/dev/null; do
  if grep -q "orca-init: ${MODE} ok" "$SERIAL_LOG" 2>/dev/null; then
    break
  fi
  if (( SECONDS >= deadline )); then
    echo "timed out waiting for Firecracker guest mode=$MODE" >&2
    echo "--- serial log ---" >&2
    cat "$SERIAL_LOG" >&2 || true
    echo "--- firecracker log ---" >&2
    cat "$FC_LOG" >&2 || true
    exit 1
  fi
  sleep 0.2
done

wait "$FC_PID" || true
FC_PID=""

log "serial output"
cat "$SERIAL_LOG"

if ! grep -q "orca-init: ${MODE} ok" "$SERIAL_LOG"; then
  echo "guest did not report successful mode=$MODE" >&2
  echo "--- firecracker log ---" >&2
  cat "$FC_LOG" >&2 || true
  exit 1
fi

log "Firecracker $FIRECRACKER_BOOT_MODE boot check passed"
