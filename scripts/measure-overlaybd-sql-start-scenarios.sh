#!/usr/bin/env bash
set -euo pipefail

# Measure the product-style OverlayBD SQL flow after conversion works:
#
#   publish time:
#     - Docker builds/pushes normal base + derived images.
#     - obdconv converts both with a shared SQL DB.
#
#   start time:
#     - no local derived image: rpull derived, then run to FIRST_OUTPUT.
#     - base local: pre-rpull base, rpull derived, then run to FIRST_OUTPUT.
#     - derived local: pre-rpull derived, measure no-op rpull, then run.
#
# Docker remains on its normal storage driver. OverlayBD is used through
# containerd/ctr only.

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "run as root" >&2
  exit 1
fi

REGISTRY="${REGISTRY:-127.0.0.1:5000}"
REPO="${REPO:-orca/overlaybd-sql-start}"
TAG_SUFFIX="${TAG_SUFFIX:-$(date -u +%Y%m%dT%H%M%SZ)}"
WORK_DIR="${WORK_DIR:-/root/overlaybd-sql-start-scenarios}"
RESULTS_FILE="${RESULTS_FILE:-/root/overlaybd-sql-start-scenarios-${TAG_SUFFIX}.md}"
REGISTRY_DIR="${REGISTRY_DIR:-/var/lib/docker-registry}"
CTR="${CTR:-/opt/overlaybd/snapshotter/ctr}"
BASE_IMAGE="${BASE_IMAGE:-alpine:3.22}"
NAMESPACE="${NAMESPACE:-moby}"
SKIP_PUBLISH="${SKIP_PUBLISH:-0}"
CTR_RUN_RUNTIME="${CTR_RUN_RUNTIME:-io.containerd.runc.v2}"
CTR_RUN_RUNC_BINARY="${CTR_RUN_RUNC_BINARY:-}"
RUNTIME_LABEL="${RUNTIME_LABEL:-runc}"

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

time_command() {
  local label="$1"
  shift
  local start end rc output
  start=$(now_ms)
  set +e
  output=$("$@" 2>&1)
  rc=$?
  set -e
  end=$(now_ms)
  printf -v "${label}_ms" '%s' "$((end - start))"
  printf -v "${label}_rc" '%s' "$rc"
  printf -v "${label}_output" '%s' "$output"
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
USER root
RUN echo base-layer > /base-layer.txt
CMD ["sh", "-lc", "echo BASE_FIRST_OUTPUT && cat /base-layer.txt"]
EOF

  cat >"$WORK_DIR/derived/Dockerfile" <<EOF
FROM ${BASE_NORMAL}
USER root
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
  local start end before_bytes before_blobs after_bytes after_blobs
  before_bytes=$(registry_size)
  before_blobs=$(registry_blob_count)
  start=$(now_ms)
  "$CTR" -n "$NAMESPACE" images pull --local --plain-http "$src"
  "$CTR" -n "$NAMESPACE" obdconv --plain-http --fstype ext4 --dbstr "$DB_STR" "$src" "$dst"
  "$CTR" -n "$NAMESPACE" images push --local --plain-http "$dst"
  end=$(now_ms)
  after_bytes=$(registry_size)
  after_blobs=$(registry_blob_count)
  printf -v "${label}_conversion_ms" '%s' "$((end - start))"
  printf -v "${label}_registry_delta_bytes" '%s' "$((after_bytes - before_bytes))"
  printf -v "${label}_registry_delta_blobs" '%s' "$((after_blobs - before_blobs))"
}

remove_image_refs() {
  "$CTR" -n "$NAMESPACE" images rm "$BASE_OBD" >/dev/null 2>&1 || true
  "$CTR" -n "$NAMESPACE" images rm "$DERIVED_OBD" >/dev/null 2>&1 || true
}

rpull_image() {
  "$CTR" -n "$NAMESPACE" rpull --plain-http "$1"
}

run_image() {
  local name="$1"
  local run_args=(
    -n "$NAMESPACE" run
    --snapshotter overlaybd
    --runtime "$CTR_RUN_RUNTIME"
  )
  if [[ -n "$CTR_RUN_RUNC_BINARY" ]]; then
    run_args+=(--runc-binary "$CTR_RUN_RUNC_BINARY")
  fi
  run_args+=(--rm "$DERIVED_OBD" "$name")
  "$CTR" "${run_args[@]}" \
    sh -lc 'echo FIRST_OUTPUT && cat /base-layer.txt && cat /user-layer.txt'
}

measure_scenario() {
  local scenario="$1"
  local prep_kind="$2"
  local prep_ms=0
  local total_ms

  require_clean_overlaybd_mounts
  remove_image_refs

  case "$prep_kind" in
    none)
      prep_ms=0
      ;;
    base)
      time_command prep rpull_image "$BASE_OBD"
      prep_ms="$prep_ms"
      ;;
    derived)
      time_command prep rpull_image "$DERIVED_OBD"
      prep_ms="$prep_ms"
      ;;
    *)
      fail "unknown prep kind: ${prep_kind}"
      ;;
  esac

  time_command rpull rpull_image "$DERIVED_OBD"
  if [[ "$rpull_rc" != "0" ]]; then
    total_ms=$((prep_ms + rpull_ms))
    rows+=("| ${scenario} | ${prep_kind} | ${prep_ms} | ${rpull_ms} | failed | ${total_ms} | rpull failed |")
    logs+=("## ${scenario} rpull failure"$'\n\n```text\n'"${rpull_output}"$'\n```')
    return
  fi

  time_command first_output run_image "overlaybd-start-${scenario}-${TAG_SUFFIX}"
  wait_for_overlaybd_mount_cleanup
  total_ms=$((prep_ms + rpull_ms + first_output_ms))

  if [[ "$first_output_rc" == "0" ]] && grep -q 'FIRST_OUTPUT' <<<"$first_output_output"; then
    rows+=("| ${scenario} | ${prep_kind} | ${prep_ms} | ${rpull_ms} | ${first_output_ms} | ${total_ms} | ok |")
  else
    rows+=("| ${scenario} | ${prep_kind} | ${prep_ms} | ${rpull_ms} | ${first_output_ms} | ${total_ms} | run failed |")
  fi
  logs+=("## ${scenario} output"$'\n\n```text\n'"${first_output_output}"$'\n```')
}

write_results() {
  {
    echo "# OverlayBD SQL Start Scenarios"
    echo
    echo "Generated: \`$(date -u +%FT%TZ)\`"
    echo
    echo "Docker stays on normal storage; OverlayBD is used through \`ctr --snapshotter overlaybd\`."
    echo
    echo "| Runtime | Value |"
    echo "| --- | --- |"
    echo "| Runtime label | \`${RUNTIME_LABEL}\` |"
    echo "| containerd runtime | \`${CTR_RUN_RUNTIME}\` |"
    echo "| runc binary | \`${CTR_RUN_RUNC_BINARY:-default}\` |"
    echo "| skip publish | \`${SKIP_PUBLISH}\` |"
    echo
    echo "| Image | Ref |"
    echo "| --- | --- |"
    echo "| Base normal | \`${BASE_NORMAL}\` |"
    echo "| Derived normal | \`${DERIVED_NORMAL}\` |"
    echo "| Base OverlayBD | \`${BASE_OBD}\` |"
    echo "| Derived OverlayBD | \`${DERIVED_OBD}\` |"
    echo
    echo "## Publish-Time Conversion"
    echo
    echo "| Step | Time | Registry delta | Blob delta |"
    echo "| --- | ---: | ---: | ---: |"
    echo "| Base conversion | ${base_conversion_ms:-skipped} ms | ${base_registry_delta_bytes:-skipped} bytes | ${base_registry_delta_blobs:-skipped} |"
    echo "| Derived conversion | ${derived_conversion_ms:-skipped} ms | ${derived_registry_delta_bytes:-skipped} bytes | ${derived_registry_delta_blobs:-skipped} |"
    echo
    echo "## Start-Time Scenarios"
    echo
    echo "| Scenario | Preloaded local image | Prep rpull | Measured rpull | First output | Total | Result |"
    echo "| --- | --- | ---: | ---: | ---: | ---: | --- |"
    printf '%s\n' "${rows[@]}"
    echo
    echo "Notes:"
    echo
    echo "- \`Prep rpull\` is setup for the scenario and is not user-visible start time."
    echo "- \`Measured rpull\` is the lazy image pull/fetch step for the target derived image."
    echo "- \`First output\` starts after measured rpull and ends when the container prints \`FIRST_OUTPUT\`."
    echo "- This benchmark does not delete low-level containerd content blobs; it removes image refs and relies on fresh tags."
    echo
    printf '%s\n\n' "${logs[@]}"
  } >"$RESULTS_FILE"
}

main() {
  rows=()
  logs=()

  verify_prereqs
  ensure_mysql_schema
  if [[ "$SKIP_PUBLISH" != "1" ]]; then
    build_and_push_images
    convert_image "$BASE_NORMAL" "$BASE_OBD" "base"
    convert_image "$DERIVED_NORMAL" "$DERIVED_OBD" "derived"
  fi

  measure_scenario "no_local_image" "none"
  measure_scenario "base_local" "base"
  measure_scenario "derived_local" "derived"

  write_results
  cat "$RESULTS_FILE"
}

main "$@"
