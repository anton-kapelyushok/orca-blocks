set -euo pipefail
CTR=/opt/overlaybd/snapshotter/ctr
NS=moby
NAME="${CONTAINER_NAME:-demo-jb-verify-${RUN_ID}}"
WORK="/root/orca-overlaybd-demo-${RUN_ID}"
LOG="$WORK/$NAME.log"
mkdir -p "$WORK"

echo "image=$IMAGE_REF"
echo "container=$NAME"
echo "touch_path=$TOUCH_PATH"

echo "Executing local: cleanup any stale verification container/snapshot"
$CTR -n "$NS" tasks kill -s SIGKILL "$NAME" >/dev/null 2>&1 || true
$CTR -n "$NS" tasks rm "$NAME" >/dev/null 2>&1 || true
$CTR -n "$NS" containers rm "$NAME" >/dev/null 2>&1 || true
$CTR -n "$NS" snapshots --snapshotter overlaybd rm "$NAME" >/dev/null 2>&1 || true

start_ms=$(date +%s%3N)
echo "Executing local: $CTR -n $NS rpull --plain-http $IMAGE_REF"
# DEMO-CMD: $CTR -n "$NS" rpull --plain-http "$IMAGE_REF"
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

fifo="/tmp/$NAME.fifo"
rm -f "$fifo"
mkfifo "$fifo"

VERIFY_CMD="ls -l '$TOUCH_PATH'; cat '$TOUCH_PATH'; test -f '$TOUCH_PATH'; echo __ORCA_DEMO_VERIFY_DONE__"
echo "Executing local: $CTR -n $NS run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc --rm $IMAGE_REF $NAME sh -lc \"$VERIFY_CMD\""
# DEMO-CMD: $CTR -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc --rm "$IMAGE_REF" "$NAME" sh -lc "ls -l '$TOUCH_PATH'; cat '$TOUCH_PATH'; test -f '$TOUCH_PATH'; echo __ORCA_DEMO_VERIFY_DONE__" >"$fifo" 2>&1 &
set +e
$CTR -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc --rm "$IMAGE_REF" "$NAME" sh -lc "$VERIFY_CMD" >"$fifo" 2>&1 &
runner_pid=$!
set -e
exec 3<"$fifo"

first_ms=""
verify_ms=""
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while true; do
  if IFS= read -r -t 1 line <&3; then
    now_ms=$(date +%s%3N)
    printf '%s\n' "$line" | tee -a "$LOG"
    if [[ -z "$first_ms" ]]; then
      first_ms=$((now_ms - after_rpull_ms))
    fi
    if [[ "$line" == "__ORCA_DEMO_VERIFY_DONE__" ]]; then
      verify_ms=$((now_ms - after_rpull_ms))
    fi
  fi
  if ! kill -0 "$runner_pid" >/dev/null 2>&1; then
    break
  fi
  if (( $(date +%s) > deadline )); then
    echo "timeout waiting for first output"
    break
  fi
done
exec 3<&-
wait "$runner_pid"

if [[ -z "$verify_ms" ]]; then
  echo "verification marker was not seen"
  $CTR -n "$NS" tasks kill -s SIGKILL "$NAME" || true
  $CTR -n "$NS" tasks rm "$NAME" || true
  $CTR -n "$NS" containers rm "$NAME" || true
  rm -f "$fifo"
  exit 1
fi

echo "Executing local: cleanup verification container"
# DEMO-CMD: $CTR -n "$NS" containers rm "$NAME" || true
$CTR -n "$NS" tasks kill -s SIGTERM "$NAME" >/dev/null 2>&1 || true
sleep 1
$CTR -n "$NS" tasks kill -s SIGKILL "$NAME" >/dev/null 2>&1 || true
$CTR -n "$NS" tasks rm "$NAME" >/dev/null 2>&1 || true
$CTR -n "$NS" containers rm "$NAME" >/dev/null 2>&1 || true
rm -f "$fifo"

echo
echo "| metric | value |"
echo "| --- | ---: |"
echo "| rpull | ${rpull_ms} ms |"
echo "| first output before verify | ${first_ms:-not seen} ms |"
echo "| verify marker after run | ${verify_ms} ms |"
