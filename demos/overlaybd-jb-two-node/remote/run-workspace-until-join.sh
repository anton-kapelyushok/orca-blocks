set -euo pipefail
CTR=/opt/overlaybd/snapshotter/ctr
NS=moby
KEEP_AFTER_JOIN="${KEEP_AFTER_JOIN:-0}"
NAME="${CONTAINER_NAME:-demo-jb-run-${RUN_ID}}"
WORK="/root/orca-overlaybd-demo-${RUN_ID}"
LOG="$WORK/$NAME.log"
ENV_FILE="$WORK/$NAME.env"
mkdir -p "$WORK"

cleanup_name() {
  local name="$1"
  "$CTR" -n "$NS" tasks kill -s SIGTERM "$name" >/dev/null 2>&1 || true
  sleep 1
  "$CTR" -n "$NS" tasks kill -s SIGKILL "$name" >/dev/null 2>&1 || true
  "$CTR" -n "$NS" tasks rm "$name" >/dev/null 2>&1 || true
  "$CTR" -n "$NS" containers rm "$name" >/dev/null 2>&1 || true
}

echo "image=$IMAGE_REF"
echo "container=$NAME"
echo "log=$LOG"

start_ms=$(date +%s%3N)
echo "Executing local: $CTR -n $NS rpull --plain-http $IMAGE_REF"
# DEMO-CMD: "$CTR" -n "$NS" rpull --plain-http "$IMAGE_REF"
"$CTR" -n "$NS" rpull --plain-http "$IMAGE_REF"
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

echo "Executing local: $CTR -n $NS run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc --rm $IMAGE_REF $NAME"
# DEMO-CMD: "$CTR" -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc --rm "$IMAGE_REF" "$NAME" >"$fifo" 2>&1 &
set +e
"$CTR" -n "$NS" run --snapshotter overlaybd --runtime io.containerd.runc.v2 --cni --allow-new-privs --runc-binary /usr/bin/sysbox-runc --rm "$IMAGE_REF" "$NAME" >"$fifo" 2>&1 &
runner_pid=$!
set -e
exec 3<"$fifo"

first_ms=""
dock_ms=""
workspace_ms=""
join_ms=""
deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))
while true; do
  if IFS= read -r -t 1 line <&3; then
    now_ms=$(date +%s%3N)
    printf '%s\n' "$line" | tee -a "$LOG"
    if [[ -z "$first_ms" ]]; then first_ms=$((now_ms - after_rpull_ms)); fi
    if [[ -z "$dock_ms" && "$line" == *"Dock HTTP Api listening"* ]]; then dock_ms=$((now_ms - after_rpull_ms)); fi
    if [[ -z "$workspace_ms" && "$line" == *"Workspace Server listening"* ]]; then workspace_ms=$((now_ms - after_rpull_ms)); fi
    if [[ "$line" == *"Join this workspace using URL:"* ]]; then
      join_ms=$((now_ms - after_rpull_ms))
      break
    fi
  fi
  if ! kill -0 "$runner_pid" >/dev/null 2>&1; then
    break
  fi
  if (( $(date +%s) > deadline )); then
    echo "timeout waiting for Join URL"
    break
  fi
done
exec 3<&-

if [[ "$KEEP_AFTER_JOIN" == "1" && -n "$join_ms" ]]; then
  cat >"$ENV_FILE" <<EOF_INNER
NAME=$NAME
IMAGE_REF=$IMAGE_REF
LOG=$LOG
EOF_INNER
  echo "kept_running_env_file=$ENV_FILE"
  cleanup_ms="kept running"
else
  if [[ -n "$join_ms" ]]; then
    echo "Join URL detected at ${join_ms} ms; cleaning up container..."
  fi
  cleanup_start_ms=$(date +%s%3N)
  cleanup_name "$NAME"
  cleanup_end_ms=$(date +%s%3N)
  cleanup_ms="$((cleanup_end_ms - cleanup_start_ms)) ms"
fi
rm -f "$fifo"

echo
echo "| metric | value |"
echo "| --- | ---: |"
echo "| rpull | ${rpull_ms} ms |"
echo "| first user text appears | ${first_ms:-not seen} ms |"
echo "| Dock HTTP API | ${dock_ms:-not seen} ms |"
echo "| Workspace Server | ${workspace_ms:-not seen} ms |"
echo "| Join URL | ${join_ms:-not seen} ms |"
echo "| cleanup after run | ${cleanup_ms:-not run} |"
