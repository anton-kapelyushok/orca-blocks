#!/usr/bin/env bash
set -euo pipefail

# Reproducible runner for the JetBrains Workspace real-env OverlayBD benchmark.
#
# Expected VM setup:
#   1. setup-sysbox-docker-vm.sh
#   2. setup-overlaybd-containerd-snapshotter.sh
#
# The durable store is the native registry at REGISTRY_ADDR. Local Docker images,
# containerd content, OverlayBD snapshots, and optional OverlayBD caches can be
# cleared without deleting registry blobs.

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "run as root" >&2
  exit 1
fi

PHASE="${1:-status}"

IMAGE="${IMAGE:-registry.jetbrains.team/p/fleet/docker-public/air-workspace-linux_x64:261.643}"
REGISTRY_ADDR="${REGISTRY_ADDR:-127.0.0.1:5000}"
REPO="${REPO:-orca/overlaybd-jb-real}"
IMAGE_KEY="${IMAGE_KEY:-air-workspace-261643}"
NORMAL_REF="${NORMAL_REF:-${REGISTRY_ADDR}/${REPO}:normal-${IMAGE_KEY}}"
OBD_REF="${OBD_REF:-${REGISTRY_ADDR}/${REPO}:obd-${IMAGE_KEY}}"
RESULTS_DIR="${RESULTS_DIR:-/root}"
SCRIPT_PATH="${SCRIPT_PATH:-/root/measure-overlaybd-jetbrains-workspace-real.py}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-300}"
CTR="${CTR:-/opt/overlaybd/snapshotter/ctr}"
NAMESPACE="${NAMESPACE:-moby}"

RUNTIME_LABEL="${RUNTIME_LABEL:-sysbox-runc-cni}"
CTR_RUN_RUNTIME="${CTR_RUN_RUNTIME:-io.containerd.runc.v2}"
CTR_RUN_RUNC_BINARY="${CTR_RUN_RUNC_BINARY:-/usr/bin/sysbox-runc}"
CTR_NET_MODE="${CTR_NET_MODE:-cni}"
ALLOW_NEW_PRIVS="${ALLOW_NEW_PRIVS:-1}"
DB_STR="${DB_STR:-overlaybd:overlaybd@tcp(127.0.0.1:3306)/overlaybd}"

# Cold means local node state is gone. It does not delete durable registry blobs
# or SQL conversion metadata unless explicitly requested.
CLEAR_DOCKER_IMAGES="${CLEAR_DOCKER_IMAGES:-1}"
CLEAR_OVERLAYBD_CACHES="${CLEAR_OVERLAYBD_CACHES:-1}"
DROP_HOST_CACHES="${DROP_HOST_CACHES:-0}"

log() {
  printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"
}

usage() {
  cat <<EOF
usage: $0 [status|prepare|clean-local|cold-run|warm-run]

phases:
  status       Print local cache/registry state.
  prepare      Mirror IMAGE to local registry and convert it to OverlayBD.
  clean-local  Remove local Docker/containerd/OverlayBD runtime state, keep registry.
  cold-run     clean-local, then run until "Join this workspace using URL:".
  warm-run     Run without clearing local runtime state first.

key env:
  IMAGE=$IMAGE
  NORMAL_REF=$NORMAL_REF
  OBD_REF=$OBD_REF
  CLEAR_OVERLAYBD_CACHES=$CLEAR_OVERLAYBD_CACHES
  DROP_HOST_CACHES=$DROP_HOST_CACHES
EOF
}

require_prereqs() {
  command -v docker >/dev/null || { echo "docker missing" >&2; exit 2; }
  command -v ctr >/dev/null || { echo "ctr missing" >&2; exit 2; }
  [[ -x "$CTR" ]] || { echo "$CTR missing" >&2; exit 2; }
  [[ -x "$SCRIPT_PATH" ]] || { echo "$SCRIPT_PATH missing" >&2; exit 2; }
  curl -fsS "http://${REGISTRY_ADDR}/v2/" >/dev/null || { echo "registry ${REGISTRY_ADDR} not reachable" >&2; exit 2; }
  ctr plugins ls | grep -F 'io.containerd.snapshotter.v1' | grep -F overlaybd >/dev/null || { echo "overlaybd snapshotter missing" >&2; exit 2; }
  if [[ -n "$CTR_RUN_RUNC_BINARY" ]]; then
    [[ -x "$CTR_RUN_RUNC_BINARY" ]] || { echo "$CTR_RUN_RUNC_BINARY missing" >&2; exit 2; }
  fi
  if [[ "$CTR_NET_MODE" == "cni" ]]; then
    [[ -x /opt/cni/bin/bridge ]] || { echo "CNI bridge plugin missing at /opt/cni/bin/bridge" >&2; exit 2; }
    [[ -f /etc/cni/net.d/10-orca-bridge.conf ]] || { echo "CNI config missing" >&2; exit 2; }
  fi
}

active_mounts() {
  findmnt -rn -o TARGET,SOURCE,FSTYPE | grep overlaybd || true
}

require_no_active_runtime() {
  local mounts tasks containers
  mounts="$(active_mounts)"
  tasks="$(ctr -n "$NAMESPACE" tasks ls -q 2>/dev/null || true)"
  containers="$(ctr -n "$NAMESPACE" containers ls -q 2>/dev/null || true)"
  if [[ -n "$mounts" || -n "$tasks" || -n "$containers" ]]; then
    echo "active runtime state exists; not cleaning" >&2
    [[ -n "$mounts" ]] && printf 'mounts:\n%s\n' "$mounts" >&2
    [[ -n "$tasks" ]] && printf 'tasks:\n%s\n' "$tasks" >&2
    [[ -n "$containers" ]] && printf 'containers:\n%s\n' "$containers" >&2
    exit 1
  fi
}

status() {
  echo "IMAGE=${IMAGE}"
  echo "NORMAL_REF=${NORMAL_REF}"
  echo "OBD_REF=${OBD_REF}"
  echo "docker_images=$(docker images -q | wc -l)"
  echo "target_docker_images=$(docker images --format '{{.Repository}}:{{.Tag}}' | grep -E "${REPO}|air-workspace|overlaybd-sql|alpine|hello-world" | wc -l)"
  echo "ctr_images=$("$CTR" -n "$NAMESPACE" images ls -q 2>/dev/null | wc -l)"
  echo "snapshots=$("$CTR" -n "$NAMESPACE" snapshots --snapshotter overlaybd ls | awk 'NR > 1' | wc -l)"
  echo "content=$("$CTR" -n "$NAMESPACE" content ls -q 2>/dev/null | wc -l)"
  echo "mounts=$(active_mounts | wc -l)"
  echo "tasks=$(ctr -n "$NAMESPACE" tasks ls -q 2>/dev/null | wc -l)"
  echo "containers=$(ctr -n "$NAMESPACE" containers ls -q 2>/dev/null | wc -l)"
  for p in /opt/overlaybd/registry_cache /opt/overlaybd/gzip_cache /var/lib/containerd/io.containerd.snapshotter.v1.overlaybd /var/lib/docker-registry; do
    if [[ -e "$p" ]]; then
      du -sh "$p"
    else
      echo "$p missing"
    fi
  done
}

remove_image_refs() {
  docker image rm -f "$IMAGE" "$NORMAL_REF" "$OBD_REF" >/dev/null 2>&1 || true
  if [[ "$CLEAR_DOCKER_IMAGES" == "1" ]]; then
    docker images --format '{{.Repository}}:{{.Tag}}' |
      grep -E "${REPO}|air-workspace|overlaybd-sql|alpine|hello-world" |
      xargs -r docker image rm -f >/dev/null 2>&1 || true
  fi
  "$CTR" -n "$NAMESPACE" images ls -q 2>/dev/null |
    grep -E "${REPO}|air-workspace|overlaybd-sql|alpine|hello-world" |
    xargs -r "$CTR" -n "$NAMESPACE" images rm >/dev/null 2>&1 || true
  ctr -n "$NAMESPACE" images ls -q 2>/dev/null |
    grep -E "${REPO}|air-workspace|overlaybd-sql|alpine|hello-world" |
    xargs -r ctr -n "$NAMESPACE" images rm >/dev/null 2>&1 || true
}

clean_local() {
  require_no_active_runtime
  log "removing local image refs"
  remove_image_refs

  log "removing OverlayBD snapshots"
  for _ in 1 2 3; do
    "$CTR" -n "$NAMESPACE" snapshots --snapshotter overlaybd ls |
      awk 'NR > 1 {print $1}' |
      tac |
      xargs -r -n1 "$CTR" -n "$NAMESPACE" snapshots --snapshotter overlaybd rm >/dev/null 2>&1 || true
  done

  log "removing containerd content"
  "$CTR" -n "$NAMESPACE" content ls -q 2>/dev/null |
    xargs -r "$CTR" -n "$NAMESPACE" content rm >/dev/null 2>&1 || true
  ctr -n "$NAMESPACE" content ls -q 2>/dev/null |
    xargs -r ctr -n "$NAMESPACE" content rm >/dev/null 2>&1 || true

  if [[ "$CLEAR_OVERLAYBD_CACHES" == "1" ]]; then
    log "clearing OverlayBD cache dirs"
    rm -rf /opt/overlaybd/registry_cache /opt/overlaybd/gzip_cache
    mkdir -p /opt/overlaybd/registry_cache /opt/overlaybd/gzip_cache
  fi

  if [[ "$DROP_HOST_CACHES" == "1" ]]; then
    log "dropping host page cache"
    sync
    echo 3 >/proc/sys/vm/drop_caches
  fi

  status
}

run_measurement() {
  local suffix="$1"
  local skip_convert="$2"
  local skip_run="$3"
  local results_file="${RESULTS_DIR}/overlaybd-jb-real-${RUNTIME_LABEL}-${suffix}.md"
  IMAGE="$IMAGE" \
  REGISTRY="$REGISTRY_ADDR" \
  REPO="$REPO" \
  NORMAL_REF="$NORMAL_REF" \
  OBD_REF="$OBD_REF" \
  TAG_SUFFIX="$suffix" \
  SKIP_CONVERT="$skip_convert" \
  SKIP_RUN="$skip_run" \
  RUNTIME_LABEL="$RUNTIME_LABEL" \
  CTR_RUN_RUNTIME="$CTR_RUN_RUNTIME" \
  CTR_RUN_RUNC_BINARY="$CTR_RUN_RUNC_BINARY" \
  CTR_NET_MODE="$CTR_NET_MODE" \
  ALLOW_NEW_PRIVS="$ALLOW_NEW_PRIVS" \
  DB_STR="$DB_STR" \
  REWRITE_REPO_BLOB_URL_SCHEME="${REWRITE_REPO_BLOB_URL_SCHEME:-}" \
  TIMEOUT_SECONDS="$TIMEOUT_SECONDS" \
  RESULTS_FILE="$results_file" \
  python3 "$SCRIPT_PATH"
}

prepare() {
  require_no_active_runtime
  local suffix="prepare-${IMAGE_KEY}-$(date -u +%Y%m%dT%H%M%SZ)"
  run_measurement "$suffix" 0 1
}

cold_run() {
  clean_local
  local suffix="cold-${IMAGE_KEY}-$(date -u +%Y%m%dT%H%M%SZ)"
  run_measurement "$suffix" 1 0
}

warm_run() {
  require_no_active_runtime
  local suffix="warm-${IMAGE_KEY}-$(date -u +%Y%m%dT%H%M%SZ)"
  run_measurement "$suffix" 1 0
}

main() {
  case "$PHASE" in
    -h|--help|help) usage ;;
    status) require_prereqs; status ;;
    prepare) require_prereqs; prepare ;;
    clean-local) require_prereqs; clean_local ;;
    cold-run) require_prereqs; cold_run ;;
    warm-run) require_prereqs; warm_run ;;
    *) usage; exit 2 ;;
  esac
}

main "$@"
