#!/usr/bin/env bash
set -euo pipefail

ASSET_DIR=${ASSET_DIR:-firecracker-assets}
OUT_BASE=${OUT_BASE:-firecracker-assets/docker-memory-snapshot-study}
COUNT=${COUNT:-5}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-35}
MEM_SIZE_MIB=${MEM_SIZE_MIB:-128}
TOUCH_MEM_MIB=${TOUCH_MEM_MIB:-}
TOUCH_MEM_PATTERN=${TOUCH_MEM_PATTERN:-zero}
STOP_CONTAINER_BEFORE_SNAPSHOT=${STOP_CONTAINER_BEFORE_SNAPSHOT:-true}
CONTAINER_DOCKER_ARGS=${CONTAINER_DOCKER_ARGS:-}
CONTAINER_READY_PATH=${CONTAINER_READY_PATH:-}

if [[ -n "$TOUCH_MEM_MIB" && -z "${CONTAINER_CMD+x}" ]]; then
  CONTAINER_DOCKER_ARGS=${CONTAINER_DOCKER_ARGS:-"--tmpfs /mem:size=${TOUCH_MEM_MIB}m"}
  CONTAINER_READY_PATH=${CONTAINER_READY_PATH:-/mem/blob.ready}
  case "$TOUCH_MEM_PATTERN" in
    zero) CONTAINER_CMD="dd if=/dev/zero of=/mem/blob bs=1M count=${TOUCH_MEM_MIB}; sync; touch /mem/blob.ready; sleep 3600" ;;
    urandom) CONTAINER_CMD="dd if=/dev/urandom of=/mem/blob bs=1M count=${TOUCH_MEM_MIB}; sync; touch /mem/blob.ready; sleep 3600" ;;
    *) echo "unsupported TOUCH_MEM_PATTERN=$TOUCH_MEM_PATTERN; use zero or urandom" >&2; exit 2 ;;
  esac
else
  CONTAINER_CMD=${CONTAINER_CMD:-sleep 3600}
fi

FIRECRACKER_BIN=${FIRECRACKER_BIN:-$ASSET_DIR/firecracker}
KERNEL_IMAGE=${KERNEL_IMAGE:-$ASSET_DIR/vmlinux}
ROOTFS_IMAGE=${ROOTFS_IMAGE:-$ASSET_DIR/rootfs.ext4}

log() { printf "\n==> %s\n" "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1" >&2; exit 2; }; }

need awk
need curl
need cp
need mount
need umount

for path in "$FIRECRACKER_BIN" "$KERNEL_IMAGE" "$ROOTFS_IMAGE"; do
  [[ -e "$path" ]] || { echo "missing required Firecracker asset: $path" >&2; exit 2; }
done
[[ -e /dev/kvm ]] || { echo "/dev/kvm is missing" >&2; exit 1; }

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUT_DIR="$OUT_BASE/$STAMP"
mkdir -p "$OUT_DIR"

MOUNT_DIR=""
FC_PID=""
cleanup() {
  if [[ -n "$FC_PID" ]] && kill -0 "$FC_PID" 2>/dev/null; then
    kill "$FC_PID" 2>/dev/null || true
    wait "$FC_PID" 2>/dev/null || true
  fi
  if [[ -n "$MOUNT_DIR" ]] && mountpoint -q "$MOUNT_DIR" 2>/dev/null; then
    sudo umount "$MOUNT_DIR" || true
  fi
}
trap cleanup EXIT

copy_image() {
  local src=$1
  local dst=$2
  if cp --reflink=auto "$src" "$dst" 2>/dev/null; then
    return 0
  fi
  cp "$src" "$dst"
}

inject_init() {
  local rootfs=$1
  MOUNT_DIR=$(mktemp -d)
  sudo mount -o loop "$rootfs" "$MOUNT_DIR"
  sudo tee "$MOUNT_DIR/init" >/dev/null <<'INIT'
#!/bin/sh
set -eu
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

log() { echo "orca-init: $*" > /dev/console; }
uptime_ms() { awk '{ printf "%d", $1 * 1000 }' /proc/uptime 2>/dev/null || printf "0"; }
timing() { echo "orca-timing: t_ms=$(uptime_ms) $*" > /dev/console; }
cmdline_value() {
  key="$1"
  for arg in $(cat /proc/cmdline); do
    case "$arg" in "$key="*) echo "${arg#*=}"; return 0 ;; esac
  done
  return 1
}

mount -t proc proc /proc || true
timing mount_proc_done
mount -t sysfs sysfs /sys || true
timing mount_sys_done
mount -t devtmpfs devtmpfs /dev || true
mkdir -p /run /tmp /sys/fs/cgroup /var/run /var/lib/docker
mount -t tmpfs tmpfs /run || true
timing mount_run_done
mount -t tmpfs tmpfs /tmp || true
mount -t cgroup2 none /sys/fs/cgroup || true
timing mount_cgroup_done

CONTAINER_CMD_B64="$(cmdline_value orca.container_cmd_b64 || true)"
if [ -n "$CONTAINER_CMD_B64" ]; then
  CONTAINER_CMD="$(printf '%s' "$CONTAINER_CMD_B64" | base64 -d)"
else
  CONTAINER_CMD="sleep 3600"
fi
CONTAINER_DOCKER_ARGS_B64="$(cmdline_value orca.container_docker_args_b64 || true)"
if [ -n "$CONTAINER_DOCKER_ARGS_B64" ]; then
  CONTAINER_DOCKER_ARGS="$(printf '%s' "$CONTAINER_DOCKER_ARGS_B64" | base64 -d)"
else
  CONTAINER_DOCKER_ARGS=""
fi
CONTAINER_READY_PATH_B64="$(cmdline_value orca.container_ready_path_b64 || true)"
if [ -n "$CONTAINER_READY_PATH_B64" ]; then
  CONTAINER_READY_PATH="$(printf '%s' "$CONTAINER_READY_PATH_B64" | base64 -d)"
else
  CONTAINER_READY_PATH=""
fi
STOP_CONTAINER_BEFORE_SNAPSHOT="$(cmdline_value orca.stop_container_before_snapshot || echo true)"

log "started docker snapshot study"
timing init_ready

timing dockerd_start
log "starting dockerd"
dockerd --host=unix:///var/run/docker.sock --storage-driver=vfs --iptables=false --bridge=none --ip-forward=false --ip-masq=false --userland-proxy=false >/tmp/dockerd.log 2>&1 &
DOCKERD_PID="$!"
i=0
while [ "$i" -lt 80 ]; do
  if docker version >/tmp/docker-version.log 2>&1; then
    timing dockerd_ready
    log "dockerd ready"
    break
  fi
  if ! kill -0 "$DOCKERD_PID" 2>/dev/null; then
    log "dockerd exited early"
    cat /tmp/dockerd.log >/dev/console 2>&1 || true
    reboot -f
    exit 3
  fi
  i=$((i + 1))
  sleep 0.2
done
if ! docker version >/dev/null 2>&1; then
  log "dockerd timed out"
  cat /tmp/dockerd.log >/dev/console 2>&1 || true
  reboot -f
  exit 4
fi

if docker image inspect orca/alpine-local:latest >/dev/null 2>&1; then
  timing image_already_loaded
else
  timing image_import_start
  log "loading offline alpine image"
  gzip -dc /opt/orca/alpine-container-rootfs.tar.gz | docker import - orca/alpine-local:latest >/dev/console 2>&1
  timing image_import_done
fi

timing docker_run_start
log "running container command: $CONTAINER_CMD"
docker run -d --name orca-study --network=none $CONTAINER_DOCKER_ARGS orca/alpine-local:latest /bin/sh -c "$CONTAINER_CMD" >/tmp/container-id 2>/tmp/docker-run.log
cat /tmp/docker-run.log >/dev/console 2>&1 || true
timing docker_run_started
log "container started"

if [ -n "$CONTAINER_READY_PATH" ]; then
  timing container_ready_wait_start
  i=0
  while [ "$i" -lt 600 ]; do
    if docker exec orca-study test -e "$CONTAINER_READY_PATH" >/tmp/container-ready.log 2>&1; then
      timing container_ready_wait_done
      log "container ready marker found: $CONTAINER_READY_PATH"
      break
    fi
    if ! docker inspect orca-study >/tmp/docker-inspect.log 2>&1; then
      log "container disappeared while waiting for ready marker"
      cat /tmp/container-ready.log >/dev/console 2>&1 || true
      reboot -f
      exit 5
    fi
    i=$((i + 1))
    sleep 0.2
  done
  if ! docker exec orca-study test -e "$CONTAINER_READY_PATH" >/dev/null 2>&1; then
    log "timed out waiting for container ready marker: $CONTAINER_READY_PATH"
    docker logs orca-study >/dev/console 2>&1 || true
    reboot -f
    exit 6
  fi
fi

if [ "$STOP_CONTAINER_BEFORE_SNAPSHOT" = "true" ]; then
  timing docker_stop_start
  docker stop orca-study >/tmp/docker-stop.log 2>&1 || true
  cat /tmp/docker-stop.log >/dev/console 2>&1 || true
  docker rm orca-study >/tmp/docker-rm.log 2>&1 || true
  timing docker_stop_done
  log "container stopped"
else
  timing docker_left_running
  log "container left running for snapshot"
fi
sync
timing sync_done
log "snapshot-ready"
while true; do sleep 3600; done
INIT
  sudo chmod +x "$MOUNT_DIR/init"
  sudo ln -sf /init "$MOUNT_DIR/sbin/init"
  sudo umount "$MOUNT_DIR"
  rmdir "$MOUNT_DIR"
  MOUNT_DIR=""
}

run_one() {
  local idx=$1
  local run_dir="$OUT_DIR/run-$idx"
  mkdir -p "$run_dir"
  local rootfs="$run_dir/rootfs.ext4"
  local socket="/tmp/orca-fc-$idx-$BASHPID.sock"
  local config="$run_dir/firecracker.json"
  local serial="$run_dir/serial.log"
  local fc_log="$run_dir/firecracker.log"
  local mem="$run_dir/memory.snap"
  local vmstate="$run_dir/vmstate.snap"
  local cmd_b64
  local docker_args_b64
  local ready_path_b64
  cmd_b64=$(printf '%s' "$CONTAINER_CMD" | base64 -w0)
  docker_args_b64=$(printf '%s' "$CONTAINER_DOCKER_ARGS" | base64 -w0)
  ready_path_b64=$(printf '%s' "$CONTAINER_READY_PATH" | base64 -w0)

  log "[$idx/$COUNT] preparing rootfs"
  copy_image "$ROOTFS_IMAGE" "$rootfs"
  inject_init "$rootfs"

  cat >"$config" <<EOF
{
  "boot-source": {
    "kernel_image_path": "$KERNEL_IMAGE",
    "boot_args": "root=/dev/vda rw console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/init orca.container_cmd_b64=$cmd_b64 orca.container_docker_args_b64=$docker_args_b64 orca.container_ready_path_b64=$ready_path_b64 orca.stop_container_before_snapshot=$STOP_CONTAINER_BEFORE_SNAPSHOT"
  },
  "drives": [
    { "drive_id": "rootfs", "path_on_host": "$rootfs", "is_root_device": true, "is_read_only": false }
  ],
  "machine-config": {
    "vcpu_count": 1,
    "mem_size_mib": $MEM_SIZE_MIB,
    "track_dirty_pages": false
  },
  "logger": {
    "log_path": "$fc_log",
    "level": "Info",
    "show_level": true,
    "show_log_origin": true
  }
}
EOF

  log "[$idx/$COUNT] booting Firecracker"
  "$FIRECRACKER_BIN" --api-sock "$socket" --config-file "$config" >"$serial" 2>>"$fc_log" &
  FC_PID=$!

  local deadline=$((SECONDS + TIMEOUT_SECONDS))
  while kill -0 "$FC_PID" 2>/dev/null; do
    if grep -q "orca-init: snapshot-ready" "$serial" 2>/dev/null; then
      break
    fi
    if (( SECONDS >= deadline )); then
      echo "timed out waiting for snapshot-ready in run $idx" >&2
      cat "$serial" >&2 || true
      exit 1
    fi
    sleep 0.2
  done

  log "[$idx/$COUNT] pausing and snapshotting"
  curl --silent --show-error --fail --unix-socket "$socket" -X PATCH http://firecracker/vm -H 'content-type: application/json' -d '{"state":"Paused"}' >/dev/null
  curl --silent --show-error --fail --unix-socket "$socket" -X PUT http://firecracker/snapshot/create -H 'content-type: application/json' -d "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$vmstate\",\"mem_file_path\":\"$mem\"}" >/dev/null

  kill "$FC_PID" 2>/dev/null || true
  wait "$FC_PID" 2>/dev/null || true
  FC_PID=""

  log "[$idx/$COUNT] serial output"
  grep -E 'orca-(init|timing):' "$serial" || true
  ls -lh "$mem" "$vmstate" "$rootfs"
}

log "writing snapshots under $OUT_DIR"
printf 'container_cmd=%s\n' "$CONTAINER_CMD" > "$OUT_DIR/README.txt"
{
  printf 'mem_size_mib=%s\n' "$MEM_SIZE_MIB"
  printf 'touch_mem_mib=%s\n' "${TOUCH_MEM_MIB:-}"
  printf 'touch_mem_pattern=%s\n' "$TOUCH_MEM_PATTERN"
  printf 'stop_container_before_snapshot=%s\n' "$STOP_CONTAINER_BEFORE_SNAPSHOT"
  printf 'container_docker_args=%s\n' "$CONTAINER_DOCKER_ARGS"
  printf 'container_ready_path=%s\n' "$CONTAINER_READY_PATH"
} >> "$OUT_DIR/README.txt"
for i in $(seq 1 "$COUNT"); do
  run_one "$i"
done

log "snapshot summary"
find "$OUT_DIR" -maxdepth 2 \( -name memory.snap -o -name vmstate.snap -o -name rootfs.ext4 -o -name serial.log \) -printf '%p %s bytes\n' | sort
find "$OUT_DIR" -maxdepth 2 \( -name memory.snap -o -name vmstate.snap -o -name rootfs.ext4 \) -exec du -h {} + | sort
log "done: $OUT_DIR"
