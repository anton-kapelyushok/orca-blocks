#!/usr/bin/env bash
set -euo pipefail

ASSET_DIR=${ASSET_DIR:-firecracker-assets}
ROOTFS_IMAGE=${1:-${ROOTFS_IMAGE:-}}
COMMAND=${COMMAND:-'cat /etc/orca-image-ref; echo orca-image-rootfs-smoke > /tmp/orca-proof; cat /tmp/orca-proof'}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-30}
MEM_SIZE_MIB=${MEM_SIZE_MIB:-256}

log() { printf '\n==> %s\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1" >&2; exit 2; }; }

need base64
need grep
need mktemp

ASSET_DIR=$(cd "$ASSET_DIR" && pwd)
FIRECRACKER_BIN=${FIRECRACKER_BIN:-$ASSET_DIR/firecracker}
KERNEL_IMAGE=${KERNEL_IMAGE:-$ASSET_DIR/vmlinux}

if [[ -z "$ROOTFS_IMAGE" ]]; then
  echo "usage: scripts/check-image-rootfs-boot.sh ROOTFS.ext4" >&2
  exit 2
fi
case "$ROOTFS_IMAGE" in
  /*) ;;
  *) ROOTFS_IMAGE="$(pwd)/$ROOTFS_IMAGE" ;;
esac

for path in "$FIRECRACKER_BIN" "$KERNEL_IMAGE" "$ROOTFS_IMAGE"; do
  [[ -e "$path" ]] || { echo "missing required asset: $path" >&2; exit 2; }
done
[[ -e /dev/kvm ]] || { echo "/dev/kvm is missing; Firecracker needs Linux KVM" >&2; exit 1; }

WORK_DIR=$(mktemp -d)
FC_PID=""
cleanup() {
  if [[ -n "$FC_PID" ]] && kill -0 "$FC_PID" 2>/dev/null; then
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
COMMAND_B64=$(printf '%s' "$COMMAND" | base64 -w0)

cat >"$CONFIG" <<EOF
{
  "boot-source": {
    "kernel_image_path": "$KERNEL_IMAGE",
    "boot_args": "root=/dev/vda rw console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/init orca.command_b64=$COMMAND_B64"
  },
  "drives": [
    {
      "drive_id": "rootfs",
      "path_on_host": "$ROOTFS_IMAGE",
      "is_root_device": true,
      "is_read_only": false
    }
  ],
  "machine-config": {
    "vcpu_count": 1,
    "mem_size_mib": $MEM_SIZE_MIB,
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

log "booting image rootfs smoke test"
"$FIRECRACKER_BIN" --api-sock "$SOCKET" --config-file "$CONFIG" >"$SERIAL_LOG" 2>>"$FC_LOG" &
FC_PID=$!

deadline=$((SECONDS + TIMEOUT_SECONDS))
while kill -0 "$FC_PID" 2>/dev/null; do
  if grep -q "orca-init: image-rootfs ok" "$SERIAL_LOG" 2>/dev/null; then
    break
  fi
  if (( SECONDS >= deadline )); then
    echo "timed out waiting for image-rootfs smoke" >&2
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

grep -q "orca-init: image-rootfs ok" "$SERIAL_LOG"
log "image rootfs boot check passed"
