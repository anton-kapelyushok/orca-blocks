#!/usr/bin/env bash
set -euo pipefail

# Measures the documented Docker runtime path for an existing OverlayBD image.
# This intentionally uses Docker's overlaybd storage driver instead of
# ctr --snapshotter=overlaybd.

IMAGE="${IMAGE:-registry.hub.docker.com/overlaybd/redis:7.2.3_obd}"
RESULTS_FILE="${RESULTS_FILE:-docs/benchmarks/overlaybd-docker-runtime-results.md}"
WORK_DIR="${WORK_DIR:-.tmp/overlaybd-docker-runtime}"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-12}"

mkdir -p "$WORK_DIR" "$(dirname "$RESULTS_FILE")"

now_ms() {
  date +%s%3N
}

log() {
  printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"
}

require_clean_mounts() {
  if findmnt -rn -o TARGET,SOURCE,FSTYPE | grep -E 'overlaybd|/dev/sd' >/tmp/overlaybd-mounts.txt; then
    echo "unexpected existing OverlayBD mounts:" >&2
    cat /tmp/overlaybd-mounts.txt >&2
    exit 2
  fi
}

measure_actual_present() {
  docker pull "$IMAGE" >/dev/null
  local start end
  start=$(now_ms)
  docker run --rm --entrypoint sh "$IMAGE" -lc 'echo FIRST_OUTPUT'
  end=$(now_ms)
  echo "$((end - start))"
}

measure_image_ref_absent() {
  docker image rm "$IMAGE" >/dev/null 2>&1 || true
  local start end
  start=$(now_ms)
  docker run --rm --entrypoint sh "$IMAGE" -lc 'echo FIRST_OUTPUT'
  end=$(now_ms)
  echo "$((end - start))"
}

measure_long_running_redis() {
  docker image rm "$IMAGE" >/dev/null 2>&1 || true
  local start end output rc
  start=$(now_ms)
  set +e
  output=$(timeout "${TIMEOUT_SECONDS}s" docker run --rm "$IMAGE" 2>&1)
  rc=$?
  set -e
  end=$(now_ms)
  printf '%s\n' "$output" >"$WORK_DIR/redis-timeout.log"
  echo "$rc $((end - start))"
}

main() {
  require_clean_mounts
  log "measuring actual-present short command"
  actual_ms=$(measure_actual_present | tail -n1)
  require_clean_mounts

  log "measuring image-ref-absent short command"
  absent_ms=$(measure_image_ref_absent | tail -n1)
  require_clean_mounts

  log "measuring documented long-running Redis command"
  read -r redis_rc redis_wall_ms < <(measure_long_running_redis)
  require_clean_mounts

  cat >"$RESULTS_FILE" <<EOF
# OverlayBD Docker Runtime Results

Generated: \`$(date -u +%FT%TZ)\`

Image: \`$IMAGE\`

This follows \`containerd/accelerated-container-image/docs/DOCKER.md\`: OverlayBD is configured with \`runtimeType=docker\`, Docker has \`containerd-snapshotter\` enabled, and Docker reports \`storage-driver=overlaybd\`.

| Scenario | First output / wall time |
| --- | ---: |
| Actual image present, short command | ${actual_ms} ms |
| Docker image ref absent, short command | ${absent_ms} ms |
| Documented Redis command, timeout wrapper | ${redis_wall_ms} ms |

Notes:

- The Redis command is long-running by design, so the timeout wrapper exits with \`${redis_rc}\`; logs are stored in \`$WORK_DIR/redis-timeout.log\`.
- Docker mode auto-removed the OverlayBD mount after \`docker run --rm\`.
- This script does not clear low-level OverlayBD/containerd content or stop OverlayBD services.
EOF

  cat "$RESULTS_FILE"
}

main "$@"
