set -euo pipefail
CTR=/opt/overlaybd/snapshotter/ctr
NS=moby
SNAPSHOT_ROOT=/var/lib/containerd/io.containerd.snapshotter.v1.overlaybd
CONFIG=/etc/overlaybd-snapshotter/config.json
IMAGE_REF="${IMAGE_REF:-${REGISTRY_HOST}/${REPO}:${BASE_TAG}}"
NAME="${CONTAINER_NAME:-demo-jb-mutable-${RUN_ID}}"
WORK="/root/orca-overlaybd-demo-${RUN_ID}"
LOG="$WORK/$NAME.log"
ENV_FILE="$WORK/mutable.env"
mkdir -p "$WORK"

echo "image=$IMAGE_REF"
echo "container=$NAME"
echo "touch_path=$TOUCH_PATH"

echo "Executing local: assert OverlayBD rwMode=overlayfs"
if [[ "$(jq -r .rwMode "$CONFIG")" != "overlayfs" ]]; then
  echo "Expected rwMode=overlayfs. Run demo cleanup first."
  cat "$CONFIG"
  exit 1
fi
echo "rwMode now: $(jq -r .rwMode "$CONFIG")"

echo "Executing local: $CTR -n $NS rpull --plain-http $IMAGE_REF"
# DEMO-CMD: $CTR -n "$NS" rpull --plain-http "$IMAGE_REF"
start_ms=$(date +%s%3N)
$CTR -n "$NS" rpull --plain-http "$IMAGE_REF"
after_rpull_ms=$(date +%s%3N)
rpull_ms=$((after_rpull_ms - start_ms))

echo "Executing local: rewrite OverlayBD repoBlobUrl https registry URLs to http"
# DEMO-CMD: find /var/lib/containerd/io.containerd.snapshotter.v1.overlaybd/snapshots -path '*/block/config.v1.json' -type f -print0 | xargs -0 -r sed -i -e "s#https://${REGISTRY_HOST}#http://${REGISTRY_HOST}#g" -e "s#https://${NODE2_REGISTRY_HOST}#http://${NODE2_REGISTRY_HOST}#g"
find /var/lib/containerd/io.containerd.snapshotter.v1.overlaybd/snapshots \
  -path '*/block/config.v1.json' -type f -print0 |
  xargs -0 -r sed -i \
    -e "s#https://${REGISTRY_HOST}#http://${REGISTRY_HOST}#g" \
    -e "s#https://${NODE2_REGISTRY_HOST}#http://${NODE2_REGISTRY_HOST}#g"

TOUCH_DIR="$(dirname "$TOUCH_PATH")"
MUTATION_CMD="mkdir -p '$TOUCH_DIR'; echo 'demo-$RUN_ID' > '$TOUCH_PATH'; ls -l '$TOUCH_PATH'; cat '$TOUCH_PATH'; echo __ORCA_DEMO_TOUCH_DONE__; while :; do sleep 3600; done"
echo "Executing local: $CTR -n $NS run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc $IMAGE_REF $NAME sh -lc \"$MUTATION_CMD\""
# DEMO-CMD: $CTR -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc "$IMAGE_REF" "$NAME" sh -lc "mkdir -p '$TOUCH_DIR'; echo 'demo-$RUN_ID' > '$TOUCH_PATH'; ls -l '$TOUCH_PATH'; cat '$TOUCH_PATH'; echo __ORCA_DEMO_TOUCH_DONE__; while :; do sleep 3600; done" > >(tee -a "$LOG") 2>&1 &
fifo="/tmp/$NAME.fifo"
rm -f "$fifo" "$LOG"
mkfifo "$fifo"
run_start_ms=$(date +%s%3N)
set +e
$CTR -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc "$IMAGE_REF" "$NAME" sh -lc "$MUTATION_CMD" >"$fifo" 2>&1 &
runner_pid=$!
set -e
exec 3<"$fifo"

first_user_text_ms=""
task_ready_ms=""
touch_ms=""
deadline=$(( $(date +%s) + 30 ))
while true; do
  if IFS= read -r -t 0.1 line <&3; then
    now_ms=$(date +%s%3N)
    printf '%s\n' "$line" | tee -a "$LOG"
    if [[ -z "$first_user_text_ms" ]]; then
      first_user_text_ms=$((now_ms - run_start_ms))
    fi
    if [[ "$line" == *"__ORCA_DEMO_TOUCH_DONE__"* ]]; then
      touch_ms=$((now_ms - run_start_ms))
    fi
  fi
  if $CTR -n "$NS" tasks ls -q 2>/dev/null | grep -Fx "$NAME" >/dev/null; then
    task_ready_ms=$(( $(date +%s%3N) - run_start_ms ))
    break
  fi
  if ! kill -0 "$runner_pid" >/dev/null 2>&1; then
    echo "container process exited before task became ready"
    break
  fi
  if (( $(date +%s) > deadline )); then
    echo "timeout waiting for container task"
    break
  fi
  sleep 0.1
done

touch_deadline=$(( $(date +%s) + 30 ))
while [[ -z "$touch_ms" ]]; do
  if IFS= read -r -t 0.1 line <&3; then
    now_ms=$(date +%s%3N)
    printf '%s\n' "$line" | tee -a "$LOG"
    if [[ -z "$first_user_text_ms" ]]; then
      first_user_text_ms=$((now_ms - run_start_ms))
    fi
    if [[ "$line" == *"__ORCA_DEMO_TOUCH_DONE__"* ]]; then
      touch_ms=$((now_ms - run_start_ms))
    fi
  fi
  if [[ -n "$touch_ms" ]]; then
    break
  fi
  if ! kill -0 "$runner_pid" >/dev/null 2>&1; then
    echo "container process exited before touch marker appeared"
    break
  fi
  if (( $(date +%s) > touch_deadline )); then
    echo "timeout waiting for touch marker"
    break
  fi
  sleep 0.1
done

if [[ -z "$task_ready_ms" || -z "$touch_ms" ]]; then
  $CTR -n "$NS" tasks kill -s SIGKILL "$NAME" || true
  $CTR -n "$NS" tasks rm "$NAME" || true
  $CTR -n "$NS" containers rm "$NAME" || true
  wait "$runner_pid" || true
  exec 3<&-
  rm -f "$fifo"
  exit 1
fi
exec 3<&-

echo "Executing local: find task pid and containerd overlay upperdir"
# DEMO-CMD: $CTR -n "$NS" tasks ls | awk -v name="$NAME" '$1==name {print $2}'; $CTR -n "$NS" snapshots --snapshotter overlaybd mounts /tmp/probe "$NAME"
PID="$($CTR -n "$NS" tasks ls | awk -v name="$NAME" '$1==name {print $2}')"
if [[ -z "$PID" ]]; then
  echo "Could not find task pid for $NAME"
  exit 1
fi
MOUNT_CMD="$($CTR -n "$NS" snapshots --snapshotter overlaybd mounts /tmp/orca-demo-probe "$NAME")"
UPPERDIR="$(printf '%s' "$MOUNT_CMD" | sed -n 's/.*upperdir=\([^,]*\).*/\1/p')"
WORKDIR="$(printf '%s' "$MOUNT_CMD" | sed -n 's/.*workdir=\([^,]*\).*/\1/p')"
LOWERDIR="$(printf '%s' "$MOUNT_CMD" | sed -n 's/.*lowerdir=\([^,]*\).*/\1/p')"
if [[ -z "$UPPERDIR" || ! -d "$UPPERDIR" ]]; then
  echo "Could not resolve overlay upperdir from snapshot mount command:"
  echo "$MOUNT_CMD"
  exit 1
fi
SNAPSHOT_DIR="$(dirname "$UPPERDIR")"
OBD_CONFIG_PATH="$SNAPSHOT_DIR/block/config.v1.json"
if [[ ! -f "$OBD_CONFIG_PATH" ]]; then
  TOP_LOWERDIR="${LOWERDIR%%:*}"
  if [[ "$TOP_LOWERDIR" == */block/mountpoint ]]; then
    OBD_CONFIG_PATH="${TOP_LOWERDIR%/mountpoint}/config.v1.json"
  elif [[ "$TOP_LOWERDIR" == */fs ]]; then
    OBD_CONFIG_PATH="$(dirname "$TOP_LOWERDIR")/block/config.v1.json"
  fi
  if [[ ! -f "$OBD_CONFIG_PATH" ]]; then
    echo "Could not find OverlayBD image config for active snapshot or top lowerdir:"
    echo "active_snapshot_config=$SNAPSHOT_DIR/block/config.v1.json"
    echo "top_lowerdir=$TOP_LOWERDIR"
    echo "lowerdir_config=$OBD_CONFIG_PATH"
    exit 1
  fi
fi
UID_MAP="$(tr '\n' ';' <"/proc/$PID/uid_map")"
GID_MAP="$(tr '\n' ';' <"/proc/$PID/gid_map")"
echo "pid=$PID"
echo "mount_command=$MOUNT_CMD"
echo "upperdir=$UPPERDIR"
echo "workdir=$WORKDIR"
echo "lowerdir=$LOWERDIR"
echo "overlaybd_config=$OBD_CONFIG_PATH"
echo "uid_map=$UID_MAP"
echo "gid_map=$GID_MAP"

echo "Executing local: stop task but keep container/snapshot for upperdir diff export"
# DEMO-CMD: $CTR -n "$NS" tasks kill -s SIGTERM "$NAME"; $CTR -n "$NS" tasks rm "$NAME"
touch_end_ms=$(date +%s%3N)
$CTR -n "$NS" tasks kill -s SIGTERM "$NAME" || true
sleep 2
$CTR -n "$NS" tasks kill -s SIGKILL "$NAME" || true
$CTR -n "$NS" tasks rm "$NAME" || true
wait "$runner_pid" || true
stop_end_ms=$(date +%s%3N)
stop_ms=$((stop_end_ms - touch_end_ms))
rm -f "$fifo"

cat >"$ENV_FILE" <<EOF_INNER
NAME=$NAME
IMAGE_REF=$IMAGE_REF
UPPERDIR=$UPPERDIR
WORKDIR=$WORKDIR
LOWERDIR=$LOWERDIR
OBD_CONFIG_PATH=$OBD_CONFIG_PATH
UID_MAP='$UID_MAP'
GID_MAP='$GID_MAP'
TOUCH_PATH=$TOUCH_PATH
LOG=$LOG
EOF_INNER

echo
echo "| metric | value |"
echo "| --- | ---: |"
echo "| rpull | ${rpull_ms} ms |"
echo "| first user text appears | ${first_user_text_ms:-not seen} ms |"
echo "| task ready after run | ${task_ready_ms} ms |"
echo "| touch marker after run | ${touch_ms} ms |"
echo "| stop task | ${stop_ms} ms |"
echo "mutable_env_file=$ENV_FILE"
