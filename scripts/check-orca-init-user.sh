#!/usr/bin/env bash
set -euo pipefail

ASSET_DIR=${ASSET_DIR:-firecracker-assets}
IMAGE=${IMAGE:-alpine:3.22}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-20}
MEM_SIZE_MIB=${MEM_SIZE_MIB:-256}
VCPU_COUNT=${VCPU_COUNT:-1}
TEST_USER=${TEST_USER:-workspace-agent}
TEST_UID=${TEST_UID:-22222}
TEST_GID=${TEST_GID:-101}
ROOTFS_SIZE_MB=${ROOTFS_SIZE_MB:-128}

log() { printf '\n==> %s\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1" >&2; exit 2; }; }

need base64
need docker
need go
need grep
need mkfs.ext4
need mount
need tar
need umount

ASSET_DIR=$(cd "$ASSET_DIR" && pwd)
FIRECRACKER_BIN=${FIRECRACKER_BIN:-$ASSET_DIR/firecracker}
KERNEL_IMAGE=${KERNEL_IMAGE:-$ASSET_DIR/vmlinux}

for path in "$FIRECRACKER_BIN" "$KERNEL_IMAGE"; do
  [[ -e "$path" ]] || { echo "missing required asset: $path" >&2; exit 2; }
done
[[ -e /dev/kvm ]] || { echo "/dev/kvm is missing; Firecracker needs Linux KVM" >&2; exit 1; }

WORK_DIR=$(mktemp -d)
MOUNT_DIR=""
CID=""
FC_PID=""
cleanup() {
  if [[ -n "$FC_PID" ]] && kill -0 "$FC_PID" 2>/dev/null; then
    kill "$FC_PID" 2>/dev/null || true
    wait "$FC_PID" 2>/dev/null || true
  fi
  if [[ -n "$CID" ]]; then
    docker rm -f "$CID" >/dev/null 2>&1 || true
  fi
  if [[ -n "$MOUNT_DIR" ]] && mountpoint -q "$MOUNT_DIR" 2>/dev/null; then
    sudo umount "$MOUNT_DIR" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

INIT_BIN="$WORK_DIR/orca-init"
ROOTFS_TAR="$WORK_DIR/rootfs.tar"
ROOTFS_IMAGE="$WORK_DIR/rootfs.ext4"
SOCKET="$WORK_DIR/firecracker.sock"
CONFIG="$WORK_DIR/firecracker.json"
SERIAL_LOG="$WORK_DIR/serial.log"
FC_LOG="$WORK_DIR/firecracker.log"

log "building static orca init"
BUILD_TIME_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTimeUTC=${BUILD_TIME_UTC}" -o "$INIT_BIN" ./cmd/orca-init

log "exporting tiny rootfs from $IMAGE"
docker pull "$IMAGE" >/dev/null
CID=$(docker create --entrypoint /bin/sh "$IMAGE" -c true)
docker export "$CID" >"$ROOTFS_TAR"
docker rm -f "$CID" >/dev/null
CID=""

log "creating ${ROOTFS_SIZE_MB}MiB ext4 rootfs"
truncate -s "${ROOTFS_SIZE_MB}M" "$ROOTFS_IMAGE"
mkfs.ext4 -F "$ROOTFS_IMAGE" >/dev/null
MOUNT_DIR=$(mktemp -d)
sudo mount -o loop "$ROOTFS_IMAGE" "$MOUNT_DIR"
sudo tar --numeric-owner -xf "$ROOTFS_TAR" -C "$MOUNT_DIR"

log "injecting orca init and synthetic image user"
sudo mkdir -p "$MOUNT_DIR"/{dev,proc,sys,run,tmp,etc,orca,home/"$TEST_USER"}
sudo install -m 0755 "$INIT_BIN" "$MOUNT_DIR/init"
if ! sudo grep -q "^${TEST_USER}:" "$MOUNT_DIR/etc/passwd"; then
  printf '%s:x:%s:%s:Orca Test User:/home/%s:/bin/sh\n' "$TEST_USER" "$TEST_UID" "$TEST_GID" "$TEST_USER" |
    sudo tee -a "$MOUNT_DIR/etc/passwd" >/dev/null
fi
if ! sudo grep -q '^docker:' "$MOUNT_DIR/etc/group"; then
  printf 'docker:x:%s:%s\n' "$TEST_GID" "$TEST_USER" | sudo tee -a "$MOUNT_DIR/etc/group" >/dev/null
fi
sudo chown "$TEST_UID:$TEST_GID" "$MOUNT_DIR/home/$TEST_USER"
sudo chmod 0755 "$MOUNT_DIR/home/$TEST_USER"
sudo umount "$MOUNT_DIR"
rmdir "$MOUNT_DIR"
MOUNT_DIR=""

COMMAND='id; echo HOME=$HOME; echo USER=$USER; touch "$HOME/orca-user-proof"; ls -l "$HOME/orca-user-proof"'
COMMAND_B64=$(printf '%s' "$COMMAND" | base64 -w0)
ENV_B64=$(printf 'HOME=/home/%s\nPATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin' "$TEST_USER" | base64 -w0)
USER_B64=$(printf '%s' "$TEST_USER" | base64 -w0)

cat >"$CONFIG" <<EOF
{
  "boot-source": {
    "kernel_image_path": "$KERNEL_IMAGE",
    "boot_args": "root=/dev/vda rw console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/init orca.command_b64=$COMMAND_B64 orca.env_b64=$ENV_B64 orca.user_b64=$USER_B64"
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
    "vcpu_count": $VCPU_COUNT,
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

log "booting Firecracker user switch check"
"$FIRECRACKER_BIN" --api-sock "$SOCKET" --config-file "$CONFIG" >"$SERIAL_LOG" 2>>"$FC_LOG" &
FC_PID=$!

deadline=$((SECONDS + TIMEOUT_SECONDS))
while kill -0 "$FC_PID" 2>/dev/null; do
  if grep -q "orca-init: image-rootfs ok" "$SERIAL_LOG" 2>/dev/null; then
    break
  fi
  if (( SECONDS >= deadline )); then
    echo "timed out waiting for user switch check" >&2
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

grep -q "uid=${TEST_UID}(${TEST_USER})" "$SERIAL_LOG"
grep -q "/home/${TEST_USER}/orca-user-proof" "$SERIAL_LOG"
grep -q "orca-init: image-rootfs ok" "$SERIAL_LOG"

log "orca init user switch check passed"
