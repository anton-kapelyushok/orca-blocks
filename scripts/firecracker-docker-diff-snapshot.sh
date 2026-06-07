#!/usr/bin/env bash
set -euo pipefail

ASSET_DIR=${ASSET_DIR:-firecracker-assets}
OUT_BASE=${OUT_BASE:-firecracker-assets/docker-diff-snapshot-study}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-45}
MEM_SIZE_MIB=${MEM_SIZE_MIB:-128}
TOUCH_MEM_MIB=${TOUCH_MEM_MIB:-}
TOUCH_MEM_PATTERN=${TOUCH_MEM_PATTERN:-zero}
STOP_CONTAINER_BEFORE_DIFF=${STOP_CONTAINER_BEFORE_DIFF:-true}
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
  CONTAINER_CMD=${CONTAINER_CMD:-trap : TERM INT; sleep 3600 & wait}
fi

FIRECRACKER_BIN=${FIRECRACKER_BIN:-$ASSET_DIR/firecracker}
KERNEL_IMAGE=${KERNEL_IMAGE:-$ASSET_DIR/vmlinux}
ROOTFS_IMAGE=${ROOTFS_IMAGE:-$ASSET_DIR/rootfs.ext4}

log() { printf '\n==> %s\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1" >&2; exit 2; }; }
need awk
need base64
need cp
need curl
need mount
need umount

for path in "$FIRECRACKER_BIN" "$KERNEL_IMAGE" "$ROOTFS_IMAGE"; do
  [[ -e "$path" ]] || { echo "missing required Firecracker asset: $path" >&2; exit 2; }
done
[[ -e /dev/kvm ]] || { echo "/dev/kvm is missing" >&2; exit 1; }

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUT_DIR="$OUT_BASE/$STAMP"
mkdir -p "$OUT_DIR"

ROOTFS="$OUT_DIR/rootfs.ext4"
SOCKET="/tmp/orca-fc-diff-$BASHPID.sock"
CONFIG="$OUT_DIR/firecracker.json"
SERIAL="$OUT_DIR/serial.log"
FC_LOG="$OUT_DIR/firecracker.log"
FULL_MEM="$OUT_DIR/full-memory.snap"
FULL_STATE="$OUT_DIR/full-vmstate.snap"
DIFF_MEM="$OUT_DIR/diff-memory.snap"
DIFF_STATE="$OUT_DIR/diff-vmstate.snap"
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
  rm -f "$SOCKET"
}
trap cleanup EXIT

copy_image() {
  if cp --reflink=auto "$1" "$2" 2>/dev/null; then return 0; fi
  cp "$1" "$2"
}

inject_init() {
  MOUNT_DIR=$(mktemp -d)
  sudo mount -o loop "$ROOTFS" "$MOUNT_DIR"
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
  CONTAINER_CMD="trap : TERM INT; sleep 3600 & wait"
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
STOP_CONTAINER_BEFORE_DIFF="$(cmdline_value orca.stop_container_before_diff || echo true)"

log "started docker diff snapshot study"
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

log "warm-ready"
timing warm_ready
sleep 2

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

if [ "$STOP_CONTAINER_BEFORE_DIFF" = "true" ]; then
  timing docker_stop_start
  docker stop orca-study >/tmp/docker-stop.log 2>&1 || true
  cat /tmp/docker-stop.log >/dev/console 2>&1 || true
  docker rm orca-study >/tmp/docker-rm.log 2>&1 || true
  timing docker_stop_done
  log "container stopped"
else
  timing docker_left_running
  log "container left running for diff snapshot"
fi
sync
timing sync_done
log "diff-ready"
while true; do sleep 3600; done
INIT
  sudo chmod +x "$MOUNT_DIR/init"
  sudo ln -sf /init "$MOUNT_DIR/sbin/init"
  sudo umount "$MOUNT_DIR"
  rmdir "$MOUNT_DIR"
  MOUNT_DIR=""
}

api() {
  local method=$1 path=$2 body=$3
  curl --silent --show-error --fail --unix-socket "$SOCKET" -X "$method" "http://firecracker$path" -H 'content-type: application/json' -d "$body" >/dev/null
}

wait_for_log() {
  local pattern=$1
  local deadline=$((SECONDS + TIMEOUT_SECONDS))
  while kill -0 "$FC_PID" 2>/dev/null; do
    if grep -q "$pattern" "$SERIAL" 2>/dev/null; then return 0; fi
    if (( SECONDS >= deadline )); then
      echo "timed out waiting for $pattern" >&2
      cat "$SERIAL" >&2 || true
      exit 1
    fi
    sleep 0.1
  done
  echo "firecracker exited before $pattern" >&2
  cat "$SERIAL" >&2 || true
  exit 1
}

log "writing diff snapshot experiment under $OUT_DIR"
{
  printf 'container_cmd=%s\n' "$CONTAINER_CMD"
  printf 'mem_size_mib=%s\n' "$MEM_SIZE_MIB"
  printf 'touch_mem_mib=%s\n' "${TOUCH_MEM_MIB:-}"
  printf 'touch_mem_pattern=%s\n' "$TOUCH_MEM_PATTERN"
  printf 'stop_container_before_diff=%s\n' "$STOP_CONTAINER_BEFORE_DIFF"
  printf 'container_docker_args=%s\n' "$CONTAINER_DOCKER_ARGS"
  printf 'container_ready_path=%s\n' "$CONTAINER_READY_PATH"
} > "$OUT_DIR/README.txt"
log "preparing rootfs"
copy_image "$ROOTFS_IMAGE" "$ROOTFS"
inject_init

CMD_B64=$(printf '%s' "$CONTAINER_CMD" | base64 -w0)
DOCKER_ARGS_B64=$(printf '%s' "$CONTAINER_DOCKER_ARGS" | base64 -w0)
READY_PATH_B64=$(printf '%s' "$CONTAINER_READY_PATH" | base64 -w0)
cat >"$CONFIG" <<EOF
{
  "boot-source": {
    "kernel_image_path": "$KERNEL_IMAGE",
    "boot_args": "root=/dev/vda rw console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/init orca.container_cmd_b64=$CMD_B64 orca.container_docker_args_b64=$DOCKER_ARGS_B64 orca.container_ready_path_b64=$READY_PATH_B64 orca.stop_container_before_diff=$STOP_CONTAINER_BEFORE_DIFF"
  },
  "drives": [
    { "drive_id": "rootfs", "path_on_host": "$ROOTFS", "is_root_device": true, "is_read_only": false }
  ],
  "machine-config": {
    "vcpu_count": 1,
    "mem_size_mib": $MEM_SIZE_MIB,
    "track_dirty_pages": true
  },
  "logger": {
    "log_path": "$FC_LOG",
    "level": "Info",
    "show_level": true,
    "show_log_origin": true
  }
}
EOF

log "booting Firecracker"
"$FIRECRACKER_BIN" --api-sock "$SOCKET" --config-file "$CONFIG" >"$SERIAL" 2>>"$FC_LOG" &
FC_PID=$!

wait_for_log 'orca-init: warm-ready'
log "creating Full snapshot at warm-ready"
api PATCH /vm '{"state":"Paused"}'
api PUT /snapshot/create "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$FULL_STATE\",\"mem_file_path\":\"$FULL_MEM\"}"
api PATCH /vm '{"state":"Resumed"}'

wait_for_log 'orca-init: diff-ready'
log "creating Diff snapshot after stopped container"
api PATCH /vm '{"state":"Paused"}'
api PUT /snapshot/create "{\"snapshot_type\":\"Diff\",\"snapshot_path\":\"$DIFF_STATE\",\"mem_file_path\":\"$DIFF_MEM\"}"

kill "$FC_PID" 2>/dev/null || true
wait "$FC_PID" 2>/dev/null || true
FC_PID=""

log "serial output"
grep -E 'orca-(init|timing):' "$SERIAL" || true
log "snapshot sizes"
ls -lh "$FULL_MEM" "$FULL_STATE" "$DIFF_MEM" "$DIFF_STATE" "$ROOTFS"
du -h "$FULL_MEM" "$FULL_STATE" "$DIFF_MEM" "$DIFF_STATE" "$ROOTFS"
log "done: $OUT_DIR"
