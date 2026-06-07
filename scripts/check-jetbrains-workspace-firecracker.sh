#!/usr/bin/env bash
set -euo pipefail

IMAGE=${IMAGE:-registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643}
ASSET_DIR=${ASSET_DIR:-firecracker-assets}
ROOTFS_SIZE_MB=${ROOTFS_SIZE_MB:-10000}
MEM_SIZE_MIB=${MEM_SIZE_MIB:-4096}
VCPU_COUNT=${VCPU_COUNT:-2}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-120}
GUEST_DNS=${GUEST_DNS:-1.1.1.1}
WORK_PARENT=${WORK_PARENT:-$PWD/.tmp/jetbrains-workspace-check}
STREAM_SERIAL=${STREAM_SERIAL:-true}
KEEP_WORKDIR=${KEEP_WORKDIR:-true}

log() { printf '\n==> %s\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1" >&2; exit 2; }; }

need base64
need docker
need go
need grep
need ip
need mkfs.ext4
need mount
need python3
need sudo
need tar
need umount

ASSET_DIR=$(cd "$ASSET_DIR" && pwd)
FIRECRACKER_BIN=${FIRECRACKER_BIN:-$ASSET_DIR/firecracker}
KERNEL_IMAGE=${KERNEL_IMAGE:-$ASSET_DIR/vmlinux}
for path in "$FIRECRACKER_BIN" "$KERNEL_IMAGE"; do
  [[ -e "$path" ]] || { echo "missing required asset: $path" >&2; exit 2; }
done
[[ -e /dev/kvm ]] || { echo "/dev/kvm is missing; Firecracker needs Linux KVM" >&2; exit 1; }

mkdir -p "$WORK_PARENT"
WORK_PARENT=$(cd "$WORK_PARENT" && pwd)
WORK_DIR=$(mktemp -d "$WORK_PARENT/run.XXXXXX")
MOUNT_DIR=""
CID=""
FC_PID=""
TAIL_PID=""
TAP_NAME="tapjb$RANDOM"
HOST_CIDR="172.31.250.1/30"
HOST_IP="172.31.250.1"
GUEST_CIDR="172.31.250.2/30"
GUEST_IP="172.31.250.2"
GUEST_MAC="06:00:ac:1f:fa:02"

cleanup() {
  if [[ -n "$TAIL_PID" ]] && kill -0 "$TAIL_PID" 2>/dev/null; then
    kill "$TAIL_PID" 2>/dev/null || true
    wait "$TAIL_PID" 2>/dev/null || true
  fi
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
  sudo iptables -t nat -D POSTROUTING -s "$GUEST_CIDR" -j MASQUERADE 2>/dev/null || true
  sudo iptables -D FORWARD -i "$TAP_NAME" -j ACCEPT 2>/dev/null || true
  sudo iptables -D FORWARD -o "$TAP_NAME" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true
  sudo ip link del "$TAP_NAME" 2>/dev/null || true
  if [[ "$KEEP_WORKDIR" == "true" ]]; then
    log "keeping workdir: $WORK_DIR"
    log "serial log: $SERIAL_LOG"
    log "firecracker log: $FC_LOG"
    log "config: $CONFIG"
  else
    rm -rf "$WORK_DIR"
  fi
}
trap cleanup EXIT

INIT_BIN="$WORK_DIR/orca-init"
INSPECT_JSON="$WORK_DIR/image-inspect.json"
ROOTFS_TAR="$WORK_DIR/rootfs.tar"
ROOTFS_IMAGE="$WORK_DIR/rootfs.ext4"
SOCKET="$WORK_DIR/firecracker.sock"
CONFIG="$WORK_DIR/firecracker.json"
SERIAL_LOG="$WORK_DIR/serial.log"
FC_LOG="$WORK_DIR/firecracker.log"

log "building current orca init"
BUILD_TIME_UTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTimeUTC=${BUILD_TIME_UTC}" -o "$INIT_BIN" ./cmd/orca-init

log "inspecting $IMAGE"
docker image inspect "$IMAGE" >"$INSPECT_JSON" || {
  log "image not present locally; pulling $IMAGE"
  docker pull "$IMAGE"
  docker image inspect "$IMAGE" >"$INSPECT_JSON"
}

mapfile -t META < <(python3 - "$INSPECT_JSON" <<'PY'
import json, shlex, sys
cfg = json.load(open(sys.argv[1]))[0]["Config"]
entrypoint = cfg.get("Entrypoint") or []
cmd = cfg.get("Cmd") or []
env = cfg.get("Env") or []
user = cfg.get("User") or ""
workdir = cfg.get("WorkingDir") or ""
command = " ".join(shlex.quote(x) for x in [*entrypoint, *cmd] if x)
print(command)
print(user)
print(workdir)
print("\n".join(env))
PY
)
IMAGE_COMMAND=${META[0]}
IMAGE_USER=${META[1]}
IMAGE_WORKDIR=${META[2]}
IMAGE_ENV=$(printf '%s\n' "${META[@]:3}")

log "image command: $IMAGE_COMMAND"
log "image user: ${IMAGE_USER:-<root>}"

log "exporting image filesystem"
CID=$(docker create --entrypoint /bin/sh "$IMAGE" -c true)
docker export "$CID" >"$ROOTFS_TAR"
docker rm -f "$CID" >/dev/null
CID=""

log "creating ${ROOTFS_SIZE_MB}MiB ext4 rootfs"
truncate -s "${ROOTFS_SIZE_MB}M" "$ROOTFS_IMAGE"
mkfs.ext4 -F "$ROOTFS_IMAGE" >/dev/null
MOUNT_DIR=$(mktemp -d)
sudo mount -o loop "$ROOTFS_IMAGE" "$MOUNT_DIR"

log "extracting image filesystem"
sudo tar --numeric-owner -xf "$ROOTFS_TAR" -C "$MOUNT_DIR"

log "injecting image metadata"
sudo mkdir -p "$MOUNT_DIR"/{dev,proc,sys,run,tmp,etc,orca}
sudo install -m 0644 "$INSPECT_JSON" "$MOUNT_DIR/etc/orca-image-inspect.json"
printf '%s\n' "$IMAGE" | sudo tee "$MOUNT_DIR/etc/orca-image-ref" >/dev/null
sudo umount "$MOUNT_DIR"
rmdir "$MOUNT_DIR"
MOUNT_DIR=""

log "sideloading current orca init"
MOUNT_DIR=$(mktemp -d)
sudo mount -o loop "$ROOTFS_IMAGE" "$MOUNT_DIR"
sudo install -m 0755 "$INIT_BIN" "$MOUNT_DIR/init"
sync
sudo umount "$MOUNT_DIR"
rmdir "$MOUNT_DIR"
MOUNT_DIR=""

log "configuring TAP networking"
sudo ip link del "$TAP_NAME" 2>/dev/null || true
sudo ip tuntap add dev "$TAP_NAME" mode tap
sudo ip addr add "$HOST_CIDR" dev "$TAP_NAME"
sudo ip link set dev "$TAP_NAME" up
sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null
sudo iptables -t nat -C POSTROUTING -s "$GUEST_CIDR" -j MASQUERADE 2>/dev/null ||
  sudo iptables -t nat -A POSTROUTING -s "$GUEST_CIDR" -j MASQUERADE
sudo iptables -C FORWARD -i "$TAP_NAME" -j ACCEPT 2>/dev/null ||
  sudo iptables -A FORWARD -i "$TAP_NAME" -j ACCEPT
sudo iptables -C FORWARD -o "$TAP_NAME" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null ||
  sudo iptables -A FORWARD -o "$TAP_NAME" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT

COMMAND_B64=$(printf '%s' "$IMAGE_COMMAND" | base64 -w0)
ENV_B64=$(printf '%s' "$IMAGE_ENV" | base64 -w0)
USER_B64=$(printf '%s' "$IMAGE_USER" | base64 -w0)
WORKDIR_ARG=""
if [[ -n "$IMAGE_WORKDIR" ]]; then
  WORKDIR_ARG=" orca.workdir_b64=$(printf '%s' "$IMAGE_WORKDIR" | base64 -w0)"
fi

cat >"$CONFIG" <<EOF
{
  "boot-source": {
    "kernel_image_path": "$KERNEL_IMAGE",
    "boot_args": "root=/dev/vda rw console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/init orca.tty=1 orca.command_b64=$COMMAND_B64 orca.env_b64=$ENV_B64 orca.user_b64=$USER_B64$WORKDIR_ARG orca.net_ip=$GUEST_CIDR orca.net_gateway=$HOST_IP orca.net_dns=$GUEST_DNS"
  },
  "drives": [
    {
      "drive_id": "rootfs",
      "path_on_host": "$ROOTFS_IMAGE",
      "is_root_device": true,
      "is_read_only": false
    }
  ],
  "network-interfaces": [
    {
      "iface_id": "eth0",
      "guest_mac": "$GUEST_MAC",
      "host_dev_name": "$TAP_NAME"
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

log "booting JetBrains workspace image in Firecracker"
log "workdir: $WORK_DIR"
log "serial log: $SERIAL_LOG"
log "firecracker log: $FC_LOG"
"$FIRECRACKER_BIN" --api-sock "$SOCKET" --config-file "$CONFIG" >"$SERIAL_LOG" 2>>"$FC_LOG" &
FC_PID=$!
if [[ "$STREAM_SERIAL" == "true" ]]; then
  tail -n +1 -F "$SERIAL_LOG" &
  TAIL_PID=$!
fi

deadline=$((SECONDS + TIMEOUT_SECONDS))
while kill -0 "$FC_PID" 2>/dev/null; do
  if grep -q "Workspace Server listening to WorkspaceHttpApiEndpoint" "$SERIAL_LOG" 2>/dev/null &&
     grep -q "Version: 261.643" "$SERIAL_LOG" 2>/dev/null &&
     grep -q "Smart Mode: enabled" "$SERIAL_LOG" 2>/dev/null &&
     grep -q "Published to JetBrains Relay: true" "$SERIAL_LOG" 2>/dev/null; then
    break
  fi
  if grep -qE "Fleet failed|BindException|UnresolvedAddressException" "$SERIAL_LOG" 2>/dev/null; then
    echo "JetBrains workspace failed before readiness" >&2
    echo "--- matching serial lines ---" >&2
    grep -E "Dock HTTP|Workspace Server|Version:|Smart Mode|Published|Fleet failed|BindException|UnresolvedAddressException|orca-init" "$SERIAL_LOG" >&2 || true
    echo "--- serial tail ---" >&2
    tail -220 "$SERIAL_LOG" >&2 || true
    exit 1
  fi
  if (( SECONDS >= deadline )); then
    echo "timed out waiting for JetBrains workspace readiness" >&2
    echo "--- matching serial lines ---" >&2
    grep -E "Dock HTTP|Workspace Server|Version:|Smart Mode|Published|Fleet failed|BindException|UnresolvedAddressException|orca-init" "$SERIAL_LOG" >&2 || true
    echo "--- serial tail ---" >&2
    tail -220 "$SERIAL_LOG" >&2 || true
    exit 1
  fi
  sleep 1
done

if [[ -n "$TAIL_PID" ]] && kill -0 "$TAIL_PID" 2>/dev/null; then
  kill "$TAIL_PID" 2>/dev/null || true
  wait "$TAIL_PID" 2>/dev/null || true
  TAIL_PID=""
fi

log "readiness lines"
grep -E "Dock HTTP|Workspace Server|Version:|Smart Mode|Published to JetBrains Relay" "$SERIAL_LOG"
log "JetBrains workspace Firecracker check passed"
