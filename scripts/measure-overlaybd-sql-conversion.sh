#!/usr/bin/env bash
set -euo pipefail

# Smoke-test OverlayBD SQL-backed conversion:
#   1. build/push a normal base image with Docker
#   2. build/push a normal derived image with Docker
#   3. convert base with --dbstr
#   4. convert derived with the same --dbstr
#   5. run the converted derived image with ctr --snapshotter overlaybd
#
# Docker remains on its normal storage driver. OverlayBD is used through
# containerd/ctr only.

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "run as root" >&2
  exit 1
fi

REGISTRY="${REGISTRY:-127.0.0.1:5000}"
REPO="${REPO:-orca/overlaybd-sql-smoke}"
TAG_SUFFIX="${TAG_SUFFIX:-$(date -u +%Y%m%dT%H%M%SZ)}"
WORK_DIR="${WORK_DIR:-/root/overlaybd-sql-conversion-test}"
REGISTRY_DIR="${REGISTRY_DIR:-/var/lib/docker-registry}"
CTR="${CTR:-/opt/overlaybd/snapshotter/ctr}"
BASE_IMAGE="${BASE_IMAGE:-alpine:3.22}"

SETUP_MYSQL="${SETUP_MYSQL:-1}"
MYSQL_DATABASE="${MYSQL_DATABASE:-overlaybd}"
MYSQL_USER="${MYSQL_USER:-overlaybd}"
MYSQL_PASSWORD="${MYSQL_PASSWORD:-overlaybd}"
DB_STR="${DB_STR:-${MYSQL_USER}:${MYSQL_PASSWORD}@tcp(127.0.0.1:3306)/${MYSQL_DATABASE}}"

BASE_NORMAL="${REGISTRY}/${REPO}:base-normal-${TAG_SUFFIX}"
DERIVED_NORMAL="${REGISTRY}/${REPO}:derived-normal-${TAG_SUFFIX}"
BASE_OBD="${REGISTRY}/${REPO}:base-obd-${TAG_SUFFIX}"
DERIVED_OBD="${REGISTRY}/${REPO}:derived-obd-${TAG_SUFFIX}"

log() {
  printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"
}

now_ms() {
  date +%s%3N
}

fail() {
  echo "error: $*" >&2
  exit 1
}

registry_size() {
  du -sb "$REGISTRY_DIR" | cut -f1
}

registry_blob_count() {
  find "$REGISTRY_DIR/docker/registry/v2/blobs" -type f | wc -l
}

require_clean_overlaybd_mounts() {
  if findmnt -rn -o TARGET,SOURCE,FSTYPE | grep -E 'overlaybd|/dev/sd'; then
    fail "OverlayBD/SCSI mounts are active; stop containers before running"
  fi
}

wait_for_overlaybd_mount_cleanup() {
  local i
  for i in $(seq 1 20); do
    if ! findmnt -rn -o TARGET,SOURCE,FSTYPE | grep -E 'overlaybd|/dev/sd' >/dev/null; then
      return 0
    fi
    sleep 1
  done
  findmnt -rn -o TARGET,SOURCE,FSTYPE | grep -E 'overlaybd|/dev/sd' || true
  fail "OverlayBD/SCSI mounts did not clean up after run"
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
}

verify_prereqs() {
  command -v docker >/dev/null || fail "docker missing"
  command -v ctr >/dev/null || fail "ctr missing"
  [[ -x "$CTR" ]] || fail "$CTR missing"
  curl -fsS "http://${REGISTRY}/v2/" >/dev/null || fail "registry ${REGISTRY} is not reachable"
  ctr plugins ls | grep -F 'io.containerd.snapshotter.v1' | grep -F overlaybd >/dev/null || fail "overlaybd snapshotter missing"
  require_clean_overlaybd_mounts
}

build_and_push_images() {
  rm -rf "$WORK_DIR"
  mkdir -p "$WORK_DIR/base" "$WORK_DIR/derived"

  cat >"$WORK_DIR/base/Dockerfile" <<EOF
FROM ${BASE_IMAGE}
RUN echo base-layer > /base-layer.txt
CMD ["sh", "-lc", "echo BASE_FIRST_OUTPUT && cat /base-layer.txt"]
EOF

  cat >"$WORK_DIR/derived/Dockerfile" <<EOF
FROM ${BASE_NORMAL}
RUN echo user-layer > /user-layer.txt
CMD ["sh", "-lc", "echo FIRST_OUTPUT && cat /base-layer.txt && cat /user-layer.txt"]
EOF

  log "building base normal image ${BASE_NORMAL}"
  docker build -t "$BASE_NORMAL" "$WORK_DIR/base"
  docker push "$BASE_NORMAL"

  log "building derived normal image ${DERIVED_NORMAL}"
  docker build -t "$DERIVED_NORMAL" "$WORK_DIR/derived"
  docker push "$DERIVED_NORMAL"
}

convert_image() {
  local src="$1"
  local dst="$2"
  local label="$3"
  local start end
  start=$(now_ms)
  "$CTR" -n moby images pull --local --plain-http "$src"
  "$CTR" -n moby obdconv --plain-http --fstype ext4 --dbstr "$DB_STR" "$src" "$dst"
  "$CTR" -n moby images push --local --plain-http "$dst"
  end=$(now_ms)
  echo "${label}_conversion_ms=$((end - start))"
}

run_converted_derived() {
  local start end out rc
  "$CTR" -n moby images rm "$DERIVED_OBD" >/dev/null 2>&1 || true
  start=$(now_ms)
  set +e
  out=$("$CTR" -n moby rpull --plain-http "$DERIVED_OBD" 2>&1 && "$CTR" -n moby run --snapshotter overlaybd --rm "$DERIVED_OBD" overlaybd-sql-derived-smoke sh -lc 'echo FIRST_OUTPUT && cat /base-layer.txt && cat /user-layer.txt' 2>&1)
  rc=$?
  set -e
  end=$(now_ms)
  echo "run_rc=$rc"
  echo "run_ms=$((end - start))"
  printf '%s\n' "$out" | sed 's/^/run_output: /'
  return "$rc"
}

db_counts() {
  mysql -N -B "$MYSQL_DATABASE" <<'SQL'
SELECT CONCAT('db_overlaybd_layers=', COUNT(*)) FROM overlaybd_layers;
SELECT CONCAT('db_overlaybd_manifests=', COUNT(*)) FROM overlaybd_manifests;
SQL
}

main() {
  verify_prereqs
  ensure_mysql_schema

  echo "base_normal=${BASE_NORMAL}"
  echo "derived_normal=${DERIVED_NORMAL}"
  echo "base_obd=${BASE_OBD}"
  echo "derived_obd=${DERIVED_OBD}"

  build_and_push_images

  before_base_bytes=$(registry_size)
  before_base_blobs=$(registry_blob_count)
  convert_image "$BASE_NORMAL" "$BASE_OBD" "base"
  after_base_bytes=$(registry_size)
  after_base_blobs=$(registry_blob_count)

  before_derived_bytes=$(registry_size)
  before_derived_blobs=$(registry_blob_count)
  convert_image "$DERIVED_NORMAL" "$DERIVED_OBD" "derived"
  after_derived_bytes=$(registry_size)
  after_derived_blobs=$(registry_blob_count)

  echo "base_registry_delta_bytes=$((after_base_bytes - before_base_bytes))"
  echo "base_registry_delta_blobs=$((after_base_blobs - before_base_blobs))"
  echo "derived_registry_delta_bytes=$((after_derived_bytes - before_derived_bytes))"
  echo "derived_registry_delta_blobs=$((after_derived_blobs - before_derived_blobs))"
  db_counts

  run_converted_derived
  wait_for_overlaybd_mount_cleanup
}

main "$@"
