#!/usr/bin/env bash
set -euo pipefail

# Measure whether a Docker-built image derived from an OverlayBD base can be
# handed off through a registry as a cheap lazy/diff image.
#
# Intended environment:
#   - Docker configured with containerd image store and storage-driver=overlaybd
#   - overlaybd-snapshotter configured with runtimeType=docker
#   - local registry listening on 127.0.0.1:5000
#
# This script intentionally stays at the Docker/ctr image API level. It does not
# remove low-level containerd content, stop OverlayBD services, or mutate mounted
# OverlayBD state.

BASE_IMAGE="${BASE_IMAGE:-registry.hub.docker.com/overlaybd/redis:7.2.3_obd}"
REGISTRY="${REGISTRY:-127.0.0.1:5000}"
REPO="${REPO:-orca/overlaybd-user}"
NORMAL_TAG="${NORMAL_TAG:-test}"
CONVERTED_TAG="${CONVERTED_TAG:-obd-$(date -u +%Y%m%dT%H%M%SZ)}"
WORK_DIR="${WORK_DIR:-/root/overlaybd-derived-diff-test}"
REGISTRY_DIR="${REGISTRY_DIR:-/var/lib/docker-registry}"
CTR="${CTR:-/opt/overlaybd/snapshotter/ctr}"
DB_STR="${DB_STR:-}"
SETUP_MYSQL="${SETUP_MYSQL:-0}"
MYSQL_DATABASE="${MYSQL_DATABASE:-overlaybd}"
MYSQL_USER="${MYSQL_USER:-overlaybd}"
MYSQL_PASSWORD="${MYSQL_PASSWORD:-overlaybd}"

NORMAL_IMAGE="${REGISTRY}/${REPO}:${NORMAL_TAG}"
CONVERTED_IMAGE="${REGISTRY}/${REPO}:${CONVERTED_TAG}"
LOCAL_IMAGE="${LOCAL_IMAGE:-local/overlaybd-user:test}"

now_ms() {
  date +%s%3N
}

log() {
  printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"
}

fail() {
  echo "error: $*" >&2
  exit 1
}

safe_preflight() {
  log "checking for existing containers/tasks/mounts"
  docker ps -a
  ctr -n moby tasks ls || true
  if findmnt -rn -o TARGET,SOURCE,FSTYPE | grep -E 'overlaybd|/dev/sd'; then
    fail "OverlayBD/SCSI mounts are active; stop containers before running"
  fi
}

require_runtime() {
  command -v docker >/dev/null || fail "docker is not installed"
  command -v ctr >/dev/null || fail "ctr is not installed"
  [[ -x "$CTR" ]] || fail "$CTR is missing"
  docker info --format 'docker_driver={{.Driver}} containerd_store={{.DriverStatus}}'
  curl -fsS "http://${REGISTRY}/v2/" >/dev/null || fail "registry ${REGISTRY} is not reachable"
}

registry_size() {
  du -sb "$REGISTRY_DIR" | cut -f1
}

registry_blob_count() {
  find "$REGISTRY_DIR/docker/registry/v2/blobs" -type f | wc -l
}

ensure_mysql_schema() {
  if [[ "$SETUP_MYSQL" != "1" ]]; then
    return
  fi

  if ! command -v mysql >/dev/null; then
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y mysql-server
  fi

  systemctl enable --now mysql
  mysql <<SQL
CREATE DATABASE IF NOT EXISTS \`${MYSQL_DATABASE}\`;
CREATE USER IF NOT EXISTS '${MYSQL_USER}'@'127.0.0.1' IDENTIFIED BY '${MYSQL_PASSWORD}';
CREATE USER IF NOT EXISTS '${MYSQL_USER}'@'localhost' IDENTIFIED BY '${MYSQL_PASSWORD}';
GRANT ALL PRIVILEGES ON \`${MYSQL_DATABASE}\`.* TO '${MYSQL_USER}'@'127.0.0.1';
GRANT ALL PRIVILEGES ON \`${MYSQL_DATABASE}\`.* TO '${MYSQL_USER}'@'localhost';
USE \`${MYSQL_DATABASE}\`;
CREATE TABLE IF NOT EXISTS overlaybd_layers (
  host VARCHAR(255) NOT NULL,
  repo VARCHAR(255) NOT NULL,
  chain_id VARCHAR(255) NOT NULL,
  data_digest VARCHAR(255) NOT NULL,
  data_size BIGINT NOT NULL,
  PRIMARY KEY (host, repo, chain_id),
  KEY index_registry_chainId (host, chain_id) USING BTREE
) DEFAULT CHARSET=utf8;
CREATE TABLE IF NOT EXISTS overlaybd_manifests (
  host VARCHAR(255) NOT NULL,
  repo VARCHAR(255) NOT NULL,
  src_digest VARCHAR(255) NOT NULL,
  out_digest VARCHAR(255) NOT NULL,
  data_size BIGINT NOT NULL,
  mediatype VARCHAR(255) NOT NULL,
  PRIMARY KEY (host, repo, src_digest, mediatype),
  KEY index_registry_src_digest (host, src_digest, mediatype) USING BTREE
) DEFAULT CHARSET=utf8;
SQL

  if [[ -z "$DB_STR" ]]; then
    DB_STR="${MYSQL_USER}:${MYSQL_PASSWORD}@tcp(127.0.0.1:3306)/${MYSQL_DATABASE}"
  fi
}

timed_run() {
  local label="$1"
  local image="$2"
  local start end rc output
  start=$(now_ms)
  set +e
  output=$(docker run --rm --entrypoint sh "$image" -lc 'echo FIRST_OUTPUT; test -f /user-layer.txt && cat /user-layer.txt' 2>&1)
  rc=$?
  set -e
  end=$(now_ms)

  echo "[$label]"
  echo "rc=$rc"
  echo "duration_ms=$((end - start))"
  printf '%s\n' "$output" | sed 's/^/output: /'
}

build_local_derived_image() {
  mkdir -p "$WORK_DIR"
  cat >"$WORK_DIR/Dockerfile" <<EOF
FROM ${BASE_IMAGE}
RUN echo user-layer > /user-layer.txt
ENTRYPOINT ["sh", "-lc"]
CMD ["echo USER_FIRST_OUTPUT"]
EOF
  docker build -t "$LOCAL_IMAGE" "$WORK_DIR"
}

main() {
  safe_preflight
  require_runtime
  ensure_mysql_schema

  log "public OverlayBD image smoke check"
  docker run --rm --entrypoint sh "$BASE_IMAGE" -lc 'echo PUBLIC_OK'
  safe_preflight

  log "building local derived image"
  build_local_derived_image
  timed_run "local_derived_image" "$LOCAL_IMAGE"
  safe_preflight

  log "pushing normal Docker-derived image to ${NORMAL_IMAGE}"
  docker tag "$LOCAL_IMAGE" "$NORMAL_IMAGE"
  docker push "$NORMAL_IMAGE"

  log "running normal registry image after removing local ref"
  docker image rm "$NORMAL_IMAGE" >/dev/null 2>&1 || true
  timed_run "normal_registry_image" "$NORMAL_IMAGE" || true
  safe_preflight

  log "pulling normal registry image into containerd local content store"
  "$CTR" -n moby images pull --local --plain-http "$NORMAL_IMAGE"

  before_bytes=$(registry_size)
  before_blobs=$(registry_blob_count)
  log "converting ${NORMAL_IMAGE} to ${CONVERTED_IMAGE}"
  if [[ -n "$DB_STR" ]]; then
    "$CTR" -n moby obdconv --plain-http --fstype ext4 --dbstr "$DB_STR" "$NORMAL_IMAGE" "$CONVERTED_IMAGE"
  else
    "$CTR" -n moby obdconv --plain-http --fstype ext4 "$NORMAL_IMAGE" "$CONVERTED_IMAGE"
  fi
  "$CTR" -n moby images push --local --plain-http "$CONVERTED_IMAGE"
  after_bytes=$(registry_size)
  after_blobs=$(registry_blob_count)

  echo "[conversion_registry_delta]"
  echo "before_bytes=$before_bytes"
  echo "after_bytes=$after_bytes"
  echo "delta_bytes=$((after_bytes - before_bytes))"
  echo "before_blobs=$before_blobs"
  echo "after_blobs=$after_blobs"
  echo "delta_blobs=$((after_blobs - before_blobs))"

  log "running converted registry image after removing local ref"
  docker image rm "$CONVERTED_IMAGE" >/dev/null 2>&1 || true
  timed_run "converted_registry_image" "$CONVERTED_IMAGE" || true
  safe_preflight
}

main "$@"
