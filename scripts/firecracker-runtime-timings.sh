#!/usr/bin/env bash
set -euo pipefail

ASSET_DIR=${ASSET_DIR:-firecracker-assets}
OUT_BASE=${OUT_BASE:-firecracker-assets/runtime-timing-study}
RUNTIME=${RUNTIME:-all}
BENCHMARK=${BENCHMARK:-cold-warm}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-120}
MEM_SIZE_MIB=${MEM_SIZE_MIB:-512}
EXPERIMENT_ROOTFS_SIZE_MB=${EXPERIMENT_ROOTFS_SIZE_MB:-2048}
CONTAINER_CMD=${CONTAINER_CMD:-printf "orca runtime payload\n"; uname -a >/tmp/uname.txt; cat /tmp/uname.txt}

FIRECRACKER_BIN=${FIRECRACKER_BIN:-$ASSET_DIR/firecracker}
KERNEL_IMAGE=${KERNEL_IMAGE:-$ASSET_DIR/vmlinux}
ROOTFS_IMAGE=${ROOTFS_IMAGE:-$ASSET_DIR/rootfs.ext4}
ALPINE_TARBALL=${ALPINE_TARBALL:-$ASSET_DIR/alpine-minirootfs-3.22.1-x86_64.tar.gz}
OCI_IMAGE_REF=${OCI_IMAGE_REF:-docker.io/library/orca-alpine-local:latest}

log() { printf '\n==> %s\n' "$*"; }
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1" >&2; exit 2; }; }

need awk
need base64
need cp
need curl
need gzip
need mount
need resize2fs
need sha256sum
need tar
need truncate
need umount

for path in "$FIRECRACKER_BIN" "$KERNEL_IMAGE" "$ROOTFS_IMAGE" "$ALPINE_TARBALL"; do
  [[ -e "$path" ]] || { echo "missing required asset: $path" >&2; exit 2; }
done
[[ -e /dev/kvm ]] || { echo "/dev/kvm is missing" >&2; exit 1; }

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUT_DIR="$OUT_BASE/$STAMP"
mkdir -p "$OUT_DIR"
RESULTS_FILE="$OUT_DIR/results.txt"

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

runtime_list() {
  case "$RUNTIME" in
    all) printf '%s\n' plain docker containerd podman ;;
    plain|docker|containerd|podman) printf '%s\n' "$RUNTIME" ;;
    *) echo "unsupported RUNTIME=$RUNTIME; use plain, docker, containerd, podman, or all" >&2; exit 2 ;;
  esac
}

benchmark_list() {
  case "$BENCHMARK" in
    cold) printf '%s\n' cold ;;
    warm-disk) printf '%s\n' warm-disk ;;
    cold-warm) printf '%s\n' cold warm-disk ;;
    *) echo "unsupported BENCHMARK=$BENCHMARK; use cold, warm-disk, or cold-warm" >&2; exit 2 ;;
  esac
}

build_oci_archive() {
  local out=$1
  local tmp
  tmp=$(mktemp -d)
  mkdir -p "$tmp/oci/blobs/sha256"

  local layer="$tmp/oci/blobs/sha256/layer"
  cp "$ALPINE_TARBALL" "$layer"
  local layer_digest layer_size diff_digest config config_digest config_size manifest manifest_digest manifest_size created
  layer_digest=$(sha256sum "$layer" | awk '{print $1}')
  layer_size=$(stat -c%s "$layer")
  mv "$layer" "$tmp/oci/blobs/sha256/$layer_digest"
  diff_digest=$(gzip -dc "$ALPINE_TARBALL" | sha256sum | awk '{print $1}')
  created=$(date -u +%Y-%m-%dT%H:%M:%SZ)

  config="$tmp/config.json"
  cat >"$config" <<EOF
{"created":"$created","architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"],"Cmd":["/bin/sh"]},"rootfs":{"type":"layers","diff_ids":["sha256:$diff_digest"]},"history":[{"created":"$created","created_by":"orca runtime timing rootfs import"}]}
EOF
  config_digest=$(sha256sum "$config" | awk '{print $1}')
  config_size=$(stat -c%s "$config")
  mv "$config" "$tmp/oci/blobs/sha256/$config_digest"

  manifest="$tmp/manifest.json"
  cat >"$manifest" <<EOF
{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:$config_digest","size":$config_size},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:$layer_digest","size":$layer_size}]}
EOF
  manifest_digest=$(sha256sum "$manifest" | awk '{print $1}')
  manifest_size=$(stat -c%s "$manifest")
  mv "$manifest" "$tmp/oci/blobs/sha256/$manifest_digest"

  cat >"$tmp/oci/oci-layout" <<'EOF'
{"imageLayoutVersion":"1.0.0"}
EOF
  cat >"$tmp/oci/index.json" <<EOF
{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:$manifest_digest","size":$manifest_size,"platform":{"architecture":"amd64","os":"linux"},"annotations":{"org.opencontainers.image.ref.name":"$OCI_IMAGE_REF"}}]}
EOF
  tar -C "$tmp/oci" -cf "$out" .
  rm -rf "$tmp"
}

install_runtime_packages() {
  local runtime=$1
  case "$runtime" in
    plain)
      return 0
      ;;
    docker)
      return 0
      ;;
    containerd)
      sudo chroot "$MOUNT_DIR" /sbin/apk add --no-cache containerd-ctr
      ;;
    podman)
      sudo chroot "$MOUNT_DIR" /sbin/apk add --no-cache podman crun conmon fuse-overlayfs slirp4netns
      ;;
  esac
}

inject_init() {
  local runtime=$1
  local rootfs=$2
  local oci_archive=$3
  local cmd_b64
  cmd_b64=$(printf '%s' "$CONTAINER_CMD" | base64 -w0)

  MOUNT_DIR=$(mktemp -d)
  sudo mount -o loop "$rootfs" "$MOUNT_DIR"
  sudo cp /etc/resolv.conf "$MOUNT_DIR/etc/resolv.conf"
  install_runtime_packages "$runtime"
  sudo mkdir -p "$MOUNT_DIR/opt/orca" "$MOUNT_DIR/etc/containers" "$MOUNT_DIR/etc/containerd"
  if [[ "$runtime" == "containerd" ]]; then
    sudo cp "$oci_archive" "$MOUNT_DIR/opt/orca/alpine-oci.tar"
  fi
  sudo tee "$MOUNT_DIR/etc/containers/storage.conf" >/dev/null <<'EOF'
[storage]
driver = "vfs"
runroot = "/run/containers/storage"
graphroot = "/var/lib/containers/storage"
EOF
  sudo tee "$MOUNT_DIR/init" >/dev/null <<INIT
#!/bin/sh
set -eu
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export XDG_RUNTIME_DIR=/run

log() { echo "orca-init: \$*" > /dev/console; }
uptime_ms() { awk '{ printf "%d", \$1 * 1000 }' /proc/uptime 2>/dev/null || printf "0"; }
timing() { echo "orca-timing: t_ms=\$(uptime_ms) \$*" > /dev/console; }
cmdline_value() {
  key="\$1"
  for arg in \$(cat /proc/cmdline); do
    case "\$arg" in "\$key="*) echo "\${arg#*=}"; return 0 ;; esac
  done
  return 1
}

RUNTIME="$runtime"
CONTAINER_CMD="\$(printf '%s' "$cmd_b64" | base64 -d)"
IMAGE_REF="$OCI_IMAGE_REF"
RUNTIME_PID=""

mount -t proc proc /proc || true
timing mount_proc_done
mount -t sysfs sysfs /sys || true
timing mount_sys_done
mount -t devtmpfs devtmpfs /dev || true
mkdir -p /run /tmp /dev/shm /sys/fs/cgroup /var/run /var/lib/docker /var/lib/containerd /var/lib/containers /libpod_lock
mount -t tmpfs tmpfs /run || true
timing mount_run_done
mount -t tmpfs tmpfs /tmp || true
mount -t tmpfs tmpfs /dev/shm || true
mount -t cgroup2 none /sys/fs/cgroup || true
timing mount_cgroup_done

PHASE="\$(cmdline_value orca.phase || echo cold)"
log "started runtime=\$RUNTIME phase=\$PHASE"
timing "init_ready runtime=\$RUNTIME phase=\$PHASE"

start_runtime() {
  case "\$RUNTIME" in
    plain)
      timing plain_ready_start
      timing plain_ready
      ;;
    docker)
      timing dockerd_start
      log "starting dockerd"
      dockerd --host=unix:///var/run/docker.sock --storage-driver=vfs --iptables=false --bridge=none --ip-forward=false --ip-masq=false --userland-proxy=false >/tmp/dockerd.log 2>&1 &
      pid="\$!"
      RUNTIME_PID="\$pid"
      i=0
      while [ "\$i" -lt 100 ]; do
        if docker version >/tmp/docker-version.log 2>&1; then timing dockerd_ready; return 0; fi
        if ! kill -0 "\$pid" 2>/dev/null; then log "dockerd exited"; cat /tmp/dockerd.log >/dev/console 2>&1 || true; reboot -f; exit 3; fi
        i=\$((i + 1)); sleep 0.2
      done
      log "dockerd timed out"; cat /tmp/dockerd.log >/dev/console 2>&1 || true; reboot -f; exit 4
      ;;
    containerd)
      timing containerd_start
      log "starting containerd"
      containerd >/tmp/containerd.log 2>&1 &
      pid="\$!"
      RUNTIME_PID="\$pid"
      i=0
      while [ "\$i" -lt 100 ]; do
        if ctr version >/tmp/ctr-version.log 2>&1; then timing containerd_ready; return 0; fi
        if ! kill -0 "\$pid" 2>/dev/null; then log "containerd exited"; cat /tmp/containerd.log >/dev/console 2>&1 || true; reboot -f; exit 5; fi
        i=\$((i + 1)); sleep 0.2
      done
      log "containerd timed out"; cat /tmp/containerd.log >/dev/console 2>&1 || true; reboot -f; exit 6
      ;;
    podman)
      timing podman_ready_start
      podman --storage-driver=vfs info >/tmp/podman-info.log 2>&1 || { log "podman info failed"; cat /tmp/podman-info.log >/dev/console 2>&1 || true; reboot -f; exit 7; }
      timing podman_ready
      ;;
  esac
}

stop_runtime() {
  case "\$RUNTIME" in
    docker|containerd)
      if [ -n "\$RUNTIME_PID" ] && kill -0 "\$RUNTIME_PID" 2>/dev/null; then
        timing runtime_stop_start
        kill "\$RUNTIME_PID" 2>/dev/null || true
        i=0
        while kill -0 "\$RUNTIME_PID" 2>/dev/null && [ "\$i" -lt 50 ]; do
          i=\$((i + 1))
          sleep 0.1
        done
        if kill -0 "\$RUNTIME_PID" 2>/dev/null; then
          kill -9 "\$RUNTIME_PID" 2>/dev/null || true
        fi
        timing runtime_stop_done
      fi
      ;;
  esac
}

image_present() {
  case "\$RUNTIME" in
    plain) return 0 ;;
    docker) docker image inspect orca/alpine-local:latest >/dev/null 2>&1 ;;
    containerd) ctr -n default images ls -q | grep -qx "\$IMAGE_REF" ;;
    podman) podman --storage-driver=vfs image exists orca/alpine-local:latest >/dev/null 2>&1 ;;
  esac
}

ensure_image() {
  timing image_present_check_start
  if [ "\$RUNTIME" = "plain" ]; then
    timing image_already_present
    return 0
  fi
  if image_present; then
    timing image_already_present
    return 0
  fi
  timing image_import_start
  case "\$RUNTIME" in
    docker)
      gzip -dc /opt/orca/alpine-container-rootfs.tar.gz | docker import - orca/alpine-local:latest >/dev/console 2>&1
      ;;
    containerd)
      ctr -n default images import --local --base-name docker.io/library/orca-alpine-local --digests --platform linux/amd64 --snapshotter native /opt/orca/alpine-oci.tar >/dev/console 2>&1
      ctr -n default images tag "\$IMAGE_REF" orca/alpine-local:latest >/dev/console 2>&1 || true
      ;;
    podman)
      podman --storage-driver=vfs import /opt/orca/alpine-container-rootfs.tar.gz orca/alpine-local:latest >/dev/console 2>&1
      ;;
  esac
  timing image_import_done
}

require_image_present() {
  timing image_present_check_start
  if [ "\$RUNTIME" = "plain" ]; then
    timing image_already_present
    return 0
  fi
  if ! image_present; then
    log "expected warm image to exist but it was missing"
    case "\$RUNTIME" in
      docker) docker image ls >/dev/console 2>&1 || true ;;
      containerd) ctr -n default images ls >/dev/console 2>&1 || true ;;
      podman) podman --storage-driver=vfs images >/dev/console 2>&1 || true ;;
    esac
    reboot -f
    exit 8
  fi
  timing image_already_present
}

run_container() {
  timing container_run_start
  case "\$RUNTIME" in
    plain)
      /bin/sh -c "\$CONTAINER_CMD" >/tmp/container-run.log 2>&1
      ;;
    docker)
      docker run --rm --network=none orca/alpine-local:latest /bin/sh -c "\$CONTAINER_CMD" >/tmp/container-run.log 2>&1
      ;;
    containerd)
      ctr -n default run --rm --snapshotter native "\$IMAGE_REF" orca-study /bin/sh -c "\$CONTAINER_CMD" >/tmp/container-run.log 2>&1
      ;;
    podman)
      podman --storage-driver=vfs run --rm --network=none orca/alpine-local:latest /bin/sh -c "\$CONTAINER_CMD" >/tmp/container-run.log 2>&1
      ;;
  esac
  timing container_run_done
  cat /tmp/container-run.log >/dev/console 2>&1 || true
}

start_runtime
case "\$PHASE" in
  prepare)
    ensure_image
    stop_runtime
    sync
    timing prepare_sync_done
    log "runtime-prepare ok"
    ;;
  warm)
    require_image_present
    run_container
    log "runtime-study ok"
    ;;
  cold)
    ensure_image
    run_container
    log "runtime-study ok"
    ;;
  *)
    log "unknown phase: \$PHASE"
    reboot -f
    exit 2
    ;;
esac

timing reboot_start
reboot -f
INIT
  sudo chmod +x "$MOUNT_DIR/init"
  sudo ln -sf /init "$MOUNT_DIR/sbin/init"
  sudo umount "$MOUNT_DIR"
  rmdir "$MOUNT_DIR"
  MOUNT_DIR=""
}

wait_for_log() {
  local pid=$1
  local serial=$2
  local pattern=$3
  local deadline=$((SECONDS + TIMEOUT_SECONDS))
  while kill -0 "$pid" 2>/dev/null; do
    if grep -q "$pattern" "$serial" 2>/dev/null; then return 0; fi
    if (( SECONDS >= deadline )); then
      echo "timed out waiting for $pattern" >&2
      cat "$serial" >&2 || true
      return 1
    fi
    sleep 0.1
  done
  grep -q "$pattern" "$serial" 2>/dev/null
}

write_config() {
  local config=$1
  local rootfs=$2
  local log_path=$3
  local phase=$4
  cat >"$config" <<EOF
{
  "boot-source": {
    "kernel_image_path": "$KERNEL_IMAGE",
    "boot_args": "root=/dev/vda rw console=ttyS0 quiet loglevel=0 reboot=k panic=1 pci=off init=/init orca.phase=$phase"
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
    "log_path": "$log_path",
    "level": "Info",
    "show_level": true,
    "show_log_origin": true
  }
}
EOF
}

run_vm_phase() {
  local runtime=$1
  local phase=$2
  local rootfs=$3
  local phase_dir=$4
  local marker=$5
  mkdir -p "$phase_dir"
  local socket="/tmp/orca-fc-runtime-$runtime-$phase-$BASHPID.sock"
  local config="$phase_dir/firecracker.json"
  local serial="$phase_dir/serial.log"
  local fc_log="$phase_dir/firecracker.log"

  write_config "$config" "$rootfs" "$fc_log" "$phase"
  log "[$runtime/$phase] booting Firecracker"
  "$FIRECRACKER_BIN" --api-sock "$socket" --config-file "$config" >"$serial" 2>>"$fc_log" &
  FC_PID=$!
  wait_for_log "$FC_PID" "$serial" "$marker"
  wait "$FC_PID" 2>/dev/null || true
  FC_PID=""

  log "[$runtime/$phase] serial output"
  grep -E 'orca-(init|timing):' "$serial" || true
  log "[$runtime/$phase] timing summary"
  awk '
    /orca-timing:/ {
      t = $2
      sub("t_ms=", "", t)
      name = ""
      for (i = 3; i <= NF; i++) name = name (i == 3 ? "" : " ") $i
      gsub(/\r/, "", name)
      printf "  %-42s %8sms\n", name, t
    }
  ' "$serial" || true
  append_phase_results "$runtime" "$phase" "$serial" "$phase_dir"
}

append_phase_results() {
  local runtime=$1
  local phase=$2
  local serial=$3
  local phase_dir=$4
  awk -v runtime="$runtime" -v phase="$phase" -v phase_dir="$phase_dir" '
    function set(name, value) { t[name] = value + 0 }
    function get(name) { return (name in t) ? t[name] : -1 }
    function delta(start, done) {
      if (get(start) < 0 || get(done) < 0) return "n/a"
      return sprintf("%dms", get(done) - get(start))
    }
    function at(name) {
      if (get(name) < 0) return "n/a"
      return sprintf("%dms", get(name))
    }
    /orca-timing:/ {
      value = $2
      sub("t_ms=", "", value)
      name = ""
      for (i = 3; i <= NF; i++) name = name (i == 3 ? "" : " ") $i
      gsub(/\r/, "", name)
      set(name, value)
    }
    END {
      ready_start = ""
      ready_done = ""
      if (runtime == "docker") {
        ready_start = "dockerd_start"
        ready_done = "dockerd_ready"
      } else if (runtime == "containerd") {
        ready_start = "containerd_start"
        ready_done = "containerd_ready"
      } else if (runtime == "podman") {
        ready_start = "podman_ready_start"
        ready_done = "podman_ready"
      } else if (runtime == "plain") {
        ready_start = "plain_ready_start"
        ready_done = "plain_ready"
      }

      printf "\n[%s/%s]\n", runtime, phase
      printf "phase_dir=%s\n", phase_dir
      printf "boot_to_init=%s\n", at("init_ready runtime=" runtime " phase=" phase)
      printf "runtime_ready_at=%s\n", at(ready_done)
      printf "runtime_ready_delta=%s\n", delta(ready_start, ready_done)
      if (get("image_import_start") >= 0) {
        printf "image_state=imported\n"
        printf "image_delta=%s\n", delta("image_import_start", "image_import_done")
      } else if (get("image_already_present") >= 0) {
        printf "image_state=already_present\n"
        printf "image_check_delta=%s\n", delta("image_present_check_start", "image_already_present")
      } else {
        printf "image_state=n/a\n"
      }
      if (get("container_run_start") >= 0) {
        printf "container_run_delta=%s\n", delta("container_run_start", "container_run_done")
        printf "payload_done_at=%s\n", at("container_run_done")
      } else {
        printf "container_run_delta=n/a\n"
        printf "payload_done_at=n/a\n"
      }
      if (get("prepare_sync_done") >= 0) {
        printf "prepare_done_at=%s\n", at("prepare_sync_done")
      }
      printf "reboot_at=%s\n", at("reboot_start")
    }
  ' "$serial" >>"$RESULTS_FILE"
}

prepare_rootfs() {
  local runtime=$1
  local rootfs=$2
  log "[$runtime] preparing rootfs $rootfs"
  copy_image "$ROOTFS_IMAGE" "$rootfs"
  truncate -s "${EXPERIMENT_ROOTFS_SIZE_MB}M" "$rootfs"
  resize2fs -f "$rootfs" >/dev/null
  inject_init "$runtime" "$rootfs" "$OUT_DIR/alpine-oci.tar"
}

run_cold() {
  local runtime=$1
  local run_dir="$OUT_DIR/$runtime/cold"
  local rootfs="$run_dir/rootfs.ext4"
  mkdir -p "$run_dir"
  prepare_rootfs "$runtime" "$rootfs"
  run_vm_phase "$runtime" cold "$rootfs" "$run_dir" "orca-init: runtime-study ok"
}

run_warm_disk() {
  local runtime=$1
  local run_dir="$OUT_DIR/$runtime/warm-disk"
  local rootfs="$run_dir/rootfs.ext4"
  mkdir -p "$run_dir"
  prepare_rootfs "$runtime" "$rootfs"
  run_vm_phase "$runtime" prepare "$rootfs" "$run_dir/prepare" "orca-init: runtime-prepare ok"
  run_vm_phase "$runtime" warm "$rootfs" "$run_dir/warm" "orca-init: runtime-study ok"
}

log "writing runtime timing study under $OUT_DIR"
{
  printf 'runtime=%s\n' "$RUNTIME"
  printf 'benchmark=%s\n' "$BENCHMARK"
  printf 'mem_size_mib=%s\n' "$MEM_SIZE_MIB"
  printf 'experiment_rootfs_size_mb=%s\n' "$EXPERIMENT_ROOTFS_SIZE_MB"
  printf 'container_cmd=%s\n' "$CONTAINER_CMD"
} >"$OUT_DIR/README.txt"
{
  printf 'Runtime Timing Study\n'
  printf 'out_dir=%s\n' "$OUT_DIR"
  printf 'runtime=%s\n' "$RUNTIME"
  printf 'benchmark=%s\n' "$BENCHMARK"
  printf 'mem_size_mib=%s\n' "$MEM_SIZE_MIB"
  printf 'experiment_rootfs_size_mb=%s\n' "$EXPERIMENT_ROOTFS_SIZE_MB"
  printf 'container_cmd=%s\n' "$CONTAINER_CMD"
} >"$RESULTS_FILE"

if [[ "$RUNTIME" == "all" || "$RUNTIME" == "containerd" ]]; then
  log "building OCI archive for containerd"
  build_oci_archive "$OUT_DIR/alpine-oci.tar"
fi

for runtime in $(runtime_list); do
  for benchmark in $(benchmark_list); do
    case "$benchmark" in
      cold) run_cold "$runtime" ;;
      warm-disk) run_warm_disk "$runtime" ;;
    esac
  done
done

log "done: $OUT_DIR"
