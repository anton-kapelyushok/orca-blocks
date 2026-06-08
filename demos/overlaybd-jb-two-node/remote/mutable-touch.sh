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

echo "Executing local: assert OverlayBD rwMode=dev"
if [[ "$(jq -r .rwMode "$CONFIG")" != "dev" ]]; then
  echo "Expected rwMode=dev. Run demo cleanup first."
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
set +e
$CTR -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc "$IMAGE_REF" "$NAME" sh -lc "$MUTATION_CMD" > >(tee -a "$LOG") 2>&1 &
runner_pid=$!
set -e

run_start_ms=$(date +%s%3N)
task_ready_ms=""
touch_ms=""
deadline=$(( $(date +%s) + 30 ))
while true; do
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
while true; do
  if grep -q "__ORCA_DEMO_TOUCH_DONE__" "$LOG" 2>/dev/null; then
    touch_ms=$(( $(date +%s%3N) - run_start_ms ))
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
  exit 1
fi

echo "Executing local: find writable OverlayBD config"
# DEMO-CMD: grep -Rsl '"upper"' "$SNAPSHOT_ROOT/snapshots" | xargs -r ls -t | head -1
CONFIG_PATH="$(grep -Rsl '"upper"' "$SNAPSHOT_ROOT/snapshots" | xargs -r ls -t | head -1)"
WD="$(jq -r '.upper.data' "$CONFIG_PATH")"
WI="$(jq -r '.upper.index' "$CONFIG_PATH")"
echo "config_path=$CONFIG_PATH"
echo "writable_data=$WD"
echo "writable_index=$WI"

echo "Executing local: stop task but keep container/snapshot for overlaybd-commit"
# DEMO-CMD: $CTR -n "$NS" tasks kill -s SIGTERM "$NAME"; $CTR -n "$NS" tasks rm "$NAME"
touch_end_ms=$(date +%s%3N)
$CTR -n "$NS" tasks kill -s SIGTERM "$NAME" || true
sleep 2
$CTR -n "$NS" tasks kill -s SIGKILL "$NAME" || true
$CTR -n "$NS" tasks rm "$NAME" || true
wait "$runner_pid" || true
stop_end_ms=$(date +%s%3N)
stop_ms=$((stop_end_ms - touch_end_ms))

cat >"$ENV_FILE" <<EOF_INNER
NAME=$NAME
IMAGE_REF=$IMAGE_REF
CONFIG_PATH=$CONFIG_PATH
WD=$WD
WI=$WI
TOUCH_PATH=$TOUCH_PATH
LOG=$LOG
EOF_INNER

echo
echo "| metric | value |"
echo "| --- | ---: |"
echo "| rpull | ${rpull_ms} ms |"
echo "| task ready after run | ${task_ready_ms} ms |"
echo "| touch marker after run | ${touch_ms} ms |"
echo "| stop task | ${stop_ms} ms |"
echo "mutable_env_file=$ENV_FILE"
